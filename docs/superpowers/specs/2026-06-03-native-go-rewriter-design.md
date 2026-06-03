# Native Go SQL Rewriter — Design Spec

- **Date:** 2026-06-03
- **Status:** Design approved; pending spec review
- **Repo:** `rewriter-go` (this repo, new)
- **Reference:** `rewriter-grpc` (C++23 service + the `rewriter.proto` contract + the
  `tests/rewriter_test.cc` golden corpus). The C++ service is the **parity oracle**.

---

## 1. Background & goal

`rewriter-grpc` is a C++23 gRPC service that rewrites ClickHouse SQL before it reaches a
backend ClickHouse cluster. It links directly against the ClickHouse source tree so it can
parse queries into ClickHouse's **own** AST, mutate them (multi-tenant logical→physical
database/table mapping, cross-shard `GLOBAL` correctness, `SHOW`→`system.tables`,
GRANT→privilege-deltas, …), and format them back out. Consumers (a proxy in front of
ClickHouse) call it over gRPC through a Go `Rewriter` interface.

**Goal:** provide a **native Go implementation of the same `Rewriter` interface** that does
the rewrite **in-process** (no gRPC hop), as an additional, pluggable option alongside the
gRPC-backed implementation. The interface is already backend-agnostic, fail-open, and
per-connection, so callers select an implementation at wiring time.

- **Engine substrate:** [tobilg/polyglot](https://github.com/tobilg/polyglot) — a Rust
  SQL transpiler (SQLGlot-derived; its own parser/generator; ClickHouse is a first-class
  dialect) consumed via its **official Go SDK** (loads `libpolyglot_sql_ffi` through PureGo,
  so `CGO_ENABLED=0` — no cgo, no network).
- **Target:** **full behavioral parity** with the C++ service across the `Rewrite` +
  `RewriteErrorMessage` surface, proven by an automated differential oracle harness.

**Non-goals:**

- The `Optimize` RPC — it is **not** part of the `Rewriter` interface. (It is a
  self-contained JOIN-swap that could be added later via polyglot's AST if ever needed.)
- Re-implementing `buildDatabaseMap` / auth/session policy — that is injected by the
  consumer (see §4).

---

## 2. The interface (contract)

The native rewriter implements the consumer's existing interface verbatim:

```go
type Rewriter interface {
    Rewrite(ctx context.Context, sql, effectiveAccount string) (RewriteResult, error)
    RewriteErrorMessage(ctx context.Context, message string) (rewroteMessage string, err error)
    Close() error
}
```

- **Fail-open:** any error short-circuits the call; the caller logs and forwards the
  original SQL unchanged. On error the returned `RewriteResult` is the zero value and must
  not be inspected.
- **`effectiveAccount`** gates the per-query `database_map` (owner address when the JWS
  signer acts on-behalf-of-owner, the bare signer otherwise, `""` = anonymous /
  `buildDatabaseMap` "no account" semantics).
- **`RewriteErrorMessage`** reverse-maps rewritten table/database names in exception text
  back to the names the client used, using the SQL + `effectiveAccount` captured during the
  most recent successful `Rewrite`.
- **`Close`** releases per-connection resources; safe to call multiple times.

`RewriteResult` **mirrors `pb.RewriteSQLResponse`** (generated from `rewriter.proto`, §7),
surfacing: `SQL` (= `sql_after_rewrite`), `table_rewrites`, `database_rewrites`,
`original_accessed_tables`, `statement_type`, `privileges_deltas`, `existence_clause`,
`failed_cte_aliases`, plus `code`/`message`. (Confirmed — §11.)

---

## 3. Substrate: polyglot

Established by inspection of the repo (pushed 2026-06-02, ~833★):

- ClickHouse dialect: `crates/polyglot-sql/src/dialects/clickhouse.rs` (~26 KB) + a
  ClickHouse function catalog + three CH-specific test suites
  (`custom_clickhouse_parser.rs`, `custom_clickhouse_coverage.rs`,
  `clickhouse_regression.rs`), the coverage suite built from ClickHouse's **own** extracted
  tests. Models `PREWHERE`, `SAMPLE`, `FINAL`, `ARRAY JOIN`, `SETTINGS`, `remote`,
  `globalIn`.
- Go SDK (`packages/go`, module `github.com/tobilg/polyglot/packages/go`, SDK v0.4.x):
  loads the native lib via **PureGo** (`github.com/ebitengine/purego`), `CGO_ENABLED=0`.
  Ship a matching `libpolyglot_sql_ffi.{so,dylib,dll}` next to the binary (point via
  `Open(path)` or `POLYGLOT_SQL_FFI_PATH` + `OpenDefault()`). The client is
  `sync.RWMutex`-guarded → safe to share process-wide.
- Relevant API surface: `ParseOne(sql,d)→JSON AST`, `Generate(ast,d)→SQL`,
  `RenameTables(ast,map,opts)`, `QualifyTables(ast,opts)`, `Tokenize(sql,d)`,
  `Diff(sql1,sql2,d)`, `Validate`, `Optimize`, `Lineage`/`SourceTables`.

**Fidelity caveat (the crux of the whole effort):** polyglot is a SQLGlot-derived
*re-implementation*, **not** ClickHouse's own parser. That is the entire premise the C++
service trades on. Coverage is high but **not total** (the coverage suite ships a
`debug_clickhouse_coverage_failures()` path). Therefore: full parity is *plausible*, and the
**Phase 0 fidelity spike + the differential harness gate every claim of parity** (§7, §9).

### 3.1 polyglot AST reality (Phase 0 characterization — 2026-06-03)

Phase 0 characterization (captured in `internal/engine/testdata/ast-shapes/`) established how
polyglot actually serializes ClickHouse SQL. Three findings reshape the engine design:

1. **The discriminator is the single top-level JSON key**, not a `class`/`type` field. The
   envelope is `{"<node_kind>": {…}}` — e.g. `{"select": …}`, `{"drop_table": …}`,
   `{"command": {"this": "<raw SQL>"}}`. Classification reads that key.
2. **Statement families split into three tiers** by how polyglot parses them:
   - **Structured under `clickhouse`:** SELECT, INSERT, CREATE/DROP/ALTER TABLE, TRUNCATE,
     DELETE, CREATE DATABASE, DROP DATABASE. Table/db names are addressable;
     `if_exists`/`if_not_exists` exposed.
   - **Opaque `command` blob under `clickhouse`, but STRUCTURED under the `generic` dialect:**
     USE, GRANT, REVOKE, SHOW TABLES, SHOW DATABASES, SHOW CREATE. These produce *synthetic*
     output anyway (SELECT…system.tables / `SELECT '…' AS stmt`), so name-extraction from a
     generic re-parse suffices — no faithful round-trip needed.
   - **No structured parse under ANY dialect (raw-SQL handling required):** RENAME TABLE,
     EXISTS TABLE, ALTER…UPDATE / ALTER…DELETE mutations. (`OPTIMIZE` too — already
     `UnsupportedStatement`.)
3. **Decision — dual-dialect + raw-SQL (chosen 2026-06-03):** the `Engine` parses `clickhouse`
   first; when the result is a `command` node it re-parses under `generic` to recover
   structure for USE/SHOW/GRANT/REVOKE; RENAME/EXISTS/mutations use focused raw-SQL handlers.
   The full-parity target is preserved. **GRANT carries the most risk** (a generic-dialect
   parse may not preserve `ON CLUSTER` or ClickHouse privilege keywords) and gets an explicit
   validation checkpoint before its handler is trusted. Caveats from the probe: GRANT's target
   comes back as a flat `"db.t"` string (manual split needed); SHOW TABLES' db sits under an
   odd `from.column.name` path.

---

## 4. Architecture

New Go package (working name `chrewrite`). Two injected collaborators keep it testable and
repo-portable:

```
NativeRewriter (implements Rewriter, one instance per client connection)
 ├─ engine   Engine                              // shared, process-wide
 ├─ options  func(account string) []*pb.RewriteOption  // injected: ACCOUNT-derived policy
 │                                                //   only — database_map / known_physical /
 │                                                //   delim / extras / remote_upstreams …
 │                                                //   via buildDatabaseMap. Does NOT set the
 │                                                //   session fields below.
 ├─ session  *sessionState                        // per-conn, HELD here (decision §11):
 │                                                //   logicalDB  -> upstream_logical_database_in_context
 │                                                //   physicalDB -> upstream_physical_database_in_context
 │                                                //   updated by the USE handler
 ├─ last     *callContext                         // per-conn: {sql, account, fwd+reverse
 │                                                //   name maps} for RewriteErrorMessage
 └─ mu       sync.Mutex                            // guards session + last
```

### The Engine seam (the one boundary that matters)

A narrow interface wrapping polyglot, so a future WASM/wazero backend can drop in without
touching any handler:

```go
type Engine interface {
    ParseOne(sql string) (AST, error)                       // polyglot Parse (clickhouse)
    // ParseStructured parses under clickhouse; if the root is an opaque `command`
    // node, re-parses under generic to recover structure (USE/SHOW/GRANT/REVOKE).
    ParseStructured(sql string) (ast AST, dialect, nodeKind string, err error) // §3.1
    NodeKind(ast AST) (string, error)                       // top-level JSON key (§3.1)
    Generate(ast AST) (string, error)                       // polyglot Generate (clickhouse)
    RenameTables(ast AST, m map[string]string) (AST, error)
    QualifyTables(ast AST, db string) (AST, error)
    Tokenize(sql string) ([]Token, error)                   // for INSERT VALUES splice
    Diff(a, b AST) (Delta, error)                           // for the parity harness
}
```

`ParseStructured` is added in Phase 1 (Phase 0 needs only clickhouse `ParseOne` + `NodeKind`
for classification and round-trip measurement). RENAME TABLE, EXISTS TABLE, and
ALTER…UPDATE/…DELETE mutations bypass the structured-AST path entirely — a small
`internal/rawsql` package handles them (§3.1 tier 3).

- One `*polyglot.Client`, created once at startup, dialect pinned to `"clickhouse"`; shared
  across all connections.
- `AST` is `json.RawMessage`; we decode only the nodes we mutate (typed structs for nodes we
  touch — table identifiers, table functions, joins, `IN`-family functions, `WITH`/CTE,
  GRANT — raw passthrough otherwise).

### Per-connection vs shared state

The `Rewriter` is per-connection, but only the **last-call context** is per-connection; the
engine is shared. `Close()` drops the context and never closes the shared client.

### Policy injection vs. session state

**Account-derived policy is injected.** Today `Rewrite(sql, account)` calls
`buildDatabaseMap(account)` to build the per-query options, then ships them to C++. The
native impl runs the **same** `options(account)` in-process and applies the result locally —
`buildDatabaseMap` is injected, never re-implemented here. `options(account)` returns
*account*-derived policy only: `database_map`, `known_physical_databases`, `delim`,
`extra_arguments`, `remote_upstreams`, `logical_database_to_remote_upstream_index` (plus any
session-independent Limit/Settings).

**Session state is held by the rewriter.** The connection-scoped fields —
`upstream_logical_database_in_context` (the most recent `USE <db>`) and
`upstream_physical_database_in_context` — are **not** produced by `options(account)`; the
`NativeRewriter` holds them in `session` and **overlays** them onto the active
`TableNameRewrite.dynamic_args` on every call. The `USE` handler updates `session.logicalDB`
after a successful resolve, so subsequent unqualified statements resolve against the right
logical DB. (In the gRPC design the proxy tracked this; per §11 it now lives in the
rewriter.)

### Components (each mirrors a C++ handler; full-parity target)

| Go unit | Mirrors (`rewriter-grpc`) | Job |
|---|---|---|
| `dispatch` | `Rewrite` switch | classify AST root → `StatementType` → route |
| `nameresolve` | `applyDynamicRewrite` + `ASTReplaceTransformer` | `(origin_db, table)` + args → `(physical_db, new_table)`; static 3-map + dynamic modes; unified static→dynamic precedence |
| `handlers/select` | `handlers/select.cc` | table walk, CTE-alias scope set, option pipeline |
| `handlers/writes` | `handlers/writes.cc` | create / drop / alter / insert / update / delete / rename / truncate / views + create-db / drop-db |
| `handlers/use`, `show_tables`, `show_databases`, `exists`, `show_create`, `grant` | same files | synthetic-SQL + single-target paths |
| `globalpass` | `forceGlobalForRemoteAsymmetry` | bottom-up JOIN-locality bump + `in`→`globalIn` |
| `response` | response population | `table_rewrites`, `database_rewrites`, `original_accessed_tables`, `privileges_deltas`, `existence_clause`, `failed_cte_aliases` |
| `reverse` | `doRewriteErrorMessage` | physical→logical substitution in error text using `last` maps |

---

## 5. Data flow — `Rewrite(ctx, sql, account)`

1. `opts := r.options(account)` → `[]*pb.RewriteOption` (account-derived policy); then
   **overlay session state** — set `dynamic_args.upstream_logical_database_in_context` (and
   `…physical…`) from `r.session` onto the active `TableNameRewrite`.
2. `ast := engine.ParseOne(sql)` → on failure: `RewriteResult{code: SyntaxError, SQL: sql}`,
   **nil error** (the Go `error` is reserved for unexpected/internal failures — see §8).
3. Read `existence_clause` from the AST and set it immediately (accurate even if a later
   step rejects — matches the proto: only `SyntaxError` leaves it `UNSPECIFIED`).
4. `classify(ast)` → `StatementType` → route to handler.
5. Handler mutates the AST and records bookkeeping:
   - **SELECT** runs the **option pipeline in request order** —
     Limit → TableName → Offset → Settings → CTE — with the CTE-alias scope set; the
     TableName step follows unified static→dynamic precedence.
   - **non-SELECT** applies the active `TableNameRewrite` (single-target path).
6. **SELECT / view body** → `globalPass(ast)` (runs after option application so introduced
   `remote()` calls are visible).
7. `engine.Generate(ast)` → `sql_after_rewrite` — **except** synthetic-SQL handlers build the
   output string directly (see §6 b).
8. If the statement was a successfully-resolved `USE <db>`, update `r.session.logicalDB`.
   Assemble `RewriteResult`; stash the last-call context (sql, account, fwd/reverse name
   maps) for `RewriteErrorMessage`; return.

`RewriteErrorMessage(message)` uses the stashed maps to substitute physical→logical
table/database names back to client-facing names (mirrors `doRewriteErrorMessage`, which
reuses the rewrite maps rather than re-parsing).

---

## 6. Parity risk register (drives Phase 0)

Where full parity is genuinely hard, and the approach for each:

| # | Hazard | Parity approach |
|---|---|---|
| a | `INSERT … VALUES` inline payload lives **off-AST** (C++ splices the raw `[data,end)` tail) | Detect INSERT; locate the `VALUES`/`FORMAT` boundary via `engine.Tokenize` offsets; generate the prelude from AST; splice the original tail verbatim — same trick as C++ |
| b | Synthetic SQL (`SHOW`→`system.tables`; GRANT/REVOKE/CREATE-DB/DROP-DB → `SELECT '…' AS xstmt`) | **Generated, not round-tripped** → low fidelity risk; port the C++ string construction + single-quote escaping **exactly** |
| c | GLOBAL cross-shard pass (remote-vs-local JOIN locality; `in`/`notIn`/`nullIn`/`notNullIn` → `globalIn`/…) | Port the bottom-up walk; only override `Unspecified` locality. Requires polyglot AST to carry JOIN locality + table-function nodes + the IN-family functions. **Spike-validated** (`globalIn` exists in polyglot) |
| d | `remote('addr', db, table, 'user', 'password')` construction | Build the table-function node and round-trip via Generate to the exact CH form. **Spike-validated** |
| e | Formatting/quoting parity (CH backticks `WhenNecessary`, e.g. `` db.`tenant.events` ``) | **Compare semantically, not byte-wise:** re-parse both C++ and Go output and diff ASTs (`engine.Diff`); exact-string compare only for the synthetic `SELECT '…'` literal statements |
| f | Preprocess: `IN (…)` ≥ 50 elements → OR-batches (a guard for CH's recursive-descent **parser**, not a semantic rewrite) | polyglot (Rust) doesn't need it → **skip it**; register the resulting `IN`-list shape as one known, **allow-listed** divergence in the harness |
| g | CTE scope set + externally-supplied CTE bodies (`CommonTableExprRewrite`) + `failed_cte_aliases` | Port the scope-set logic; parse each injected CTE tolerantly; record parse failures in `failed_cte_aliases` |

---

## 7. Parity validation

### Contract types

Generate Go types from `rewriter-grpc/protos/rewriter.proto` (buf/protoc) and vendor them in
`rewriter-go`. Both the native rewriter and the harness use `pb.*`, so the proxy can swap
gRPC↔native behind identical types and comparison is type-identical.

### Differential oracle harness (the backbone of "full parity")

For each `(sql, opts)` case, call **both**:

- the **C++ service** — run the existing `rewriter-grpc` binary/docker, call via the
  generated gRPC client, and
- the **native Go rewriter**,

then compare the two `RewriteSQLResponse`s field-by-field:

- `code` / `statement_type` / `existence_clause` / `table_rewrites` / `database_rewrites` /
  `original_accessed_tables` / `privileges_deltas` / `failed_cte_aliases` → **exact**.
- `sql_after_rewrite` → **semantic** (re-parse + `engine.Diff` AST-equal); **exact-string**
  for synthetic statements; a tiny **allow-list** for intentional divergences (the
  `IN`-split, §6 f).

**Corpus:** (1) every case ported from `rewriter-grpc/tests/rewriter_test.cc`;
(2) ClickHouse's own extracted test SQL (polyglot already ships an extractor);
(3) anonymized prod query logs. The harness runs in CI against a **pinned C++ image** and
**gates every phase**.

**Test types:** differential (primary), golden (ported gtests as Go table tests),
differential-fuzz (mutate the corpus; run both engines; AST-diff).

---

## 8. Error handling & concurrency

- **Two return channels, matching the proto's "`Status::OK` + non-Success code" model:**
  - **Go `error` (≠ nil) → zero `RewriteResult`** — reserved for *unexpected / internal*
    failures only (engine load / FFI failure, JSON (un)marshal bug). This is the true
    fail-open path; the caller logs and forwards the original SQL. `RewriteResult` must not
    be inspected.
  - **Classified outcomes → `RewriteResult{code, message, SQL}` with nil error.** Every
    `RewriteCode` (including `SyntaxError`) is surfaced here; `SQL` is set to the **original
    input** on any non-Success code so the caller can unconditionally run
    `RewriteResult.SQL`. (Confirmed — §11.)
- **`RewriteCode` mapping:** parse failure → `SyntaxError`; valid-but-unhandled
  variant → `UnsupportedStatement` (caller MAY pass through); caller policy wrong for the SQL
  (logical DB unresolvable; unqualified target with empty
  `upstream_logical_database_in_context`; …) → `InvalidRewriteRequest` (caller MUST surface,
  not pass through); unexpected → `RewriteError`. (Taxonomy per `rewriter.proto`.)
- **Concurrency:** the shared polyglot client is `RWMutex`-guarded (SDK). The per-connection
  `last` context is guarded by its own mutex. `Close()` is idempotent and never closes the
  shared client.

---

## 9. Phasing / milestones

Each phase merges only when its slice of the differential harness is green.

- **Phase 0 — Engine + harness + fidelity spike.** Wire the polyglot Go SDK; build the
  `Engine` adapter; establish FFI-lib build/vendor for CI; build the differential-harness
  skeleton + generated `pb` types; run the **full corpus through a pass-through native impl**
  to emit the **divergence report** — the authoritative punch-list and the real go/no-go on
  full parity for our traffic.
- **Phase 1 — SELECT.** table walk, CTE-alias scope set, static/dynamic name resolution,
  the full option pipeline (Limit/Offset/Settings/CTE), the GLOBAL pass, `table_rewrites`,
  `original_accessed_tables`.
- **Phase 2 — writes.** create / drop / alter / insert (+ VALUES splice) / update / delete /
  rename / truncate / views.
- **Phase 3 — db-level.** use / show tables / show databases / create-db / drop-db (mostly
  synthetic SQL).
- **Phase 4 — exists / show-create / grant + revoke** (privilege deltas).
- **Phase 5 — `RewriteErrorMessage`** reverse-mapping + the preprocess decision +
  differential-fuzz hardening.

---

## 10. Out of scope

- The `Optimize` RPC (not in the `Rewriter` interface).
- The `IN`-split preprocess (§6 f) — intentionally skipped; allow-listed divergence.

---

## 11. Decisions & open questions

**Resolved (2026-06-03):**

1. **`RewriteResult` shape** — **mirrors `pb.RewriteSQLResponse`.** ✅
2. **Session state** — the **native rewriter holds it** (per-connection
   `upstream_logical_database_in_context` / `…physical…`, updated by the `USE` handler);
   `options(account)` supplies *account*-derived policy only. ✅ (see §4)
3. **Error-vs-code channel** — non-Success outcomes (incl. `SyntaxError`) are returned in
   **`RewriteResult.code` with a nil Go error**; the Go `error` return is reserved for
   unexpected/internal failures only. ✅ (see §8)

**Still open (not blockers for the plan — folded into Phase 0):**

4. **Phase 0 corpus** — which anonymized prod-log set is available to feed the differential
   spike (in addition to the ported `rewriter_test.cc` cases + ClickHouse's extracted tests).
5. **FFI-lib distribution** — build `libpolyglot_sql_ffi` from source in CI vs. vendor a
   pinned release artifact; platform matrix (prod linux/amd64; dev darwin/arm64).
