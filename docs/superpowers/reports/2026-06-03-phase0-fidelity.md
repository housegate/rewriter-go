# Phase 0 — Fidelity Spike & Divergence Report

- **Date:** 2026-06-03
- **Scope:** polyglot (`tobilg/polyglot` v0.4.3, ClickHouse dialect) as the engine for a
  native Go ClickHouse SQL rewriter — does it support **full parity** with the C++
  `rewriter-grpc` service?
- **Verdict:** **GO**, with the **dual-dialect + raw-SQL** architecture (already adopted in
  the spec, §3.1). One genuinely hard spot remains: **GRANT/REVOKE** (validate before
  trusting). RENAME / EXISTS / ALTER-mutations need raw-SQL handlers.

---

## 1. Executive summary

polyglot parses and **losslessly round-trips** the entire seed corpus (12/12 idempotent),
and the structured statement families (the bulk of traffic) are both **faithfully
round-tripped and structurally rewritable** under the `clickhouse` dialect. The risk is not
round-trip fidelity — it's **structural addressability**: six families are opaque under
`clickhouse`. The dialect probe shows four of them (USE/SHOW/GRANT/REVOKE) are recoverable
via a `generic` re-parse, and only three (RENAME/EXISTS/ALTER-mutations) truly need raw-SQL
handling. Full parity is achievable; this report is the punch-list.

> **Key nuance:** in the spike table below, the six `command` families read `fidelity: OK`.
> That means only that the pass-through **preserves their raw SQL verbatim** (lossless) — it
> does **NOT** mean they're rewritable. They are opaque blobs under `clickhouse`; see §3.

---

## 2. Spike output (seed corpus, 12 statements)

```
#     node_kind         fidelity        sql
------------------------------------------------------------------------------------------
1     select            OK              SELECT a FROM db.t WHERE x IN (1, 2, 3)
2     select            OK              SELECT * FROM a GLOBAL JOIN b ON a.id = b.id
3     insert            OK              INSERT INTO db.t (a) VALUES (1)
4     create_table      OK              CREATE TABLE db.t (a Int64) ENGINE = MergeTree ...
5     drop_table        OK              DROP TABLE IF EXISTS db.t
6     alter_table       OK              ALTER TABLE db.t ADD COLUMN b Int64
7     command           OK              RENAME TABLE db.a TO db.b
8     command           OK              USE db
9     command           OK              SHOW TABLES FROM db
10    command           OK              SHOW CREATE TABLE db.t
11    command           OK              EXISTS TABLE db.t
12    command           OK              GRANT SELECT ON db.t TO u

summary:  OK 12 / total 12
```

Reproduce: `make ffi && POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go run ./cmd/fidelity-spike`

---

## 3. The fidelity map (by ClickHouse-dialect structural addressability)

Established by characterization (`internal/engine/testdata/ast-shapes/`) + a dialect probe
(clickhouse / generic / mysql / postgres). The discriminator is the **single top-level JSON
key** (no `class`/`type` field).

| Tier | Families | polyglot behavior | Handling |
|---|---|---|---|
| **A — structured under `clickhouse`** | SELECT, INSERT, CREATE/DROP/ALTER TABLE, TRUNCATE, DELETE, CREATE DATABASE, DROP DATABASE | Names addressable; `if_exists`/`if_not_exists` exposed; idempotent round-trip | Native AST rewrite (Phase 1–3) |
| **B — opaque `command` under `clickhouse`, structured under `generic`** | USE, GRANT, REVOKE, SHOW TABLES, SHOW DATABASES, SHOW CREATE | `{"command":{"this":"<raw SQL>"}}` under clickhouse; full structure under generic | Dual-dialect: re-parse under `generic` to extract names; these emit **synthetic** SQL anyway, so no faithful round-trip needed |
| **C — opaque under ALL dialects** | RENAME TABLE, EXISTS TABLE, ALTER…UPDATE/…DELETE mutations | `command` (clickhouse) or parse-error (generic/mysql/postgres) | Focused **raw-SQL** handlers (`internal/rawsql`) |

Probe caveats to carry into Phase 1:
- **GRANT/REVOKE** (tier B): under `generic`, the target comes back as a **flat string**
  `securable.name = "db.t"` (must `SplitN` on `.`); privileges at `privileges[].name`;
  grantees at `principals[].name.name`. **Unvalidated:** whether `ON CLUSTER` and ClickHouse
  privilege keywords survive a `generic` parse. **→ GRANT gets a dedicated validation
  checkpoint before its handler is trusted.** This is the single highest-risk item.
- **SHOW TABLES** (tier B): the FROM db sits at `show.from.column.name.name` (wrapped as a
  `column` node) — addressable but non-obvious.

---

## 4. Other divergences found

- **Parser leniency (D-1):** polyglot **accepts `SELECT FROM`**, which ClickHouse's parser
  rejects. So the native rewriter will *not* emit `SyntaxError` for some inputs the C++
  oracle would. Impact: low (fail-open — the input still reaches ClickHouse, which errors),
  but the differential harness must **allow-list parse-leniency divergences** (compare "both
  reject" vs "both accept"; tolerate "polyglot accepts, CH rejects" as a known class). The
  `SyntaxError` *contract* is therefore unit-tested with a fake engine, not a magic string.
- **`IN`-list preprocess (D-2, expected):** the C++ service splits `IN (≥50)` into OR-batches
  (a guard for CH's recursive-descent parser). polyglot (Rust) doesn't need it → we skip it;
  register as one allow-listed divergence (spec §6 f).
- **Array vs object shape (D-3, handled):** polyglot's `Generate`/`RenameTables`/
  `QualifyTables` take/return a JSON **array** while `ParseOne` returns a single object. The
  `engine` package bridges this with `wrap/unwrap`; the `Engine` interface stays
  single-object. No downstream impact.

---

## 5. Positive confirmations

- **`RenameTables` works** (`old_table` → `new_table` verified) — the SELECT-path table-remap
  primitive is viable.
- **Round-trip is byte-clean** for the structured families (`SELECT a FROM db.t` →
  identical). No reformat churn that would swamp the harness with cosmetic diffs.
- **No FFI/engine instability** across all 12 shapes.

---

## 6. Recommendations for Phase 1+

1. **Order by tier-A first** (SELECT → writes → CREATE/DROP DATABASE): highest traffic,
   lowest risk, all natively structured.
2. **Add `Engine.ParseStructured`** (clickhouse → generic fallback) before the tier-B
   handlers (USE/SHOW/GRANT/REVOKE).
3. **GRANT validation checkpoint:** before building the GRANT handler, spike `GRANT … ON
   CLUSTER …` and ClickHouse-specific privilege keywords through the `generic` parse; if they
   don't survive, fall back to a raw-SQL GRANT parser (it's the one family rich enough to
   justify it).
4. **`internal/rawsql`** for tier C (RENAME/EXISTS/mutations) — small, well-tested regex/parsers.
5. **Harness:** wire the C++ oracle (`REWRITER_ORACLE_ADDR`) and the allow-list for D-1/D-2
   before declaring any family at parity.

---

## 7. Open items (non-blocking)

- Prod-log corpus for a larger spike run (seed corpus is 12 hand-picked statements).
- FFI-lib distribution in CI/prod (build-from-source vs vendored artifact; platform matrix).
