# Usage: python3 -m venv .venv && .venv/bin/pip install pyte==0.8.2
#        .venv/bin/python -I tools/pty_smoke.py target/debug/tuimeta target/debug/tuimeta-helper /tmp/tuimeta-smoke
# Fake accounts only (--fake): never point it at a real session.
"""Drives `tuimeta --fake` in a pseudo-terminal and prints the screen after
each step, rendered with pyte. Fake accounts only: no network, no real data."""

import os
import pty
import select
import sys
import time

import pyte

APP = sys.argv[1]
HELPER = sys.argv[2]
DATA = sys.argv[3]
COLS, ROWS = 120, 34

screen = pyte.Screen(COLS, ROWS)
stream = pyte.ByteStream(screen)

os.makedirs(DATA, mode=0o700, exist_ok=True)
pid, fd = pty.fork()
if pid == 0:
    env = dict(os.environ)
    env.update(
        TERM="xterm-256color",
        TM_DATA_DIR=DATA,
        TM_HELPER=HELPER,
        COLUMNS=str(COLS),
        LINES=str(ROWS),
    )
    os.execve(APP, [APP, "--fake"], env)

import fcntl
import struct
import termios

fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))


def pump(seconds):
    end = time.time() + seconds
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.05)
        if r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                return
            if not data:
                return
            # Answer the terminal queries the app sends at startup, as a
            # terminal without graphics or keyboard enhancements would: the
            # primary device attributes end every query batch.
            if b"\x1b[c" in data:
                os.write(fd, b"\x1b[?62;22c")
            stream.feed(data)


def show(title):
    print(f"===== {title}")
    for line in screen.display:
        print(line.rstrip())


def send(keys, wait=0.4):
    os.write(fd, keys.encode())
    pump(wait)


def wait_for(text, seconds=20):
    end = time.time() + seconds
    while time.time() < end:
        pump(0.2)
        if any(text in line for line in screen.display):
            return True
    return False


print("login screen up:", wait_for("Log in to"))
show("start")
send("\r", 0.2)
print("cookies up:", wait_for("Messenger cookies"))
send("\x1b[200~c_user=1; xs=2; datr=3\x1b[201~", 1.0)
show("cookies pasted")
send("\r", 0.2)
print("chats up:", wait_for("Chats ("))
show("after login")
send("\r", 2.0)
show("chat opened")
send("i", 0.3)
for c in "hello from smoke":
    send(c, 0.05)
send("\r", 3.0)
show("after sending")
send("\x1b", 0.3)
send(":", 0.2)
for c in "login":
    send(c, 0.05)
send("\r", 0.8)
show(":login")
send("j\r", 0.5)
send("\x1b[200~sessionid=a; ds_user_id=b; csrftoken=c\x1b[201~\r", 3.0)
show("both logged in")
send("q", 2.0)
show("quit")
try:
    _, status = os.waitpid(pid, os.WNOHANG)
    print("exit status", status)
except ChildProcessError:
    print("exited")
