#!/bin/sh
# install.sh: install or update timeskip's kit for this machine, and link
# the CLI into a directory on PATH.
#
#   curl -fsSL https://github.com/ryanworl/timeskip/releases/latest/download/install.sh | sh
#   sh install.sh [KIT.tgz]     a kit you built (out/timeskip-*.tgz), not a release
#
# TIMESKIP_URL is where releases are (default: GitHub's latest release),
# TIMESKIP_HOME where the kit goes (default ~/.local/share/timeskip) and
# TIMESKIP_BIN where the timeskip link goes (default ~/.local/bin).
# It replaces only a timeskip kit: it won't touch an existing TIMESKIP_HOME
# that isn't one.
set -eu
url=${TIMESKIP_URL:-https://github.com/ryanworl/timeskip/releases/latest/download}
home=${TIMESKIP_HOME:-$HOME/.local/share/timeskip}
bin=${TIMESKIP_BIN:-$HOME/.local/bin}
fail() { echo "install.sh: $*" >&2; exit 1; }

# (All in a function, run on the last line, so that a download cut short
# runs nothing.)
main() {
case $(uname -s)/$(uname -m) in
Darwin/arm64)
	platform=macos-arm64
	[ "$(sw_vers -productVersion | cut -d. -f1)" -ge 15 ] || fail "timeskip needs macOS 15 or later";;
Linux/x86_64) platform=linux-x86_64;;
*) fail "timeskip runs on macOS on Apple silicon or Linux on x86_64, not $(uname -s) on $(uname -m)";;
esac
if [ -e "$home" ] || [ -L "$home" ]; then
	case $(head -n 1 "$home/MANIFEST" 2> /dev/null) in
	"timeskip "*) ;;
	*) fail "$home exists and isn't a timeskip kit; remove it, or set TIMESKIP_HOME to another directory";;
	esac
fi

# (Next to $home, so that the kits are moved, not copied.)
mkdir -p "$(dirname "$home")" "$bin"
tmp=$(mktemp -d "$(dirname "$home")/.timeskip-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM
if [ $# -gt 0 ]; then
	kit=$1
	[ -f "$kit" ] || fail "no such kit: $kit"
else
	kit=$tmp/timeskip-$platform.tgz
	echo "downloading $url/timeskip-$platform.tgz"
	curl -fL --progress-bar -o "$kit" "$url/timeskip-$platform.tgz"
	curl -fsSL -o "$kit.sha256" "$url/timeskip-$platform.tgz.sha256"
	want=$(cut -d' ' -f1 < "$kit.sha256")
	got=$( (command -v sha256sum > /dev/null && sha256sum "$kit" || shasum -a 256 "$kit") | cut -d' ' -f1)
	[ "$got" = "$want" ] || fail "the download's SHA-256 is $got, not $want"
fi
mkdir -p "$tmp/kit"
tar -xzf "$kit" -C "$tmp/kit"
grep -q "^timeskip .*$platform\$" "$tmp/kit/timeskip/MANIFEST" 2> /dev/null || fail "$kit isn't a $platform kit"

# (The old kit goes, with $tmp, only once the new one is in place.)
if [ -e "$home" ] || [ -L "$home" ]; then
	mv "$home" "$tmp/old"
fi
if ! mv "$tmp/kit/timeskip" "$home"; then
	[ ! -e "$tmp/old" ] || mv "$tmp/old" "$home" || { trap - EXIT; fail "couldn't put the old kit back; it's in $tmp/old"; }
	fail "couldn't move the new kit to $home"
fi
ln -sf "$home/timeskip" "$bin/timeskip"
"$bin/timeskip" version

# What it needs to run.
command -v docker > /dev/null || echo "note: timeskip needs Docker (on macOS, Docker Desktop or Colima: brew install colima docker)"
if [ "$platform" = linux-x86_64 ]; then
	[ -r /dev/kvm ] && [ -w /dev/kvm ] || echo "note: timeskip needs read and write access to /dev/kvm (e.g. sudo usermod -aG $(stat -c %G /dev/kvm 2> /dev/null || echo kvm) $(id -un), then log in again)"
	PATH=$PATH:/usr/sbin:/sbin command -v mkfs.ext4 > /dev/null || echo "note: timeskip needs mkfs.ext4 (e2fsprogs)"
	# (Older kernels can't set a vCPU's TSC offset, so a paused VM's clock doesn't fully stand still.)
	kv=$(uname -r)
	major=${kv%%.*} minor=${kv#*.}
	minor=${minor%%[!0-9]*}
	[ "$major" -gt 5 ] || { [ "$major" -eq 5 ] && [ "${minor:-0}" -ge 16 ]; } ||
		echo "warning: timeskip needs Linux 5.16 or later on the host, not $kv"
fi
case :$PATH: in
*:"$bin":*) ;;
*) echo "note: $bin isn't on your PATH; add it, e.g. in ~/.profile: export PATH=\"$bin:\$PATH\"";;
esac
echo "installed timeskip in $home; try: timeskip test $home/examples/pingpong/compose.yaml"
}

main "$@"
