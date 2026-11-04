#!/usr/bin/env python3
# For the ending drawn anew: every launch asserts the same, so only the first
# timeline's first launch reaches a state new to the run, a branch point.
from antithesis.assertions import sometimes

sometimes(True, "tick")
