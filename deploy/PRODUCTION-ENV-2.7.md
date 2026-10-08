# spoond 2.7 production environment — vm2

The 2.7 generic-defaults change removes every deployment-specific host
name and LAN address from code. Functional behaviour is unchanged only
if the variables below are set to vm2's existing values **before** the
new binary is deployed (orch-1 applies them in a maintenance window).

The generic defaults are now `localhost`, `127.0.0.1`,
`.sandbox.example.com` or "none". Any line not listed here keeps the
same behaviour under the generic default.

## /etc/spoond/backend.env (backend unit)

```ini
# Guest resolver: was hardcoded 10.1.0.2 in egressForLocked and baked
# into every image's spoond-guest-init.
SPOOND_GUEST_DNS_ADDR=10.1.0.2
# Proxy wildcard suffix: was hardcoded .sandbox.lacy.casa.
SPOOND_PROXY_HOST_SUFFIX=.sandbox.lacy.casa
```

Unchanged lines (already present on vm2):
`HOST_GUEST_SERVICE_ADDR=10.1.0.11`, `HOST_GUEST_SERVICE_PORT=8891`,
`HOST_API_PORT`, `TLS_CERT`, `TLS_KEY`, `CONSUMER_TOKENS`,
`E2B_TOKEN_SEED_FILE`, `IMAGE_REGISTRY`, `E2B_TEMPLATE_STORAGE_PATH`,
`SPOOND_DB_PATH`, `SPOOND_BACKUP_DIR`, `USERS_FILE`, `BOOTSTRAP_TOKEN`,
`GATEWAY_TOKEN`, `ADMIN_TOKEN`, `METRICS_TOKEN`, `EVENTS_TOKEN`,
`BIND_ADDR`, `PROXY_ADDR`, `LLM_*`.

## /etc/spoond-gateway.env (gateway unit)

```ini
# Public hostname advertised in MOTDs and SAN URLs; was the hardcoded
# default sandbox.lacy.casa.
SPOOND_GATEWAY_HOST=sandbox.lacy.casa
# Guest-service host address used by the gateway for SSH_CONNECTION and
# as the default base for SHELLY_BINARY_URL / LLM_GATEWAY_URL.
HOST_GUEST_SERVICE_ADDR=10.1.0.11
HOST_GUEST_SERVICE_PORT=8891
# Explicit is safest: set the base URLs to the pre-2.7 values.
SHELLY_BINARY_URL=http://10.1.0.11:8891/assets/shelley
LLM_GATEWAY_URL=http://10.1.0.11:8891/llm/
# Existing gateway credentials (unchanged).
SPOOND_GATEWAY_TOKEN=<existing value>
```

If `SHELLY_BINARY_URL`/`LLM_GATEWAY_URL` are omitted, the gateway now
derives them from `HOST_GUEST_SERVICE_ADDR:PORT` (default
`127.0.0.1:8891`), so vm2 must set `HOST_GUEST_SERVICE_ADDR`.

## /etc/spoond-runner.env (runner unit)

```ini
# Forgejo instance base URL: no default any more, required.
FORGEJO_URL=https://code.lacy.casa
# Git host for actions/checkout clones: no default any more, required
# for checkout jobs.
REPO_BASE_URL=https://code.lacy.casa
```

Unchanged: `RUNNER_TOKEN`, `LEASE_URL`, `LEASE_TOKEN`, `IMAGE_MAP`,
`DEFAULT_IMAGE`, pool knobs, `RUNNER_STATE_FILE`, `JOB_RECORD_DIR`.

## spoond dash environment (dash unit / Invocation)

```ini
# TLS server name for METRICS_URL: no default any more.
METRICS_SERVER_NAME=vm2.lacy.casa
```

Without it, `METRICS_SERVER_NAME` is empty; a `METRICS_URL` whose host
does not resolve to the certificate SAN may then fail. vm2 should set
it. Other dash variables unchanged.

## /etc/default/spoond-netwatch (netwatch unit)

```ini
# Probe targets: formerly the hardcoded default "10.1.0.1 10.1.0.2".
NETWATCH_TARGETS=10.1.0.1 10.1.0.2
```

Netwatch now exits with an error if `NETWATCH_TARGETS` is unset.

## Image build (spoond images build)

Every image Dockerfile now takes `ARG SPOOND_GUEST_DNS_ADDR`. Set it in
the build environment (or the image layer's `build_args` in
`images/manifest.yaml`) so `spoond-guest-init` writes the pre-2.7
resolver:

```bash
SPOOND_GUEST_DNS_ADDR=10.1.0.2 spoond images build --all
```

Without it, `spoond-guest-init` leaves the image's own `resolv.conf`
alone (generic, no pinned resolver).

## Guest DNS list (ships with the next release)

The next release lets `SPOOND_GUEST_DNS_ADDR` name several resolvers
(comma-separated) so a guest survives one slow or dead DNS server. On
that release set the LAN's two resolvers in the backend and on the build
host:

- **sb** (backend) — `/etc/spoond/backend.env`:
  `SPOOND_GUEST_DNS_ADDR=10.1.0.2,10.1.0.3`
- **agent-hub build-worker** — the image build environment must export
  the same value before `spoond images build --all`, so every rebuilt
  image bakes both resolvers and the `options timeout:2 attempts:3
  rotate` line:
  `SPOOND_GUEST_DNS_ADDR=10.1.0.2,10.1.0.3 spoond images build --all`

The backend grants each address a port-53 allowance and sends no public
DNS fallback. The sb env change and the image rebuilds happen together
in that release's window (owner + Honey). Images built with `10.1.0.2`
alone keep working — they simply carry one resolver — until they are
rebuilt.

## /etc/e2b/orchestrator.env (E2B orchestrator unit)

The repo's `deploy/e2b/orchestrator.env` now ships the generic
`NODE_ID=node1`, and `docs/install.md` §2 / `deploy/README.md` install
that file verbatim. `NODE_ID` is **required and not cosmetic**: the E2B
runtime uses it as `ServiceInfo.ClientId` and the telemetry host id, so
vm2 must be pinned back to its existing value before the orchestrator
unit is restarted with the new file:

```ini
# Mandatory: the orchestrator's node identity (ClientId, telemetry
# host id). Was the repo default before 2.7; the repo example is
# generic now.
NODE_ID=vm2
```

Everything else in that file is unchanged on vm2.

## spoondctl / callers that relied on the old default

`SPOOND_CTL_HOST` now defaults to `sandbox.example.com` instead of
`sandbox.lacy.casa`. VM2 callers that relied on the default must export
`SPOOND_CTL_HOST=sandbox.lacy.casa` (or pass `-host`).

## Integration tests (not deployed units)

`tests/integration/*.sh` take their deployment-specific values from the
environment; the old vm2 values are no longer baked in. A vm2 run needs:

```bash
BE_TLS_HOST=vm2.lacy.casa \
PROXY_SUFFIX=sandbox.lacy.casa \
NETPOL_TARGET=10.1.0.47:3000 \
NETPOL_BLOCKED=10.1.0.203:80 \
SSHHOST=root@10.1.0.11 \
  tests/integration/run.sh
```

`BE_TLS_HOST` is the name on the backend certificate (the Go clients
verify TLS while `curl -sk` callers use `BE_API` directly).
`NETPOL_TARGET`/`NETPOL_BLOCKED` are a reachable and an unreachable LAN
host:port for the policy probes. `SSHHOST` is the remote-runner host
used by `run.sh`; the ctl tests dial `ctl@127.0.0.1 -p 2222` directly.

Conformance (`conformance/README.md`) already uses `CONFORMANCE_*`
environment values; LAN probe addresses in `network_test.go` are now
`10.0.0.203`/`10.0.0.11` placeholders.

## Deprecated-name fallbacks

2.7 keeps the existing 2.0 `FORKD_*` fallbacks
(`SPOOND_GATEWAY_HOST` over `FORKD_GATEWAY_HOST`, etc.) so the
pre-2.0 names still work with a deprecation warning. No new
`FORKD_*` names were introduced.
