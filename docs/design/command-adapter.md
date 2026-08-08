# Command Adapter — Design

The **command adapter** is a synchronous, caller-driven consumer of the
lease API. Where the Forgejo runner is *event-driven* ("a push happened,
execute the workflow"), the command adapter is *imperative* ("run this
command/snippet in a sandbox, return the result"). It is the
exe.dev/Replit-style shape of the platform.

It is a thin HTTP front-end over the same hexagonal core the Forgejo
runner uses. It implements the `JobSource`/`JobSink` ports with a simple
HTTP/JSON contract instead of the Forgejo Connect RPC protocol.

## Why it exists

The homelab has callers that want on-demand compute without going
through Forgejo: a Raspberry Pi, an open-code harness, a Hermes agent, a
web form. They want to say "spin me up a sandbox, run this, give me the
answer" and get it back synchronously. The command adapter is that
front door.

## Job contract

A "job" here is not a workflow YAML — it's a command (or snippet) plus
an image and a narrow set of knobs. The core `Executor` already runs
`run:` steps; the command adapter maps a single request onto that.

### Request

```
POST /v1/run
Authorization: Bearer <consumer-token>

{
  "image": "py-numpy",          // forkd snapshot tag (required)
  "command": "analyze(data)",   // shell command or snippet (required)
  "cwd": "/workspace",          // optional working dir
  "env": { "DATA": "..." },     // optional env vars
  "timeout": 60,                // seconds, capped at 300
  "ttl": 300                    // sandbox lease TTL, capped at maxTTL
}
```

### Response (synchronous)

```
200 OK
{
  "job_id": "abc123",
  "stdout": "...",
  "stderr": "...",
  "exit": 0,
  "duration_ms": 1234
}
```

### Errors

```
400 { "error": "unknown image tag: py-numpy" }        // bad request
401 { "error": "unauthorized" }                       // bad/missing token
429 { "error": "rate limited" }                       // per-consumer quota
500 { "error": "internal error" }                    // sanitized, no internals
```

## Design decisions

1. **Synchronous by default.** The caller blocks for the result. This is
   the simplest contract and covers the pi/harness/agent use cases. A
   later async mode (`202 Accepted` + `GET /v1/jobs/{id}`) can be added
   without breaking the sync path.

2. **Reuses the core `Executor`.** The command adapter is a
   `JobSource`/`JobSink` pair that turns one HTTP request into one
   `Job` and returns the `JobState`. No new execution logic.

3. **Image-tag spec, not inline env.** The caller picks a pre-baked
   snapshot tag (`py-numpy`); environment definition lives in the baked
   layer, not the request. This matches the lease API's R2.

4. **Per-consumer quota.** The lease API has no per-consumer sandbox
   quota (a known gap). The command adapter should enforce a cap on
   concurrent in-flight jobs per token to prevent one consumer from
   exhausting the pool.

5. **Auth at the adapter, isolation at the kernel.** Same posture as the
   lease API: bearer tokens gate access; forkd provides microVM
   isolation.

## What it enables

- A **Raspberry Pi** posts a sensor-analysis command and gets the result.
- An **open-code harness** runs a snippet in a clean, pre-cached
  environment.
- A **Hermes agent** (or any agent) calls `POST /v1/run` for a quick
  code/analysis task instead of shelling out on the host.
- A **web form** offers "run this in a sandbox" without standing up a
  full CI pipeline.

## Not in scope (yet)

- Async job submission + polling
- Streaming output (partial results before completion)
- File upload/download into the sandbox
- Multi-step workflows (the Forgejo runner already covers that shape)

## Open questions

- Should the response include the sandbox address for interactive use,
  or is it strictly fire-and-collect?
- Do we need a `GET /v1/images` to let callers discover available tags?
- Rate limiting: fixed per-token cap, or a shared pool budget?
