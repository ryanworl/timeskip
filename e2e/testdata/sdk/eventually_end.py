#!/usr/bin/env python3
# The ending, at a moment drawn as the drivers start: it says when, by the
# VM's uptime, which each branch of a timeline draws anew.
from antithesis.assertions import sometimes

with open("/proc/uptime") as f:
    up = float(f.read().split()[0])
sometimes(True, f"ending at {up:.2f}")
