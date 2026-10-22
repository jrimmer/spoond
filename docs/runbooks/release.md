# Release artifacts

What a 4.x release attaches and publishes, so `spoond install` and the
Docker install work from it (plan: `docs/plans/2026-10-16-4.0-install.md`,
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

3. **The Docker image.** Needs a GitHub token with `write:packages`
   (`gh auth refresh -h github.com -s write:packages`, then
   `gh auth token | docker login ghcr.io -u jrimmer --password-stdin`):

   ```bash
   cp out/spoond-runtime_${VERSION}_linux_amd64.tar.gz deploy/docker/artifacts/
   docker build -f deploy/docker/Dockerfile --build-arg VERSION=$VERSION \
     -t ghcr.io/jrimmer/spoond:$VERSION -t ghcr.io/jrimmer/spoond:latest .
   docker push ghcr.io/jrimmer/spoond:$VERSION && docker push ghcr.io/jrimmer/spoond:latest
   ```

4. **The base images.** On a host whose catalog has the release's
   images built (`spoond images build --all`), with the same login:

   ```bash
   spoond images publish --all --prefix ghcr.io/jrimmer/spoond-images --tag v$VERSION
   ```

   A release binary's images phase pulls `<name>:v$VERSION` from that
   prefix and builds locally when a tag is missing, so a release without
   this step still installs, only slower.

5. **Check.** `go list -m github.com/jrimmer/spoond/v4@v$VERSION` from a
   directory outside the module, and a fresh-VM `spoond install` from the
   downloaded tarball (the 4.0 test: Debian 13 with nested KVM).
