# forkd-service

The **forkd lease service**: a fast, isolated, ephemeral compute API backed
by forkd microVMs on a warm pool. Consumers request a sandbox, run work in
it, and release it — each job gets its own isolated KVM environment in
milliseconds.

## Components

This repo is the service/API layer. The runners that consume it
(Forgejo Actions, command adapter) live alongside in `cmd/`/`runner/`.

| Path               | What it is                                                       |
|--------------------|------------------------------------------------------------------|
| `cmd/forkd-backend` | The lease API frontend (`:8890`): sandboxes, images, exec, pool |
| `api/`             | Backend server logic (sandbox service, warm pool, TLS)           |
| `forkd/`           | forkd controller client                                          |
| `cmd/forkd-runner` | Forgejo Actions runner (adaptive pool of concurrent workers)    |
| `runner/`          | Runner internals: Forgejo adapter, lease client, executor, pool  |
| `commandadapter/`  | Command adapter that leases sandboxes                            |
| `workflow/`        | Workflow parser/types                                            |
| `deploy/`          | systemd units + deploy docs                                      |

## Build

```bash
go build -o forkd-backend ./cmd/forkd-backend
go build -o forkd-runner   ./cmd/forkd-runner
```

## Deploy

See [`deploy/README.md`](deploy/README.md) for the lease API (backend) and
the Forgejo Actions runner — build, systemd units, env, warm pool, TLS.

## Tests

```bash
go test ./...
```

## History

Previously `hyper-forgejo-runner`. Renamed to `forkd-service` when the
decommissioned Hyper runner was replaced by the forkd lease service.
