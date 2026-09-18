#!/usr/bin/env python3
"""Fail closed on missing/skipped measured suites and incomplete paired results."""
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
required = {
    "root.log": ["TestSnapshotMeasuredSemanticSupport", "TestSnapshotMeasuredResourceRefusals"],
    "engine.log": ["TestSnapshotClosedAnalysis", "TestSnapshotPreparationBoundGraph", "TestSnapshotMeasuredSealedSourceAndLifetime"],
    "harness.log": ["TestSnapshotQuery", "TestSnapshotQueryPrepareExecution", "TestSnapshotQueryTransport"],
}
summary = {}
for name, suites in required.items():
    text = (root / name).read_text()
    if re.search(r"--- (?:FAIL|SKIP):", text) or not text.endswith("PASS\n"):
        raise SystemExit(f"failed, skipped or incomplete measured package: {name}")
    # t.Log preserves loader subprocess output, including indented top-level
    # PASS lines. Only the enclosing process emits column-zero RUN events and
    # top-level completions; its real subtests have slash-bearing names.
    started = re.findall(r"^=== RUN\s+(\S+)\s*$", text, re.M)
    passed = [case for indent, case in re.findall(r"^( *)--- PASS: (\S+) ", text, re.M)
              if not indent or "/" in case]
    if (len(started) != len(set(started)) or len(passed) != len(set(passed))
            or set(started) != set(passed) or not set(suites) <= set(passed)):
        raise SystemExit(f"missing/duplicate required measured suite: {name}")
    summary[name] = {"top_level": sum("/" not in p for p in passed), "total": len(passed)}
    if name == "harness.log":
        for backend in ("service", "native", "oracle"):
            count = sum(bool(re.fullmatch(r"TestSnapshotQuery/[^/]+/" + backend, p)) for p in passed)
            if count != 636:
                raise SystemExit(f"incomplete {backend} frozen corpus: {count}")
            count = sum(bool(re.fullmatch(r"TestSnapshotQueryPrepareExecution/[^/]+/" + backend, p)) for p in passed)
            if count != 102:
                raise SystemExit(f"incomplete {backend} execution matrix: {count}")
        if "PREPARE_EXECUTION fixtures=102 backends=3 executions=228 refusals=78" not in text:
            raise SystemExit("missing exact execution count")
rows = [json.loads(line) for line in (root / "execution.jsonl").read_text().splitlines()]
if len(rows) != 306 or len({(r["fixture"], r["backend"]) for r in rows}) != 306:
    raise SystemExit("missing/duplicate execution evidence")
for backend in ("service", "native", "oracle"):
    records = [r for r in rows if r["backend"] == backend]
    if len(records) != 102 or sum("execution" in r for r in records) != 76:
        raise SystemExit("incorrect per-backend execution evidence count")
summary["execution"] = {"fixtures": 102, "backends": 3, "executions": 228, "analyzer_refusals": 78}


transport = [json.loads(line) for line in (root / "transport.jsonl").read_text().splitlines()]
if len(transport) != 14 or len({r["name"] for r in transport}) != 14:
    raise SystemExit("incomplete transport qualification")
summary["transport"] = {"calls": 14}
print(json.dumps(summary, sort_keys=True, indent=2))
