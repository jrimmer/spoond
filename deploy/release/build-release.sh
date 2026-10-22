#!/usr/bin/env bash
# Build a release's artifacts from a checkout of its tag: the spoond
# binary tarball, the runtime tarball (built by build-runtime.sh, passed
# in) and SHA256SUMS over both, the files `spoond install runtime`
# downloads and checks. The plan is docs/plans/2026-10-16-4.0-install.md,
# section R.
#
# Usage: build-release.sh VERSION RUNTIME_TARBALL OUTDIR
#   VERSION          the release version without the v (e.g. 4.0.0)
#   RUNTIME_TARBALL  spoond-runtime_<VERSION>_linux_amd64.tar.gz
#   OUTDIR           directory the artifacts are written to
#
# Then, for the Docker image (VERSION as above):
#   cp RUNTIME_TARBALL deploy/docker/artifacts/
#   docker build -f deploy/docker/Dockerfile --build-arg VERSION=$VERSION \
#     -t ghcr.io/jrimmer/spoond:$VERSION -t ghcr.io/jrimmer/spoond:latest .
set -euo pipefail

die() {
	echo "build-release: $*" >&2
	exit 1
}

[ $# -eq 3 ] || {
	echo "usage: $0 VERSION RUNTIME_TARBALL OUTDIR" >&2
	exit 2
}
VERSION=$1
RUNTIME=$2
OUTDIR=$3

case "$VERSION" in
v*) die "VERSION is written without the v: ${VERSION#v}" ;;
[0-9]*.[0-9]*.[0-9]*) ;;
*) die "VERSION $VERSION is not X.Y.Z" ;;
esac
want=spoond-runtime_${VERSION}_linux_amd64.tar.gz
[ "$(basename "$RUNTIME")" = "$want" ] || die "RUNTIME_TARBALL must be $want"
[ -f "$RUNTIME" ] || die "$RUNTIME not found"
tar -xzOf "$RUNTIME" spoond-runtime/RUNTIME | grep -qx "spoond $VERSION" ||
	die "$RUNTIME was not built for $VERSION (its RUNTIME stamp says otherwise)"

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
[ -z "$(git -C "$ROOT" status --porcelain)" ] || die "the checkout has uncommitted changes"
tag=$(git -C "$ROOT" describe --exact-match --tags HEAD 2>/dev/null || true)
[ "$tag" = "v$VERSION" ] || die "HEAD is not tagged v$VERSION (it is ${tag:-untagged})"

mkdir -p "$OUTDIR"
OUTDIR=$(cd "$OUTDIR" && pwd)
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

echo "build-release: building spoond v$VERSION"
mkdir -p "$workdir/spoond"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
	-ldflags "-s -w -X main.version=v$VERSION" -o "$workdir/spoond/spoond" ./cmd/spoond)
"$workdir/spoond/spoond" version | grep -q "spoond v$VERSION " || die "the built binary does not report v$VERSION"
install -m 0644 "$ROOT/LICENSE" "$ROOT/NOTICE" "$workdir/spoond/"

SPOOND_TARBALL=spoond_${VERSION}_linux_amd64.tar.gz
tar -czf "$OUTDIR/$SPOOND_TARBALL" \
	--sort=name \
	--owner=0 --group=0 --numeric-owner \
	--mtime='UTC 2026-01-01' \
	-C "$workdir/spoond" spoond LICENSE NOTICE
cp "$RUNTIME" "$OUTDIR/$want"

(cd "$OUTDIR" && sha256sum "$SPOOND_TARBALL" "$want" >SHA256SUMS)
echo "build-release: $OUTDIR"
cat "$OUTDIR/SHA256SUMS"
