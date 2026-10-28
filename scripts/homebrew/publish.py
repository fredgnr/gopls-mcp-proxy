#!/usr/bin/env python3
"""Publish a tested formula with a Contents API compare-and-swap update."""

import argparse
import base64
import json
from pathlib import Path
import subprocess

from generate import guard_current, read_checksums, render

REPOSITORY = "fredgnr/homebrew-tap"
FORMULA_PATH = "Formula/gopls-mcp-proxy.rb"


def api(endpoint, payload=None):
    command = ["gh", "api", endpoint]
    if payload is not None:
        command += ["--method", "PUT", "--input", "-"]
    result = subprocess.run(
        command, input=json.dumps(payload) if payload is not None else None,
        text=True, capture_output=True, check=True, timeout=60,
    )
    return json.loads(result.stdout)


def publish(tag, formula_path, checksums_path):
    checksums = read_checksums(tag, Path(checksums_path).read_text())
    candidate = Path(formula_path).read_text()
    if candidate != render(tag, checksums):
        raise ValueError("Tested formula does not match this source template and release checksums")
    endpoint = f"repos/{REPOSITORY}/contents/{FORMULA_PATH}"
    # The tap is bootstrapped explicitly. Any missing file, auth failure or API
    # error must fail closed rather than being mistaken for an empty repository.
    current = api(endpoint + "?ref=main")
    if current.get("encoding") != "base64" or current.get("type") != "file":
        raise ValueError("Expected an existing base64-encoded formula file")
    content = base64.b64decode(current["content"]).decode("utf-8")
    guard_current(tag, checksums, content)
    if content == candidate:
        print(f"{REPOSITORY} already contains the tested {tag} formula")
        return False
    result = api(endpoint, {
        "message": f"chore: update gopls-mcp-proxy to {tag}",
        "content": base64.b64encode(candidate.encode()).decode(),
        "sha": current["sha"],
        "branch": "main",
    })
    print(f"Published {tag}: {result['commit']['html_url']}")
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--formula", type=Path, required=True)
    parser.add_argument("--checksums", type=Path, required=True)
    args = parser.parse_args()
    try:
        publish(args.tag, args.formula, args.checksums)
    except (ValueError, KeyError, OSError, subprocess.SubprocessError) as exc:
        # Never echo credential-bearing command output. The SHA guard rejects
        # concurrent edits; rerun the Homebrew workflow after reviewing the tap.
        parser.exit(1, f"Homebrew publication failed: {exc}\n")


if __name__ == "__main__":
    main()
