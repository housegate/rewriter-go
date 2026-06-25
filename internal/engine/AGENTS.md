# ENGINE PACKAGE KNOWLEDGE

## OVERVIEW

`internal/engine` is the only first-party package allowed to talk to polyglot; all other code consumes its narrow AST and rewrite helpers.

## WHERE TO LOOK

| Task | Location | Notes |
| --- | --- | --- |
| Polyglot lifecycle | `polyglot.go` | `NewPolyglot`, FFI load path, `ParseOne`, `Generate`, `DiffSQL` |
| Engine interface | `engine.go` | Contract used by handlers and harness |
| SELECT table walking | `nodes.go` | CTE-aware collection and table rewrites |
| Write/DDL classification | `writes.go` | Structured vs raw write nodes and rewrite slots |
| DB/global/grant/object helpers | `dblevel.go`, `global.go`, `grant.go`, `objtarget.go` | Statement-family AST handling; GRANT/REVOKE uses generic-dialect recovery |
| Parser guard | `guard.go` | Bracket nesting limit before polyglot parse |
| AST shape fixtures | `testdata/ast-shapes`, `characterize_test.go` | Snapshot corpus and its regeneration path |

## CONVENTIONS

- Keep the package boundary strict: polyglot imports belong here and nowhere else.
- Treat `engine.AST` as polyglot JSON. Decode only the nodes being inspected or mutated; preserve unknown fields when re-encoding.
- `ParseOne` returns a single-statement object. Polyglot operations that require arrays must use the local wrap/unwrap helpers.
- `ParseGeneric` exists for GRANT/REVOKE forms that ClickHouse dialect leaves as opaque `command` nodes; keep that recovery local to the engine seam.
- Wrap errors with an `engine:` prefix so fail-open callers can distinguish unexpected engine failures.
- Table traversal must stay CTE-aware: real tables are visited, in-scope CTE aliases are skipped, and column-qualifier false positives are not treated as table references.
- Raw SQL helpers in `writes.go` exist because polyglot can lose object-kind fidelity. Preserve explicit raw-text checks when structured AST fields are insufficient.
- `TestCharacterizeAST` in `characterize_test.go` regenerates `testdata/ast-shapes/*.json` when `POLYGLOT_SQL_FFI_PATH` is set; treat those snapshots as generated fixtures.
- For generated SQL formatting differences, prefer semantic comparison through `DiffSQL` rather than exact string expectations.

## ANTI-PATTERNS

- Do not leak polyglot SDK types through the `Engine` interface.
- Do not add handler or name-resolution policy here; callers decide actions through callback-style decisions.
- Do not assume raw SQL and structured AST disagree safely. When polyglot loses object-kind fidelity, preserve raw-text checks in callers or explicit inspector fields.
- Do not replace generic-dialect GRANT recovery with string parsing unless the harness and oracle prove the same privilege-delta semantics.
- Do not bypass `exceedsNestingDepth` on new parse entrypoints.

## LOCAL VERIFICATION

```bash
go test ./internal/engine
POLYGLOT_SQL_FFI_PATH="$PWD/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so)" go test ./internal/engine -count=1
```

Engine-backed tests skip when `POLYGLOT_SQL_FFI_PATH` is unset.
