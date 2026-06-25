# HANDLERS PACKAGE KNOWLEDGE

## OVERVIEW

`internal/handlers` ports the statement-level `rewriter-grpc` behavior: classify the already-parsed AST, apply rewrite options, and populate `pb.RewriteSQLResponse`.

## WHERE TO LOOK

| Task | Location | Notes |
| --- | --- | --- |
| SELECT rewrite | `select.go` | CTE injection, table rewrite/access bookkeeping, options, GLOBAL pass |
| Write/DDL rewrite | `writes.go` | Strict target decisions, short-circuit rejects, raw SQL splices |
| DB-level statements | `dblevel.go` | USE/SHOW/CREATE/DROP database handling |
| GRANT/REVOKE | `grant.go` | Generic-dialect recovery, privilege deltas, ALL/ANY parity quirks |
| EXISTS/SHOW CREATE | `exists.go` | Existence clause and show-create behavior |
| Option helpers | `options.go` | Limit/offset/settings option application |
| Name policy | `../nameresolve` | Active static/dynamic table policy |

## CONVENTIONS

- Handlers return protobuf response objects, not public `RewriteResult`; root code converts at the boundary.
- SELECT is lenient for invalid table rewrites: skip unresolved/invalid targets rather than rejecting the statement.
- Writes are strict: record access before the first reject, short-circuit at the first failing slot, and do not record later slots.
- `table_rewrites`, `original_accessed_tables`, `failed_cte_aliases`, `database_rewrites`, and `privileges_deltas` are parity fields. Preserve ordering and nil/empty semantics tested by harnesses.
- CTE parse failures are recorded deterministically. Sort `failed_cte_aliases` before returning.
- For view bodies, reuse the SELECT pipeline and merge its bookkeeping into the write response.
- Remote upstreams are SELECT-side `remote()` functions only; write targets that resolve remote are unsupported.
- Preserve the GRANT/REVOKE asymmetry in `grant.go`: `GRANT TO ALL` is mirrored as a normal grantee, while `REVOKE FROM ALL/ANY` rejects per C++ oracle behavior.
- `native.go` stamps `existence_clause`, clears `statement_type` on rejects, and echoes rejected SQL when empty. Handler responses should carry statement-family fields and let `finalize` normalize shared parity behavior.
- `EXISTS` / `SHOW CREATE` / GRANT handling runs after DB-level dispatch and before SELECT. Do not reorder without checking command-node overlap and oracle parity.

## ANTI-PATTERNS

- Do not add polyglot SDK imports in this package; use `engine.Engine` and `engine.AST`.
- Do not silently turn strict write rejects into SELECT-style skips.
- Do not let a later write slot clobber the first reject code/message.
- Do not resolve names directly in handlers when `internal/nameresolve` already owns the logical/physical policy.
- Do not compare generated SQL byte-for-byte against the C++ oracle when semantic equality is the intended gate.

## LOCAL VERIFICATION

```bash
go test ./internal/handlers
POLYGLOT_SQL_FFI_PATH="$PWD/third_party/lib/libpolyglot_sql_ffi.$(uname | grep -qi darwin && echo dylib || echo so)" go test ./internal/handlers -count=1
```

Handler tests are table-driven and package-local; add cases next to the statement family being changed.
