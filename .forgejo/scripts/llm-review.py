#!/usr/bin/env python3
"""LLM-based PR review for spoond Forgejo Actions.

Reads the PR diff from /tmp/pr.diff, sends it to the configured LLM,
writes the review to /tmp/review.md, and posts it as a PR comment.

Environment variables:
  LLM_API_KEY         — (required) provider API key
  LLM_PROVIDER        — "openai-compatible" (default), "openai", or "anthropic"
  LLM_MODEL           — model name (default: mimo-v2.5-free)
  LLM_ENDPOINT        — optional endpoint override (defaults to opencode.ai Zen)
  LLM_MODELS_ENDPOINT — optional models list endpoint for validation
  LLM_MAX_TOKENS      — (optional) max output tokens; default 8000
  LLM_REASONING_EFFORT — (optional) reasoning/thinking effort; default medium
  LLM_MAX_ATTEMPTS    — (optional) max LLM call attempts on transient errors; default 4
  LLM_RETRY_BACKOFF_BASE — (optional) base seconds for retry backoff; default 5
  PR_NUMBER           — pull request number (for context)
  FORGEJO_API         — Forgejo API base URL (e.g. https://host/api/v1/repos/owner/repo)
  FORGEJO_TOKEN       — Forgejo API token for posting comments
"""
import os
import json
import sys
import time
import random
import urllib.request
import urllib.error
from pathlib import Path

MAX_DIFF_CHARS = 80000

# Fetch available models from the models endpoint to validate the configured model.
models_endpoint = os.environ.get("LLM_MODELS_ENDPOINT", "https://opencode.ai/zen/v1/models")
provider = os.environ.get("LLM_PROVIDER", "openai-compatible")
model = os.environ.get("LLM_MODEL", "mimo-v2.5-free")

# Provider credentials and Forgejo context. The model is validated against
# the models endpoint further down, after the helper functions are defined.
api_key = os.environ.get("LLM_API_KEY") or ""
endpoint_override = os.environ.get("LLM_ENDPOINT", "")
forgejo_api = os.environ.get("FORGEJO_API", "")
forgejo_token = os.environ.get("FORGEJO_TOKEN", "")
pr_number = os.environ.get("PR_NUMBER", "")


def post_pr_comment(body):
    """Post a top-level PR comment when Forgejo context is available."""
    if not (forgejo_api and forgejo_token and pr_number):
        print("cannot post PR comment: missing Forgejo API/token/PR context", flush=True)
        return False
    try:
        payload = json.dumps({"body": body}).encode()
        comment_req = urllib.request.Request(
            f"{forgejo_api}/issues/{pr_number}/comments",
            data=payload,
            headers={
                "Authorization": f"token {forgejo_token}",
                "Content-Type": "application/json",
            },
            method="POST",
        )
        comment_resp = urllib.request.urlopen(comment_req, timeout=30)
        print(f"posted comment: HTTP {comment_resp.status}", flush=True)
        return True
    except Exception as e:
        print(f"failed to post comment: {e}", flush=True)
        return False


def review_header():
    pr_label = pr_number or "unknown"
    return f"""_🤖 AI Architecture Review — PR #{pr_label} — model: {model}_

"""


def fail_with_comment(message, exit_code=1):
    review = review_header() + f"_LLM review unavailable: {message}_"
    try:
        Path("/tmp/review.md").write_text(review)
    except OSError as e:
        print(f"failed to write review file: {e}", flush=True)
    post_pr_comment(review)
    print(f"llm-review: FAILED — {message}", flush=True)
    sys.exit(exit_code)


# Fetch and validate the model against the models endpoint. This must run
# AFTER the helper defs above so fail_with_comment is bound when validation
# fires.
try:
    models_req = urllib.request.Request(
        models_endpoint,
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
        },
    )
    models_resp = urllib.request.urlopen(models_req, timeout=15)
    models_data = json.loads(models_resp.read())
    if isinstance(models_data, dict) and "data" in models_data:
        available_models = [m.get("id", "") for m in models_data["data"]]
    elif isinstance(models_data, list):
        available_models = [m.get("id", "") if isinstance(m, dict) else str(m) for m in models_data]
    else:
        available_models = []
    print(f"llm-review: available models ({len(available_models)}): {', '.join(available_models)}", flush=True)
    if model not in available_models:
        fail_with_comment(
            f"configured model {model!r} is not in the available models list. "
            f"Available models: {', '.join(available_models)}"
        )
    print(f"llm-review: using model {model!r} (validated against {models_endpoint})", flush=True)
except Exception as e:
    print(f"llm-review: could not fetch/validate models from {models_endpoint}: {e}; proceeding with configured model {model!r}", flush=True)

diff = ""
try:
    diff = Path("/tmp/pr.diff").read_text()
except OSError as e:
    fail_with_comment(f"failed to read PR diff: {e}")

if len(diff) > MAX_DIFF_CHARS:
    diff = diff[:MAX_DIFF_CHARS] + "\n\n[... diff truncated ...]"

if not api_key:
    fail_with_comment(
        "missing required LLM_API_KEY secret. Configure the Forgejo Actions "
        "repository secret for the selected provider."
    )


def env_int(name, default):
    raw = os.environ.get(name, str(default))
    try:
        return int(raw)
    except ValueError:
        print(f"invalid integer env {name}={raw!r}; using {default}", flush=True)
        return default


def env_float(name, default):
    raw = os.environ.get(name, str(default))
    try:
        return float(raw)
    except ValueError:
        print(f"invalid float env {name}={raw!r}; using {default}", flush=True)
        return default


max_tokens = env_int("LLM_MAX_TOKENS", 8000)
reasoning_effort = os.environ.get("LLM_REASONING_EFFORT", "medium")

endpoint_preview = endpoint_override or "(provider default)"
print(
    f"llm-review: provider={provider} model={model} "
    f"max_tokens={max_tokens} reasoning_effort={reasoning_effort} endpoint={endpoint_preview}",
    flush=True,
)

prompt = (
    "You are a senior Go engineer reviewing a pull request for spoond — "
    "a control plane for forkd microVM sandboxes providing lease management, "
    "SSH gateway, HTTP proxy, LLM gateway, MCP/ACP agent endpoints, "
    "Forgejo Actions runner, and multi-user tenancy. "
    "Respond in English. "
    "Focus on **architecture, design, correctness, security, and logic** "
    "-- the kind of feedback that formatters, type checkers, go vet, and "
    "ordinary linting already catch. Skip style, naming, import ordering, and "
    "any issue a linter would flag. "
    "Only report problems that affect correctness, security, data integrity, "
    "auth/authorization boundaries, resource isolation (netns, tap devices, "
    "sandbox lifecycle), concurrency/race conditions, network policy, or "
    "architectural consistency. "
    "Be specific and reference line numbers.\n"
    "If nothing significant comes up, leave the **Architecture & Design** or "
    "**Correctness & Security** section out rather than writing filler.\n\n"
    f"Pull request #{os.environ['PR_NUMBER']}\n\n"
    "```diff\n"
    f"{diff}\n"
    "```\n\n"
    "Format your review as:\n\n"
    "## Summary\n"
    "(1-2 sentence high-level assessment)\n\n"
    "## Architecture & Design\n"
    "- ... (only non-trivial architectural concerns)\n\n"
    "## Correctness & Security\n"
    "- ... (actual bugs, data-loss risks, auth/bypass issues, race conditions)\n\n"
    "## Positive Highlights\n"
    "- ... (optional)\n"
)

# --- Retry with backoff for transient LLM provider errors -----------------
MAX_LLM_ATTEMPTS = env_int("LLM_MAX_ATTEMPTS", 4)
RETRY_BACKOFF_BASE = env_float("LLM_RETRY_BACKOFF_BASE", 5.0)


def _is_retryable(exc):
    if isinstance(exc, urllib.error.HTTPError):
        return exc.code == 429 or exc.code >= 500
    if isinstance(exc, urllib.error.URLError):
        return True
    if isinstance(exc, (TimeoutError, ConnectionError, OSError)):
        return True
    return False


def retry_or_raise(exc, attempt):
    if not _is_retryable(exc) or attempt >= MAX_LLM_ATTEMPTS:
        raise exc
    delay = RETRY_BACKOFF_BASE * (2 ** (attempt - 1))
    if isinstance(exc, urllib.error.HTTPError) and exc.code == 429:
        retry_after = exc.headers.get("Retry-After")
        if retry_after:
            try:
                delay = max(delay, float(retry_after))
            except ValueError:
                pass
    delay *= 1 + (random.random() * 0.25)
    print(
        f"llm-review: attempt {attempt}/{MAX_LLM_ATTEMPTS} failed "
        f"({type(exc).__name__}: {exc}); retrying in {delay:.1f}s",
        flush=True,
    )
    time.sleep(delay)


def urlopen_with_retry(req, timeout):
    for attempt in range(1, MAX_LLM_ATTEMPTS + 1):
        try:
            return urllib.request.urlopen(req, timeout=timeout)
        except urllib.error.HTTPError as e:
            retry_or_raise(e, attempt)
        except urllib.error.URLError as e:
            retry_or_raise(e, attempt)
        except TimeoutError as e:
            retry_or_raise(e, attempt)
        except ConnectionError as e:
            retry_or_raise(e, attempt)
        except OSError as e:
            retry_or_raise(e, attempt)
    raise RuntimeError("urlopen retry loop exhausted unexpectedly")


review_ok = False
try:
    if provider == "anthropic":
        url = endpoint_override or "https://api.anthropic.com/v1/messages"
        body = json.dumps({
            "model": model,
            "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": prompt}],
        }).encode()
        req = urllib.request.Request(
            url, data=body,
            headers={
                "x-api-key": api_key,
                "anthropic-version": "2023-06-01",
                "Content-Type": "application/json",
                "User-Agent": "Spoond-CodeReview/1.0",
            }
        )
        resp = urlopen_with_retry(req, 300)
        result = json.loads(resp.read())
        review_text = result["content"][0]["text"]
        if not review_text:
            raise RuntimeError(
                f"Anthropic returned no review content "
                f"(stop_reason={result.get('stop_reason')!r})."
            )
        review_ok = True
    else:
        url = endpoint_override or "https://opencode.ai/zen/v1/chat/completions"
        body = json.dumps({
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": max_tokens,
            "reasoning_effort": reasoning_effort,
        }).encode()
        req = urllib.request.Request(
            url, data=body,
            headers={
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json",
                "User-Agent": "Spoond-CodeReview/1.0",
            }
        )
        resp = urlopen_with_retry(req, 300)
        result = json.loads(resp.read())
        choice = result["choices"][0]
        message = choice.get("message") or {}
        review_text = message.get("content") or ""
        if not review_text:
            reasoning = (
                message.get("reasoning_content")
                or message.get("reasoning")
                or ""
            )
            raise RuntimeError(
                f"LLM returned no review content (content=null). "
                f"finish_reason={choice.get('finish_reason')!r}, "
                f"reasoning_discarded={len(reasoning)} chars. "
                "The model likely exhausted its output budget on thinking; "
                "raise LLM_MAX_TOKENS or switch to a higher-capacity model "
                "(LLM_MODEL)."
            )
        if choice.get("finish_reason") == "length":
            review_text += (
                "\n\n_⚠️ Review truncated — model hit its output token limit "
                f"(finish_reason=length, max_tokens={max_tokens}). Raise the "
                "LLM_MAX_TOKENS repo variable or use a higher-capacity model "
                "(LLM_MODEL)._"
            )
        review_ok = True
except Exception as e:
    review_text = f"_LLM review unavailable: {e}_"
    review_ok = False

review = review_header() + review_text

try:
    Path("/tmp/review.md").write_text(review)
except OSError as e:
    fail_with_comment(f"failed to write review file: {e}")

posted = post_pr_comment(review)

if not review_ok:
    print(
        "llm-review: FAILED — no meaningful review produced "
        "(see PR comment if it was posted)",
        flush=True,
    )
    sys.exit(1)
if not posted:
    print("llm-review: FAILED — could not post review comment", flush=True)
    sys.exit(1)

print("llm-review: ok", flush=True)