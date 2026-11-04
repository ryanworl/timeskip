#!/bin/sh
# test.sh: test the kit ./build.sh built, end to end (e2e/). Needs a Mac, or
# Linux with KVM. Arguments go to the test binary: ./test.sh -test.run Demo
cd "$(dirname "$0")" || exit 2
[ -x out/e2e.test ] || { echo "test.sh: no out/e2e.test; run ./build.sh" >&2; exit 2; }
exec out/e2e.test -test.v -test.timeout 15m "$@"
