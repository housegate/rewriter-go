# rewriter-go

Native **Go** implementation of the ClickHouse SQL `Rewriter` interface — an in-process
(no gRPC hop) alternative to the C++ [`rewriter-grpc`](../rewriter-grpc) service, built on
[tobilg/polyglot](https://github.com/tobilg/polyglot) (Rust SQL transpiler, ClickHouse
dialect) via its PureGo SDK (`CGO_ENABLED=0`).

Target: **full behavioral parity** with `rewriter-grpc` across `Rewrite` +
`RewriteErrorMessage`, validated by a differential oracle harness that runs both engines and
diffs their responses.

## Status

**Phase 0 complete** (engine + harness + fidelity spike). Verdict: **GO** for full parity
via a **dual-dialect + raw-SQL** architecture — see the
[fidelity report](docs/superpowers/reports/2026-06-03-phase0-fidelity.md). Phases 1–5
(the statement handlers) land on `feat/*` branches via PR.

**Phase 1 complete** (SELECT handler, branch `feat/phase-1-select`). Implemented:

- Table resolution — static 3-map (`table_map`, `remote_table_map`, `table_with_database_map`) + dynamic (logical→physical, `buildDynamicTableName`, known-physical passthrough), including `remote()` wrapping for remote upstreams.
- Option pipeline — `LimitRewrite` (force + replace), `OffsetRewrite`, `SettingsRewrite`.
- CTE injection (`CommonTableExprRewrite`) — parse-and-inject bodies before the table walk, failing aliases recorded and sorted.
- GLOBAL cross-shard pass (`ForceGlobalForRemoteAsymmetry`) to handle mixed local/remote JOIN patterns.
- Full response population (`table_rewrites`, `original_accessed_tables`, `failed_cte_aliases`, `sql_after_rewrite`).
- Validated by `internal/harness` golden tests (10 cases covering dynamic rename, static maps, remote, CTE injection, limit, JOIN, passthrough) and, when `REWRITER_ORACLE_ADDR` is set, an env-gated differential against the live C++ oracle.

Known limitation: a GLOBAL JOIN whose left operand is a `remote()` function cannot be synthesised through polyglot's generator — such cases are left un-GLOBAL and allow-listed in CI.

## Layout

| Path | What |
|---|---|
| `rewriter.go` / `native.go` | Public `Rewriter` interface + `RewriteResult`; the `NativeRewriter` (Phase 0 = pass-through: parse + classify + regenerate) |
| `internal/engine` | The polyglot seam — the ONLY package that imports the polyglot SDK (`Engine` interface, `NodeKind`/`CommandSQL`, fidelity metric) |
| `internal/corpus` | SQL corpus loader |
| `internal/harness` | Differential comparator + env-gated `rewriter-grpc` oracle client |
| `cmd/fidelity-spike` | Round-trip fidelity probe over a corpus |
| `gen/pb` | Types generated from `rewriter-grpc/protos/rewriter.proto` (shared contract) |

## Build & test

The polyglot FFI library is **not** vendored — build it once from source (clones polyglot +
`cargo build`, a few minutes):

```bash
make ffi      # builds third_party/lib/libpolyglot_sql_ffi.<ext>
make test     # sets POLYGLOT_SQL_FFI_PATH and runs `go test ./...`
make proto    # regenerate gen/pb from the vendored proto (buf)
```

Tests that need the engine skip themselves when `POLYGLOT_SQL_FFI_PATH` is unset, so
`go test ./...` alone still runs the pure-Go units (comparator, corpus, fidelity-metric,
contract tests).

### CI oracle differential (the true parity gate)

The `oracle` CI job (`.github/workflows/ci.yml`) runs the C++ `rewriter-grpc` image as a
service and runs the harness with `REWRITER_ORACLE_ADDR` set, so every golden corpus is
diffed field-by-field against the real C++ behavior. To run it locally:

```bash
docker run -d -p 50051:50051 \
  us-west1-docker.pkg.dev/sentio-352722/sentio/housegate-rewriter:0.11.0 \
  /clickhousegate_rewriter 50051
make ffi
POLYGLOT_SQL_FFI_PATH="$PWD/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so)" \
  REWRITER_ORACLE_ADDR=localhost:50051 go test ./internal/harness -count=1
```

- The oracle image lives in a **private GCP Artifact Registry**, so the CI job needs a repo
  secret **`GCP_ORACLE_KEY`** — a GCP service-account JSON key with `roles/artifactregistry.reader`
  on the `sentio-352722` `sentio` repo. Without it the job is skipped (the `if` guard), so the
  rest of CI still runs.
- `ORACLE_TAG` (in the workflow) must track the C++ version whose source was ported — bump it
  in lockstep when porting newer C++ behavior.
- A small, documented allow-list of intentional divergences is carved out per-case in the
  corpora (`allow_*_divergence` flags): the `IN(>50)` OR-batch preprocess we skip (§6f), admin
  statements polyglot parses but ClickHouse's parser rejects (COPY / ATTACH GRANT →
  `SyntaxError`), the `GRANT … TO ALL` marker backtick-quoting, and the static-`table_map`
  db-qualifier (a C++-internal inconsistency, flagged for review).

## Design

See [`docs/superpowers/specs/2026-06-03-native-go-rewriter-design.md`](docs/superpowers/specs/2026-06-03-native-go-rewriter-design.md)
for the full design: architecture, the polyglot engine seam (incl. the dual-dialect parse
strategy, §3.1), the parity risk register, the differential-harness validation strategy, and
the phased build plan. The Phase 0 implementation plan is in
[`docs/superpowers/plans/`](docs/superpowers/plans/).
