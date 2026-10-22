# spoond in Docker: the backend, gateway, dashboard and the E2B
# orchestrator in one image (docs/install.md, "Docker install").
#
# Build from the repository root:
#
#   docker build -f deploy/docker/Dockerfile -t spoond .
#
# The orchestrator and envd come from the release's
# spoond-runtime_<v>_linux_amd64.tar.gz: put it in deploy/docker/artifacts/
# (a local build, deploy/release/build-runtime.sh), or set VERSION and the
# build downloads it from the release and checks it against SHA256SUMS.
# Firecracker, the guest kernel and busybox are fetched from the pin table
# `spoond install runtime` uses, so both installs carry the same files.

ARG GO_VERSION=1.27.1

FROM golang:${GO_VERSION}-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false \
	-ldflags "-s -w ${VERSION:+-X main.version=v${VERSION}}" \
	-o /out/spoond ./cmd/spoond

FROM debian:trixie-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		bash ca-certificates curl docker-buildx docker-cli e2fsprogs iproute2 iptables kmod nftables procps rsync util-linux \
	&& rm -rf /var/lib/apt/lists/* \
	&& update-alternatives --set iptables /usr/sbin/iptables-legacy \
	&& update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy
COPY --from=build /out/spoond /opt/spoond/spoond
COPY deploy/docker/artifacts/ /tmp/artifacts/
RUN set -e; \
	rt=$(ls /tmp/artifacts/spoond-runtime_*_linux_amd64.tar.gz 2>/dev/null | head -n 1); \
	/opt/spoond/spoond install runtime --artifacts-only ${rt:+--runtime "$rt"}; \
	rm -rf /tmp/artifacts
COPY deploy/docker/entrypoint.sh /usr/local/bin/spoond-container
# spoond doctor: compose, not systemd, runs the services.
ENV SPOOND_CONTAINER=1
ENTRYPOINT ["/usr/local/bin/spoond-container"]
CMD ["help"]
