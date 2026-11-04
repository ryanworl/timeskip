#!/bin/sh
# Every driver has finished: every ping got its answer.
. "$(dirname "$0")/helper_assert.sh"
echo "$(wc -l < /tmp/answered) of $(wc -l < /tmp/sent) pings answered"
assert always "every ping is answered" "$([ "$(wc -l < /tmp/answered)" = "$(wc -l < /tmp/sent)" ] && echo true || echo false)"
