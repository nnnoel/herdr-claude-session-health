#!/usr/bin/env python3
"""Safe synthetic memory growth for manually testing Claude Session Health.

Run it from a shell tool inside a Claude Code session so it becomes a
descendant of that session's claude process. It touches pages so RSS really
grows, never exceeds --cap-mb, and exits by itself after --minutes.
"""
import argparse
import time

p = argparse.ArgumentParser()
p.add_argument("--mb-per-min", type=float, default=15.0)
p.add_argument("--cap-mb", type=float, default=900.0)
p.add_argument("--minutes", type=float, default=45.0)
a = p.parse_args()

step = int(a.mb_per_min * 1024 * 1024 / 6)  # every 10 s
cap = int(a.cap_mb * 1024 * 1024)
deadline = time.time() + a.minutes * 60
chunks, held = [], 0
while time.time() < deadline:
    if held + step <= cap:
        b = bytearray(step)
        for i in range(0, step, 4096):
            b[i] = 1  # touch every page so it becomes resident
        chunks.append(b)
        held += step
    time.sleep(10)
print(f"synthetic growth done: held {held / 2**20:.0f} MB")
