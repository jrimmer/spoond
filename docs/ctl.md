# Control plane (`ctl`) reference

The SSH gateway doubles as a control plane: connect with the `ctl`
username and pass a command. The gateway authenticates your key, calls
the backend on your behalf, and prints the result.

```bash
ssh ctl@sandbox.example.com "ls"
ssh ctl@sandbox.example.com -p 2222 "stat <id>"
```

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
| `new` | `new [dev\|go\|py\|elixir\|llm]` | create a sandbox; `new` alone = dev-base |
| `ls` | `ls [--json]` | list leases (pretty table default) |
| `stat` | `stat <id> [--json]` | guest metrics (cpu/mem/disk/net) |
| `rm` | `rm <id>` | delete a lease |
| `exec` | `exec <id> <command…>` | run a command via the API and print stdout |
| `keepalive` | `keepalive <id>` (alias `ka`) | extend persistent lease |
| `suspend` | `suspend <id>` | snapshot + stop (workspace-backed only) |
| `resume` | `resume <id>` | start from snapshot |
| `restart` | `restart <id>` | reboot the guest |
| `cp` | `cp <id> [tag]` (alias `clone`) | branch snapshot + spawn clone |
| `tag` | `tag <id> <name>` | friendly name (then `ssh <name>@…`) |
| `comment` | `comment <id> [text…]` | annotate; no text clears |
| `env` | `env ls` / `env new <repo> <pr> [image]` / `env rm <repo> <pr>` / `env id <repo> <pr>` | per-PR ephemeral environments (see `docs/environments.md`) |
| `share` | `share add <id> <user> [ssh\|http] [ttl]` / `share ls <id>` / `share rm <id> <user>` | grant/list/revoke lease access (epic #26 U9) |
| `ssh-key` | `ssh-key ls` / `ssh-key add <pubkey> <name>` / `ssh-key rm <user-id>` | manage users & SSH keys (v1.1; `ls`/`add` are admin after bootstrap) |
| `shelly` | `shelly <id>` (alias `agent`) | start the in-sandbox Shelley coding agent |
| `prompt` | `prompt <id> <message…>` | message the Shelley agent (waits for reply) |

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

## SSH gateway usernames (the non-ctl flows)

| Username | Behavior |
|---|---|
| `ctl` | control plane (this page) |
| `new`, `new-<image>` | auto-create a sandbox (persistent dev-base by default) and attach |
| `<lease-id>` | attach to an existing lease |
| `<name>` | attach by friendly name (after `tag`) |
