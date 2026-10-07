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
| `new` | `new [dev\|go\|py\|python\|elixir\|llm\|base]` or `new <full-image>` | create a lease; `new` alone = dev-base. A full image name works too (checked against `/api/images`); more short names via the gateway's `--image-aliases` |
| `ls` | `ls [--json]` | list leases (pretty table default) |
| `stat` | `stat <id> [--json]` | guest metrics (cpu/mem/disk/net) |
| `rm` | `rm <id>` | delete a lease |
| `keepalive` | `keepalive <id>` (alias `ka`) | extend persistent lease |
| `suspend` | `suspend <id>` | snapshot + stop (persistent leases) |
| `resume` | `resume <id>` | start from snapshot |
| `restart` | `restart <id>` | persistent: pause + resume, guest state kept (not a reboot: a hung process stays hung); plain: fresh guest from the image. `restart <id> --cold` gives any lease a fresh guest from the image's current build, keeping the lease id (memory, ephemeral disk and the resume chain lost; generation bumps; secrets re-written) |
| `cp` | `cp <id> [tag]` (alias `clone`) | checkpoint the running lease + spawn a clone from it |
| `create` | `create [image] [--snapshot <name[@v]>] [--ttl N] [--persistent]` | create a lease; with `--snapshot` it starts from a named snapshot version instead of the image's current build (the version's memory, a new lease id and generation `1`; see [api.md](api.md#named-snapshots-27-83)) |
| `snapshot save` | `snapshot save <lease> <name> [--key K] [--keep N]` | save a live lease as a named snapshot: checkpoint it, insert a new version, and scrub `/run/secrets` first. `--key` makes the save idempotent; `--keep` sets the name's retention (`1`–`20`) |
| `snapshot ls` | `snapshot ls [prefix]` | list your named snapshots with their versions, `in_use` and `stale` (pretty table; `--json` for raw). `prefix` narrows the list |
| `snapshot show` | `snapshot show <name[@v]>` | show one version (the latest when `@v` is omitted) |
| `snapshot rm` | `snapshot rm <name[@v]> [--force]` | delete one version, or every version when `@v` is omitted. `409` while a live lease started from it; `--force` drops the row anyway |
| `tag` | `tag <id> <name>` | friendly name (then `ssh <name>@…`) |
| `comment` | `comment <id> [text…]` | annotate; no text clears |
| `share` | `share add <id> <user> [ssh\|http] [ttl]` / `share ls` / `share rm <id> <user>` | grant/list/revoke lease access (`share ls` lists every share on your leases; mode defaults to `http`) |
| `ssh-key` | `ssh-key ls` / `ssh-key add <pubkey> <name>` / `ssh-key rm <user-id>` (alias `keys`) | manage users & SSH keys (`ls`/`add` are admin after bootstrap) |
| `shelly` | `shelly <id>` (alias `agent`) | start the in-lease Shelley coding agent |
| `prompt` | `prompt <id> <message…>` | message the Shelley agent (waits for reply) |

To run a command *inside* a lease over SSH, connect as the lease
(`ssh <id>@sandbox.example.com "uname -a"`) — there is no `exec` control
verb; `POST /api/leases/{id}/exec` is the API equivalent (see
[api.md](api.md)).

`suspend`/`resume`/`restart`/`cp` act on the E2B snapshot machinery
(pause = snapshot + stop, resume = restore with the same sandbox id):
what that means for a live tmux session is covered in
[substrate.md](substrate.md#restart-and-crash-behaviour). The exception
is `restart <id> --cold`, which skips the snapshot machinery: the lease
gets a brand-new guest from the image's current build (see the restart
row above and [api.md](api.md#post-apileasesidrestart--pause-and-resume-or-a-fresh-guest)).

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
| `SPOOND_CTL_HOST` | `sandbox.example.com` | gateway host |
| `SPOOND_CTL_PORT` | `2222` | gateway port |
| `SPOOND_CTL_KEY` | `~/.ssh/id_ed25519` | your key |

(The pre-2.0 `FORKD_CTL_*` names still work and log a one-line
deprecation warning — see the "Renamed in 2.0" table in
[setup.md](setup.md).)

```bash
spoondctl new go          # create
spoondctl ls              # list
spoondctl ssh buildbox    # drop into a shell (delegates to ssh)
spoondctl cp <id> exp1    # clone
spoondctl rm <id>
spoondctl help
```

Its verb set is `new`, `create`, `snapshot`, `ls`, `rm`, `keepalive`, `suspend`, `resume`,
`restart`, `cp`/`clone`, `shelly`/`agent`, `tag`, `comment`, `whoami`,
`prompt` and `ssh`. `stat`, `share` and `ssh-key` are gateway verbs
only — use `ssh ctl@… "stat <id>"` for those.

## SSH gateway usernames (the non-ctl flows)

| Username | Behavior |
|---|---|
| `ctl` | control plane (this page) |
| `new`, `new-<image>` | auto-create a lease (persistent dev-base by default) and attach |
| `<lease-id>` | attach to an existing lease |
| `<name>` | attach by friendly name (after `tag`) |
