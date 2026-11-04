#!/bin/sh
# Five pings, one at a time; others run alongside. A ping that a fault
# holds up (a partition, or pong paused, stopped or killed) is sent again
# until it's answered.
. "$(dirname "$0")/helper_assert.sh"
for n in 1 2 3 4 5; do
	echo "$$.$n" >> /tmp/sent
	touch "/tmp/inflight.$$"
	until r=$(echo "ping $n" | nc -w 5 pong 7000) && case $r in "pong $n "*) true ;; *) false ;; esac; do
		sleep 1
	done
	echo "$r"
	assert sometimes "two pings are in flight at once" "$([ "$(ls /tmp/inflight.* | wc -l)" -gt 1 ] && echo true || echo false)"
	rm -f "/tmp/inflight.$$"
	echo "$$.$n" >> /tmp/answered
done
