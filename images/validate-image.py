#!/usr/bin/env python3
"""
forkd image inquiry — validate a repo against the image manifest.

Usage:
    images/validate-image.py <repo-path-or-url>
    images/validate-image.py --manifest images/manifest.yaml <repo-path>

Detects the repo's language/capability from its files (go.mod, Cargo.toml,
mix.exs, pyproject.toml, etc.), then checks the manifest for a baked image
that covers it. Reports:
    COVERED   -> a baked image exists; use it
    NEEDS     -> no baked image; a new one must be created (name suggested)
    UNKNOWN   -> couldn't detect; needs human input

This is the guardrail against image proliferation: it only ever suggests
a NEW image when a genuinely new capability is missing, and it names by
capability (go-base, llm-review), never by repo.
"""
import argparse
import os
import re
import subprocess
import sys
import tempfile
import urllib.request

# capability -> (detector files, regex, suggested image name)
DETECTORS = [
    # (capability, image_name, [marker files], [regex patterns])
    ("golang",    "go-base",     ["go.mod", "go.sum", "Gopkg.toml"], [r"^module\s"]),
    ("rust",      "rust-base",   ["Cargo.toml", "Cargo.lock"],       [r"\[package\]"]),
    ("elixir",    "elixir-base", ["mix.exs", "mix.lock"],            [r"defmodule"]),
    ("python",    "py-base",     ["pyproject.toml", "setup.py", "requirements.txt", "Pipfile"], [r"^\[project\]", r"^from setuptools", r"^[a-zA-Z0-9_\-]+==|^[a-zA-Z0-9_\-]+>=|^[a-zA-Z0-9_\-]+~="]),
    ("node",      "node-base",   ["package.json", "yarn.lock", "pnpm-lock.yaml"], [r'"name"']),
    ("llm-review", "llm-review", [".llm-review", "llm-review.yml", "review.yml"], [r"llm", r"review"]),
]

# Capabilities that are language-agnostic functions (not tied to a language).
FUNCTION_CAPABILITIES = {"llm-review"}


def detect_capability(repo_path):
    """Return (capability, image_name) or (None, None) if undetectable."""
    hits = []
    for capability, image, markers, patterns in DETECTORS:
        for marker in markers:
            p = os.path.join(repo_path, marker)
            if os.path.isfile(p):
                # Confirm with a regex on the file content when patterns given.
                if patterns:
                    try:
                        with open(p, "r", errors="ignore") as f:
                            content = f.read(20000)
                        if any(re.search(pat, content, re.MULTILINE) for pat in patterns):
                            hits.append((capability, image))
                            break
                    except OSError:
                        pass
                else:
                    hits.append((capability, image))
                    break
    if not hits:
        return None, None
    # Prefer a function capability if present (e.g. llm-review over a language).
    for cap, img in hits:
        if cap in FUNCTION_CAPABILITIES:
            return cap, img
    return hits[0]


def load_manifest(path):
    """Load manifest.yaml into a dict of image_name -> entry."""
    import yaml
    with open(path) as f:
        data = yaml.safe_load(f)
    return {img["name"]: img for img in data.get("images", [])}


def check_coverage(manifest, capability, image_name):
    """Return (status, message)."""
    if image_name is None:
        return "UNKNOWN", "could not detect language/capability from repo files"
    entry = manifest.get(image_name)
    if entry and entry.get("baked"):
        return "COVERED", f"'{image_name}' covers capability '{capability}' (baked)"
    if entry and not entry.get("baked"):
        return "NEEDS", f"'{image_name}' is planned but NOT baked — create it"
    return "NEEDS", f"no image for capability '{capability}' — create '{image_name}'"


def fetch_repo(url):
    """Clone a repo URL to a temp dir, return the path."""
    tmp = tempfile.mkdtemp(prefix="forkd-inquiry-")
    subprocess.run(["git", "clone", "--depth", "1", url, tmp],
                   check=True, capture_output=True)
    return tmp


def main():
    ap = argparse.ArgumentParser(description="Validate a repo against the forkd image manifest")
    ap.add_argument("repo", help="local path or git URL of the repo to interrogate")
    ap.add_argument("--manifest", default=os.path.join(os.path.dirname(__file__), "manifest.yaml"),
                    help="path to manifest.yaml")
    args = ap.parse_args()

    manifest = load_manifest(args.manifest)

    repo_path = args.repo
    cleanup = None
    if re.match(r"^(https?|git)://", args.repo) or args.repo.endswith(".git"):
        repo_path = fetch_repo(args.repo)
        cleanup = repo_path

    capability, image_name = detect_capability(repo_path)
    status, message = check_coverage(manifest, capability, image_name)

    print(f"repo:        {args.repo}")
    print(f"capability:  {capability or '(none detected)'}")
    print(f"image:       {image_name or '(none)'}")
    print(f"status:      {status}")
    print(f"message:     {message}")

    if cleanup:
        import shutil
        shutil.rmtree(cleanup, ignore_errors=True)

    # Exit code: 0 = covered, 1 = needs image, 2 = unknown
    return {"COVERED": 0, "NEEDS": 1, "UNKNOWN": 2}[status]


if __name__ == "__main__":
    sys.exit(main())
