# timeskip

A CLI that runs a Docker Compose project's Antithesis-style test templates in single-vCPU Linux VMs whose patched kernel skips idle time, each timeline restored from a checkpoint. README.md is the short user documentation; docs/reference.md is the full reference and the spec of the project conventions, the scheduling and the host–guest control channel.

## Build and test

- `./install-deps.sh`: checks for and sets up what building and running needs (Docker, which it installs only on macOS, as Colima; the Xcode command line tools on macOS; access to `/dev/kvm` on Linux), asking before it changes anything. Rerunnable.
- `./build.sh`: the kit for this machine in `out/` (`timeskip-macos-arm64.tgz` or `timeskip-linux-x86_64.tgz`), plus `out/e2e.test` and the fixture bundles in `out/tests`. It needs Docker, and on macOS the Xcode command line tools. The build itself (`cmd/build`) runs in the builder container `cmd/build/builder.Dockerfile`.
- `./build.sh lint`: gofmt, go vet (darwin and linux, with `-tags e2e`) and shellcheck. Keep it clean.
- `go test ./...`: runs anywhere. The end-to-end tests are behind the `e2e` build tag.
- `./test.sh`: the end-to-end tests (`e2e/`) against the built kit. They need macOS on Apple silicon, or Linux on x86_64 with `/dev/kvm`, and Docker. Pass test flags through, e.g. `./test.sh -test.run Protocol`. Changes to the CLI, the guest or the kernel need a run on both platforms.
- `./build.sh clean`: remove `out/` and the cache volume.
- Releases: tag the commit `vX.Y.Z` and push the tag; on a Mac and on Linux, fetch the tags (`git fetch --tags`) and build the tag (`./build.sh`); check that both kits' `timeskip version` shows `vX.Y.Z` (it shows the tag, from `git describe`: e.g. `tar -xzf out/timeskip-*.tgz -C /tmp timeskip/timeskip timeskip/MANIFEST && /tmp/timeskip/timeskip version`). Then create the GitHub release of ryanworl/timeskip as a draft (`gh release create vX.Y.Z --draft --verify-tag`), upload both platforms' `out/timeskip-*.tgz` and `.tgz.sha256`, and `install.sh` (`gh release upload vX.Y.Z FILES`), and publish it (`gh release edit vX.Y.Z --draft=false`), not as a pre-release: `install.sh` downloads from releases/latest, which skips pre-releases.

Everything but `hvrun` builds in the pinned builder container. Rebuilds are incremental (seconds); a build from nothing takes minutes, mostly the kernel and, on Linux, Firecracker.

Build steps are cached by their inputs (see `step` in `cmd/build/main.go`). Changing `deps.lock`, `go.mod`, `vendor/`, `cmd/build` or `internal/archive` reruns every step, including the kernel's (incremental) make.

## Layout

| Path | What |
| --- | --- |
| `cmd/timeskip/` | The CLI |
| `cmd/guest/` | The guest agent: setup, edits, the test command scheduler and the fault injector |
| `internal/bundle/` | `timeskip bundle`: a Compose project to a bundle |
| `internal/protocol/` | The host–guest control channel: control lines, the GO fields and the guest's reports |
| `hv/` | `hvrun`, the macOS VMM on Hypervisor.framework (Swift) |
| `guest/` | The kernel patch and configs, `init`, and `libvoidstar.c`, the library the Antithesis SDKs write their output through |
| `firecracker/` | Patches to Firecracker: a paused VM's clock stands still, and a snapshot doesn't sync its disks |
| `examples/` | The examples the kit ships |
| `docs/reference.md` | The full user reference: conventions, scheduling, faults, results, internals and the protocol |
| `e2e/` | The end-to-end tests, and in `testdata/protocol` a fixture for each of the protocol's cases |
| `install.sh` | Installs a release's kit (or one you built) |
| `install-deps.sh`, `build.sh`, `cmd/build/`, `internal/archive/`, `deps.lock`, `test.sh` | Build dependencies, build and test |

## Rules

- Go code uses the standard library only, except `github.com/moby/patternmatcher` (vendored), the matcher Compose uses for `develop.watch`; and no cgo: the CLI is cross-compiled for macOS from the Linux builder, and the guest agent (`cmd/guest`, Linux only) for the guest's architecture.
- Builds are reproducible: the same commit gives the same kit on the same kind of host (on macOS, with the same Swift toolchain, which builds `hvrun` outside the builder). Every download is pinned in `deps.lock` by SHA-256, the builder image by digest and a Debian snapshot date, and Firecracker's Rust toolchain and crates by its `rust-toolchain.toml` and `Cargo.lock` (built with `--locked`). Archives get sorted entries, root ownership and the inputs' commit time. Check a change to the build by building twice from an empty `out/` and comparing `out/*.sha256`.
- `guest/libvoidstar.c` is mounted into every container, whatever its libc: raw system calls, no libc.
- `guest/init` runs in BusyBox `sh` inside the VM, with no network: POSIX shell and BusyBox applets only.
- The control channel's lines and messages are `internal/protocol`'s types, used by both the CLI (`cmd/timeskip`) and the guest agent (`cmd/guest`). The inputs' layout and the bundle's files are shared too, but not typed: change both sides together, and docs/reference.md with them. `.agents/skills/using-timeskip/SKILL.md` describes the CLI's use; keep it current too.
- Test templates, setup_complete and assertions follow Antithesis's conventions (the Test Composer's command prefixes, `$ANTITHESIS_OUTPUT_DIR/sdk.jsonl`), so that a project runs unchanged in both. Don't add timeskip-only conventions where Antithesis has one, and list what differs in docs/reference.md's "Differences from Antithesis".
- A new protocol case (a setup failure, a kind of result) gets a fixture in `e2e/testdata/protocol` with a `# expect: N` exit code line.
- Keep the docs short and current: describe what the project does now, not its history.
- timeskip is not a security boundary; don't describe or design it as one.
