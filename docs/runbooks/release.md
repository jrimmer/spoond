# Release artifacts

What a 4.x release attaches, so `spoond install` and the Docker install
work from it. A release publishes no images: the Docker install builds
its image from the tag checkout, and sandbox images are built on the
host (plan: `docs/plans/2026-10-16-4.0-install.md`,
sections R and R5). `VERSION` is written without the `v` (`4.0.0`); the
tag is `vVERSION`.

1. **The runtime tarball.** In an `e2b-fork-worker` lease, from the fork
   checked out at the commit `deploy/e2b/RUNTIME` pins:

   ```bash
   E2B_RUNTIME_SRC=<fork checkout> deploy/release/build-runtime.sh $VERSION out/
   ```

   It runs the fork's unit tests, builds the orchestrator and envd, and
   writes `spoond-runtime_${VERSION}_linux_amd64.tar.gz` with its
   `RUNTIME` stamp.

2. **The release files.** From a clean checkout of the tag:

   ```bash
   deploy/release/build-release.sh $VERSION out/spoond-runtime_${VERSION}_linux_amd64.tar.gz dist/
   ```

   `dist/` then holds `spoond_${VERSION}_linux_amd64.tar.gz`, the runtime
   tarball and `SHA256SUMS`. Attach all three to the GitHub release (the
   runtime phase downloads from
   `https://github.com/jrimmer/spoond/releases/download/v$VERSION/`) and
   the Forgejo release.

3. **Check.** `go list -m github.com/jrimmer/spoond/v4@v$VERSION` from a
   directory outside the module, and a fresh-VM `spoond install` from the
   downloaded tarball (the 4.0 test: Debian 13 with nested KVM).
