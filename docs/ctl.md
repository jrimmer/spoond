# Control plane (`ctl`) reference

The SSH gateway doubles as a control plane: connect with the `ctl`
username and pass a command. The gateway authenticates your key, calls
the backend on your behalf, and prints the result.

```bash
ssh ctl@sandbox.example.com "ls"
ssh ctl@sandbox.example.com -p 2222 "stat <id>"
```

The backend routes these calls as *you* (the gateway impersonates the
authenticated SSH user via its service token), so `ls` shows your leases
only.

## Output contract

- **Default: human-readable** — `ls` prints a table, `whoami` a single
  line, `stat` a shaped report.
- **`--json` anywhere in the command** switches to raw machine JSON
  (identical to the backend API response). Scripts and LLM tools should
  always use `--json`.

## Verbs

| Verb | Usage | Notes |
|---|---|---|
| `help` | `help` | list all verbs |
| `whoami` | `whoami` | current key identity (user id/name when the identity store is active) |
| `new` | `new [dev\|go\|py\|python\|elixir\|llm\|base]` or `new <full-image>` | create a sandbox; `new` alone = dev-base. A full image name works too (checked against `/api/images`); more short names via the gateway's `--image-aliases` |
| `ls` | `ls [--json]` | list leases (pretty table default) |
| `stat` | `stat <id> [--json]` | guest metrics (cpu/mem/disk/net) |
| `rm` | `rm <id>` | delete a lease |
| `keepalive` | `keepalive <id>` (alias `ka`) | extend persistent lease |
| `suspend` | `suspend <id>` | snapshot + stop (persistent leases) |
| `resume` | `resume <id>` | start from snapshot |
| `restart` | `restart <id>` | reboot — persistent: pause + resume (lossless); plain: fresh sandbox from the image |
| `cp` | `cp <id> [tag]` (alias `clone`) | checkpoint running sandbox + spawn a clone from it |
| `tag` | `tag <id> <name>` | friendly name (then `ssh <name>@…`) |
| `comment` | `comment <id> [text…]` | annotate; no text clears |
| `share` | `share add <id> <user> [ssh\|http] [ttl]` / `share ls` / `share rm <id> <user>` | grant/list/revoke lease access (`share ls` lists every share on your leases; mode defaults to `http`) |
| `ssh-key` | `ssh-key ls` / `ssh-key add <pubkey> <name>` / `ssh-key rm <user-id>` (alias `keys`) | manage users & SSH keys (`ls`/`add` are admin after bootstrap) |
| `shelly` | `shelly <id>` (alias `agent`) | start the in-sandbox Shelley coding agent |
| `prompt` | `prompt <id> <message…>` | message the Shelley agent (waits for reply) |

To run a command *inside* a sandbox over SSH, connect as the lease
(`ssh <id>@sandbox.example.com "uname -a"`) — there is no `exec` control
verb; `POST /api/sandboxes/{id}/exec` is the API equivalent (see
[api.md](api.md)).

`suspend`/`resume`/`restart`/`cp` act on the E2B snapshot machinery
(pause = snapshot + stop, resume = restore with the same sandbox id):
what that means for a live tmux session is covered in
[substrate.md](substrate.md#restart-and-crash-behaviour).

## Examples

```bash
# List with a friendly table
ssh ctl@sandbox.example.com "ls"
#  ID             IMAGE      STATE      EXPIRES                ADDRESS          NAME
#  39c5099a82e4…  dev-base   running    2026-08-11 01:56 UTC

# Raw JSON for scripts
ssh ctl@sandbox.example.com "ls --json" | jq '.sandboxes[0].id'

# Create + tag + use the name
ssh ctl@sandbox.example.com "new dev"
ssh ctl@sandbox.example.com "ls --json"
ssh ctl@sandbox.example.com "tag <id> buildbox"
ssh buildbox@sandbox.example.com "uname -a"

# Stats
ssh ctl@sandbox.example.com "stat <id>"

# Clean up
ssh ctl@sandbox.example.com "rm <id>"
```

## `spoondctl` — the same verbs from a shell

`spoondctl` is a thin CLI over exactly this surface: it builds the
`ssh ctl@…` command for you, runs it and prints the JSON. No business
logic — the backend stays the only source of state. It is also
reachable as `spoond ctl` from the consolidated binary
(`go build ./cmd/spoond`); `go build ./cmd/spoondctl` produces the
standalone `spoondctl`.

| Variable | Default | Purpose |
|---|---|---|
| `FORKD_CTL_HOST` | `sandbox.lacy.casa` | gateway host |
| `FORKD_CTL_PORT` | `2222` | gateway port |
| `FORKD_CTL_KEY` | `~/.ssh/id_ed25519` | your key |

(`FORKD_CTL_*` are the live names; the `FORKD_` prefix is historical.)

```bash
spoondctl new go          # create
spoondctl ls              # list
spoondctl ssh buildbox    # drop into a shell (delegates to ssh)
spoondctl cp <id> exp1    # clone
spoondctl rm <id>
spoondctl help
```

Its verb set is `new`, `ls`, `rm`, `keepalive`, `suspend`, `resume`,
`restart`, `cp`/`clone`, `shelly`/`agent`, `tag`, `comment`, `whoami`,
`prompt` and `ssh`. `stat`, `share` and `ssh-key` are gateway verbs
only — use `ssh ctl@… "stat <id>"` for those.

## SSH gateway usernames (the non-ctl flows)

| Username | Behavior |
|---|---|
| `ctl` | control plane (this page) |
| `new`, `new-<image>` | auto-create a sandbox (persistent dev-base by default) and attach |
| `<lease-id>` | attach to an existing lease |
| `<name>` | attach by friendly name (after `tag`) |
