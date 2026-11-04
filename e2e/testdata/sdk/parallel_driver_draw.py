#!/usr/bin/env python3
from antithesis.assertions import always, sometimes
from antithesis.random import get_random
from e2e_coverage import reach

n = get_random()
sometimes(True, f"drew {n}")
always(n < 2**64, "the driver drew a number")
reach(1)
reach(2 + n % 3)  # f1's second edge, or one of f2's
