# lockd: a renewal that races its lease's expiry

`lockd` (`lockd/main.go`) is a lock service with leases. `POST /acquire`
grants a free lock with a token and a 1 s lease; the holder keeps it with
`POST /renew` and gives it up with `POST /release`, and a timer frees it when
the lease runs out. Before it acknowledges a grant or a renewal, lockd
replicates it to a standby (the same binary), without holding its mutex
across that network call.

**The bug.** A renewal checks the lease under the mutex, lets go of it,
replicates (0.1 to 0.2 ms), then takes the mutex again and extends the lease,
without checking that it's still the same lease. If the lease's timer fires
during the replication, the lease is deleted, a polling client acquires the
lock, and the renewal still answers `ok`: two clients hold the lock, the
renewing one for another full second. With `LOCKD_FIX=1`, the renewal checks
again after replicating and refuses if the lease is gone.

It hides well: a client renews at the end of a work step (up to 0.6 s), once
less than half its lease is left, so nearly every renewal lands hundreds of
milliseconds before the expiry. Only one sent in the last fraction of a
millisecond of its lease races the timer, and that takes a long step ending
just in time.

**The test.** The `client` service holds the test template
(`client/test`, at `/opt/antithesis/test/v1/lockd`): each
`parallel_driver_client.sh` runs `client` (`client/main.go`), which contends
for one lock for ten minutes of guest time: it polls to acquire the lock,
works a few random steps, renews and releases. Each time it gets or keeps the
lock it notes its token and deadline (TTL after it sent the request) in a
shared file, and asserts `Always` "two clients never hold the lock at once"
if another client's deadline there is still ahead. `Sometimes` "a client
takes over the lock from a holder whose lease ran out" shows that expiries
happen. A timeline (10 clients, the default `drivers: 10`) is ten minutes of
guest time, almost all of it asleep.

```sh
timeskip test examples/lockd/compose.yaml --timelines 60 -j 6
echo LOCKD_FIX=1 > examples/lockd/.env     # the fix (Compose reads .env, not the shell, as in Antithesis)
timeskip test examples/lockd/compose.yaml --timelines 60 -j 6
```

**Cost to find it.** On a 12-core Linux host (60 timelines each, `-j 6`,
the host shared with other work; CPU is user+sys of `timeskip test` and its
VMs, guest time is the sum of the timelines' `guest_ms`, what the same
timelines would take natively):

| lockd | Faults | Timelines | Failed | CPU-s | Wall s | Guest s |
| --- | --- | --- | --- | --- | --- | --- |
| buggy, `--seed 1000` | default | 60 | 59 | 601 | 96 | 36,250 (10.1 h) |
| fixed, `--seed 1000` | default | 60 | 0 | 613 | 101 | 36,266 (10.1 h) |
| buggy, `--seed 1000` | `--set faults=none` | 60 | 3 | 651 | 148 | 36,226 (10.1 h) |
| buggy, `--seed 2000` | `--set faults=none` | 60 | 4 | 499 | 94 | 36,241 (10.1 h) |
| fixed, `--seed 1000` | `--set faults=none` | 60 | 0 | 584 | 140 | 36,238 (10.1 h) |

So a run of about 600 CPU-seconds and under two minutes, where running the
same timelines natively would take ten hours, finds it in nearly every
timeline with the default faults. (client is labeled to be spared stops and
kills, which would end every client and the timeline with them.) Without
faults a timeline makes about 800 renewals, and about 1 in 17 timelines
fails: roughly 1 renewal in 13,000 races the expiry. Replaying a failing
seed doesn't reliably fail again: the race turns on a fraction of a
millisecond of compute time, which varies from run to run.
