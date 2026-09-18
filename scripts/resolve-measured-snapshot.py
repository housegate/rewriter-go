#!/usr/bin/env python3
"""Resolve identity slots only after independent actual-artifact generation."""
import json
import pathlib
import sys
out, source = map(pathlib.Path, sys.argv[1:])
profile = json.loads((out / "profiles.json").read_text())
provenance = json.loads((out / "provenance.json").read_text())
if len(profile["profiles"]) != 1:
    raise SystemExit("measured corpus requires exactly one profile")
q = profile["profiles"][0]["query_profile_id"]
if provenance["query_profile_id"] != q:
    raise SystemExit("profile and provenance Q disagree")
resolver = pathlib.Path(__file__).resolve().parents[1] / "internal/harness/resolve_snapshot_corpus.py"
# The existing CLI owns strict slot validation and exclusive output creation.
import subprocess
subprocess.run([sys.executable, str(resolver), "--input", str(source), "--output", str(out / "resolved-corpus.json"), "--query-profile-id", q], check=True)
