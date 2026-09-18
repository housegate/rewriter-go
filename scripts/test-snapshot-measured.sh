#!/usr/bin/env bash
# Linux compile -> external actual-file generation -> unchanged direct execution.
set -euo pipefail
usage() {
  echo 'usage: test-snapshot-measured.sh compile OUT | generate OUT TOOL RECIPE CORPUS | run OUT FFI PROFILE RESOLVED_CORPUS' >&2
  exit 2
}
[[ $(uname -s) == Linux ]] || { echo 'measured tests require Linux' >&2; exit 1; }
[[ $# -ge 2 ]] || usage
phase=$1
out=$2
case "$phase" in
 compile)
  [[ $# == 2 ]] || usage
  mkdir -p "$out"
  out=$(cd "$out" && pwd)
  go test -buildvcs=false -c -o "$out/rewriter.test" .
  go test -buildvcs=false -c -o "$out/engine.test" ./internal/engine
  go test -buildvcs=false -c -o "$out/harness.test" ./internal/harness
  (cd "$out" && sha256sum rewriter.test engine.test harness.test > compiled.sha256)
  ;;
 generate)
  [[ $# == 5 ]] || usage
  (cd "$out" && sha256sum -c compiled.sha256)
  "$3" -recipe "$4" -profile-out "$out/profiles.json" -provenance-out "$out/provenance.json"
  python3 scripts/resolve-measured-snapshot.py "$out" "$5"
  (cd "$out" && sha256sum -c compiled.sha256)
  ;;
 run)
  [[ $# == 5 ]] || usage
  for name in SNAPSHOT_DEADLINE_PROFILE REWRITER_ORACLE_ADDR SNAPSHOT_CLICKHOUSE SNAPSHOT_TZDATA_ARCHIVE TZDIR SNAPSHOT_RUN_LOG_DIR; do
    [[ -n ${!name:-} ]] || { echo "paired qualification requires $name" >&2; exit 1; }
  done
  [[ ${TZ:-} == UTC ]] || { echo 'paired qualification requires TZ=UTC' >&2; exit 1; }
  mkdir -p "$SNAPSHOT_RUN_LOG_DIR"
  export SNAPSHOT_EXECUTION_EVIDENCE="$SNAPSHOT_RUN_LOG_DIR/execution.jsonl"
  [[ ! -e "$SNAPSHOT_EXECUTION_EVIDENCE" ]] || { echo 'evidence output already exists' >&2; exit 1; }
  sha256sum "$3" "$4" "$5" "$SNAPSHOT_CLICKHOUSE" "$SNAPSHOT_TZDATA_ARCHIVE" > "$SNAPSHOT_RUN_LOG_DIR/inputs.sha256"
  (cd "$out" && sha256sum -c compiled.sha256)
  export SNAPSHOT_MEASURED_FFI=$3 SNAPSHOT_MEASURED_PROFILE=$4 SNAPSHOT_QUERY_CORPUS=$5
  export POLYGLOT_SQL_FFI_PATH=$3
  unset SNAPSHOT_QUERY_ORDINARY
  "$out/rewriter.test" -test.run '^TestSnapshot' -test.v 2>&1 | tee "$SNAPSHOT_RUN_LOG_DIR/root.log"
  (cd internal/engine && "$out/engine.test" -test.run '^TestSnapshot' -test.v) 2>&1 | tee "$SNAPSHOT_RUN_LOG_DIR/engine.log"
  (cd internal/harness && "$out/harness.test" -test.run '^TestSnapshotQuery' -test.v) 2>&1 | tee "$SNAPSHOT_RUN_LOG_DIR/harness.log"
  sha256sum -c "$SNAPSHOT_RUN_LOG_DIR/inputs.sha256"
  python3 scripts/verify-snapshot-run.py "$SNAPSHOT_RUN_LOG_DIR" | tee "$SNAPSHOT_RUN_LOG_DIR/counts.json"
  (cd "$out" && sha256sum -c compiled.sha256)
  ;;
 *) usage ;;
esac
