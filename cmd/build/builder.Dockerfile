# The build environment: Go on Debian bookworm, pinned by image digest and a
# snapshot.debian.org date, native to the host (the target's architecture).
FROM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9
ARG SNAPSHOT=20261101T000000Z
RUN rm -f /etc/apt/sources.list.d/debian.sources && \
    for s in "debian bookworm" "debian bookworm-updates" "debian-security bookworm-security"; do \
      set -- $s; echo "deb [check-valid-until=no] http://snapshot.debian.org/archive/$1/$SNAPSHOT $2 main"; \
    done > /etc/apt/sources.list && \
    apt-get -o Acquire::Retries=5 update -qq && \
    DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 install -y -qq --no-install-recommends \
      flex bison bc libssl-dev libelf-dev patch xz-utils pigz shellcheck \
      musl-tools clang libclang-dev libseccomp-dev erofs-utils > /dev/null && \
    rm -rf /var/lib/apt/lists/*
ENV GOCACHE=/cache/go-build GOFLAGS=-buildvcs=false
