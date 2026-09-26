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
| gRPC/protobuf contract changes | `github.com/housegate/rewriter-proto` | Commit the contract there, pin that module version here, and publish the dependency commit first |
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
| `handlers.RewriteDescribe` | function | `internal/handlers/describe.go` | DESCRIBE classification; SI metadata SELECT |
| `handlers.RewriteGrant` | function | `internal/handlers/grant.go` | GRANT/REVOKE validation and privilege deltas |
| `nameresolve.LookupStorageIntegrity` | function | `internal/nameresolve/resolve.go` | SI table lookup consulted before dynamic resolution |
| `nameresolve.Resolve` | function | `internal/nameresolve/resolve.go` | Pure logical-to-physical table/database policy |
| `engine.ActionSubquery` | const | `internal/engine/nodes.go` | Derived-table substitution used by the SI read surface |
| `reverse.Invert` | function | `internal/reverse/reverse.go` | Best-effort physical-to-logical error-message inversion |
| `harness.Compare` | function | `internal/harness/compare.go` | Field-by-field native/oracle diff |
| `harness.DialOracle` | function | `internal/harness/oracle.go` | Optional `rewriter-grpc` oracle client |
| `corpus.Load` | function | `internal/corpus/corpus.go` | JSON SQL seed loader for fidelity tooling |

## CONVENTIONS

- Protobuf source and generated Go types are owned by `github.com/housegate/rewriter-proto`; this repo imports its `gen/pb` package and does not generate protobuf code locally.
- `third_party/polyglot-src` is a git submodule, not first-party code. Do not refactor upstream internals from this repo.
- `go.mod` uses `replace github.com/tobilg/polyglot/packages/go => ./third_party/polyglot-src/packages/go`; submodules are required even for pure-Go builds.
- `go.mod` currently declares `go 1.25.0`; GitHub workflows currently install Go `1.22`. Verify the intended toolchain before changing either side.
- There is no repo-local formatter/linter config. Use `gofmt`; CI enforces `go vet ./...`.
- `POLYGLOT_SQL_FFI_PATH` gates engine-backed tests. Plain `go test ./...` runs pure-Go tests and skips FFI-dependent tests.
- `REWRITER_ORACLE_ADDR` enables optional differential checks against a live `rewriter-grpc` oracle; default local tests do not require it.
- Package tests keep fixtures in package-local `testdata/`. Harness corpora are JSON; engine AST characterization snapshots live under `internal/engine/testdata/ast-shapes` and are regenerated by `internal/engine/characterize_test.go`.
- Storage-integrity (Spec G) goldens live in `internal/harness/testdata/storage_integrity_cases.json`; the C++ repo carries a byte-identical copy. The frozen schema, exact-SQL rules, paired-copy procedure, and `UPDATE_GOLDEN` commands live in `internal/harness/AGENTS.md` under **STORAGE-INTEGRITY CORPUS CONTRACT**.
- Storage-integrity lookup precedence selects a physical surface but does not authorize it: logical SI databases (including INSERT targets) must be present in `database_map`. Every database containing a configured safe/unsafe table is protocol-owned; table nodes/functions, current-db context, DB-level DDL/DCL, and USE/SHOW must reject direct access with SI-classified metadata.
- Prefer semantic SQL equality through polyglot AST diff where output formatting can differ from ClickHouse formatting. The storage-integrity corpus is the deliberate exception: every successful case is pinned exactly after its shared literal-aware identifier-quote normalization, with per-engine pins for declared divergences.
- `NativeRewriter` is per-connection and stashes the last successful rewrite for error inversion. `Service` is stateless and re-derives forward maps from the request for `RewriteErrorMessage`.
- `doRewrite` owns dispatch order and reject normalization: writes, DB-level, DESCRIBE, EXISTS/SHOW CREATE, GRANT/REVOKE, SELECT, then pass-through.

## Table-reference policy (spec 2026-09-26)

Files carrying the policy (spec 2026-09-26 §5, table-reference hardening), each read directly to confirm the role stated:

- `internal/handlers/preflight.go` — `PreflightTableReferences`, the entry point applying the position-independent halves of the policy before any statement handler runs, plus `rejectDisallowedCarriers` (the shared T5 allowlist check).
- `internal/engine/parameters.go` — T2 identifier-parameter detection (`TablePositionParameter`, `commandTextParameterHit`, `IdentifierParameterInText`) across every db/table position, including opaque `command` nodes.
- `internal/engine/references.go` — T3 database collection (`CollectDatabaseReferences`, `CollectSIHandlerBlindDatabaseReferences`) and the T6 string-lookup family (`StringLookupCalls`, `RewriteStringLookups`, `CollectCommandTextFindings`, `CollectRawActionStringLookupCalls`).
- `internal/engine/allowlists.go` — T5 table-function/table-engine/table-setting classification (`ClassifyTableFunction`, `TableEngineAllowed`, `RefusedTableSetting`) and every T5 rejection message helper.
- `internal/handlers/lookups.go` — `rewriteStringLookups` (the SELECT-body-only rewrite path) and `decideStringLookup`, plus the shared "does not resolve" message builder `stringLookupUnresolvedMessage`.
- `internal/nameresolve/protected.go` — T3 protected-database predicate `ProtectedDatabase` and its rejection message `ProtectedDatabaseRejectMessage`.
- `internal/engine/objtarget.go` — `ParseObjectTarget` / `parseObjectTargetFunctionCallFromTokens`, the EXISTS/SHOW CREATE/DESCRIBE function-target verb gate T5 needs when the target itself is a call rather than a plain `[db.]name`.
- `internal/engine/writes.go` — the embedded-body `Extract*`/`Set*` pairs (`ExtractInsertBody`/`SetInsertBody`, `ExtractCreateSelectBody`/`SetCreateSelectBody`, `ExtractViewBody`/`SetViewBody`) that let T4/T6 reach an INSERT … SELECT / CTAS / CREATE VIEW body.
- `internal/handlers/writes.go` — `rewriteEmbeddedBody`, which routes those embedded bodies through the same SELECT pipeline (and hence the same T5/T6 checks) a top-level SELECT gets.

Message constants (exact identifiers, quoted from source, not invented):

- `engine.IdentifierParameterMessage` (`internal/engine/parameters.go:10`) — T2 identifier-parameter rejection: `"query parameters are not supported in a database or table position"`.
- `nameresolve.ProtectedDatabaseRejectMessage(db)` (`internal/nameresolve/protected.go:33`) — T3 protected-database rejection.
- `engine.UnsupportedStatementMessage` (`internal/engine/allowlists.go:78`) — the generic `"statement is not supported"` rejection for a command-node tokenizer failure or an unmodelled class.
- `engine.TableFunctionRefusedMessage(name)` / `engine.TableFunctionUnknownMessage(name)` / `engine.TableEngineRefusedMessage(name)` / `engine.TableSettingRefusedMessage(name)` (all `internal/engine/allowlists.go`) — the T5 allowlist rejections.
- `stringLookupUnresolvedMessage(call)` (`internal/handlers/lookups.go:145`) — the T6 "does not resolve" message, shared verbatim by every mechanism that finds a joinGet/dictGet/hasColumnInTable-family call: the generic AST walk, the command-node raw-text scan, and the embedded `Raw`-action scan.

Precedence, as implemented in `internal/handlers/preflight.go`'s `PreflightTableReferences` (read directly; two points below differ from the controller's context-notes summary, noted explicitly):

1. Gate: only a request selecting `nameresolve.ModeDynamic` runs any of this; static/no-rewrite requests are untouched.
2. T2 (`engine.TablePositionParameter`): any db/table position, including an opaque `command` node, holding an unresolved Identifier parameter rejects immediately with `IdentifierParameterMessage`.
3. T3: every database `engine.CollectDatabaseReferences` finds (write targets before read sources, document order) plus the dynamic args' `upstream_logical_database_in_context` — a protected database rejects immediately with `ProtectedDatabaseRejectMessage`, UNLESS it is an SI physical/reserved database (`nameresolve.IsStorageIntegrityPhysicalDatabase`) that is NOT also one of `engine.CollectSIHandlerBlindDatabaseReferences`'s "SI-handler-blind" databases (a joinGet/dictGet/hasColumnInTable string argument, or a parenthesized single-element `IN` list) — that one case is deferred entirely to the SI handlers further down the dispatch chain, which own the SI-specific rejection message instead.
4. T5 (`rejectDisallowedCarriers`, general source-function-name and CREATE TABLE engine/setting checks): runs HERE only while `!nameresolve.StorageIntegritySurfaceActive`. While the surface IS active, this exact same helper instead runs inside `rewriteSelectCore` (`internal/handlers/select.go`) and `preflightStorageIntegrityWrite` (`internal/handlers/writes.go`), each immediately after their own SI-namespace rejection pass finds nothing to reject — so an SI-owned message always wins over a T5 message for the same statement.
5. Command-node (`command` AST node) findings from one shared tokenize pass (`engine.CollectCommandTextFindings`) — UNCONDITIONAL in both SI states (the code's own comment: "unlike SELECT/write dispatch, no later handler defers either concern for an active SI surface"): a tokenizer failure rejects as `UnsupportedStatementMessage`; any joinGet/dictGet/hasColumnInTable-family call found in the raw text rejects with the "does not resolve" message; and, when the command is an EXISTS/SHOW CREATE/DESCRIBE whose target is itself a function call, `engine.ClassifyTableFunction` classifies the name (Refused/Unknown reject with the matching T5 message) and each argument-derived database is checked against `ProtectedDatabase` — both unconditional, unlike step 4's general table-function-name check.
6. Embedded `{"Raw":{"sql":…}}` action text anywhere in the structured AST (e.g. an `ALTER TABLE … MODIFY COLUMN … DEFAULT …` action) is scanned the same way as step 5's raw-text lookup scan, independently, also unconditional in both SI states.
7. The one generic structured-AST walk (`engine.StringLookupCalls`) over every "function" node: a joinGet/dictGet-family call rejects wherever found; a `hasColumnInTable` call rejects UNLESS `InSelectBody` is set, in which case `rewriteSelectCore`'s own SELECT pipeline resolves/rewrites/refuses it instead. Also unconditional in both SI states.
8. If nothing above fired, `PreflightTableReferences` returns `(nil, false, nil)` and ordinary dispatch proceeds, including the SI-owned namespace checks and, when the surface is active, the deferred T5 allowlist check from step 4.

Where this differs from the controller's context-notes summary ("parameter → unmodelled class → protected database → table function/engine/setting allowlist (only while the SI surface is inactive) → string lookup → ordinary rewrite"): the code does not gate an "unmodelled class" check ahead of the protected-database check as a standalone step — `UnsupportedStatementMessage` only fires later, inside the command-node handling of step 5, interleaved with (not preceding) the protected-database and allowlist checks. And the "only while the SI surface is inactive" gate applies ONLY to `rejectDisallowedCarriers` (step 4); the EXISTS/SHOW CREATE/DESCRIBE function-target classification in step 5 and every string-lookup check in steps 5/6/7 have no such gate at all — they run in both SI states unconditionally.

Two points the corpus/comments also make explicit and worth keeping alongside the above: with the SI surface active, the T5 allowlist check runs *inside* the handlers (`rewriteSelectCore` / `preflightStorageIntegrityWrite`), immediately after their own SI namespace check finds nothing to reject, so an SI-classified message always wins over a T5 message. And `hasColumnInTable` is rewritten (rather than refused) only under the SELECT roots the rewrite pipeline actually processes — i.e. only when `engine.StringLookupCalls` marks the call `InSelectBody` (a select/union/intersect/except subtree, or an embedded INSERT/CTAS/CREATE VIEW body's own rewrite-root field); everywhere else, including the raw-text/command-node mechanisms of steps 5–6 which have no SELECT-body rewrite pipeline of their own, it is refused exactly like the always-refuse joinGet/dictGet family.

## ANTI-PATTERNS

- Do not import polyglot outside `internal/engine`.
- Do not let `internal/nameresolve` import `internal/engine`, handlers, or the polyglot SDK.
- Do not copy or regenerate protobuf definitions here; make contract changes in `github.com/housegate/rewriter-proto` and update the pinned module version.
- Do not treat `go test ./...` as proof of FFI-backed parity unless `POLYGLOT_SQL_FFI_PATH` is set.
- Do not make `RewriteErrorMessage` inversion failures break exception handling. `Service.RewriteErrorMessage` is best-effort and returns pass-through output with nil error on failure or empty input.
- Do not move statement dispatch out of `doRewrite` without checking reject normalization, `existence_clause`, and `statement_type` parity.
- Do not treat oracle divergences as harmless unless they are explicitly allow-listed in the relevant harness corpus/test.
- Do not mutate `third_party/polyglot-src` except through submodule bump workflows.

## COMMANDS

```bash
make ffi
make test
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
