"""Measure an unchanged synthetic archive, including stalled large-record tails.

Linux /proc counters report CPU, logical I/O and accounted storage writes. Source
files are private synthetic fixtures; the live daemon/configuration is untouched.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]


def counters(pid):
    proc = Path('/proc') / str(pid)
    fields = (proc / 'stat').read_text().split(') ', 1)[1].split()
    io = {key: int(value) for key, value in
          (line.split(':') for line in (proc / 'io').read_text().splitlines())}
    return time.monotonic(), int(fields[11]) + int(fields[12]), io


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, default=ROOT / 'bin/skald')
    parser.add_argument('--sessions', type=int, default=174)
    parser.add_argument('--seconds', type=float, default=10)
    parser.add_argument('--blocked', type=int, default=0)
    parser.add_argument('--prefix-mib', type=int, default=8)
    parser.add_argument('--work-dir', type=Path, help='existing directory on the filesystem to measure (default: system temporary directory)')
    args = parser.parse_args()
    if not 0 <= args.blocked <= args.sessions <= 4096 or args.sessions < 1 or not 0 < args.seconds <= 300 or not 1 <= args.prefix_mib <= 32:
        parser.error('invalid bounded workload')
    with tempfile.TemporaryDirectory(prefix='skald-idle-', dir=args.work_dir) as directory:
        work = Path(directory)
        sources = work / 'sources'
        sources.mkdir(mode=0o700)
        registrations = []
        for i in range(args.sessions):
            sid = f'fixture-{i}'
            content = 'Synthetic idle evidence.' if i >= args.blocked else 'x' * (1 << 20)
            line = json.dumps(dict(type='assistant', sessionId=sid, uuid=sid,
                                  message=dict(role='assistant', content=content))) + '\n'
            with (sources / f'{sid}.jsonl').open('w') as source:
                if i < args.blocked:
                    for j in range(args.prefix_mib):
                        source.write(line.replace('"uuid": "' + sid + '"', '"uuid": "' + sid + '-' + str(j) + '"'))
                    source.write('x' * ((4 << 20) + 1) + '\n')
                else:
                    source.write(line)
            registrations.append(dict(namespace='fixture:idle', provider='claude_code', stream_id=sid,
                                      root=str(sources), path=f'{sid}.jsonl'))
        config = work / 'config.json'
        config.write_text(json.dumps(dict(version=1, poll_interval_ms=1000, max_database_bytes=2 << 30,
                                         min_free_bytes=0, sources=registrations)))
        socket = work / 'run/skald.sock'
        process = subprocess.Popen([str(args.binary.resolve()), 'serve', '--config', str(config),
                                    '--data-dir', str(work / 'archive'), '--socket', str(socket)],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)

        def status():
            result = subprocess.run([str(args.binary.resolve()), 'status', '--socket', str(socket)],
                                    capture_output=True, text=True, timeout=20)
            if result.returncode:
                raise RuntimeError('status not ready')
            return json.loads(result.stdout)

        try:
            expected = args.sessions + args.blocked * (args.prefix_mib - 1)
            deadline = time.monotonic() + 90
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    raise RuntimeError(process.stderr.read())
                try:
                    ready = status()
                    if ready['record_versions'] == expected and sum(s['health'] == 'blocked' for s in ready['sources']) == args.blocked:
                        break
                except RuntimeError:
                    pass
                time.sleep(0.2)
            else:
                raise RuntimeError('capture did not settle')
            # Exclude first-run creation/checkpoint writes and first empty poll.
            time.sleep(2)
            before = status()
            first = counters(process.pid)
            time.sleep(args.seconds)
            last = counters(process.pid)
            after = status()
            elapsed = last[0] - first[0]
            print(json.dumps(dict(binary=str(args.binary.resolve()), sessions=args.sessions, blocked=args.blocked,
                                  blocked_prefix_mib=args.prefix_mib, seconds=round(elapsed, 3),
                                  cpu_one_core_percent=round((last[1] - first[1]) / os.sysconf('SC_CLK_TCK') / elapsed * 100, 3),
                                  io_per_second={key: round((last[2][key] - first[2][key]) / elapsed, 3) for key in last[2]},
                                  record_delta=after['record_versions'] - before['record_versions'],
                                  boundary_delta=after['boundary'] - before['boundary']), indent=2))
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)


if __name__ == '__main__':
    main()
