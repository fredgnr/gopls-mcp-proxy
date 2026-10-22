#!/usr/bin/env python3
"""Reject missing/skipped lifecycle tests and incomplete real-process evidence."""
import json
from pathlib import Path
import re
import sys


def main():
    root = Path(__file__).resolve().parent.parent
    kind, log, *extra = sys.argv[1:]
    if kind == "unit":
        paths = sorted((root / "internal/proxy").glob("*lifecycle*_test.go")) + sorted((root / "internal/proxy").glob("*suspend*_test.go"))
        paths += sorted((root / "internal/proxy").glob("*on_demand*_test.go"))
        prefix = "Test"
    elif kind == "raw":
        paths = [root / "integration/on_demand_test.go", root / "integration/lifecycle_observer_test.go"]
        prefix = "TestRaw"
    elif kind == "real":
        paths = [root / "integration/on_demand_real_gopls_test.go"]
        prefix = "TestRealGopls"
    else:
        raise SystemExit("expected unit, raw, or real")
    names = {name for path in paths for name in re.findall(r"^func (Test\w+)\(t \*testing.T\)", path.read_text(), re.M) if name.startswith(prefix)}
    if kind == "unit":
        names.add("TestBackendLifecycleOptions")
    if not names or (kind == "real" and "TestRealGoplsOnDemandLifecycle" not in names):
        raise SystemExit("lifecycle test source coverage missing")
    actions = {}
    for line in Path(log).read_text().splitlines():
        event = json.loads(line)
        name = event.get("Test", "")
        if name.split("/")[0] in names:
            actions.setdefault(name, []).append(event["Action"])
    missing = sorted(name for name in names if not {"run", "pass"} <= set(actions.get(name, [])))
    failed = sorted(name for name, values in actions.items() if {"skip", "fail"} & set(values))
    if missing or failed:
        raise SystemExit(f"lifecycle tests missing/failed/skipped: {missing=} {failed=}")
    if kind == "real":
        if len(extra) != 1:
            raise SystemExit("real gate requires evidence directory")
        for mode, name in (("resident", "TestRealGoplsOnDemandLifecycle_resident"), ("on-demand", "TestRealGoplsOnDemandLifecycle_on-demand"), ("killed-parent", "TestRealGoplsOnDemandKilledParentSidecarExit")):
            directory = Path(extra[0]) / name
            summary = json.loads((directory / "lifecycle-summary.json").read_text())
            observations = json.loads((directory / "process-samples.json").read_text())
            if observations["status"] != "observed" or not observations["samples"] or observations["errors"]:
                raise SystemExit(f"{mode}: process observation unverified")
            owned = observations["owned"]
            mains = {p["process"]["namespace_pid"]: identity for identity, p in owned.items() if p["kind"] == "main"}
            if len(mains) != sum(p["kind"] == "main" for p in owned.values()) or len({b["pid"] for b in summary["boots"]}) != len(summary["boots"]):
                raise SystemExit(f"{mode}: ambiguous reused PID-to-boot mapping")
            telemetry_parents = {p["parent_identity"] for p in owned.values() if p["kind"] == "telemetry"}
            if any(b["pid"] not in mains or mains[b["pid"]] not in telemetry_parents for b in summary["boots"]):
                raise SystemExit(f"{mode}: missing main/telemetry identity coverage")
            if any(p.get("first_observed_exit_ms") is None or p.get("first_observed_absent_ms") is None for p in owned.values()):
                raise SystemExit(f"{mode}: attributed process remains alive or unreaped")
            for p in owned.values():
                if p["kind"] == "telemetry":
                    parent = owned[p["parent_identity"]]
                    budget = observations["sidecar_exit_budget_ms"] + observations["sampling_slack_ms"]
                    if p["first_observed_absent_ms"] - parent["first_observed_exit_ms"] > budget:
                        raise SystemExit(f"{mode}: telemetry exceeded its own parent-exit budget")
    print(json.dumps({"suite": kind, "status": "pass", "tests": sorted(names), "scope": "sampled attributed main/telemetry and protocol assertions; not complete process trees or real Codex"}, indent=2))


if __name__ == "__main__":
    main()
