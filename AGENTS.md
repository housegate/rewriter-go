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

Applies only to requests that select `nameresolve.ModeDynamic`; static and no-rewrite requests are untouched.

Where it lives:

- `internal/handlers/preflight.go` — `PreflightTableReferences` (the ordered checks below), `commandClassModelled`, `rejectDisallowedCarriers` (T5), `rejectUngovernedReads` (R2 / R5), `CheckSessionSet` (the SET carve-out).
- `internal/engine/parameters.go` — T2 (`TablePositionParameter`, `IdentifierParameterInText`).
- `internal/engine/nodes.go` — the ordered walker (`walkStatementObjects`) and the one IN-operand decoder, `decodeInOperand`, used by every IN consumer.
- `internal/engine/references.go` — T3 database collection (`CollectDatabaseReferences`, `CollectSIHandlerBlindDatabaseReferences`) and the T6 string-lookup family.
- `internal/engine/ungoverned.go` — `walkGenericExpression` (column / constraint / storage-property / ALTER-action expressions), `ExpressionPositionHasReads`, and the opaque ALTER text scans (`OpaqueAlterTexts`, `OpaqueTextDatabases`, `OpaqueTextIsUngoverned`).
- `internal/engine/settings.go` — SQL-bearing settings and the setting-value rule.
- `internal/engine/allowlists.go` — T5 classification and messages.
- `native.go` — dispatch order, the final unmodelled-class refusal and the SET carve-out, and `sealStorageIntegrityHandlerError`.

Message families (five; exact texts are cross-engine contract):

- T2 `engine.IdentifierParameterMessage` — `query parameters are not supported in a database or table position` (InvalidRewriteRequest).
- T3 `nameresolve.ProtectedDatabaseRejectMessage(db)` — `protected database <db> is not addressable` (InvalidRewriteRequest).
- T5 `engine.TableFunctionRefusedMessage` / `TableFunctionUnknownMessage` / `TableEngineRefusedMessage` / `TableEngineUnknownMessage` / `TableSettingRefusedMessage` — `table function|engine <name> is not accepted|recognised`, `table setting <name> is not accepted` (UnsupportedStatement).
- T6 `stringLookupUnresolvedMessage` — `<fn> target "<arg>" does not resolve through the caller's databases` (InvalidRewriteRequest in the preflight; UnsupportedStatement inside an embedded INSERT … SELECT / CTAS body). A non-literal first argument reports `target ""` on every path.
- T7 `engine.UnsupportedStatementMessage` — `statement is not supported` (UnsupportedStatement): an unmodelled statement class, a read in a position no rewrite reaches, a non-plain setting value, a Go error. With the SI surface active the unmodelled-class refusal is `nameresolve.StorageIntegrityUnmodelledMessage` instead, and an SI object the statement names upgrades it to the SI message for that object.

Precedence (first match wins):

1. T2 — an Identifier parameter in any database or table position: every walker position (FROM / JOIN / subquery / CTE / IN and callable-IN operands at any paren depth / write targets / MV TO / column and ALTER-action expressions), every Raw ALTER action's text, and, for a `command` node, anywhere in its text.
2. T7 unmodelled class — a `command` node that is not EXISTS / SHOW CREATE / DESCRIBE, USE / SHOW, RENAME / EXCHANGE TABLE, ALTER TABLE … UPDATE, GRANT / REVOKE / ATTACH GRANT, or (surface inactive only) a session SET. With the surface inactive it is refused here, before its names are examined (so `DETACH TABLE phys.x` is unmodelled, not protected). With the surface active the SI pipeline refuses it: the SI write preflight or the final-response annotation names an SI object, anything else gets the SI catch-all.
3. T3 protected database — every database the walker, the write-target collector, the string-lookup arguments and the opaque ALTER text name, plus `upstream_logical_database_in_context`. With the surface active an SI physical / reserved database defers to the SI handlers (they own its message), except in an SI-handler-blind position: a string-lookup argument or a parenthesized single-operand IN.
4. T5 allowlists — source-role table functions, CREATE TABLE / materialized-view ENGINE and SETTINGS, ALTER … MODIFY SETTING. With the surface active this runs inside the SI handlers after their namespace policy, so an SI message wins.
5. Command-text findings — a tokenizer failure (T7), a lookup-family call in the text (T6), an EXISTS / SHOW CREATE / DESCRIBE whose target is a table function (T5 classification, T3 on its arguments).
6. T6 string lookups — joinGet / dictGet-family calls anywhere, hasColumnInTable outside a SELECT body; Raw ALTER action text is scanned the same way.
7. R5 settings, then R2 ungoverned reads (`rejectUngovernedReads`; with the surface active, inside the SI handlers after their own checks): a SQL-bearing setting (`additional_table_filters`, `additional_result_filter`, `parallel_replicas_custom_key`) in any query-level SETTINGS clause is a T5 table-setting refusal and a setting value that is not a numeric / string literal or bare identifier / keyword is T7; a table, IN-table operand, table function or namespace carrier in a structured UPDATE / DELETE, INSERT VALUES, column / constraint / non-engine storage property or structured ALTER action is T7; opaque ALTER text carrying a subquery, a table-operand IN, FETCH PARTITION, ATTACH / REPLACE PARTITION … FROM, MOVE PARTITION … TO TABLE or MODIFY QUERY is T7.
8. Ordinary dispatch. Handlers resolve what they model: DESCRIBE / EXISTS / SHOW CREATE / SHOW COLUMNS resolve an unqualified target like FROM; `DESCRIBE (SELECT …)` and an empty EXISTS / SHOW CREATE are T7; `CREATE MATERIALIZED VIEW … REFRESH` and `INSERT … FROM INFILE` are T7 (the generator does not round-trip them).
9. The final fallthrough refuses every statement no handler took, except a session SET the carve-out admits (surface inactive only; every assignment `<name> = <value>` with a numeric literal, string literal or bare identifier / keyword value).

Any handler, walk or generate error (including a polyglot recursion-limit error) is sealed as UnsupportedStatement: `statement is not supported` with the surface inactive, the SI catch-all with it active. With the surface active, the live-view classification in `doRewrite` still runs before T2.

IN operands: `decodeInOperand` unwraps parentheses to any depth, treats a parameter operand as T2, and decodes an identifier structurally — a bare quoted `` `db2.x` `` is an unqualified table named `db2.x` in the session's logical database (`phys."db1.db2.x"`), exactly like FROM. A literal list is a value list.

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
