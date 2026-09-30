# U01 — Go 1.27.1 and dependency upgrades (no regressions)

## Purpose

Move spoond to Go 1.27.1 and current versions of every dependency, without
changing behaviour. Later units need current `modernc.org/sqlite`
(requires Go ≥ 1.26) and gRPC.

## Preconditions

None. This is the first unit.

## Facts relied on

- `go.mod` declares `go 1.25.0` (A1 §1).
- `connectrpc.com/connect v1.20.0`, `google.golang.org/protobuf v1.36.11` and
  `github.com/gorilla/websocket v1.5.3` are direct dependencies (A1 §1).
- CI (`.forgejo/workflows/test.yml`, workflow `ci`, job `test`) runs on
  runner label `go`, with `PATH` starting `/usr/local/go/bin`. It triggers only
  on pushes to `main` and on pull requests. Its first step is the unnamed
  `- run: go version` (shown as `Run go version`), then `gofmt -l .`,
  `go vet ./...`, `go build ./...` and `go test ./...`.
- CI jobs with `runs-on: go` run in the `go-base` image. `images/manifest.yaml`
  still describes go-base as "Go 1.25.12", so step 9a verifies the image's Go
  version before the commit is pushed.
- vm2 has Go 1.25.1 at `/usr/local/go`, used by `deploy/install-spoond.sh`
  to build spoond.
- Latest stable Go is 1.27.1. Latest module versions checked 2026-09-30:
  - `modernc.org/sqlite v1.60.1` (needs Go 1.26);
  - `google.golang.org/grpc v1.84.0`;
  - `google.golang.org/protobuf v1.36.12`;
  - `connectrpc.com/connect v1.21.0`;
  - `github.com/google/uuid v1.6.0`.

## Steps

1. Create branch `feat/e2b-substrate` from `main`.
2. Edit `go.mod`: change the `go` line to `go 1.27.1`. Do not add a
   `toolchain` line.
3. Run `go get -u ./...` and then `go mod tidy`.
4. Verify **no downgrades**: compare `go list -m all` before step 3 and after.
   Every module present in both must have an equal or higher version. If any
   module went down, STOP and report it.
5. Run `gofmt -l .`. If it lists files, run `gofmt -w` on exactly those files.
   Go 1.27 may reformat. That is allowed only for files `gofmt` lists.
6. Run `go vet ./...` and fix **only** new findings introduced by the
   upgrade. If a fix would change behaviour, STOP and report.
7. Run `go build ./... && go test ./...`. All tests must pass. A test that
   fails only because of a changed dependency API must be fixed at the call
   site, keeping behaviour identical.
8. Update `docs/install.md`: replace the line `# Go toolchain (1.22+)` with
   `# Go toolchain (1.27.1+)`.
9. **Commit:** `chore(go): Go 1.27.1 and current dependency versions`.
9a. **go-base check (Ops runner, on vm2, against the production lease
    API).** Create a `go-base` lease and exec `go version`:
    ```bash
    set -a; . /etc/spoond/conformance.env; set +a
    H="Authorization: Bearer $CONFORMANCE_TOKEN"
    ID=$(curl -fsS -H "$H" -d '{"image":"go-base","ttl":120}' "$CONFORMANCE_API/api/sandboxes" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
    curl -fsS -H "$H" -d '{"cmd":"go version"}' "$CONFORMANCE_API/api/sandboxes/$ID/exec"
    curl -fsS -X DELETE -H "$H" "$CONFORMANCE_API/api/sandboxes/$ID"
    ```
    `stdout` must contain `go1.27.1`. If it does not, STOP: CI cannot build a
    `go 1.27.1` module until go-base is rebaked, which is an OPERATOR task.
10. **vm2 host Go** (runs on vm2 as root; it does not touch running services):
    ```bash
    cd /tmp
    curl -fsSLO https://go.dev/dl/go1.27.1.linux-amd64.tar.gz
    curl -fsSL https://go.dev/dl/?mode=json | python3 -c "import json,sys; r=[r for r in json.load(sys.stdin) if r['version']=='go1.27.1'][0]; print([f['sha256'] for f in r['files'] if f['filename']=='go1.27.1.linux-amd64.tar.gz'][0])" > go.sha256
    echo "$(cat go.sha256)  go1.27.1.linux-amd64.tar.gz" | sha256sum -c -
    mv /usr/local/go /usr/local/go1.25.1.bak
    tar -C /usr/local -xzf go1.27.1.linux-amd64.tar.gz
    /usr/local/go/bin/go version   # must print go1.27.1 linux/amd64
    ```
    Keep `/usr/local/go1.25.1.bak` until U12.

## Tests

- The full existing suite passes: `go test ./...`.
- `go version` in CI prints `go1.27.1`. Push the branch, open a **draft** PR
  from `feat/e2b-substrate` to `main` (a branch push alone does not trigger
  CI), and check step `Run go version` of job `test` in workflow `ci`. The
  draft PR stays open and is never merged by an agent.

## Done when

- The branch builds and passes `gofmt`, `vet`, `build` and `test` locally and
  in CI.
- `go list -m all` shows no downgrade.
- vm2 `/usr/local/go/bin/go version` prints `go1.27.1`.

## Do not

- Do not pin older versions to avoid a breaking change. Adapt call sites
  instead.
- Do not change application behaviour or refactor.
- Do not touch `forkd/` semantics.
