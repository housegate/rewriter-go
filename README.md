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

## Design

See [`docs/superpowers/specs/2026-06-03-native-go-rewriter-design.md`](docs/superpowers/specs/2026-06-03-native-go-rewriter-design.md)
for the full design: architecture, the polyglot engine seam (incl. the dual-dialect parse
strategy, §3.1), the parity risk register, the differential-harness validation strategy, and
the phased build plan. The Phase 0 implementation plan is in
[`docs/superpowers/plans/`](docs/superpowers/plans/).
