---
name: using-timeskip
description: "Runs a Docker Compose project's Antithesis-style test templates in timeskip: single-vCPU Linux VMs whose kernel skips idle time, many timelines in parallel, each restored from a checkpoint saved after setup. Covers fitting a project, writing test commands and assertions, the edit-and-test loop with develop.watch, settings, reading results, and diagnosing slow or failed runs. Use when testing a Compose project with timeskip or running a .tsb bundle."
---

# Using timeskip

`timeskip` runs a Compose project in VMs where the clock jumps ahead whenever every thread is waiting on a timer, and drives it with test commands the way Antithesis's Test Composer does. This assumes `timeskip` is installed (the kit's `./timeskip`); to build and install it, see the README.

## Make the project fit

- **The system** is the default-profile services. Every image goes into the bundle, pulled per `pull_policy`: the VM has no network. A service built from source needs an `image:` name; its `build:` may use only `context`, `dockerfile`, `args` and `target`. On a Mac, `platform: linux/amd64` and amd64-only images run emulated (slower). Bind mounts and `file:` configs and secrets of files in the compose file's directory are bundled; other host paths are refused (bundling fails and says so), but the VM's own `/var/run/docker.sock`, `/proc`, `/sys` and `/dev` are fine. No `internal: true` networks. Compose variables come from the project's `.env`, not the shell, as in Antithesis.
- **No test templates** is fine: the workload then runs by itself (in the entrypoints) for `timeout` seconds per timeline (see Settings), faults included.
- **Setup complete**: once the system is ready, some container appends `{"antithesis_setup": {"status": "complete", "details": null}}` to `$ANTITHESIS_OUTPUT_DIR/sdk.jsonl` (or calls an Antithesis SDK's `setup_complete`). Usually the test container's entrypoint does it after polling the services, then stays up (`exec sleep 2147483647`). Everything before it, loading fixture data included, is saved once as the checkpoint every timeline starts from.
- **Test commands** live in `/opt/antithesis/test/v1/<template>/` in a running container, executable, named by when they run:
  - `first_*`: setup for the template (one per timeline, before anything else).
  - `parallel_driver_*`: the workload, run alongside each other; `serial_driver_*`: run alone; `singleton_driver_*`: the only driver of its timeline (a ported integration test).
  - `anytime_*`: invariant checks at random moments.
  - `finally_*`: after every driver finished (checks on the final state); `eventually_*`: after killing whatever runs (checks recovery).
  - `helper_*` and subdirectories: ignored; shared code. Any other file there (no prefix, or not executable) is an invalid command and fails every timeline.
  Prefer several small commands over one big one; each template is a separate strategy.
- **Assertions**: use an Antithesis SDK (it writes through the `/usr/lib/libvoidstar.so` timeskip mounts in, as on Antithesis), or have a command append lines to `$ANTITHESIS_OUTPUT_DIR/sdk.jsonl` itself: `{"antithesis_assert": {"id": "every write is readable", "message": "every write is readable", "assert_type": "always", "display_type": "Always", "condition": true, "hit": true, "must_hit": true}}`. `assert_type` is `always`, `sometimes` or `reachability`. A command that exits non-zero fails too, as do a container exiting with a code other than 0, 137 or 143 and a process killed by SIGSEGV, SIGABRT and the like.
- **Edits**: each component you rebuild on the host, test commands included, has a `develop.watch` sync rule, e.g. `{action: sync, path: ./test, target: /opt/antithesis/test/v1/main}` or `{action: sync+restart, path: ./bin/api, target: /usr/local/bin/api}`. Edits elsewhere in a build context rebuild the image by themselves; a `rebuild` rule is only needed for paths outside it.
- **Tests that start containers through Docker** (dockertest, Testcontainers): give the test container the VM's `/var/run/docker.sock` and `network_mode: host`, and list each image the tests start, under its exact name, in a never-started profile so it's bundled: the VM can't pull.
- Polling is fine but each poll is a wake-up: long poll intervals and healthcheck `interval`s keep the speedup high. `results.json`'s `wakers` (and the run's `most wake-ups` line) name the processes that woke up most in each timeline, with `every_ms`, their average interval.

## The loop

```sh
timeskip test                                      # compose.yaml here: bundles (needs Docker), sets up once, runs a timeline per template
GOOS=linux go build -o bin/api ./cmd/api           # rebuild one component on the host...
timeskip test                                      # ...and test it: bin/api is synced in
timeskip test --timelines 20                       # look for rare failures: 20 seeds per template, in parallel
timeskip test --seed 1234 --template main          # rerun a failing timeline's schedule (the replay line it printed)
```

- Setup applies the sync rules as `docker compose watch` would, to the containers before they start, as if their images held the files: files that differ from the image's are copied in (Compose's ignore rules, host owners), and files deleted on the host since the image was built are deleted (`sync+exec` rules are refused). So setup runs what you test, and a changed synced file sets the project up again (from its containers created, its images not loaded again). A synced binary runs in a Linux container on the host's architecture (arm64 on a Mac, amd64 on Linux): one for the wrong OS or architecture, or missing its execute bit, fails setup (exit 125).
- An image is rebuilt when its build inputs changed. A rebuilt image or a `compose.yaml` change sets the project up again before the timelines run; the images the project pulls stay loaded in a checkpoint of their own. Once a run has bundled and set up the project, later runs that rebuild nothing (only synced files changed, which sets the project up again) work with Docker stopped, leaving its VM's CPU and memory to timeskip's; so do new setups after a kit update or a new `memory`. So do changes to `compose.yaml` or bind-mounted files, which re-bundle with the cached images. So does a new checkout of the project (a git worktree) whose images' build inputs match another checkout's. One that needs a build says to start Docker.
- `--max-failures N` stops a run once N timelines failed, and `--max-failures first:N` once N failed in their `first_` command (or before it): a broken build then costs one timeline, not all of them. The timelines not run show as such.
- `--no-skip` runs the same timelines in real time, for comparison. `--no-build` takes the images as they are (built some other way), as Antithesis does.
- `timeskip bundle -o app.tsb` packs what `test` would test, synced files included, for `timeskip test app.tsb` on another machine of the same architecture. The same project gives the same bytes.

## Settings and faults

The compose file's `x-timeskip` block sets how timelines run; `--set KEY=VALUE` overrides a key for one run:

```yaml
x-timeskip:
  faults: high_availability   # or resilient, legacy, none
  disk_faults: false          # or true, or a list of fail, full and power
  fault_gap: 10               # guest seconds between faults, about
  drivers: 10                 # driver launches per timeline
  timeout: 3600               # guest seconds a timeline may run
  memory: 4                   # the VMs' memory, GiB
  timer_slack: 1ms            # how late the guest's timers may fire; larger: fewer wakeups, faster timelines
```

- **Faults are on by default**, by Antithesis's fault tolerance levels, from the end of the `first_` command until the ending: baseline latency, partitions and clogs (`legacy`); plus pauses, throttles, CPU modulation and clock skips (`resilient`); plus stops and kills of containers, then restarts (`high_availability`). Thread pausing runs at every level. Timelines mix them: each draws its gap, and half leave node faults out. docs/reference.md's Faults has the details.
- **Exclusions:** a `com.antithesis.exclude_from_faults` label listing `network`, `pause`, `stop`, `kill`, `throttle`, `clock` and `disk` spares a container's node from those; label a harness container that must stay up with all of them. Excluding any container from `clock` turns clock skips off. Measure durations with a monotonic clock, which never jumps.
- **Disk faults** (`disk_faults`; Antithesis has none) give each node an ext4 disk of its own: `fail` returns `EIO` for data and metadata operations, `full` returns `ENOSPC`, and `power` makes half the kills power losses that drop unsynced data and metadata. Assert durability only of what was `fsync`ed before it was acknowledged, and label a test container that keeps unsynced records with `disk`.
- `com.timeskip.zone` and `com.timeskip.region` labels put nodes in zones and regions, which some faults take as a whole.
- Pair faults with Sometimes assertions that they were felt and Always assertions on recovery in the ending. `"$ANTITHESIS_STOP_FAULTS" 10` heals the faults and holds off new ones for 10 s, as in Antithesis.

## Results

timeskip prints a line per timeline, the commands and containers that used the most CPU (idle time is skipped, so CPU time is what makes a run slow), the coverage of code instrumented for Antithesis per module and whether it's levelling off, and a line per property, as Antithesis would report it over all timelines, with a replay line for each failed one. It exits 0 if everything passed, 1 if not, 125 if setup failed, 130 if interrupted, 2 for a usage or harness error. With one timeline the console streams, each command's output prefixed `[template/command]`; with more, failed timelines' logs are kept (with `--out DIR`, in the run's own `DIR/logs/TIME`; else in the cache, the last 10 runs') and their path printed. A failed timeline (or setup) also keeps `.containers.log`: every container's stdout and stderr, merged, each line timed in guest seconds since the timeline started. `--out DIR` writes `results.json` (every timeline's seed, commands, assertions, faults and times; every property with its failing seeds) and each instrumented module's functions, reached or not; with `--profile`, also each timeline's CPU profile as folded stacks. `--explore` branches half the timelines from moments that reached new edges or assertion states; a failure from a branch replays with the `--branch DIR --seed N` it prints.

The same seed replays the same schedule (which commands, in what order, with what pauses), faults and kernel random numbers, not necessarily the same outcome: the guest's clock runs at the host's speed while it computes, so timing still varies a little, and with it races.

## When it goes wrong

- **Exit 125.** Setup failed: the output shows `setup failed: ...` with `docker compose ps` and the services' last logs, and the paths of the whole setup log and the containers' logs. Common causes: no container wrote setup_complete (it gives up after 600 s of guest time), a service exits non-zero (other than 137 or 143) or needs the network.
- **Slow (wall time close to guest time).** Something never sleeps: a busy loop or a short poll interval. Time only skips when every thread waits on a timer; CPU-bound work runs at real speed and shares one vCPU with everything.
- **Fails only with faults.** Rerun the failing seed with `--set faults=none`: if it passes, read the timeline's faults (in its log or `results.json`) next to the failure. A harness container that faults take down needs an exclusion label.
- **A Sometimes assertion fails.** It was never true in any timeline: run more timelines, or the behavior really doesn't happen.
- **Timing-sensitive tests.** The guest clock runs ahead of the host's: compare times measured inside the VM only.

timeskip isolates tests for repeatability, not security: only run code you trust.
