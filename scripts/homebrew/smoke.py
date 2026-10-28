#!/usr/bin/env python3
"""Exercise an installed proxy with independently provisioned Go and gopls."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import selectors
import subprocess
import tempfile
import time


def exercise(proxy: Path, gopls: Path, go: Path, root: Path, explicit: bool) -> None:
    mode = "explicit" if explicit else "PATH"
    workspace = root / mode / "workspace"
    workspace.mkdir(parents=True)
    (workspace / "go.mod").write_text("module example.com/homebrew-smoke\n\ngo 1.25.0\n")
    (workspace / "main.go").write_text("package main\n\nfunc main() {}\n")
    log_path = workspace.parent / "proxy.log"
    # Neither Homebrew's bin directory nor the user's shell environment is
    # required. Explicit mode also excludes the directory containing gopls.
    path = [str(go.parent), "/usr/bin", "/bin"]
    if not explicit:
        path.insert(0, str(gopls.parent))
    env = dict(os.environ, PATH=os.pathsep.join(path), GOWORK="off",
               GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off")
    command = [str(proxy)]
    if explicit:
        command.extend(["--", str(gopls), "mcp"])

    with log_path.open("wb") as log:
        process = subprocess.Popen(command, cwd=workspace, env=env,
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=log)
        assert process.stdin is not None and process.stdout is not None
        selector = selectors.DefaultSelector()
        selector.register(process.stdout, selectors.EVENT_READ)
        pending = bytearray()
        deadline = time.monotonic() + 60

        def send(message: dict) -> None:
            process.stdin.write((json.dumps(dict(jsonrpc="2.0", **message)) + "\n").encode())
            process.stdin.flush()

        def receive_result(request_id: int) -> dict:
            while True:
                while b"\n" not in pending:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0 or not selector.select(remaining):
                        raise TimeoutError(f"{mode}: timed out waiting for MCP response {request_id}")
                    chunk = os.read(process.stdout.fileno(), 65536)
                    if not chunk:
                        raise RuntimeError(f"{mode}: stdout closed before response {request_id}")
                    pending.extend(chunk)
                line, _, rest = pending.partition(b"\n")
                pending[:] = rest
                message = json.loads(line)
                if message.get("method") == "roots/list" and "id" in message:
                    send({"id": message["id"], "result": {"roots": [
                        {"uri": workspace.as_uri(), "name": "homebrew-smoke"},
                    ]}})
                elif "method" in message and "id" in message:
                    send({"id": message["id"], "error": {
                        "code": -32601, "message": "Method not found",
                    }})
                elif message.get("id") == request_id:
                    if "error" in message:
                        raise RuntimeError(f"{mode}: MCP error: {message['error']}")
                    return message["result"]

        try:
            send({"id": 1, "method": "initialize", "params": {
                "protocolVersion": "2025-11-25",
                "clientInfo": {"name": "homebrew-smoke", "version": "1"},
                "capabilities": {"roots": {"listChanged": False}},
            }})
            initialization = receive_result(1)
            if "gopls" not in initialization["serverInfo"]["name"]:
                raise AssertionError(f"{mode}: unexpected backend: {initialization}")
            send({"method": "notifications/initialized"})
            send({"id": 2, "method": "tools/list", "params": {}})
            if not receive_result(2)["tools"]:
                raise AssertionError(f"{mode}: gopls returned no tools")
            process.stdin.close()
            if process.wait(timeout=15) != 0:
                raise AssertionError(f"{mode}: proxy failed after stdin EOF")
        finally:
            if not process.stdin.closed:
                process.stdin.close()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
            selector.close()
            process.stdout.close()
            log.flush()
            print(log_path.read_text(), end="")

    events = []
    for line in log_path.read_text().splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(event, dict):
            events.append(event)
    starts = [event for event in events if event.get("msg") == "proxy starting"]
    if len(starts) != 1 or starts[0].get("selection_source") != mode:
        raise AssertionError(f"{mode}: expected user-selected backend source, got {starts}")
    if Path(starts[0]["backend"]).resolve() != gopls:
        raise AssertionError(f"{mode}: proxy selected a different gopls: {starts[0]}")
    print(f"PASS: {mode} backend discovery, initialize, tools/list, and EOF cleanup")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--proxy", type=Path, required=True)
    parser.add_argument("--gopls", type=Path, required=True)
    parser.add_argument("--go", type=Path, required=True)
    args = parser.parse_args()
    # Preserve the public proxy symlink: the test must exercise the command the
    # MCP host uses. Resolve only the independently installed backend/toolchain.
    proxy, gopls, go = args.proxy.absolute(), args.gopls.resolve(), args.go.resolve()
    for program in (proxy, gopls, go):
        if not program.is_file() or not os.access(program, os.X_OK):
            parser.error(f"not an executable file: {program}")
    if go.parent == gopls.parent:
        parser.error("install the CI gopls in its own directory to isolate explicit mode")
    with tempfile.TemporaryDirectory(prefix="homebrew-mcp-") as directory:
        root = Path(directory).resolve()
        exercise(proxy, gopls, go, root, explicit=False)
        exercise(proxy, gopls, go, root, explicit=True)


if __name__ == "__main__":
    main()
