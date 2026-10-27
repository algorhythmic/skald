"""Compare capture write amplification on a deterministic synthetic workload.

Each round appends one small record to every source, then waits for committed
archive counts and an empty delivery queue. Linux /proc I/O includes SQLite WAL
and checkpoint writes; it is kernel accounting, not SSD wear or net file growth.
No live source, daemon, configuration, or monitoring service is touched.
"""
import argparse
import hashlib
import json
import os
import sqlite3
from pathlib import Path
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]


def counters(pid):
    proc = Path('/proc') / str(pid)
    fields = (proc / 'stat').read_text().rsplit(')', 1)[1].split()
    io = {key: int(value) for key, value in
          (line.split(':') for line in (proc / 'io').read_text().splitlines())}
    return time.monotonic(), int(fields[11]) + int(fields[12]), io


def line(source, ordinal):
    token = hashlib.sha256(f'{source}/{ordinal}'.encode()).hexdigest()
    return (json.dumps(dict(type='assistant', sessionId=f'synthetic-{source}',
                           uuid=token, version='2.1.303',
                           message=dict(role='assistant', content='Synthetic capture evidence ' + token)),
                       separators=(',', ':')) + '\n').encode()


def measure(binary, args):
    with tempfile.TemporaryDirectory(prefix='skald-write-bench-', dir=args.work_dir) as directory:
        work = Path(directory)
        sources = work / 'sources'
        sources.mkdir(mode=0o700)
        registrations = []
        paths = []
        for source in range(args.sources):
            path = sources / f'{source}.jsonl'
            path.write_bytes(b''.join(line(source, n) for n in range(args.seed_records)))
            paths.append(path)
            registrations.append(dict(namespace='fixture:write-bench', provider='claude_code',
                                      stream_id=f'synthetic-{source}', root=str(sources), path=path.name))
        config = work / 'config.json'
        config.write_text(json.dumps(dict(version=1, poll_interval_ms=100,
                                         max_database_bytes=2 << 30, min_free_bytes=0,
                                         sources=registrations)))
        socket = work / 'run/skald.sock'
        data = work / 'archive'
        command = [str(binary), 'serve', '--config', str(config), '--data-dir', str(data), '--socket', str(socket)]
        process = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)

        def request(name):
            result = subprocess.run([str(binary), name, '--socket', str(socket)],
                                    capture_output=True, text=True, timeout=20)
            if result.returncode:
                raise RuntimeError('private daemon not ready')
            return json.loads(result.stdout)

        def wait(expected):
            deadline = time.monotonic() + 120
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    raise RuntimeError('synthetic daemon exited: ' + process.stderr.read())
                try:
                    state = request('status')
                    if state['record_versions'] == expected and state['capture_delivery']['pending'] == 0:
                        return state
                except RuntimeError:
                    pass
                time.sleep(.01)
            raise RuntimeError('synthetic capture did not settle')

        try:
            expected = args.sources * args.seed_records
            wait(expected)
            time.sleep(.4)  # Exclude initial backfill and first empty observations.
            first = counters(process.pid)
            before_metrics = request('metrics')
            native_bytes = 0
            latencies = []
            for round_number in range(args.rounds):
                started = time.monotonic()
                for source, path in enumerate(paths):
                    raw = line(source, args.seed_records + round_number)
                    native_bytes += len(raw)
                    with path.open('ab', buffering=0) as stream:
                        stream.write(raw)
                expected += args.sources
                wait(expected)
                latencies.append(time.monotonic() - started)
            final_state = wait(expected)
            after_metrics = request('metrics')
            last = counters(process.pid)
            elapsed = last[0] - first[0]
            delta = {key: last[2][key] - first[2][key] for key in last[2]}
            rss = {key: value.strip() for key, value in
                   (row.split(':', 1) for row in (Path('/proc') / str(process.pid) / 'status').read_text().splitlines())}
            # Reopen after shutdown to establish durable records and checkpoints.
            process.terminate()
            process.wait(timeout=30)
            process.stderr.close()
            with sqlite3.connect('file:' + str(data / 'archive.sqlite') + '?mode=ro', uri=True) as archive:
                archived = {key: json.loads(wire) for key, wire in archive.execute(
                    'SELECT stream_key,checkpoint_json FROM ingest_checkpoints')}
            with sqlite3.connect('file:' + str(data / 'spool/capture-spool.sqlite') + '?mode=ro', uri=True) as spool:
                produced = {key: json.loads(wire) for key, wire in spool.execute(
                    'SELECT source_key,checkpoint FROM source_progress')}
                accepted = {key: json.loads(wire) for key, wire in spool.execute(
                    'SELECT source_key,checkpoint FROM destination_progress WHERE destination=?',
                    ('skald:standalone-archive',))}
            if len(archived) != args.sources or archived != produced or archived != accepted:
                raise AssertionError('durable source and destination checkpoints diverged')
            process = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
            recovered = wait(expected)
            if recovered['archive_instance'] != final_state['archive_instance']:
                raise AssertionError('archive identity changed on restart')
            return dict(binary=str(binary), binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),
                        sources=args.sources, seed_records_per_source=args.seed_records,
                        rounds=args.rounds, added_records=args.rounds * args.sources,
                        native_input_bytes=native_bytes, seconds=elapsed,
                        cpu_seconds=(last[1] - first[1]) / os.sysconf('SC_CLK_TCK'),
                        io_bytes=delta, accounted_storage_write_amplification=delta['write_bytes'] / native_bytes,
                        logical_write_amplification=delta['wchar'] / native_bytes,
                        mean_round_seconds=sum(latencies) / len(latencies), max_round_seconds=max(latencies),
                        final_rss=rss['VmRSS'], restart_verified=True, checkpoint_parity_verified=True,
                        metrics_delta={key: after_metrics['counters'].get(key, 0) - value
                                       for key, value in before_metrics['counters'].items()})
        finally:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=30)
            if not process.stderr.closed:
                process.stderr.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', action='append', type=Path, help='repeat for baseline and new build')
    parser.add_argument('--sources', type=int, default=16)
    parser.add_argument('--seed-records', type=int, default=512)
    parser.add_argument('--rounds', type=int, default=20)
    parser.add_argument('--work-dir', type=Path, default=ROOT / '.cache',
                        help='existing directory on the filesystem to measure (default: repo .cache; /tmp may be tmpfs)')
    parser.add_argument('--output', type=Path, help='new JSON receipt; existing files are not overwritten')
    args = parser.parse_args()
    if not 1 <= args.sources <= 64 or not 1 <= args.seed_records <= 4096 or not 1 <= args.rounds <= 100:
        parser.error('invalid bounded workload')
    binaries = [p.resolve() for p in (args.binary or [ROOT / 'bin/skald'])]
    results = [measure(binary, args) for binary in binaries]
    encoded = json.dumps(results, indent=2) + '\n'
    if args.output:
        with args.output.open('x') as stream:
            stream.write(encoded)
    print(encoded, end='')


if __name__ == '__main__':
    main()
