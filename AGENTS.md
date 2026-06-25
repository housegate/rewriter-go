# PROJECT KNOWLEDGE BASE

**Generated:** 2026-06-25
**Commit:** dc557f3
**Branch:** main

## OVERVIEW

`rewriter-go` is a native Go implementation of the ClickHouse SQL `Rewriter` interface, backed by the `tobilg/polyglot` Rust engine through PureGo FFI. It is library-first: root package APIs and the stateless `Service` are the product surface; the implementation routes SELECT, write/DDL, DB-level, EXISTS/SHOW CREATE, and GRANT/REVOKE statements through internal handlers.

## STRUCTURE

```text
rewriter-go/
|-- rewriter.go, native.go       # Public API and NativeRewriter implementation
|-- service.go                   # Stateless request/response Service entry point
|-- cmd/rewrite/                 # CLI: rewrite one SQL statement
|-- cmd/fidelity-spike/          # CLI: run corpus fidelity probe
|-- internal/engine/             # Only package that imports polyglot
|-- internal/handlers/           # Statement handlers that build RewriteSQLResponse
|-- internal/harness/            # Golden/oracle/fuzz comparison harness
|-- internal/corpus/             # SQL corpus loader for fidelity tooling
|-- internal/nameresolve/        # Pure name-resolution policy; no engine imports
|-- internal/reverse/            # RewriteErrorMessage inversion helpers
|-- proto/                       # Source protobuf contract
|-- gen/pb/                      # Checked-in generated protobuf/gRPC Go files
|-- .github/workflows/           # CI, release, and polyglot bump automation
|-- scripts/                     # Release versioning and polyglot bump helpers
|-- third_party/polyglot-src/    # Git submodule; external upstream source
`-- third_party/lib/             # Locally built FFI library output
```

## WHERE TO LOOK

| Task | Location | Notes |
| --- | --- | --- |
| Public API behavior | `rewriter.go`, `native.go`, `service.go` | `Rewriter`, `RewriteResult`, `NativeRewriter`, `Service` |
| Core rewrite routing | `native.go` | `doRewrite` dispatch order, fail-open errors, reject normalization |
| Request/response embedding | `service.go` | Stateless proto-shaped API for host processes |
| SQL parse/generate/AST mutation | `internal/engine` | Polyglot seam; child instructions apply |
| SELECT/write/GRANT/db-level handling | `internal/handlers` | Statement-specific parity code; child instructions apply |
| Golden corpus and oracle comparisons | `internal/harness` | Env-gated C++ oracle and fuzzing; child instructions apply |
| Fidelity corpus loading | `internal/corpus`, `cmd/fidelity-spike` | SQL seed corpus and fidelity probe |
| Logical-to-physical table policy | `internal/nameresolve` | Keep pure: do not import `internal/engine` or polyglot |
| Error-message position inversion | `internal/reverse` | Pure helpers with close unit coverage |
| gRPC/protobuf contract changes | `proto/rewriter.proto`, `buf.yaml`, `buf.gen.yaml` | Then run `make proto`; do not hand-edit `gen/pb` |
| CI behavior | `.github/workflows/ci.yml` | Pure-Go lane plus FFI lane and fidelity smoke |
| Polyglot bumps | `.gitmodules`, `scripts/update-polyglot.sh`, `.github/workflows/update-polyglot.yml`, `go.mod` | Submodule commit and module version must move together |
| Release versioning | `scripts/next-version.sh`, `.github/workflows/release.yml` | Annotated tags; date logic uses Asia/Shanghai unless overridden |

## CODE MAP

| Symbol | Type | Location | Role |
| --- | --- | --- | --- |
| `RewriteResult` | struct | `rewriter.go` | Public result shape exposed by this module |
| `Rewriter` | interface | `rewriter.go` | Public rewrite contract |
| `NativeRewriter` | type | `native.go` | In-process implementation over engine + handlers |
| `doRewrite` | function | `native.go` | Shared routing pipeline used by `NativeRewriter` and `Service` |
| `Service` | type | `service.go` | Stateless proto request/response embedding surface |
| `engine.Engine` | interface | `internal/engine/engine.go` | Narrow polyglot abstraction |
| `handlers.RewriteSelect` | function | `internal/handlers/select.go` | SELECT pipeline and response population |
| `handlers.RewriteWrite` | function | `internal/handlers/writes.go` | Strict write/DDL dispatch and short-circuit rejects |
| `handlers.RewriteDBLevel` | function | `internal/handlers/dblevel.go` | USE/SHOW/CREATE/DROP database policy |
| `handlers.RewriteExistsShowCreate` | function | `internal/handlers/exists.go` | EXISTS/SHOW CREATE single-target handling |
| `handlers.RewriteGrant` | function | `internal/handlers/grant.go` | GRANT/REVOKE validation and privilege deltas |
| `nameresolve.Resolve` | function | `internal/nameresolve/resolve.go` | Pure logical-to-physical table/database policy |
| `reverse.Invert` | function | `internal/reverse/reverse.go` | Best-effort physical-to-logical error-message inversion |
| `harness.Compare` | function | `internal/harness/compare.go` | Field-by-field native/oracle diff |
| `harness.DialOracle` | function | `internal/harness/oracle.go` | Optional `rewriter-grpc` oracle client |
| `corpus.Load` | function | `internal/corpus/corpus.go` | JSON SQL seed loader for fidelity tooling |

## CONVENTIONS

- Generated files under `gen/pb` are checked in but regenerated from `proto/rewriter.proto` via `make proto`; `buf.yaml` and `buf.gen.yaml` own the codegen policy and output layout.
- `third_party/polyglot-src` is a git submodule, not first-party code. Do not refactor upstream internals from this repo.
- `go.mod` uses `replace github.com/tobilg/polyglot/packages/go => ./third_party/polyglot-src/packages/go`; submodules are required even for pure-Go builds.
- `go.mod` currently declares `go 1.25.0`; GitHub workflows currently install Go `1.22`. Verify the intended toolchain before changing either side.
- There is no repo-local formatter/linter config. Use `gofmt`; CI enforces `go vet ./...`.
- `POLYGLOT_SQL_FFI_PATH` gates engine-backed tests. Plain `go test ./...` runs pure-Go tests and skips FFI-dependent tests.
- `REWRITER_ORACLE_ADDR` enables optional differential checks against a live `rewriter-grpc` oracle; default local tests do not require it.
- Package tests keep fixtures in package-local `testdata/`. Harness corpora are JSON; engine AST characterization snapshots live under `internal/engine/testdata/ast-shapes` and are regenerated by `internal/engine/characterize_test.go`.
- Prefer semantic SQL equality through polyglot AST diff where output formatting can differ from ClickHouse formatting.
- `NativeRewriter` is per-connection and stashes the last successful rewrite for error inversion. `Service` is stateless and re-derives forward maps from the request for `RewriteErrorMessage`.
- `doRewrite` owns dispatch order and reject normalization: writes, DB-level, EXISTS/SHOW CREATE, GRANT/REVOKE, SELECT, then pass-through.

## ANTI-PATTERNS

- Do not import polyglot outside `internal/engine`.
- Do not let `internal/nameresolve` import `internal/engine`, handlers, or the polyglot SDK.
- Do not hand-edit `gen/pb/*.go`; edit `proto/rewriter.proto` and regenerate.
- Do not treat `go test ./...` as proof of FFI-backed parity unless `POLYGLOT_SQL_FFI_PATH` is set.
- Do not make `RewriteErrorMessage` inversion failures break exception handling. `Service.RewriteErrorMessage` is best-effort and returns pass-through output with nil error on failure or empty input.
- Do not move statement dispatch out of `doRewrite` without checking reject normalization, `existence_clause`, and `statement_type` parity.
- Do not treat oracle divergences as harmless unless they are explicitly allow-listed in the relevant harness corpus/test.
- Do not mutate `third_party/polyglot-src` except through submodule bump workflows.

## COMMANDS

```bash
make ffi
make test
make proto
make tidy
go build ./...
go vet ./...
go test ./...
POLYGLOT_SQL_FFI_PATH="$PWD/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so)" \
  go run ./cmd/fidelity-spike
scripts/update-polyglot.sh --check
scripts/update-polyglot.sh --no-verify TAG
POLYGLOT_SQL_FFI_PATH="$PWD/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so)" \
  REWRITER_ORACLE_ADDR=localhost:50051 go test ./internal/harness -count=1
go test ./internal/harness -run x -fuzz FuzzRewrite -fuzztime 30s
```

## NOTES

- `make test` builds the Rust FFI library first, then exports `POLYGLOT_SQL_FFI_PATH` for `go test ./...`.
- CI has a fast pure-Go lane and a full FFI lane; both check out submodules.
- Release artifacts are the built FFI libraries plus `SHA256SUMS`; release tags are annotated because version calculation reads tag creator dates.
- `cmd/` has no child `AGENTS.md` and no dedicated CLI test harness in this snapshot; root guidance covers both command packages.
