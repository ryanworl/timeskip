#!/bin/sh
# pong comes back from every fault: it's reachable within a minute.
. "$(dirname "$0")/helper_assert.sh"
ok=false
for _ in $(seq 60); do
	if nc -z -w 1 pong 7000; then ok=true && break; fi
	sleep 1
done
assert always "pong is reachable" "$ok"
