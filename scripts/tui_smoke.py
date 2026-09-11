"""Compiled TUI acceptance: private Unix daemon, PTY input, resize and termios cleanup."""
import fcntl
import os
import pty
import select
import signal
import struct
import subprocess
import termios
import time

from tui_demo import BINARY, fixture_daemon


def child_terminal():
    os.setsid()
    fcntl.ioctl(0, termios.TIOCSCTTY, 0)


with fixture_daemon() as (socket, daemon):
    for shutdown in ("q", "SIGTERM"):
        master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 120, 0, 0))
        before = termios.tcgetattr(slave)
        environment = {**os.environ, "TERM": "xterm-256color", "COLORTERM": "truecolor"}
        if shutdown == "q":
            environment.pop("NO_COLOR", None)
        else:
            environment["NO_COLOR"] = "1"
        argv = [str(BINARY), "tui", "--socket", str(socket)]
        if shutdown == "q":
            argv += ["--theme", "desktop"]
        proc = subprocess.Popen(argv, stdin=slave, stdout=slave, stderr=slave,
                                env=environment, preexec_fn=child_terminal)
        output = bytearray()

        def drain_until(token, timeout=10):
            end = time.monotonic() + timeout
            while time.monotonic() < end:
                if select.select([master], [], [], 0.1)[0]:
                    output.extend(os.read(master, 65536))
                if any(part in output for part in (token if isinstance(token, tuple) else (token,))):
                    return
                if proc.poll() is not None:
                    break
            raise AssertionError(f"TUI output missing {token!r}: {bytes(output[-2000:])!r}")

        try:
            drain_until(b"Review the conversation archive")
            color_sequences = (b"38;2;", b"38:2:", b"48;2;", b"48:2:")
            if shutdown == "q":
                assert not any(seq in output for seq in color_sequences), "desktop theme hardcodes RGB colors"
                palette_sequences = (b"[33m", b"[32m", b"[38;5;3m", b"[38;5;2m", b"[38:5:3m", b"[38:5:2m")
                assert any(seq in output for seq in palette_sequences), "terminal palette colors missing"
                # Switch the running TUI to the original amber theme.
                os.write(master, b"t")
                drain_until(color_sequences)
            else:
                assert not any(seq in output for seq in color_sequences), "NO_COLOR not honored"
            os.write(master, b"\r")
            drain_until(b"transcript")
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 18, 60, 0, 0))
            output.clear()
            proc.send_signal(signal.SIGWINCH)
            drain_until(b"help")
            if shutdown == "q":
                os.write(master, b"q")
            else:
                proc.terminate()
            end = time.monotonic() + 5
            while proc.poll() is None and time.monotonic() < end:
                if select.select([master], [], [], 0.05)[0]:
                    output.extend(os.read(master, 65536))
            proc.wait(timeout=1)
            assert proc.returncode == 0, proc.returncode
            after = termios.tcgetattr(slave)
            assert before == after, "TUI did not restore terminal modes"
            assert daemon.poll() is None, "quitting TUI stopped daemon"
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
            os.close(master)
            os.close(slave)
print("TUI acceptance passed: desktop palette, live amber switch, monochrome, keyboard input, resize, quit/SIGTERM restore terminal, daemon stays running.")
