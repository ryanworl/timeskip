#!/bin/sh
# install-deps.sh: install what building and running timeskip needs. Safe to
# rerun: it skips what's already there, and asks before installing Docker or
# changing your groups.
#
#   macOS (Apple silicon, 15+): the Xcode command line tools, and Docker
#     (Colima from Homebrew, unless Docker Desktop or OrbStack is installed)
#   Linux (x86_64): Docker (you install it: https://docs.docker.com/engine/install/),
#     access to /dev/kvm and mkfs.ext4
set -eu
say() { echo "install-deps: $*"; }
die() { say "$*" >&2; exit 1; }
# ask QUESTION: whether the user answers yes (no, without a terminal).
ask() {
	printf 'install-deps: %s [y/N] ' "$1"
	answer=
	read -r answer 2> /dev/null < /dev/tty || echo
	case $answer in [yY]*) return 0;; *) return 1;; esac
}
relogin=""
me=$(id -un)

case $(uname -s)/$(uname -m) in
Darwin/arm64)
	[ "$(sw_vers -productVersion | cut -d. -f1)" -ge 15 ] || die "timeskip needs macOS 15 or later"
	if ! xcode-select -p > /dev/null 2>&1; then
		xcode-select --install
		die "finish installing the Xcode command line tools, then run this again"
	fi
	say "Xcode command line tools: ok"
	if docker info > /dev/null 2>&1; then
		say "Docker: running"
	elif [ -d /Applications/Docker.app ]; then
		die "start Docker Desktop, then run this again"
	elif [ -d /Applications/OrbStack.app ]; then
		die "start OrbStack, then run this again"
	else
		command -v brew > /dev/null || die "needs Homebrew (https://brew.sh) to install Docker, or Docker Desktop or OrbStack"
		say "Docker: not installed. This installs Colima and Docker from Homebrew"
		say "  (brew install colima docker docker-buildx docker-compose), points the Docker CLI"
		say "  at Homebrew's plugins in ~/.docker/config.json, and starts Colima with every core"
		say "  and half the memory."
		ask "Install them?" || die "install Docker (Colima, Docker Desktop or OrbStack), then run this again"
		brew install colima docker docker-buildx docker-compose
		# The Docker CLI finds Homebrew's Compose and buildx through its config.
		mkdir -p ~/.docker
		[ -f ~/.docker/config.json ] || echo '{}' > ~/.docker/config.json
		grep -q cliPluginsExtraDirs ~/.docker/config.json ||
			plutil -insert cliPluginsExtraDirs -json "[\"$(brew --prefix)/lib/docker/cli-plugins\"]" ~/.docker/config.json
		# Every core and half the memory: the build compiles a kernel.
		colima start --vm-type vz --mount-type virtiofs \
			--cpu "$(sysctl -n hw.ncpu)" --memory $(($(sysctl -n hw.memsize) / 2147483648))
		say "Docker: running in Colima (after a restart, run: colima start)"
	fi;;
Linux/x86_64)
	[ -c /dev/kvm ] || die "no /dev/kvm: turn on virtualization (VT-x or AMD-V) in the firmware, or nested virtualization for a VM"
	if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
		say "KVM: ok"
	else
		group=$(stat -c %G /dev/kvm)
		[ "$group" != root ] || die "you can't use /dev/kvm, and its group is root: give yourself read and write access to it, then run this again"
		say "KVM: you can't use /dev/kvm. This adds you to its group: sudo usermod -aG $group $me"
		ask "Add you to $group?" || die "run: sudo usermod -aG $group $me, log out and back in, then run this again"
		sudo usermod -aG "$group" "$me"
		say "KVM: added $me to the $group group"; relogin=1
	fi
	command -v mkfs.ext4 > /dev/null || [ -x /usr/sbin/mkfs.ext4 ] || [ -x /sbin/mkfs.ext4 ] ||
		die "install e2fsprogs (mkfs.ext4), then run this again"
	say "mkfs.ext4: ok"
	command -v docker > /dev/null ||
		die "install Docker Engine (https://docs.docker.com/engine/install/), then run this again"
	if docker info > /dev/null 2>&1; then
		say "Docker: running"
	elif sudo docker info > /dev/null 2>&1; then
		say "Docker: runs only as root. This adds you to the docker group, which is as good as root:"
		say "  sudo usermod -aG docker $me"
		ask "Add you to docker?" || die "run: sudo usermod -aG docker $me, log out and back in, then run this again"
		sudo usermod -aG docker "$me"
		say "Docker: added $me to the docker group"; relogin=1
	else
		die "Docker is installed but not running (try: sudo systemctl start docker)"
	fi;;
*) die "timeskip runs on macOS on Apple silicon or Linux on x86_64";;
esac
[ -z "$relogin" ] || say "log out and back in for the new group membership, then run ./build.sh"
[ -n "$relogin" ] || say "done; next: ./build.sh"
