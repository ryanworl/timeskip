#!/usr/bin/env python3
# For --explore: the driver draws a number, reaches an edge (of a module of
# its own, through ABI v2), which is new to the run the first time, so a
# branch point; 2 s later asserts, a branch point too; then draws another
# number, which differs in each timeline branched from there (a branch
# point too, but withdrawn as the timeline fails 0.1 s later). Every
# timeline fails, so that its branch point is kept, for a replay.
import time

from antithesis.assertions import always, sometimes
from antithesis.random import get_random
from e2e_coverage import lease, module

sometimes(True, f"before: {get_random()}")
module(b"e2e-branch", 1, v2=True)(1)
always(lease(), "ABI v2 grants a lease")
time.sleep(2)
sometimes(True, "2 s in")
time.sleep(1)
sometimes(True, f"after: {get_random()}")
time.sleep(0.1)
always(False, "the timeline fails")
