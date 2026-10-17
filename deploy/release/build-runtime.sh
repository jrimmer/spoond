#!/usr/bin/env bash
# Build the spoond runtime release artifact: the orchestrator and envd
# binaries from our e2b-runtime fork, plus a RUNTIME stamp of the release
# version, fork commit, Go version and build date.
#
# The fork commit is pinned by deploy/e2b/RUNTIME (one line:
# "e2b-runtime <full commit>"). The plan is
# docs/plans/2026-10-16-4.0-install.md, section R.
#
# Usage: build-runtime.sh VERSION OUTDIR
#   VERSION  release version stamped into the artifact name (e.g. 4.0.0)
#   OUTDIR   directory the tarball and .sha256 are written to
#
# Environment:
#   E2B_RUNTIME_SRC  path to an existing fork checkout to build from
#                    instead of cloning. It must be clean and checked
#                    out at the RUNTIME commit; the script checks both.
#
# Runs in an e2b-fork-worker lease, so no production host is touched.
set -euo pipefail

usage() {
	echo "usage: $0 VERSION OUTDIR" >&2
	exit 2
}

die() {
	echo "build-runtime: $*" >&2
	exit 1
}

[ $# -eq 2 ] || usage
VERSION=$1
OUTDIR=$2

[ "$VERSION" != "" ] || die "VERSION must not be empty"
[ "$OUTDIR" != "" ] || die "OUTDIR must not be empty"

RUNTIME_FILE=$(dirname "$0")/../e2b/RUNTIME
RUNTIME_REPO=ssh://git@git.lacy.casa/lacy.casa/e2b-runtime.git

[ -f "$RUNTIME_FILE" ] || die "$RUNTIME_FILE not found"
# read fails at end of file when the last line has no newline; the
# checks below catch a short or garbled file.
read -r runtime_name runtime_commit <"$RUNTIME_FILE" || true
[ "$runtime_name" = "e2b-runtime" ] || die "unexpected content in $RUNTIME_FILE"
case "$runtime_commit" in
[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
*) die "not a full commit SHA in $RUNTIME_FILE: $runtime_commit" ;;
esac

command -v git >/dev/null || die "git not found"
command -v go >/dev/null || die "go not found"
command -v make >/dev/null || die "make not found"
command -v sha256sum >/dev/null || die "sha256sum not found"
command -v tar >/dev/null || die "tar not found"

mkdir -p "$OUTDIR"
OUTDIR=$(cd "$OUTDIR" && pwd)

# The clone (if any) and the tarball staging tree live in one temporary
# directory, removed on exit.
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

if [ "${E2B_RUNTIME_SRC:-}" != "" ]; then
	# Verify the provided checkout before trusting it.
	SRC=$E2B_RUNTIME_SRC
	[ -d "$SRC/.git" ] || die "E2B_RUNTIME_SRC=$SRC is not a git checkout"
	current_commit=$(git -C "$SRC" rev-parse HEAD)
	[ "$current_commit" = "$runtime_commit" ] ||
		die "E2B_RUNTIME_SRC=$SRC is at $current_commit, not $runtime_commit (run git -C \"$SRC\" checkout $runtime_commit)"
	[ -z "$(git -C "$SRC" status --porcelain)" ] ||
		die "E2B_RUNTIME_SRC=$SRC has uncommitted changes; commit or stash them first"
	echo "build-runtime: using E2B_RUNTIME_SRC=$SRC at $runtime_commit"
else
	echo "build-runtime: cloning $RUNTIME_REPO into $workdir/src"
	git clone --quiet "$RUNTIME_REPO" "$workdir/src"
	git -C "$workdir/src" checkout --quiet "$runtime_commit"
	SRC=$workdir/src
fi

TARBALL=spoond-runtime_${VERSION}_linux_amd64.tar.gz

# Fork unit tests: the checks the e2b upgrade runbook and its build
# window run before every fork build (see docs/runbooks/e2b-upgrade.md
# step 5). One tcpfirewall test drives a testcontainers Docker
# container, which many build hosts have no Docker daemon for; it fails
# without one, so it is skipped explicitly.
echo "build-runtime: running the fork unit tests"
(cd "$SRC/packages/orchestrator" && go test ./pkg/startupreclaim/... ./pkg/tcpfirewall/... -skip 'TestResilientListener_EMFILEWithContainer' -count=1)
(cd "$SRC/packages/shared" && go test ./pkg/featureflags/... -count=1)

echo "build-runtime: building orchestrator"
make -C "$SRC/packages/orchestrator" build-local BUILD_ARCH=amd64

echo "build-runtime: building envd"
make -C "$SRC/packages/envd" build BUILD_ARCH=amd64

orchestrator=$SRC/packages/orchestrator/bin/orchestrator
envd=$SRC/packages/envd/bin/envd
[ -x "$orchestrator" ] || die "orchestrator binary not built"
[ -x "$envd" ] || die "envd binary not built"

go_version=$(go version | sed 's/^go version //')
build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Deterministic-enough tarball: fixed order, no ownership, no mtime,
# sorted names, gzip without a timestamp.
stage=$workdir/stage
mkdir -p "$stage/spoond-runtime"
install -m 0755 "$orchestrator" "$stage/spoond-runtime/orchestrator"
install -m 0755 "$envd" "$stage/spoond-runtime/envd"

{
	echo "spoond $VERSION"
	echo "e2b-runtime $runtime_commit"
	echo "go $go_version"
	echo "built $build_date"
} >"$stage/spoond-runtime/RUNTIME"

tar -czf "$OUTDIR/$TARBALL" \
	--sort=name \
	--owner=0 --group=0 --numeric-owner \
	--mtime='UTC 2026-01-01' \
	-C "$stage" spoond-runtime

(cd "$OUTDIR" && sha256sum "$TARBALL" >"$TARBALL.sha256")

echo "$OUTDIR/$TARBALL"
cat "$OUTDIR/$TARBALL.sha256"
