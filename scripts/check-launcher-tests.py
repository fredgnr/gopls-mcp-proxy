#!/usr/bin/env python3
"""Require successful default-launcher and per-call executions in go test JSON."""
import json
from pathlib import Path
import re
import sys


def main():
    root = Path(__file__).resolve().parent.parent
    kind, log = sys.argv[1:]
    if kind == "unit":
        paths = [root / "cmd/gopls-mcp-proxy/main_test.go", *sorted((root / "internal/launcher").glob("*_test.go"))]
        per_call_path = root / "internal/proxy/per_call_test.go"
        required = {"TestDefaultGoplsIntent", "TestIdentityChecksStartupAndCurrentInodes", "TestPerCallSchedulerWaitsForRequestAndResponseDelivery"}
        prefix = "Test"
    elif kind == "raw":
        paths = sorted((root / "integration").glob("default_launcher*_test.go"))
        per_call_path = root / "integration/per_call_test.go"
        required = {"TestRawDefaultLauncherFixtureNeverRefreshesItself", "TestRawDefaultLauncherRefreshAndNoReplay", "TestRawPerCallDefaultLauncherFreshBackends"}
        prefix = "TestRaw"
    elif kind == "real":
        paths = [root / "integration/real_gopls_test.go"]
        per_call_path = root / "integration/per_call_real_gopls_test.go"
        required = {"TestRealGoplsDefaultLauncherTwoProtobufUpgrades", "TestRealGoplsPerCallFreshProcessAndFileChanges"}
        prefix = "TestRealGopls"
    else:
        raise SystemExit("expected unit, raw, or real")
    pattern = r"^func (Test\w+)\(t \*testing.T\)"
    per_call_names = {name for name in re.findall(pattern, per_call_path.read_text(), re.M) if name.startswith(prefix)}
    if not per_call_names:
        raise SystemExit(f"per-call tests missing from source: {per_call_path}")
    names = {name for path in paths for name in re.findall(pattern, path.read_text(), re.M) if name.startswith(prefix)} | per_call_names
    if not required <= names:
        raise SystemExit(f"required launcher tests missing from source: {sorted(required - names)}")
    actions = {}
    for line in Path(log).read_text().splitlines():
        event = json.loads(line)
        name = event.get("Test", "")
        if name.split("/")[0] in names:
            actions.setdefault(name, []).append(event["Action"])
    missing = sorted(name for name in names if "run" not in actions.get(name, []) or "pass" not in actions.get(name, []))
    failed = sorted(name for name, events in actions.items() if "fail" in events or "skip" in events)
    if missing or failed:
        raise SystemExit(f"launcher tests not executed successfully: missing={missing}, failed/skipped={failed}")
    print(json.dumps({"suite": kind, "status": "pass", "tests": sorted(names), "assertion_scope": "go test execution and assertions; not real Codex acceptance"}, indent=2))


if __name__ == "__main__":
    main()
