#!/bin/sh
# build.sh: build the timeskip kit for this machine (cmd/build, in a pinned
# builder container). Needs Docker, plus the Xcode command line tools on macOS.
#
#   ./build.sh         out/timeskip-macos-arm64.tgz (Apple silicon) or
#                      out/timeskip-linux-x86_64.tgz (x86_64), out/e2e.test
#                      and out/tests
#   ./build.sh lint    gofmt, go vet and shellcheck
#   ./build.sh clean   remove out/ and the build cache
#
# The build cache is the Docker volume $TS_BUILD_CACHE (default timeskip-build).
set -eu
cache=${TS_BUILD_CACHE:-timeskip-build}
cd "$(dirname "$0")"
case ${1:-}/$(uname -s)/$(uname -m) in
clean/*) rm -rf out; docker volume rm -f "$cache" > /dev/null; exit;;
lint/*) target=lint;;
/Darwin/arm64)
	target=macos-arm64
	# hvrun uses Apple's Hypervisor.framework, so it's built here, again
	# when its sources, this command or the toolchain change.
	mkdir -p out
	swiftc="swiftc -O -target arm64-apple-macos15 -o out/hvrun"
	key=$( (cat hv/*; echo "$swiftc"; swiftc --version 2>&1) | shasum -a 256)
	if [ "$(cat out/hvrun.key 2>/dev/null)" != "$key" ]; then
		rm -f out/hvrun.key
		$swiftc hv/*.swift
		codesign -f -s - --entitlements hv/hv.entitlements out/hvrun
		swiftc --version 2>&1 | head -1 > out/hvrun.toolchain
		echo "$key" > out/hvrun.key
	fi;;
/Linux/x86_64) target=linux-x86_64;;
/*) echo "build.sh: macOS on Apple silicon or Linux on x86_64 only" >&2; exit 1;;
*) echo "usage: ./build.sh [lint|clean]" >&2; exit 2;;
esac
mkdir -p out
# The builder image is built again only when its Dockerfile changes (its
# label), not whenever Docker's build cache lacks it: its apt-get needs
# snapshot.debian.org, which can be down.
key=$(cksum < cmd/build/builder.Dockerfile)
if [ "$(docker image inspect -f '{{index .Config.Labels "dockerfile"}}' timeskip-builder 2>/dev/null)" != "$key" ]; then
	docker build -q --label "dockerfile=$key" -t timeskip-builder - < cmd/build/builder.Dockerfile > /dev/null
fi
exec docker run --rm -v "$PWD:/src:ro" -v "$PWD/out:/out" -v "$cache:/cache" -w /src \
	-v /var/run/docker.sock:/var/run/docker.sock -e OWNER="$(id -u):$(id -g)" \
	timeskip-builder go run ./cmd/build "$target"
