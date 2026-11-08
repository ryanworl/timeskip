#!/bin/sh
# One ping before the drivers start.
. "$(dirname "$0")/helper_assert.sh"
r=$(echo "ping 0" | nc pong 7000)
echo "$r"
assert always "pong answers the handshake" "$(case $r in "pong 0 "*) echo true;; *) echo false;; esac)"
: > /tmp/sent; : > /tmp/answered
