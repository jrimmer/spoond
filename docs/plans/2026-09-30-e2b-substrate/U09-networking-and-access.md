# U09 — Networking and access: exposed ports, live policy, HTTP proxy, SSH gateway

## Purpose

Finish the network layer on E2B:
- sandbox-to-sandbox exposed ports;
- live policy changes;
- the per-port HTTP proxy through the orchestrator;
- the SSH gateway relayed onto envd processes.

After this unit, spoond has **no `setns`, no netns and no `iptables`**.

## Preconditions

U08 is done, and the staging backend is running.

## Facts relied on

- **Policy and exposed ports today** (A1 §6.1):
  - `expose_ports` publishes guest TCP ports for other sandboxes to reach.
  - Reachable from leases with `lan` or `internet`, or from a `restricted`
    lease whose allowlist names it.
  - `exposedMap(l)` = `{"<port>":"<ExposedIP>:<port>"}`.
  - Reserved guest ports refused by `ValidateExposePorts`: `8888` (agent) and
    `9000` (shelley).
  - `handleCreate` returns 501 when the policy applier cannot expose.
- **HTTP proxy** (A1 §6.3):
  - Host forms `<lease-or-name>[-<port>].sandbox.lacy.casa` and
    `<label>.<user>.sandbox.lacy.casa`;
  - default port 3000;
  - forward-auth via `X-Proxy-Auth` and `Remote-User`;
  - dials `GuestHost:port` inside the netns (`dialInNetns`).
- **SSH gateway** (A1 §7):
  - listens on `:2222`;
  - a username starting `new` creates a sandbox; `ctl` is the control
    plane; otherwise the username is a lease id or name;
  - accepts **session** channels only;
  - relays `pty-req`, `env`, `window-change`, `signal`, `subsystem`,
    `shell`/`exec` and `exit-status` to the guest's sshd via `dialSandbox`;
  - `restartSSHD` re-runs sshd after a restore;
  - it gets `netns` and `guest_addr` from `GET /api/sandboxes/{id}/endpoint`;
  - flags `--shelly-binary-url` (default `http://10.43.0.1:8891/assets/shelley`),
    `--llm-gateway-url` (default `http://10.43.0.1:8891/llm/`) and
    `--ssh-images` (default `dev-base`);
  - backend auth: the gateway token plus the `X-Spoond-User-Id`
    impersonation header (A1 §5.3).
- **E2B's orchestrator proxy** (`127.0.0.1:5007`) routes by headers
  `E2b-Sandbox-Id` and `E2b-Sandbox-Port` when the Host is an IP. Every port
  except 49983 requires header `e2b-traffic-access-token`. It is HTTP only
  (A2 §5).
- **From the host,** `<HostIP>:<port>` reaches the guest port directly (A3 D4).
- **envd signals** are SIGTERM and SIGKILL only (U06).

## Changes

### 1. Exposed ports and peers (`api/service.go`, `api/netpolicy.go`)

- **Reserved ports.** `ValidateExposePorts` reserved ports become
  `[49983]` (envd). Remove the 501 "needs policy enforcement" branch from
  `handleCreate`: exposure is always supported.
- **`exposedMap(l)`** = `{"<port>": "<l.HostIP>:<port>"}` when
  `l.live()` (U08) and `HostIP` is set, else `{}`. Remove
  `ExposedIP`'s separate meaning: set `ExposedIP = HostIP`, and keep the
  field for the store column.
- **`peerAllowances(l)`** (replacing U08's `nil` stub). For every other
  lease `p` with `len(p.ExposePorts)>0`, `p.live()` and
  `p.HostIP != ""`. It is called only with `s.store.mu` held (from
  `egressForLocked`):
  - `l.NetPolicy` is `internet` or `lan` → add
    `{CIDR: p.HostIP+"/32", TCPPorts: p.ExposePorts}`;
  - `restricted` → add it **only if** `l.NetAllow` contains `p.ID`,
    `p.Name`, `"lease:"+p.ID` or `"lease:"+p.Name`;
  - `none` → nothing.

  Peers of **any** owner are included. That is parity with today's shared
  bridge.
- **Peer refresh.** Add `func (s *Service) refreshPeers(ctx context.Context)`.
  - Under `s.store.mu`, for every lease with `l.live()` and policy ≠
    `none`, compute `egressForLocked(l)`. Release the lock before any
    `sub` call.
  - Canonical JSON = `json.Marshal` of the `substrate.Egress` after
    `sort.Strings` on `AllowedCIDRs`, `DeniedCIDRs` and `AllowedDomains`,
    sorting `Private` by `CIDR`, and sorting each `TCPPorts`.
  - If it differs from the last applied value (kept in memory as
    `map[leaseID]string`), call `sub.UpdateEgress` and store the new value.
  - Call `refreshPeers` **asynchronously** (in a goroutine, serialized by a
    mutex) after any grant, resume, restart, clone, fork or release of a
    lease with `ExposePorts`, and after any network policy change.
- **Classification in `restricted`.** A `NetAllow` entry that matches a lease
  id or name is a peer reference, not a domain. Do not pass it to
  `AllowedDomains`.

### 1b. Stream access for SSH shares (`handleStream`)

Today SSH attach checks an `ssh` share (`handleEndpoint`), and `/stream`
checks an `http` share. Change `handleStream`'s lookup to: when the request
authenticated with the gateway token (`s.svc.gatewayToken`, constant-time
compare), accept `lookupWithShare(owner, id, ShareSSH)` or
`lookupWithShare(owner, id, ShareHTTP)`; otherwise only `ShareHTTP`, as
today.

### 2. Live policy change (new route)

`POST /api/sandboxes/{id}/network`:
- Body `{"network_policy":"none|lan|internet|restricted","egress_allowlist":[...]}`.
- Owner only. 400 on invalid policy, 404 unknown, 409 suspended.
- Update the lease (`NetPolicy`, `NetAllow`) and save.
- `sub.UpdateEgress(l.SandboxID, egressFor(l))`, then `refreshPeers`.
- Response `200`
  `{"id","network_policy","egress_allowlist"}`.

### 3. Stream: binary mode and new controls (`handleStream`, additive)

Extend the U08 relay. Existing clients see no change.

**Client first frame** gains `"binary": bool` (default `false`),
`"cols"` and `"rows"` (initial PTY size, default 80×24).

**When `binary` is true:**
- **Server → client:**
  - process output goes as **WebSocket binary frames**;
  - byte 0 is the channel: `1` stdout, `2` stderr, `3` pty;
  - the payload follows.
  - `started`, `exit_code` and `error` stay **text** JSON frames, exactly as
    in text mode.
- **Client → server:**
  - **binary** frames are raw input, passed to `proc.Write`;
  - text frames are control JSON (below).

**Control text frames (both modes):**

| Frame | Effect |
|---|---|
| `{"in":"..."}` | input (text mode only) |
| `{"resize":{"cols":C,"rows":R}}` | `proc.Resize` |
| `{"action":"stop"}` | `proc.Signal(false)` (SIGTERM) |
| `{"action":"kill"}` | `proc.Signal(true)` (SIGKILL) |
| `{"action":"eof"}` | `proc.CloseStdin()` |

### 4. HTTP proxy (`api/proxy.go`)

- Add `ProxyURL string` to `ServiceConfig` (U08), set in
  `cmd/spoond-backend/main.go` from the same `e2b.Config.ProxyURL` that
  `e2b.FromEnv()` returns (env `E2B_PROXY_URL`, default
  `http://127.0.0.1:5007`).
- Replace U08's interim direct dial.
- For a resolved lease and port, build an `httputil.ReverseProxy` whose
  director:
  - sets the URL scheme and host from `ServiceConfig.ProxyURL`, keeping the
    path and query;
  - sets `req.Host` to the host part of `ServiceConfig.ProxyURL` (on vm2
    `127.0.0.1:5007`). The orchestrator proxy routes by headers only when
    the Host is an IP (A2 §5);
  - to preserve the public hostname for guest apps, sets:
    - `X-Forwarded-Host: <the inbound request's Host>`;
    - `X-Forwarded-Proto: https` when the inbound request arrived over TLS or
      carries `X-Forwarded-Proto: https`, otherwise `http`;
    - `X-Forwarded-For` (appended; `httputil.ReverseProxy` does this).

    Guest apps see `Host: 127.0.0.1:5007`. Frameworks that validate the
    Host (e.g. dev servers with host allowlists) must use
    `X-Forwarded-Host`. Record this behaviour change in `docs/api.md`
    (U12 step 19).
  - sets headers:
    - `E2b-Sandbox-Id: <lease.SandboxID>`;
    - `E2b-Sandbox-Port: <port>`;
    - `e2b-traffic-access-token: <sub.TrafficToken(lease.SandboxID)>`;
  - first removes `X-Proxy-Auth`, `Remote-User`, `X-Spoond-User-Id`,
    `X-Bootstrap-Token`, `e2b-traffic-access-token` and every header whose
    name starts with `E2b-` (case-insensitive), then sets its own three.
- WebSocket upgrades must work (`httputil.ReverseProxy` handles them).
- A suspended lease returns 409, as today.
- Port 49983 is refused with 403 (envd is never exposed through spoond's
  proxy).
- Delete `api/netns_linux.go` and `api/netns_other.go`.

### 5. `GET /api/sandboxes/{id}/endpoint`

Keep the route for compatibility, with U08's interim response unchanged:
`{"id":l.ID,"forkd_id":l.SandboxID,"image":l.Image,"netns":"","guest_addr":l.HostIP}`
(additive-only, D5). The gateway no longer uses it.

### 6. Network policy code cleanup

- Delete from `api/netpolicy.go`: `NetnsPolicyApplier` and its `Apply` and
  `Expose` methods, `policyCommands`, `resolveEntry`, the `PolicyApplier`
  and `PortExposer` interfaces, `exposeCommands` and `parseIPv4Addr`. Delete
  `CanExposePorts` from `api/service.go`. (`SetNetpol`, `applyNetpol` and
  `NETPOL_DNS` were removed in U08.)
- Keep `NetworkPolicy`, `ValidNetworkPolicy` and `ValidateExposePorts`.
- Tests: delete the cases in `api/netpolicy_test.go` and
  `api/expose_test.go` that exercise the deleted functions (and the
  `fakeNetpol`/`fakeExposer` types). Their behavioural assertions are
  replaced by the new `egressFor`/`peerAllowances` tests (U08, this unit):
  policy → egress for each policy, and exposed ports reachable only by
  allowed peers. Keep every `ValidNetworkPolicy`/`ValidateExposePorts`
  case, updating reserved ports to `[49983]`.

### 7. SSH gateway (`cmd/spoond-sshd-gateway/main.go`)

- **Delete** `dialSandbox`, the netns and setns code, the nested SSH client
  to the guest, `restartSSHD`, the gateway's `endpoint` type and its
  `resolveEndpoint` function, and every comment that mentions `netns` or
  `setns`.
- **Change the flag defaults:**
  - `--shelly-binary-url` → `http://10.1.0.11:8891/assets/shelley`;
  - `--llm-gateway-url` → `http://10.1.0.11:8891/llm/`.

  (Staging passes `:18891` through `SHELLY_BINARY_URL` and
  `LLM_GATEWAY_URL`.)
- **New relay**, per accepted session channel:
  1. Collect requests until `shell`, `exec` or `subsystem`:
     - `pty-req` stores TERM, cols and rows, and replies true;
     - `env` stores the name/value, and replies true;
     - `x11-req` and `auth-agent-req@openssh.com` reply false.
  2. Choose the command:
     - `shell` → `["/bin/bash","-l"]`, with a PTY iff a `pty-req` was
       received;
     - `exec <cmd>` → `["/bin/bash","-c",cmd]`, with a PTY iff `pty-req`;
     - `subsystem sftp` →
       `["/bin/sh","-c","if [ -x /usr/lib/openssh/sftp-server ]; then exec /usr/lib/openssh/sftp-server; else exec /usr/lib/ssh/sftp-server; fi"]`,
       never a PTY;
     - any other subsystem → reply false and close.
  3. Set Env:
     - the collected env;
     - `TERM` (from `pty-req`, default `xterm-256color`);
     - `SSH_CONNECTION="<clientIP> <clientPort> 10.1.0.11 22"`;
     - `SSH_CLIENT="<clientIP> <clientPort> 22"`;
     - `USER=root`, `HOME=/root`, `LOGNAME=root`.

     `SSH_CONNECTION` is what triggers dev-base's tmux attach (U07).
  4. For `shell` requests only, write the MOTD (the existing `motd` string)
     to the channel's stdout. Then open a WebSocket to
     `<backend>/api/sandboxes/<leaseID>/stream` (`wss://` for an `https://`
     backend):
     - with a `websocket.Dialer` whose `TLSClientConfig` is the same TLS
       config the gateway's `backendClient()` uses;
     - headers `Authorization: Bearer <gateway token>` and
       `X-Spoond-User-Id: <user id>`;
     - the first frame is
       `{"args":...,"env":...,"pty":bool,"binary":true,"cols":C,"rows":R}`.
  5. **Relay:**
     - channel stdin → binary frames;
     - binary frames from the server: channel byte `1` or `3` → channel
       stdout, `2` → channel stderr;
     - `window-change` → `{"resize":...}`;
     - `signal`: `INT` → PTY input byte `0x03`, or ignored without a PTY;
       `TERM` → `{"action":"stop"}`; `KILL` → `{"action":"kill"}`; others
       ignored;
     - client EOF (CloseWrite) → `{"action":"eof"}`;
     - server `{"exit_code":N}` → send `exit-status` N, then close the
       channel;
     - server `{"error":...}` → write the message to stderr, send
       `exit-status` 255, then close.
  6. **Suspended lease** (new behaviour; today the gateway does not
     auto-resume): before step 4, `GET /api/sandboxes/<leaseID>`; if
     `"state"` is `suspended`, `POST /api/sandboxes/<leaseID>/resume`. If the
     resume response is not `200`, write
     `spoond: cannot resume sandbox <id>: <error>\n` to the channel's
     stderr, send `exit-status` 1 and close the channel.
- Everything else is unchanged: authentication, `new`/`ctl` routing, `ctl`
  verbs, the MOTD, `--ssh-images` gating and metrics.

### 8. Staging gateway on vm2 (Ops runner)

1. `/etc/spoond-staging/gateway.env` (mode 0600, exact):
   ```ini
   SPOOND_GATEWAY_TOKEN=<GATEWAY_TOKEN from /etc/spoond-staging/backend.env>
   SHELLY_BINARY_URL=http://10.1.0.11:18891/assets/shelley
   LLM_GATEWAY_URL=http://10.1.0.11:18891/llm/
   ```
   Do **not** copy `/etc/spoond-gateway.env`: its `GATEWAY_METRICS_LISTEN`
   would clash with production's port and kill the staging gateway. The
   backend URL is a flag (`--backend`), not an env variable.
2. `/etc/systemd/system/spoond-sshd-gateway-staging.service` (exact):
   ```ini
   [Unit]
   Description=spoond SSH gateway (staging)
   After=network.target spoond-backend-staging.service
   Wants=spoond-backend-staging.service

   [Service]
   EnvironmentFile=/etc/spoond-staging/gateway.env
   ExecStart=/opt/spoond-staging/spoond gateway \
     --listen :12222 \
     --host-key /etc/spoond-gateway/ssh_host_ed25519_key \
     --gateway-key /etc/spoond-gateway/gateway_ed25519 \
     --client-keys /etc/spoond-gateway/keys \
     --backend https://127.0.0.1:18890 \
     --backend-token ${SPOOND_GATEWAY_TOKEN} \
     --gateway-host sandbox.lacy.casa
   Restart=on-failure
   RestartSec=5

   [Install]
   WantedBy=multi-user.target
   ```
3. `systemctl daemon-reload && systemctl enable --now spoond-sshd-gateway-staging`.

### 9. Redeploy staging before conformance (Ops runner)

After this unit's commits are merged into `feat/e2b-substrate`:

```bash
export PATH=/usr/local/go/bin:$PATH
cd /root/src/spoond && git fetch && git checkout feat/e2b-substrate && git pull --ff-only
go build -o /opt/spoond-staging/spoond ./cmd/spoond
systemctl restart spoond-backend-staging
systemctl restart spoond-sshd-gateway-staging
```

## Tests

- **Unit tests:**
  - `peerAllowances` for each policy, including a restricted lease naming a
    peer by name and by `lease:<id>`;
  - `refreshPeers` calls `UpdateEgress` only on change (fake call log);
  - the stream binary framing, both directions, with a fake Process;
  - the proxy director sets exactly the three E2B headers, strips the
    inbound auth headers, and sets `X-Forwarded-Host`/`X-Forwarded-Proto`
    from the inbound request;
  - the gateway command selection for shell, exec, sftp and an unknown
    subsystem;
  - the gateway env composition, including `SSH_CONNECTION`.
- **Conformance** against staging, after §9, with the U08 staging
  settings plus `CONFORMANCE_SSH_GATEWAY=127.0.0.1:12222`,
  `CONFORMANCE_PROXY_URL=http://127.0.0.1:18891`, and the staging
  `PROXY_AUTH_SECRET`:
  - N1–N6;
  - L3–L4 still pass.

## Commits

1. `feat(api): exposed-port peers and live network policy on E2B egress`
2. `feat(api): binary stream mode, resize and signal controls`
3. `feat(api): HTTP proxy through the orchestrator; remove netns code`
4. `feat(gateway): relay SSH sessions onto sandbox processes`

## Done when

- The unit tests pass.
- Conformance N1–N6, L1–L6, S1–S4, D1–D2 and I1–I2 pass against staging.
- `grep -rn "setns\|iptables" --include=*.go api cmd` returns nothing.
  `grep -rn "netns" --include=*.go api cmd` returns only the line with the
  `"netns"` JSON key in `handleEndpoint`.

## Do not

- Do not touch production services.
- Do not add owner restrictions to peers (that would be a behaviour change).
