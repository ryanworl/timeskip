# Third-party software in the kits

timeskip's own code is under the MIT license (`LICENSE`), except the patches to other projects, which are under those projects' licenses: `guest/kernel/timeskip.patch` under GPL-2.0, and `firecracker/*.patch` under Apache-2.0.

The kits include the software below. The license texts and the upstream NOTICE files are in `licenses/`.

| Component | Where | License | Source |
| --- | --- | --- | --- |
| Linux 6.12.56, with `guest/kernel/timeskip.patch` | `share/Image-arm64`, `share/vmlinux-x86_64` | GPL-2.0 (`GPL-2.0`) | kernel.org (URL in `deps.lock`), plus the patch and configs in `guest/kernel/` |
| Alpine Linux 3.20 minirootfs (BusyBox, musl, ...) and the packages in `deps.lock` (e2fsprogs, iptables, iproute2, ...) | `share/rootfs-*.erofs` | Per package, as named in each package's metadata: mostly GPL-2.0, LGPL-2.1, BSD-3-Clause and MIT (`GPL-2.0`, `LGPL-2.1`, `musl-COPYRIGHT`) | alpinelinux.org (URLs in `deps.lock`); each package's build is in Alpine's aports, 3.20-stable branch |
| qemu-x86_64 9.0.2 (Alpine package; arm64 kit only) | `share/rootfs-aarch64.erofs` | GPL-2.0 (`GPL-2.0`) | download.qemu.org, and aports' `community/qemu` |
| Docker Engine 29.9.2 static: dockerd and docker-proxy (moby), containerd and its shim 2.4.2, runc 1.5.2 | `share/rootfs-*.erofs` | Apache-2.0 (`Apache-2.0`, `moby-NOTICE`, `containerd-NOTICE`, `runc-NOTICE`) | github.com/moby/moby (tag docker-v29.9.2), containerd/containerd, opencontainers/runc |
| tini 0.19.0, as Docker's docker-init | `share/rootfs-*.erofs` | MIT (`tini-LICENSE`) | github.com/krallin/tini |
| Docker Compose 5.6.1 | `share/rootfs-*.erofs` | Apache-2.0 (`Apache-2.0`, `compose-NOTICE`) | github.com/docker/compose |
| crun 1.30.1 (static) | `share/rootfs-*.erofs` | GPL-2.0-or-later (`GPL-2.0`, `LGPL-2.1`) | github.com/containers/crun |
| Go 1.27.3 standard library and runtime | `timeskip`, and the guest agent in `share/rootfs-*.erofs` | BSD-3-Clause (`Go-LICENSE`) | go.dev |
| moby/patternmatcher 0.6.1 | `timeskip` | Apache-2.0 (`Apache-2.0`, `patternmatcher-NOTICE`) | github.com/moby/patternmatcher (vendored in `vendor/`) |
| Firecracker 1.17.0, with the patches in `firecracker/` | `bin/firecracker` (Linux kit) | Apache-2.0 (`Apache-2.0`, `firecracker-NOTICE`, `firecracker-THIRD-PARTY`) | github.com/firecracker-microvm/firecracker (URL in `deps.lock`), plus the patches |
| Firecracker's Rust crates, as pinned in its `Cargo.lock` | `bin/firecracker` (Linux kit) | MIT, Apache-2.0, BSD-3-Clause, 0BSD, ISC or Unicode-3.0 (what Firecracker's `deny.toml` allows); each crate's license is in its source | crates.io |
| musl (static) | `bin/firecracker` (Linux kit) | MIT (`musl-COPYRIGHT`) | musl.libc.org |

## Source code

The corresponding source of each kit is the upstream source of the components above at the versions listed (the pinned tarballs in `deps.lock`, and for Alpine's packages their aports build files and the upstream tarballs those name), plus this repository at the release's tag, which holds the patches and configs applied to them.

For three years after we distribute a kit, we will give anyone a copy of the corresponding source of its GPL- and LGPL-licensed parts, for no more than the cost of providing it: ask at https://github.com/ryanworl/timeskip/issues.
