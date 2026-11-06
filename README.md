# timeskip

Runs a Docker Compose project's tests in single-vCPU Linux VMs whose kernel skips idle time: when every thread is waiting on a timer, the clock jumps to the next one. Tests that mostly sleep, poll or wait on timeouts finish in a fraction of the wall time. Each test timeline starts from a checkpoint saved after setup, with its own seed for the schedule, the faults and the guest's random numbers. The VMs have no network.

Tests follow [Antithesis's Test Composer](https://antithesis.com/docs/product/writing_tests/test_templates/test_composer_reference/) conventions, so the same project runs unchanged in Antithesis.

timeskip is not a security boundary: only run code you trust.

## Install

macOS 15+ on Apple silicon, or Linux on x86_64 with KVM (host kernel 5.16+, access to `/dev/kvm`, `mkfs.ext4`). Both need Docker.

```sh
curl -fsSL https://github.com/ryanworl/timeskip/releases/latest/download/install.sh | sh
```

To build from source, see [AGENTS.md](AGENTS.md).

## Try it

```sh
cd ~/.local/share/timeskip
./timeskip test examples/pingpong/compose.yaml                 # one timeline
./timeskip test examples/lockd/compose.yaml --timelines 60     # 60 timelines; finds lockd's race
```

## Your project

`timeskip test path/to/compose.yaml` builds the project as `docker compose up --build` would, bundles its images, sets it up once and runs timelines from that checkpoint. In the project:

- **Setup complete:** a container appends `{"antithesis_setup": {"status": "complete", "details": null}}` to `$ANTITHESIS_OUTPUT_DIR/sdk.jsonl` (the Antithesis SDKs' `setup_complete` does this).
- **Tests:** executables in `/opt/antithesis/test/v1/<template>/` inside a container, named with Test Composer prefixes: `first_`, `parallel_driver_`, `serial_driver_`, `singleton_driver_`, `anytime_`, `finally_`, `eventually_`.
- **Assertions:** the Antithesis SDKs, or JSON lines appended to `$ANTITHESIS_OUTPUT_DIR/sdk.jsonl`.
- **Edits:** `develop.watch` sync rules, as for `docker compose watch`, apply your host-side changes to each timeline.
- **Settings:** an optional `x-timeskip` block in the compose file; override any key with `--set KEY=VALUE`.

  ```yaml
  x-timeskip:
    faults: high_availability   # resilient, legacy or none
    disk_faults: false          # true, or a list of fail, full, power
    fault_gap: 10               # guest seconds between faults, roughly
    drivers: 10                 # driver launches per timeline
    timeout: 3600               # guest seconds a timeline may run
    memory: 4                   # VM memory, GiB
    timer_slack: 1ms            # how late the guest's timers may fire
  ```

Useful flags: `--timelines N`, `--seed N`, `--template NAME`, `-j N` (VMs at once; default one per core), `--no-skip` (real time), `--out DIR` (writes `results.json`), `--explore` (branch timelines from new behavior), `--max-failures N` (stop once N timelines failed; `first:N`, in their `first_` command), `--profile`. `timeskip bundle -o app.tsb` writes a bundle that runs elsewhere with `timeskip test app.tsb`.

## Results

timeskip prints a line per timeline and per property, with a replay command for each failure. It keeps each failed timeline's console log and `.containers.log` (every container's output, timed in guest seconds) in `~/.cache/timeskip/logs` (the last 10 runs'), or with `--out DIR` in `DIR/logs/TIME`, a directory of the run's own.

Exit codes: 0 passed, 1 failed, 125 setup failed, 130 interrupted, 2 usage or harness error.

- **Fails only with faults:** replay with `--set faults=none`, then compare the timeline's faults (in its log or `results.json`) with the failure.
- **Setup failed:** usually no container wrote `setup_complete` within 600 s of guest time, a service exited, or something needs the network.
- **Slow:** something never sleeps (a busy loop, a tight poll); time skips only when every thread waits.

## More

[docs/reference.md](docs/reference.md) has the details: project conventions, scheduling, faults, results fields, exploring, how it works, the guest protocol, and differences from Antithesis. Building, testing and the source layout are in [AGENTS.md](AGENTS.md).
