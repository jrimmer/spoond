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

## Not environment (still needs vm2 values at config level)

- `deploy/e2b/orchestrator.env`: `NODE_ID` changed `vm2` → `node1`
  (cosmetic node label; set `NODE_ID=vm2` locally if the label matters).
- `spoondctl`: `SPOOND_CTL_HOST` default changed `sandbox.lacy.casa` →
  `sandbox.example.com`. VM2 callers that relied on the default must
  export `SPOOND_CTL_HOST=sandbox.lacy.casa`.
- Integration tests (`tests/integration/*.sh`) take `NETPOL_TARGET`,
  `NETPOL_BLOCKED`, `PROXY_SUFFIX` and `BE_TLS_HOST` from the
  environment; the old vm2 values are no longer baked in. (`SSHHOST` is
  the remote-runner host used by `run.sh`; the ctl tests dial
  `ctl@127.0.0.1 -p 2222` directly.)
- Conformance (`conformance/README.md`) already uses
  `CONFORMANCE_*` environment values; LAN probe addresses in
  `network_test.go` are now `10.0.0.203`/`10.0.0.11` placeholders.

## Deprecated-name fallbacks

2.7 keeps the existing 2.0 `FORKD_*` fallbacks
(`SPOOND_GATEWAY_HOST` over `FORKD_GATEWAY_HOST`, etc.) so the
pre-2.0 names still work with a deprecation warning. No new
`FORKD_*` names were introduced.
