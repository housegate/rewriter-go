# Native Go Rewriter — Phase 2 (writes) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port `rewriter-grpc/src/handlers/writes.cc`'s table-level write handling to native Go — CREATE/DROP/ALTER TABLE, TRUNCATE, INSERT (+VALUES/FORMAT payload splice), UPDATE, DELETE, RENAME/EXCHANGE, and CREATE VIEW/MATERIALIZED VIEW — with full behavioral parity against the C++ oracle.

**Architecture:** Mirror C++ `handleWriteQuery`'s dispatch in a new `handlers/writes.go`. The single-target rewrite core reuses the Phase-1 `nameresolve` policy, but with **strict rejection** semantics (a remote/invalid hit rejects the whole statement, unlike SELECT's lenient skip). The `engine` package gains a write-AST layer (`writes.go`) that hides polyglot's JSON shapes behind typed accessors — every write's table reference uses the **same node shape** the SELECT walk already handles (`.name.name` / `.schema.name` / `.alias`), so `decodeTableTarget` + a new `setTableRef` cover the structured kinds. Three statement families need special mechanics: **INSERT** (polyglot drops `FORMAT` payloads, so splice the original tail via `Tokenize` byte offsets — spec §6a), **tier-C raw SQL** (RENAME/EXCHANGE/ALTER…UPDATE parse to opaque `command` nodes — rewrite table names by token-span splice on the original SQL), and **views** (rewrite the view name + MV `TO` target, then run the embedded body SELECT through the Phase-1 pipeline).

**Tech Stack:** Go (CGO_ENABLED=0), `tobilg/polyglot` via PureGo FFI, `gen/pb` protobuf types, TDD with `go test`.

**Scope boundary:** `CREATE DATABASE` / `DROP DATABASE` are **Phase 3** per design spec §9 (they are database-level, emit synthetic `SELECT '…' AS xstmt`, and need db-map validation helpers that are Phase 3's concern). The C++ co-locates them in `writes.cc`, but this plan rejects no-table CREATE/DROP as out-of-phase `UnsupportedStatement` and Phase 3 replaces that stub. All bare-reject kinds (OPTIMIZE/UNDROP/MOVE/BACKUP/RESTORE/KILL/…) are handled here (they would otherwise fall through to SELECT).

---

## Empirical AST shape reference (verified 2026-06-08 against the live engine)

Every table reference below uses the **standard table-ref shape**: `{ "name": {"name":"<table>","quoted":bool}, "schema": {"name":"<db>"}|null, "alias": …|null, "catalog": null, … }`. `decodeTableTarget` (engine/nodes.go) already reads it.

| Statement | NodeKind | Table-ref path(s) | Flags / notes |
|---|---|---|---|
| CREATE TABLE | `create_table` | `.name` (created), `.clone_source` (`AS src`) | `.if_not_exists`; `.as_select` (CREATE…AS SELECT body — left untouched, like C++) |
| DROP TABLE | `drop_table` | `.names[]` (array) | `.if_exists`; **reject if `len(names)>1`** |
| DROP VIEW | `drop_view` | `.name` | `.if_exists`, `.materialized` |
| TRUNCATE | `truncate` | `.table` | `.target`=="Table", `.if_exists` |
| ALTER TABLE | `alter_table` | `.name` | `.if_exists`; `.actions[]` — a `{"Raw":{"sql":"ATTACH PARTITION … FROM …"}}` action signals a **cross-table ref → reject** |
| INSERT | `insert` | `.table` | `.columns`, `.values[]` (kept by Generate), `.query` (INSERT…SELECT — **left untouched**, like C++) |
| UPDATE | `update` | `.table` | structured, round-trips |
| DELETE | `delete` | `.table` | structured, round-trips |
| CREATE VIEW | `create_view` | `.name`; body `.query` (a `{"select":…}` object) | `.materialized`==false |
| CREATE MATERIALIZED VIEW | `create_view` | `.name`; `.to_table` (TO target); body `.query` | `.materialized`==true |
| RENAME TABLE / EXCHANGE / ALTER…UPDATE | `command` | raw SQL in `.this` | **tier-C**: no structured parse; token-span splice |
| OPTIMIZE / others | `command` | raw SQL in `.this` | bare-reject by leading keyword |
| CREATE/DROP DATABASE | `create_database`/`drop_database` | `.name` (flat db ident) | **Phase 3** — out-of-phase reject here |

**INSERT payload behavior (verified):** `Generate` **keeps** `VALUES` tuples (reformatted, e.g. `(1,'a')`→`(1, 'a')` — semantically equal, harness compares semantically) but **drops** `FORMAT <fmt> <payload>` (e.g. `INSERT … FORMAT JSONEachRow {"x":1}` → `INSERT … FORMAT JSONEachRow`). `FORMAT Values (1)(2)(3)` is mangled to `VALUES (1)`. So INSERT must preserve the original `FORMAT`/payload tail by splice. `Tokenize` returns tokens with byte `span:{start,end}` — the `FORMAT`-name token's `end` is the payload boundary.

**Strict decide mapping** (C++ `rewriteOneTarget`): `nameresolve.Resolve` `Outcome.Status` →
`StatusRewrite` → set table + record `table_rewrites`; `StatusRemote` / `StatusRemoteUnsupported` → **reject `UnsupportedStatement`**; `StatusInvalid` → **reject `InvalidRewriteRequest`** (with `RejectReason`); `StatusPassthrough` → no-op.

---

## File Structure

**Create:**
- `internal/engine/writes.go` — write-AST layer: `WriteInfo`/`WriteSlot`, `InspectWrite`, `RewriteWriteTargets`, `setTableRef`, view body extract/set, `GenerateInsert`, `SpliceRawTables`, command sub-classification.
- `internal/engine/writes_test.go` — engine write-layer unit tests.
- `internal/engine/testdata/ast-shapes/*.json` — new golden AST-shape files (Task 1).
- `internal/handlers/writes.go` — `RewriteWrite` dispatch + strict `decideWriteTarget` + reject helpers + `recordAccessedWrite`.
- `internal/handlers/writes_test.go` — handler unit tests.
- `internal/harness/writes_cases.json` + `internal/harness/writes_golden_test.go` — differential corpus + oracle gate.

**Modify:**
- `internal/engine/characterize_test.go` — register the new shape cases (Task 1).
- `internal/handlers/select.go` — extract an AST-returning SELECT core (`rewriteSelectCore`) so the view body reuses the exact Phase-1 pipeline (Task 8).
- `native.go` — route writes through `handlers.RewriteWrite` before the SELECT/pass-through branches (Task 11).

---

## Canonical shared types (defined in Task 2, referenced everywhere)

```go
// internal/engine/writes.go

// WriteRole labels each table slot a write statement exposes.
type WriteRole string

const (
	RoleCreate      WriteRole = "create"       // create_table.name / create_view.name
	RoleCloneSource WriteRole = "clone_source" // create_table.clone_source (CREATE TABLE x AS y)
	RoleViewTo      WriteRole = "view_to"      // create_view.to_table (MATERIALIZED VIEW ... TO)
	RoleDrop        WriteRole = "drop"         // drop_table.names[0] / drop_view.name
	RoleTruncate    WriteRole = "truncate"     // truncate.table
	RoleAlter       WriteRole = "alter"        // alter_table.name
	RoleInsert      WriteRole = "insert"       // insert.table
	RoleUpdate      WriteRole = "update"       // update.table
	RoleDelete      WriteRole = "delete"       // delete.table
)

// WriteSlot is one table reference a write statement targets.
type WriteSlot struct {
	Role   WriteRole
	Target TableTarget // engine/nodes.go: {DB, Table, Alias}
}

// CommandSub sub-classifies a tier-C `command` node by leading keyword(s).
type CommandSub string

const (
	CmdNone        CommandSub = ""             // not a write command (USE/SHOW/GRANT/EXISTS/...) → not handled here
	CmdRename      CommandSub = "rename"       // RENAME TABLE
	CmdExchange    CommandSub = "exchange"     // EXCHANGE TABLES
	CmdAlterUpdate CommandSub = "alter_update" // ALTER TABLE ... UPDATE (parses to a command, not alter_table)
	CmdBareReject  CommandSub = "bare_reject"  // OPTIMIZE/UNDROP/MOVE/BACKUP/RESTORE/KILL/... → UnsupportedStatement
)

// WriteInfo is the read view of a write statement. Kind is the node kind
// (engine.Node*). For command nodes, Sub is set and (for rename/exchange/
// alter_update) RawTargets holds the table refs extracted from the raw SQL.
type WriteInfo struct {
	Kind        string      // engine.NodeCreateTable / NodeDropTable / ... / NodeCommand / "create_view" / "drop_view" / "update"
	Slots       []WriteSlot // structured kinds: ordered table refs
	IfExists    bool
	IfNotExists bool

	// Reject signals (mirror C++ guard checks):
	Multi            bool // DROP/TRUNCATE multi-table (drop_table.names>1)
	CrossTable       bool // ALTER cross-table ref (a Raw action with FROM/TO TABLE)
	Materialized     bool // create_view MV (has TO target + body) / drop_view materialized
	AsTableFunction  bool // CREATE TABLE AS table_function(...) / INSERT INTO FUNCTION(...)
	MissingTable     bool // INSERT with no table
	IsView           bool // create_view (ordinary or materialized)
	HasViewBody      bool // create_view has a .query select body to rewrite

	// command (tier-C) only:
	Sub        CommandSub
	RawTargets []TableTarget // rename/exchange/alter_update: refs parsed from raw SQL
}
```

New node-kind constants to add to `engine/ast.go` (the probe confirmed these top-level keys):

```go
NodeCreateView = "create_view" // CREATE [MATERIALIZED] VIEW (materialized flag inside)
NodeDropView   = "drop_view"
NodeUpdate     = "update"
```

---

## Test conventions (real Phase-1 helpers — use these exact names)

The illustrative test snippets below use shorthand names; map them to the **actual** Phase-1 helpers (verified) and add the two small missing ones:

| Snippet shorthand | Real helper | Where |
|---|---|---|
| `newTestEngine(t)` | `newTestEngine(t) Engine` (exists) | `internal/engine/*_test.go` |
| `newEngine(t)` (handlers/native) | `newEngine(t) engine.Engine` (exists) | `internal/handlers/select_test.go:11` |
| `dynOpt(args)` | `dynOpt(*pb.RewriteTableDynamicArgs) []*pb.RewriteOption` (exists) | `internal/handlers/select_test.go:24` |
| `mapEq(a,b)` | `mapEq(map,map) bool` (exists) | `internal/handlers/select_test.go:29` |
| `staticTableMap(m)` / `remoteTableMap(m)` / `dynamicArgs(...)` | **ADD** `statOpt(*pb.RewriteTableStaticArgs) []*pb.RewriteOption` (mirror `dynOpt`); build args inline | new in `writes_test.go` |
| `semanticEq` / `semanticSQL(t,e,got,want)` | **ADD** `sqlEq(t,e,a,b) bool` = parse both + `Generate` both + string-equal (normalized form) | new in `writes_test.go` (engine pkg: same, local) |

`statOpt` and the static-args shapes:

```go
func statOpt(a *pb.RewriteTableStaticArgs) []*pb.RewriteOption {
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{StaticArgs: a}}}}
}
// rename map:        &pb.RewriteTableStaticArgs{TableMap: map[string]string{"db.t": "t2"}}
// remote map:        &pb.RewriteTableStaticArgs{RemoteTableMap: map[string]*pb.RemoteTable{"db.t": {Addr:"h", Database:"d", Table:"t"}}}
// with-database map: &pb.RewriteTableStaticArgs{TableWithDatabaseMap: map[string]*pb.TableWithDatabase{"db.t": {Database:"p", Table:"t2"}}}
```

`sqlEq` (semantic SQL compare, robust for writes — normalize both sides through the engine):

```go
func sqlEq(t *testing.T, e engine.Engine, a, b string) bool {
	t.Helper()
	na, err := normalize(e, a)
	if err != nil { t.Fatalf("normalize %q: %v", a, err); return false }
	nb, err := normalize(e, b)
	if err != nil { t.Fatalf("normalize %q: %v", b, err); return false }
	return na == nb
}
func normalize(e engine.Engine, sql string) (string, error) {
	ast, err := e.ParseOne(sql)
	if err != nil { return "", err }
	return e.Generate(ast)
}
```

> For INSERT FORMAT payload assertions, do NOT use `sqlEq` (the payload tail isn't re-parseable in isolation) — assert `strings.HasPrefix`(prelude) + `strings.Contains`(payload) as shown in the INSERT tasks. The verify-the-name-survives-as-single-identifier pattern (re-parse + `engine.CollectSelectTables`, select_test.go:63-70) is the right check for dotted dynamic names spliced into raw SQL (Task 6/10).

In the handlers package, the engine-using illustrative helper `newTestEngine` in handler tests means `newEngine`. In the engine package it means the real `newTestEngine`.

---

### Task 1: Lock Phase 2 write-AST shapes as golden contracts

**Files:**
- Modify: `internal/engine/characterize_test.go`
- Create (generated): `internal/engine/testdata/ast-shapes/{create_table,create_table_as,create_table_ifne,drop_table,drop_table_ife,drop_view,truncate,alter_add,alter_delete,alter_attach_from,insert_values,insert_select,update,delete,rename,exchange,alter_update,create_view,create_mv_to}.json`

These golden files freeze the JSON shapes every later engine task depends on. The existing `characterize_test.go` already writes/compares golden files for SELECT shapes (Phase 1, Task 1); follow that exact mechanism.

- [ ] **Step 1: Read the existing characterization harness**

Read `internal/engine/characterize_test.go` to learn the table-driven `cases` slice and the golden write/compare helper (env-gated on `POLYGLOT_SQL_FFI_PATH`, regenerates with `-update` or equivalent). Match its conventions exactly.

- [ ] **Step 2: Add the Phase 2 write cases**

Append these `{name, sql}` cases to the characterization `cases` slice:

```go
{"create_table", "CREATE TABLE db.t (x Int32) ENGINE=Memory"},
{"create_table_as", "CREATE TABLE db.t AS db2.src"},
{"create_table_ifne", "CREATE TABLE IF NOT EXISTS db.t (x Int32) ENGINE=Memory"},
{"drop_table", "DROP TABLE db.t"},
{"drop_table_ife", "DROP TABLE IF EXISTS db.t"},
{"drop_view", "DROP VIEW db.v"},
{"truncate", "TRUNCATE TABLE db.t"},
{"alter_add", "ALTER TABLE db.t ADD COLUMN y Int32"},
{"alter_delete", "ALTER TABLE db.t DELETE WHERE y = 2"},
{"alter_attach_from", "ALTER TABLE db.t ATTACH PARTITION 1 FROM db.src"},
{"insert_values", "INSERT INTO db.t (x) VALUES (1)"},
{"insert_select", "INSERT INTO db.t SELECT * FROM db.s"},
{"update", "UPDATE db.t SET x = 1 WHERE y = 2"},
{"delete", "DELETE FROM db.t WHERE x = 1"},
{"rename", "RENAME TABLE db.a TO db.b"},
{"exchange", "EXCHANGE TABLES db.a AND db.b"},
{"alter_update", "ALTER TABLE db.t UPDATE x = 1 WHERE y = 2"},
{"create_view", "CREATE VIEW db.v AS SELECT * FROM db.s"},
{"create_mv_to", "CREATE MATERIALIZED VIEW db.mv TO db.dst AS SELECT * FROM db.s"},
```

- [ ] **Step 3: Generate the golden files**

Run the characterization test in update mode (the same flag/env Phase 1 used) with `POLYGLOT_SQL_FFI_PATH` set. Confirm 19 new `*.json` files appear under `testdata/ast-shapes/`.

Run: `POLYGLOT_SQL_FFI_PATH=<abs>/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine/ -run TestCharacterize -update` (use Phase 1's actual flag name).

- [ ] **Step 4: Verify the shapes match the reference table**

Spot-check the generated files against the "Empirical AST shape reference" table above: `create_table.name`/`clone_source`, `drop_table.names[0]`, `truncate.table`, `alter_table.actions[0].Raw.sql` (for `alter_attach_from`), `insert.table`/`values`/`query`, `create_view.name`/`materialized`/`to_table`/`query`, and that `rename`/`exchange`/`alter_update` are `{"command":{"this":"…"}}`.

- [ ] **Step 5: Run and commit**

Run: `POLYGLOT_SQL_FFI_PATH=… go test ./internal/engine/ -run TestCharacterize` → PASS. Also `go test ./internal/engine/` (no engine) → PASS (golden compare is env-gated).

```bash
git add internal/engine/characterize_test.go internal/engine/testdata/ast-shapes/
git commit -m "test(engine): characterize Phase 2 write AST shapes"
```

---

### Task 2: Engine write layer — shared types, `setTableRef`, and the simple single-target kinds

Implements the `WriteInfo`/`WriteSlot` types, the `setTableRef` mutator, and `InspectWrite`/`RewriteWriteTargets` for the kinds with **one fixed-path target + simple flags**: `drop_table` (names[0] + multi guard + if_exists), `drop_view` (name + materialized + if_exists), `truncate` (table + if_exists), `update` (table), `delete` (table). The internal `writeSlots` visitor (driven by both Inspect and Rewrite) prevents read/mutate drift.

**Files:**
- Create: `internal/engine/writes.go`, `internal/engine/writes_test.go`
- Modify: `internal/engine/ast.go` (add `NodeCreateView`/`NodeDropView`/`NodeUpdate` constants)

- [ ] **Step 1: Write failing tests**

```go
// internal/engine/writes_test.go
package engine

import "testing"

func mustInspect(t *testing.T, sql string) (WriteInfo, AST) {
	t.Helper()
	e := newTestEngine(t) // reuse Phase 1's engine test helper (POLYGLOT_SQL_FFI_PATH-gated; t.Skip if unset)
	ast, err := e.ParseOne(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	info, err := InspectWrite(ast)
	if err != nil {
		t.Fatalf("InspectWrite %q: %v", sql, err)
	}
	return info, ast
}

func TestInspectWrite_simpleTargets(t *testing.T) {
	cases := []struct {
		sql      string
		kind     string
		role     WriteRole
		db, tbl  string
		ifExists bool
	}{
		{"DROP TABLE db.t", NodeDropTable, RoleDrop, "db", "t", false},
		{"DROP TABLE IF EXISTS db.t", NodeDropTable, RoleDrop, "db", "t", true},
		{"DROP VIEW db.v", NodeDropView, RoleDrop, "db", "v", false},
		{"TRUNCATE TABLE db.t", NodeTruncate, RoleTruncate, "db", "t", false},
		{"UPDATE db.t SET x = 1 WHERE y = 2", NodeUpdate, RoleUpdate, "db", "t", false},
		{"DELETE FROM db.t WHERE x = 1", NodeDelete, RoleDelete, "db", "t", false},
		{"DROP TABLE t", NodeDropTable, RoleDrop, "", "t", false}, // unqualified
	}
	for _, c := range cases {
		info, _ := mustInspect(t, c.sql)
		if info.Kind != c.kind {
			t.Errorf("%q kind=%q want %q", c.sql, info.Kind, c.kind)
		}
		if len(info.Slots) != 1 {
			t.Fatalf("%q slots=%d want 1", c.sql, len(info.Slots))
		}
		s := info.Slots[0]
		if s.Role != c.role || s.Target.DB != c.db || s.Target.Table != c.tbl {
			t.Errorf("%q slot=%+v want role=%s db=%s tbl=%s", c.sql, s, c.role, c.db, c.tbl)
		}
		if info.IfExists != c.ifExists {
			t.Errorf("%q ifExists=%v want %v", c.sql, info.IfExists, c.ifExists)
		}
	}
}

func TestInspectWrite_dropMultiTable(t *testing.T) {
	info, _ := mustInspect(t, "DROP TABLE db.a, db.b")
	if !info.Multi {
		t.Errorf("multi-table DROP: Multi=false want true")
	}
}

func TestRewriteWriteTargets_rename(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("DROP TABLE db.t")
	out, err := RewriteWriteTargets(ast, func(s WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "t_renamed"}
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	want := "DROP TABLE phys.t_renamed"
	if !semanticEq(t, e, got, want) { // reuse Phase 1's semantic-eq test helper
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRewriteWriteTargets_renameKeepsDBWhenNewDBEmpty(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("DROP TABLE db.t")
	out, _ := RewriteWriteTargets(ast, func(s WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "", NewTable: "t2"} // empty NewDB keeps db
	})
	got, _ := e.Generate(out)
	if !semanticEq(t, e, got, "DROP TABLE db.t2") {
		t.Errorf("got %q want DROP TABLE db.t2", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `POLYGLOT_SQL_FFI_PATH=… go test ./internal/engine/ -run 'TestInspectWrite|TestRewriteWriteTargets' -v`
Expected: FAIL (undefined: InspectWrite / WriteInfo / RewriteWriteTargets / WriteSlot).

- [ ] **Step 3: Implement the shared types + simple kinds**

```go
// internal/engine/writes.go
package engine

import (
	"encoding/json"
	"fmt"
)

// (WriteRole, WriteSlot, CommandSub, WriteInfo type blocks from "Canonical shared
//  types" above go here.)

// setTableRef mutates a standard table-ref node in place: always sets the table
// name; sets the schema only when newDB is non-empty (mirrors C++ applyStaticLookup
// / setDatabase, which preserve the origin db when the map entry only renames).
// Writes never alias the rewritten table (unlike SELECT's back-alias).
func setTableRef(tbl map[string]any, newDB, newTable string) {
	tbl["name"] = ident(newTable)
	if newDB != "" {
		tbl["schema"] = ident(newDB)
	}
}

// writeSlots drives both InspectWrite (read) and RewriteWriteTargets (mutate) over
// the per-kind table-ref nodes of a STRUCTURED write body, calling visit(role, tbl)
// for each in document order. body is the value under the single top-level key.
// Returns the structured flags that live alongside the slots. command/database
// kinds are NOT handled here (InspectWrite routes them separately).
func writeSlots(kind string, body map[string]any, visit func(role WriteRole, tbl map[string]any)) {
	tblOf := func(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok }
	switch kind {
	case NodeDropTable:
		if names, ok := body["names"].([]any); ok && len(names) > 0 {
			if tbl, ok := tblOf(names[0]); ok {
				visit(RoleDrop, tbl)
			}
		}
	case NodeDropView:
		if tbl, ok := tblOf(body["name"]); ok {
			visit(RoleDrop, tbl)
		}
	case NodeTruncate:
		if tbl, ok := tblOf(body["table"]); ok {
			visit(RoleTruncate, tbl)
		}
	case NodeUpdate:
		if tbl, ok := tblOf(body["table"]); ok {
			visit(RoleUpdate, tbl)
		}
	case NodeDelete:
		if tbl, ok := tblOf(body["table"]); ok {
			visit(RoleDelete, tbl)
		}
	case NodeCreateTable: // Task 3 extends
		if tbl, ok := tblOf(body["name"]); ok {
			visit(RoleCreate, tbl)
		}
		if tbl, ok := tblOf(body["clone_source"]); ok {
			visit(RoleCloneSource, tbl)
		}
	case NodeAlterTable: // Task 3 extends
		if tbl, ok := tblOf(body["name"]); ok {
			visit(RoleAlter, tbl)
		}
	case NodeInsert: // Task 5 uses
		if tbl, ok := tblOf(body["table"]); ok {
			visit(RoleInsert, tbl)
		}
	case NodeCreateView: // Task 4 extends
		if tbl, ok := tblOf(body["name"]); ok {
			visit(RoleCreate, tbl)
		}
		if tbl, ok := tblOf(body["to_table"]); ok {
			visit(RoleViewTo, tbl)
		}
	}
}

// bodyOf decodes the AST and returns the single top-level kind + its body object.
func bodyOf(ast AST) (kind string, body map[string]any, root map[string]any, err error) {
	if err = json.Unmarshal(ast, &root); err != nil {
		return "", nil, nil, fmt.Errorf("engine: decode write: %w", err)
	}
	if len(root) != 1 {
		return "", nil, nil, fmt.Errorf("engine: expected one top-level key, got %d", len(root))
	}
	for k, v := range root {
		kind = k
		body, _ = v.(map[string]any)
	}
	return kind, body, root, nil
}

// InspectWrite returns the read view of a write statement. Task 2 fills the simple
// single-target kinds + flags; Tasks 3-6 extend it (create/alter/view/insert/command).
func InspectWrite(ast AST) (WriteInfo, error) {
	kind, body, _, err := bodyOf(ast)
	if err != nil {
		return WriteInfo{}, err
	}
	info := WriteInfo{Kind: kind}
	if body == nil {
		return info, nil
	}
	switch kind {
	case NodeDropTable:
		if names, ok := body["names"].([]any); ok && len(names) > 1 {
			info.Multi = true
		}
		info.IfExists, _ = body["if_exists"].(bool)
	case NodeDropView:
		info.IfExists, _ = body["if_exists"].(bool)
		info.Materialized, _ = body["materialized"].(bool)
	case NodeTruncate:
		info.IfExists, _ = body["if_exists"].(bool)
	}
	writeSlots(kind, body, func(role WriteRole, tbl map[string]any) {
		info.Slots = append(info.Slots, WriteSlot{Role: role, Target: decodeTableTarget(tbl)})
	})
	return info, nil
}

// RewriteWriteTargets applies decide to every structured table-ref slot, in place.
// Writes only honor ActionRename / ActionSkip (they never route to remote()); a
// non-Rename decision leaves the node untouched.
func RewriteWriteTargets(ast AST, decide func(WriteSlot) TableDecision) (AST, error) {
	kind, body, root, err := bodyOf(ast)
	if err != nil {
		return nil, err
	}
	if body != nil {
		writeSlots(kind, body, func(role WriteRole, tbl map[string]any) {
			d := decide(WriteSlot{Role: role, Target: decodeTableTarget(tbl)})
			if d.Action == ActionRename {
				setTableRef(tbl, d.NewDB, d.NewTable)
			}
		})
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode write: %w", err)
	}
	return AST(out), nil
}
```

Add to `internal/engine/ast.go`:

```go
NodeCreateView = "create_view"
NodeDropView   = "drop_view"
NodeUpdate     = "update"
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `POLYGLOT_SQL_FFI_PATH=… go test ./internal/engine/ -run 'TestInspectWrite|TestRewriteWriteTargets' -v` → PASS.

- [ ] **Step 5: Vet + full engine suite + commit**

Run: `go vet ./internal/engine/` and `POLYGLOT_SQL_FFI_PATH=… go test ./internal/engine/`.

```bash
git add internal/engine/writes.go internal/engine/writes_test.go internal/engine/ast.go
git commit -m "feat(engine): write-AST layer — simple single-target kinds + setTableRef"
```

---

### Task 3: Engine — `create_table` and `alter_table` support

Extends `InspectWrite` for the two structured kinds with extra slots/flags: CREATE TABLE (`.name` + `.clone_source` + `if_not_exists` + `as_table_function` detection) and ALTER TABLE (`.name` + `if_exists` + cross-table-ref detection). The `writeSlots` cases for both are already present from Task 2; this task adds the flag extraction.

**Files:**
- Modify: `internal/engine/writes.go`, `internal/engine/writes_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestInspectWrite_createTable(t *testing.T) {
	info, _ := mustInspect(t, "CREATE TABLE db.t (x Int32) ENGINE=Memory")
	if info.Kind != NodeCreateTable || len(info.Slots) != 1 || info.Slots[0].Role != RoleCreate {
		t.Fatalf("got %+v", info)
	}
	if info.Slots[0].Target.DB != "db" || info.Slots[0].Target.Table != "t" {
		t.Errorf("target=%+v", info.Slots[0].Target)
	}
}

func TestInspectWrite_createTableAs(t *testing.T) {
	info, _ := mustInspect(t, "CREATE TABLE db.t AS db2.src")
	if len(info.Slots) != 2 {
		t.Fatalf("slots=%d want 2 (%+v)", len(info.Slots), info.Slots)
	}
	if info.Slots[0].Role != RoleCreate || info.Slots[1].Role != RoleCloneSource {
		t.Errorf("roles=%s,%s", info.Slots[0].Role, info.Slots[1].Role)
	}
	if info.Slots[1].Target.DB != "db2" || info.Slots[1].Target.Table != "src" {
		t.Errorf("clone=%+v", info.Slots[1].Target)
	}
}

func TestInspectWrite_createTableIfNotExists(t *testing.T) {
	info, _ := mustInspect(t, "CREATE TABLE IF NOT EXISTS db.t (x Int32) ENGINE=Memory")
	if !info.IfNotExists {
		t.Errorf("IfNotExists=false want true")
	}
}

func TestInspectWrite_alterTable(t *testing.T) {
	info, _ := mustInspect(t, "ALTER TABLE db.t ADD COLUMN y Int32")
	if info.Kind != NodeAlterTable || len(info.Slots) != 1 || info.Slots[0].Role != RoleAlter {
		t.Fatalf("got %+v", info)
	}
	if info.CrossTable {
		t.Errorf("plain ALTER ADD flagged CrossTable")
	}
}

func TestInspectWrite_alterCrossTable(t *testing.T) {
	info, _ := mustInspect(t, "ALTER TABLE db.t ATTACH PARTITION 1 FROM db.src")
	if !info.CrossTable {
		t.Errorf("ATTACH ... FROM not flagged CrossTable")
	}
}

func TestRewriteWriteTargets_createTableAs(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("CREATE TABLE db.t AS db2.src")
	out, _ := RewriteWriteTargets(ast, func(s WriteSlot) TableDecision {
		switch s.Role {
		case RoleCreate:
			return TableDecision{Action: ActionRename, NewDB: "p1", NewTable: "t2"}
		case RoleCloneSource:
			return TableDecision{Action: ActionRename, NewDB: "p2", NewTable: "src2"}
		}
		return TableDecision{Action: ActionSkip}
	})
	got, _ := e.Generate(out)
	if !semanticEq(t, e, got, "CREATE TABLE p1.t2 AS p2.src2") {
		t.Errorf("got %q", got)
	}
}
```

- [ ] **Step 2: Run to verify failure** (`CrossTable`/`IfNotExists` assertions fail, create-as slot count fails).

- [ ] **Step 3: Implement the flag extraction**

Add to `InspectWrite`'s switch in `writes.go`:

```go
case NodeCreateTable:
	info.IfNotExists, _ = body["if_not_exists"].(bool)
	if body["as_table_function"] != nil {
		info.AsTableFunction = true
	}
case NodeAlterTable:
	info.IfExists, _ = body["if_exists"].(bool)
	info.CrossTable = alterHasCrossTableRef(body)
```

```go
// alterHasCrossTableRef reports whether any ALTER action references a second table
// (ATTACH/REPLACE/FETCH PARTITION FROM, MOVE PARTITION TO TABLE). Polyglot models
// these as {"Raw":{"sql":"…"}} actions, so detection is a keyword scan over each
// Raw action's SQL. Mirrors C++ alterHasCrossTableRef (writes.cc:160-169).
func alterHasCrossTableRef(body map[string]any) bool {
	actions, ok := body["actions"].([]any)
	if !ok {
		return false
	}
	for _, a := range actions {
		am, ok := a.(map[string]any)
		if !ok {
			continue
		}
		raw, ok := am["Raw"].(map[string]any)
		if !ok {
			continue
		}
		sql, _ := raw["sql"].(string)
		if rawActionIsCrossTable(sql) {
			return true
		}
	}
	return false
}

// rawActionIsCrossTable matches the cross-table ALTER partition forms by keyword.
// Case-insensitive: "<PARTITION-op> ... FROM <table>" and "MOVE PARTITION ... TO TABLE <table>".
func rawActionIsCrossTable(sql string) bool {
	u := strings.ToUpper(sql)
	hasPartition := strings.Contains(u, "PARTITION")
	if hasPartition && strings.Contains(u, " FROM ") {
		return true // ATTACH/REPLACE/FETCH PARTITION ... FROM <table>
	}
	if hasPartition && strings.Contains(u, " TO TABLE ") {
		return true // MOVE PARTITION ... TO TABLE <table>
	}
	return false
}
```

Add `"strings"` to the `writes.go` import block.

> **Implementer note:** `rawActionIsCrossTable` is a parity heuristic over Raw-action SQL (polyglot doesn't structurally separate `from_table`/`to_table` the way ClickHouse's C++ AST does). The differential corpus (Task 12) must include `ATTACH PARTITION … FROM`, `REPLACE PARTITION … FROM`, `FETCH PARTITION … FROM`, and `MOVE PARTITION … TO TABLE` to validate it against the oracle. If a case diverges, refine the keyword match — do not broaden to "reject any Raw action" (that would over-reject legitimate alters polyglot models as Raw).

- [ ] **Step 4: Run to verify pass.** `POLYGLOT_SQL_FFI_PATH=… go test ./internal/engine/ -run 'TestInspectWrite|TestRewriteWriteTargets' -v` → PASS.

- [ ] **Step 5: Vet + commit**

```bash
git add internal/engine/writes.go internal/engine/writes_test.go
git commit -m "feat(engine): create_table (+clone_source) and alter_table (+cross-table guard)"
```

---

### Task 4: Engine — view support (name + MV `TO` target + body extract/set)

CREATE VIEW / MATERIALIZED VIEW arrive as `create_view`. This task adds: the `Materialized`/`IsView`/`HasViewBody` flags + the `RoleViewTo` slot (already wired in `writeSlots`), and `ExtractViewBody`/`SetViewBody` so the handler (Task 8) can run the embedded `.query` select through the Phase-1 pipeline and splice it back.

**Files:**
- Modify: `internal/engine/writes.go`, `internal/engine/writes_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestInspectWrite_createView(t *testing.T) {
	info, _ := mustInspect(t, "CREATE VIEW db.v AS SELECT * FROM db.s")
	if info.Kind != NodeCreateView || !info.IsView || info.Materialized {
		t.Fatalf("got %+v", info)
	}
	if len(info.Slots) != 1 || info.Slots[0].Role != RoleCreate {
		t.Fatalf("slots=%+v", info.Slots)
	}
	if !info.HasViewBody {
		t.Errorf("HasViewBody=false want true")
	}
}

func TestInspectWrite_createMVTo(t *testing.T) {
	info, _ := mustInspect(t, "CREATE MATERIALIZED VIEW db.mv TO db.dst AS SELECT * FROM db.s")
	if !info.Materialized {
		t.Fatalf("Materialized=false")
	}
	if len(info.Slots) != 2 || info.Slots[1].Role != RoleViewTo ||
		info.Slots[1].Target.DB != "db" || info.Slots[1].Target.Table != "dst" {
		t.Fatalf("slots=%+v", info.Slots)
	}
}

func TestViewBody_extractRewriteSet(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("CREATE VIEW db.v AS SELECT * FROM db.s")
	body, ok, err := ExtractViewBody(ast)
	if err != nil || !ok {
		t.Fatalf("extract: ok=%v err=%v", ok, err)
	}
	// body is a {"select":...} AST: CollectSelectTables sees db.s.
	tts, _ := CollectSelectTables(body)
	if len(tts) != 1 || tts[0].Table != "s" {
		t.Fatalf("body tables=%+v", tts)
	}
	// Rewrite the body and splice it back.
	body2, _ := RewriteSelectTables(body, func(tt TableTarget) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "s2"}
	})
	out, err := SetViewBody(ast, body2)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	if !semanticEq(t, e, got, "CREATE VIEW db.v AS SELECT * FROM phys.s2") {
		t.Errorf("got %q", got)
	}
}
```

- [ ] **Step 2: Run to verify failure** (undefined `ExtractViewBody`/`SetViewBody`; view flags unset).

- [ ] **Step 3: Implement view flags + body extract/set**

Add to `InspectWrite`'s switch:

```go
case NodeCreateView:
	info.IsView = true
	info.Materialized, _ = body["materialized"].(bool)
	info.IfNotExists, _ = body["if_not_exists"].(bool)
	if _, ok := body["query"].(map[string]any); ok {
		info.HasViewBody = true
	}
```

```go
// ExtractViewBody returns the view's embedded body as a standalone {"select":…}
// AST (the value of create_view.query), or ok=false when absent. The returned AST
// is a deep copy safe to rewrite independently before SetViewBody splices it back.
func ExtractViewBody(ast AST) (AST, bool, error) {
	_, body, _, err := bodyOf(ast)
	if err != nil {
		return nil, false, err
	}
	q, ok := body["query"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	b, err := json.Marshal(q)
	if err != nil {
		return nil, false, fmt.Errorf("engine: encode view body: %w", err)
	}
	return AST(b), true, nil
}

// SetViewBody replaces create_view.query with the given {"select":…} body AST and
// re-encodes the whole statement.
func SetViewBody(ast AST, body AST) (AST, error) {
	kind, b, root, err := bodyOf(ast)
	if err != nil {
		return nil, err
	}
	if kind != NodeCreateView {
		return nil, fmt.Errorf("engine: SetViewBody on non-view kind %q", kind)
	}
	var bodyNode map[string]any
	if err := json.Unmarshal(body, &bodyNode); err != nil {
		return nil, fmt.Errorf("engine: decode view body: %w", err)
	}
	b["query"] = bodyNode
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode view: %w", err)
	}
	return AST(out), nil
}
```

> **Note:** `ExtractViewBody` returns `{"select":…}`, exactly the AST shape `CollectSelectTables`/`RewriteSelectTables`/`outerSelect` consume — so Task 8's handler reuses the full Phase-1 SELECT pipeline on the body unchanged.

- [ ] **Step 4: Run to verify pass.**

- [ ] **Step 5: Vet + commit**

```bash
git add internal/engine/writes.go internal/engine/writes_test.go
git commit -m "feat(engine): view name/TO target + embedded body extract/set"
```

---

### Task 5: Engine — INSERT target + `GenerateInsert` payload splice

INSERT's `.table` slot is already wired in `writeSlots`. This task adds INSERT-specific flags (`AsTableFunction` for `INSERT INTO FUNCTION(...)`, `MissingTable`) and `GenerateInsert(originalSQL, rewrittenAST)`, which preserves a `FORMAT` payload tail (dropped by `Generate`) by splicing the original bytes after the FORMAT-name token. For `VALUES`, `Generate` already keeps the tuples (semantic compare), so no splice is needed; only a `FORMAT` token triggers the tail splice.

**Files:**
- Modify: `internal/engine/writes.go`, `internal/engine/writes_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestInspectWrite_insert(t *testing.T) {
	info, _ := mustInspect(t, "INSERT INTO db.t (x) VALUES (1)")
	if info.Kind != NodeInsert || len(info.Slots) != 1 || info.Slots[0].Role != RoleInsert {
		t.Fatalf("got %+v", info)
	}
	if info.Slots[0].Target.DB != "db" || info.Slots[0].Target.Table != "t" {
		t.Errorf("target=%+v", info.Slots[0].Target)
	}
}

func TestInspectWrite_insertFunctionRejectFlags(t *testing.T) {
	info, _ := mustInspect(t, "INSERT INTO FUNCTION remote('h', db, t) VALUES (1)")
	if !info.AsTableFunction {
		t.Errorf("INSERT INTO FUNCTION: AsTableFunction=false")
	}
}

func TestGenerateInsert_valuesKept(t *testing.T) {
	e := newTestEngine(t)
	orig := "INSERT INTO db.t (x, y) VALUES (1, 'a'), (2, 'b')"
	ast, _ := e.ParseOne(orig)
	rw, _ := RewriteWriteTargets(ast, func(s WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "t2"}
	})
	got, err := GenerateInsert(e, orig, rw)
	if err != nil {
		t.Fatal(err)
	}
	if !semanticEq(t, e, got, "INSERT INTO phys.t2 (x, y) VALUES (1, 'a'), (2, 'b')") {
		t.Errorf("got %q", got)
	}
}

func TestGenerateInsert_formatPayloadPreserved(t *testing.T) {
	e := newTestEngine(t)
	orig := "INSERT INTO db.t FORMAT JSONEachRow {\"x\":1}\n{\"x\":2}"
	ast, _ := e.ParseOne(orig)
	rw, _ := RewriteWriteTargets(ast, func(s WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "t2"}
	})
	got, err := GenerateInsert(e, orig, rw)
	if err != nil {
		t.Fatal(err)
	}
	// Prelude rewritten, payload bytes preserved verbatim.
	wantPrefix := "INSERT INTO phys.t2 FORMAT JSONEachRow"
	wantTail := "{\"x\":1}\n{\"x\":2}"
	if !strings.HasPrefix(got, wantPrefix) || !strings.Contains(got, wantTail) {
		t.Errorf("got %q\n want prefix %q + tail %q", got, wantPrefix, wantTail)
	}
}
```

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement INSERT flags + `GenerateInsert`**

Add to `InspectWrite`'s switch:

```go
case NodeInsert:
	if body["table_function"] != nil { // INSERT INTO FUNCTION(...)
		info.AsTableFunction = true
	}
	if _, ok := body["table"].(map[string]any); !ok {
		info.MissingTable = true
	}
```

> **Implementer note:** verify the `INSERT INTO FUNCTION` shape empirically — the probe didn't cover it. If polyglot nests the function elsewhere (e.g. under `table` with a `function` key rather than a `table_function` key), adjust the detection. The C++ guard is `insert_query->table_function` (writes.cc:504) and `!insert_query->table` (writes.cc:508).

```go
// GenerateInsert generates the rewritten INSERT. Generate() reproduces VALUES
// tuples (semantically) but DROPS a `FORMAT <fmt> <payload>` tail, so when the
// original carried a FORMAT payload we splice the original bytes back, mirroring
// C++ ASTInsertQuery's data/end splice (writes.cc:520-535). Boundary = the byte
// `end` of the format-name token (the identifier right after FORMAT).
func GenerateInsert(e Engine, originalSQL string, rewritten AST) (string, error) {
	prelude, err := e.Generate(rewritten)
	if err != nil {
		return "", err
	}
	tail, ok, err := insertFormatTail(e, originalSQL)
	if err != nil {
		return "", err
	}
	if !ok {
		return prelude, nil // VALUES (kept by Generate) or no payload
	}
	return prelude + tail, nil
}

// insertFormatTail tokenizes originalSQL and, if it contains a top-level FORMAT
// clause, returns the original substring from the end of the format-name token to
// EOF (the payload tail, including its leading whitespace/newline). ok=false when
// there is no FORMAT clause.
func insertFormatTail(e Engine, originalSQL string) (string, bool, error) {
	toksAST, err := e.Tokenize(originalSQL)
	if err != nil {
		return "", false, err
	}
	var toks []struct {
		TokenType string `json:"token_type"`
		Span      struct {
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"span"`
	}
	if err := json.Unmarshal(toksAST, &toks); err != nil {
		return "", false, fmt.Errorf("engine: decode tokens: %w", err)
	}
	for i, tk := range toks {
		if tk.TokenType == "FORMAT" && i+1 < len(toks) {
			boundary := toks[i+1].Span.End // end of the format-name identifier
			if boundary >= 0 && boundary <= len(originalSQL) {
				return originalSQL[boundary:], true, nil
			}
		}
	}
	return "", false, nil
}
```

> **Implementer note (boundary verification):** the Task-1 characterization and the Phase-2 probe established that for `INSERT … FORMAT JSONEachRow {payload}` the format-name token (`JSONEachRow`) `span.end` is the correct cut point; `originalSQL[end:]` = ` {payload}` (leading space preserved → valid SQL). Add a focused test for `FORMAT CSV\n…` (newline tail) and confirm the boundary holds. The pathological `FORMAT Values (1)(2)(3)` form (Generate mangles it to `VALUES (1)`) is out of scope for v1 — if the corpus surfaces it, document it as an allow-listed divergence (spec §6 f style) rather than special-casing.

- [ ] **Step 4: Run to verify pass.**

- [ ] **Step 5: Vet + commit**

```bash
git add internal/engine/writes.go internal/engine/writes_test.go
git commit -m "feat(engine): INSERT target flags + FORMAT-payload-preserving GenerateInsert"
```

---

### Task 6: Engine — tier-C raw SQL (RENAME / EXCHANGE / ALTER…UPDATE) command sub-classification + table-span splice

RENAME TABLE, EXCHANGE TABLES, and ALTER…UPDATE parse to opaque `{"command":{"this":"<raw SQL>"}}` nodes (no structured table refs). This task adds `classifyCommand` (sub-kind by leading keyword), a token-based table-ref extractor for each form, and `SpliceRawTables(originalSQL, rewrites)` that rewrites table-name byte spans in place while leaving everything else verbatim.

**Files:**
- Modify: `internal/engine/writes.go`, `internal/engine/writes_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestInspectWrite_commandSubKinds(t *testing.T) {
	cases := []struct {
		sql string
		sub CommandSub
	}{
		{"RENAME TABLE db.a TO db.b", CmdRename},
		{"RENAME TABLE db.a TO db.b, db.c TO db.d", CmdRename},
		{"EXCHANGE TABLES db.a AND db.b", CmdExchange},
		{"ALTER TABLE db.t UPDATE x = 1 WHERE y = 2", CmdAlterUpdate},
		{"OPTIMIZE TABLE db.t", CmdBareReject},
		{"USE db", CmdNone},   // not a write command
		{"EXISTS TABLE db.t", CmdNone},
	}
	for _, c := range cases {
		info, _ := mustInspect(t, c.sql)
		if info.Kind != NodeCommand || info.Sub != c.sub {
			t.Errorf("%q sub=%q want %q", c.sql, info.Sub, c.sub)
		}
	}
}

func TestInspectWrite_renameRawTargets(t *testing.T) {
	info, _ := mustInspect(t, "RENAME TABLE db.a TO db.b, c TO d")
	got := info.RawTargets
	want := []TableTarget{{DB: "db", Table: "a"}, {DB: "db", Table: "b"}, {Table: "c"}, {Table: "d"}}
	if len(got) != len(want) {
		t.Fatalf("rawTargets=%+v want %+v", got, want)
	}
	for i := range want {
		if got[i].DB != want[i].DB || got[i].Table != want[i].Table {
			t.Errorf("rawTargets[%d]=%+v want %+v", i, got[i], want[i])
		}
	}
}

func TestSpliceRawTables_rename(t *testing.T) {
	e := newTestEngine(t)
	orig := "RENAME TABLE db.a TO db.b"
	out, err := SpliceRawTables(e, orig, map[string]string{"db.a": "phys.a1", "db.b": "phys.b1"})
	if err != nil {
		t.Fatal(err)
	}
	if !semanticEq(t, e, out, "RENAME TABLE phys.a1 TO phys.b1") {
		t.Errorf("got %q", out)
	}
}

func TestSpliceRawTables_alterUpdate(t *testing.T) {
	e := newTestEngine(t)
	orig := "ALTER TABLE db.t UPDATE x = 1 WHERE y = 2"
	out, _ := SpliceRawTables(e, orig, map[string]string{"db.t": "phys.t1"})
	if !semanticEq(t, e, out, "ALTER TABLE phys.t1 UPDATE x = 1 WHERE y = 2") {
		t.Errorf("got %q", out)
	}
}
```

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement command sub-classification + raw extractor + splice**

In `InspectWrite`, add the command branch (after the structured switch, before returning) — note command nodes have `body == nil` for the `bodyOf` decode (their value is `{"this":"…"}`, an object, so `body` is non-nil; handle accordingly):

```go
case NodeCommand:
	raw, _ := body["this"].(string)
	info.Sub = classifyWriteCommand(raw)
	if info.Sub == CmdRename || info.Sub == CmdExchange || info.Sub == CmdAlterUpdate {
		info.RawTargets = extractRawTableRefs(ast2Tokens(raw)) // see helper note below
	}
```

Because the token extractor needs the engine, `InspectWrite` cannot tokenize on its own (it has no `Engine`). **Design adjustment:** split command-target extraction into a separate engine call the handler makes when needed:

```go
// InspectWrite (command branch) sets only Kind + Sub:
case NodeCommand:
	raw, _ := body["this"].(string)
	info.Sub = classifyWriteCommand(raw)
```

```go
// classifyWriteCommand sub-classifies a raw `command` SQL string by leading
// keyword(s). Returns CmdNone for non-write commands (USE/SHOW/GRANT/REVOKE/
// EXISTS/SHOW CREATE) so the dispatcher passes them through to later phases.
func classifyWriteCommand(sql string) CommandSub {
	u := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(u, "RENAME TABLE"):
		return CmdRename
	case strings.HasPrefix(u, "EXCHANGE TABLE"): // EXCHANGE TABLES
		return CmdExchange
	case strings.HasPrefix(u, "ALTER TABLE") && containsWord(u, "UPDATE"):
		return CmdAlterUpdate
	case strings.HasPrefix(u, "OPTIMIZE"), strings.HasPrefix(u, "UNDROP"),
		strings.HasPrefix(u, "MOVE"), strings.HasPrefix(u, "BACKUP"),
		strings.HasPrefix(u, "RESTORE"), strings.HasPrefix(u, "KILL"):
		return CmdBareReject
	default:
		return CmdNone
	}
}

// containsWord reports whether upper-cased haystack contains word as a
// whitespace-delimited token (avoids matching UPDATE inside an identifier).
func containsWord(haystack, word string) bool {
	for _, f := range strings.Fields(haystack) {
		if f == word {
			return true
		}
	}
	return false
}
```

Now the engine-level raw extraction + splice (the handler calls `RawTableRefs` for read/accessed and `SpliceRawTables` for mutation; both tokenize via the engine):

```go
// rawToken is the decoded shape of one Tokenize token we care about.
type rawToken struct {
	TokenType string `json:"token_type"`
	Text      string `json:"text"`
	Span      struct {
		Start int `json:"start"`
		End   int `json:"end"`
	} `json:"span"`
}

func tokenizeRaw(e Engine, sql string) ([]rawToken, error) {
	toksAST, err := e.Tokenize(sql)
	if err != nil {
		return nil, err
	}
	var toks []rawToken
	if err := json.Unmarshal(toksAST, &toks); err != nil {
		return nil, fmt.Errorf("engine: decode tokens: %w", err)
	}
	return toks, nil
}

// tableRefSpan is one [db.]table reference located in a raw SQL token stream.
type tableRefSpan struct {
	Target TableTarget
	Start  int // byte offset of the first name token
	End    int // byte offset just past the last name token
}

// scanTableRefs walks tokens and extracts every `name [DOT name]` run that sits in
// a table-name position for the given command sub-kind. The grammar positions:
//   RENAME TABLE  : every name-run is a table (separated by TO / AND / COMMA)
//   EXCHANGE TABLE: every name-run is a table (separated by AND / COMMA)
//   ALTER…UPDATE  : the single name-run immediately after the TABLE keyword
// A name-run is [VAR|QUOTED] (DOT [VAR|QUOTED])? . We stop ALTER…UPDATE after the
// first run (the rest is the UPDATE/WHERE expression, not a table position).
func scanTableRefs(toks []rawToken, sub CommandSub) []tableRefSpan {
	isName := func(tt string) bool { return tt == "VAR" || tt == "QUOTED" || tt == "IDENTIFIER" }
	// Find the index just after the leading TABLE / TABLES keyword.
	start := 0
	for i, tk := range toks {
		if strings.EqualFold(tk.Text, "TABLE") || strings.EqualFold(tk.Text, "TABLES") {
			start = i + 1
			break
		}
	}
	var out []tableRefSpan
	i := start
	for i < len(toks) {
		if !isName(toks[i].TokenType) {
			i++
			continue
		}
		// Begin a name-run: name [DOT name].
		first := i
		dbTok := toks[i]
		var span tableRefSpan
		if i+2 < len(toks) && toks[i+1].TokenType == "DOT" && isName(toks[i+2].TokenType) {
			span.Target = TableTarget{DB: dbTok.Text, Table: toks[i+2].Text}
			span.Start = dbTok.Span.Start
			span.End = toks[i+2].Span.End
			i += 3
		} else {
			span.Target = TableTarget{Table: dbTok.Text}
			span.Start = dbTok.Span.Start
			span.End = dbTok.Span.End
			i++
		}
		out = append(out, span)
		_ = first
		if sub == CmdAlterUpdate {
			break // only the table immediately after ALTER TABLE
		}
	}
	return out
}

// RawTableRefs returns the table references in a tier-C raw command (read-only),
// for accessed-table recording and strict validation before splicing.
func RawTableRefs(e Engine, ast AST) ([]TableTarget, CommandSub, error) {
	_, body, _, err := bodyOf(ast)
	if err != nil {
		return nil, CmdNone, err
	}
	raw, _ := body["this"].(string)
	sub := classifyWriteCommand(raw)
	if sub != CmdRename && sub != CmdExchange && sub != CmdAlterUpdate {
		return nil, sub, nil
	}
	toks, err := tokenizeRaw(e, raw)
	if err != nil {
		return nil, sub, err
	}
	spans := scanTableRefs(toks, sub)
	out := make([]TableTarget, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Target)
	}
	return out, sub, nil
}

// SpliceRawTables rewrites the table-name spans of a tier-C raw command. rewrites
// maps qualify(origDB,origTable) → new qualified name. Spans are replaced
// right-to-left so earlier byte offsets stay valid. Tables absent from the map are
// left verbatim. Returns the spliced SQL string.
func SpliceRawTables(e Engine, originalSQL string, rewrites map[string]string) (string, error) {
	sub := classifyWriteCommand(originalSQL)
	toks, err := tokenizeRaw(e, originalSQL)
	if err != nil {
		return "", err
	}
	spans := scanTableRefs(toks, sub)
	out := originalSQL
	for i := len(spans) - 1; i >= 0; i-- {
		s := spans[i]
		key := qualifyTT(s.Target)
		nv, ok := rewrites[key]
		if !ok {
			continue
		}
		out = out[:s.Start] + nv + out[s.End:]
	}
	return out, nil
}

// qualifyTT builds "db.table" (or bare "table") from a TableTarget — the splice/
// rewrites map key. Mirrors nameresolve.qualify / handlers.qualify.
func qualifyTT(tt TableTarget) string {
	if tt.DB == "" {
		return tt.Table
	}
	return tt.DB + "." + tt.Table
}
```

> **Implementer notes:**
> 1. Token type strings (`VAR`/`DOT`/`QUOTED`) come from the Phase-2 probe (`Tokenize` emits `{"token_type":"VAR","text":"db","span":{"start":12,"end":14}}`). Verify `QUOTED`/`IDENTIFIER` names against the live tokenizer for backtick-quoted identifiers (e.g. `` RENAME TABLE `db`.`a` TO … ``); adjust `isName` to whatever the tokenizer actually emits.
> 2. The new qualified name `nv` from the handler is already correctly quoted by `nameresolve` semantics — but raw splice inserts a literal string. For a dotted dynamic name the handler must pass a backtick/quote-safe form. Reuse the `ident`+Generate path is not available for raw, so the handler builds `nv` via a helper that quotes each segment when `needsQuoting`. Add `QuoteQualified(db, table string) string` to engine and use it when constructing the rewrites map (Task 10).
> 3. `EXCHANGE TABLES a AND b`: `AND` is a name separator here, but `scanTableRefs` already skips non-name tokens, so `AND` is naturally skipped. Confirm `AND` doesn't tokenize as a name.

Add `QuoteQualified`:

```go
// QuoteQualified renders "db.table" with each segment backtick-quoted only when
// needsQuoting, for splicing a rewritten name into raw SQL. Bare db ("") → table only.
func QuoteQualified(db, table string) string {
	q := func(s string) string {
		if needsQuoting(s) {
			return "`" + s + "`"
		}
		return s
	}
	if db == "" {
		return q(table)
	}
	return q(db) + "." + q(table)
}
```

- [ ] **Step 4: Run to verify pass.** Add a backtick round-trip test: `SpliceRawTables` on `RENAME TABLE db.a TO db.b` with a dotted dynamic target (`db.a`→`` `tenant1.a` ``) generates SQL that re-parses to the intended single identifier.

- [ ] **Step 5: Vet + commit**

```bash
git add internal/engine/writes.go internal/engine/writes_test.go
git commit -m "feat(engine): tier-C raw-SQL command sub-classify + table-span splice"
```

---

### Task 7: Handlers — write spine, strict decide, rejects, accessed, and structured-kind dispatch

Creates `handlers/writes.go` with `RewriteWrite` (the `handleWriteQuery` port), the strict `decideWriteTarget`, reject helpers, `recordAccessedWrite`, and dispatch for the structured single-target kinds: CREATE TABLE, DROP TABLE, DROP VIEW, TRUNCATE, ALTER TABLE, UPDATE, DELETE. Views (Task 8), INSERT (Task 9), and tier-C/bare-reject/db-level (Task 10) are added next.

**Files:**
- Create: `internal/handlers/writes.go`, `internal/handlers/writes_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/handlers/writes_test.go
package handlers

import (
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// staticOpts builds a TableNameRewrite option with a static table_map (reuse the
// Phase-1 test helper if one exists; otherwise construct pb types inline).
func staticTableMap(m map[string]string) []*pb.RewriteOption { /* … as in select_test.go … */ }

func TestRewriteWrite_dropTableStaticRename(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("DROP TABLE db.t")
	opts := staticTableMap(map[string]string{"db.t": "t_phys"})
	resp, handled, err := RewriteWrite(e, ast, opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DROP_TABLE {
		t.Errorf("stmt=%v", resp.GetStatementType())
	}
	if !semanticSQL(t, e, resp.GetSqlAfterRewrite(), "DROP TABLE db.t_phys") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
	if resp.GetTableRewrites()["db.t"] != "db.t_phys" {
		t.Errorf("table_rewrites=%v", resp.GetTableRewrites())
	}
	if len(resp.GetOriginalAccessedTables()) != 1 || resp.GetOriginalAccessedTables()[0].GetOriginalTable() != "t" {
		t.Errorf("accessed=%+v", resp.GetOriginalAccessedTables())
	}
}

func TestRewriteWrite_dropMultiRejected(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("DROP TABLE db.a, db.b")
	resp, handled, _ := RewriteWrite(e, ast, nil)
	if !handled || resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("code=%v handled=%v", resp.GetCode(), handled)
	}
}

func TestRewriteWrite_alterCrossTableRejected(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("ALTER TABLE db.t ATTACH PARTITION 1 FROM db.src")
	resp, _, _ := RewriteWrite(e, ast, nil)
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("code=%v", resp.GetCode())
	}
}

func TestRewriteWrite_dynamicInvalidRejected(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("DROP TABLE t") // unqualified
	opts := dynamicArgs(/* no upstream_logical_database_in_context → StatusInvalid */)
	resp, _, _ := RewriteWrite(e, ast, opts)
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
	}
}

func TestRewriteWrite_remoteRejectedForWrite(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("DROP TABLE db.t")
	opts := remoteTableMap(map[string]/*RemoteTable*/{"db.t": {Addr: "h", Database: "d", Table: "t"}})
	resp, _, _ := RewriteWrite(e, ast, opts)
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("remote write: code=%v", resp.GetCode())
	}
}

func TestRewriteWrite_passthroughNoOpts(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("UPDATE db.t SET x = 1 WHERE y = 2")
	resp, handled, _ := RewriteWrite(e, ast, nil)
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v", handled, resp.GetCode())
	}
	if !semanticSQL(t, e, resp.GetSqlAfterRewrite(), "UPDATE db.t SET x = 1 WHERE y = 2") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
}
```

- [ ] **Step 2: Run to verify failure** (undefined `RewriteWrite`).

- [ ] **Step 3: Implement the write spine + structured dispatch**

```go
// internal/handlers/writes.go
package handlers

import (
	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
)

// RewriteWrite ports handleWriteQuery. Returns (resp, handled, err):
//   handled=true  → resp is final (Success or a reject code).
//   handled=false → not a write this phase handles; caller falls through to SELECT.
//   err != nil    → unexpected/internal engine failure → native fail-opens.
//
// The `sql` (original source) is threaded from the start (used by INSERT payload
// splice in Task 9 and tier-C raw splice in Task 10; structured kinds ignore it).
//
// INCREMENTAL WIRING: Task 7 implements only the structured single-target kinds.
// The view/insert/command/database cases are added by Tasks 8/9/10 — until then
// they fall through (handled=false), which is safe (no Task-7 test exercises them).
func RewriteWrite(e engine.Engine, ast engine.AST, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, bool, error) {
	info, err := engine.InspectWrite(ast)
	if err != nil {
		return nil, false, err
	}
	sel := nameresolve.FindActive(opts)

	switch info.Kind {
	case engine.NodeCreateTable:
		return dispatchCreateTable(e, ast, info, sel)
	case engine.NodeDropTable, engine.NodeDropView, engine.NodeTruncate:
		return dispatchDropLike(e, ast, info, sel)
	case engine.NodeAlterTable:
		return dispatchAlter(e, ast, info, sel)
	case engine.NodeUpdate:
		return dispatchSingle(e, ast, info, sel, pb.StatementType_STATEMENT_TYPE_UPDATE)
	case engine.NodeDelete:
		return dispatchSingle(e, ast, info, sel, pb.StatementType_STATEMENT_TYPE_DELETE)
	// case engine.NodeCreateView:           → Task 8  (dispatchView(e, ast, info, opts, sel))
	// case engine.NodeInsert:               → Task 9  (dispatchInsert(e, ast, sql, info, sel))
	// case engine.NodeCommand:              → Task 10 (dispatchCommand(e, ast, sql, info, sel))
	// case engine.NodeCreateDB, NodeDropDB: → Task 10 (dispatchDatabaseOutOfPhase(info))
	default:
		return nil, false, nil // not handled this task → caller falls through to SELECT
	}
}

// newWriteResp seeds a Success response with an empty table_rewrites map.
func newWriteResp(stmt pb.StatementType) *pb.RewriteSQLResponse {
	return &pb.RewriteSQLResponse{
		Code: pb.RewriteCode_Success, Message: "success",
		StatementType: stmt, TableRewrites: map[string]string{},
	}
}

func rejectUnsupported(resp *pb.RewriteSQLResponse, msg string) {
	resp.Code = pb.RewriteCode_UnsupportedStatement
	resp.Message = msg
}

func rejectInvalid(resp *pb.RewriteSQLResponse, msg string) {
	resp.Code = pb.RewriteCode_InvalidRewriteRequest
	resp.Message = msg
}

// decideWriteTarget is the STRICT per-target policy (C++ rewriteOneTarget): it
// records the access + table_rewrites, and on a remote/invalid hit populates resp
// with the reject code and returns ok=false so the caller short-circuits.
func decideWriteTarget(tt engine.TableTarget, kind string, sel nameresolve.Selection, resp *pb.RewriteSQLResponse) (engine.TableDecision, bool) {
	// Record the access before any reject path (C++ recordAccessedTable, writes.cc:118).
	recordAccessedWrite(resp, tt, sel)
	o := nameresolve.Resolve(tt.DB, tt.Table, sel)
	switch o.Status {
	case nameresolve.StatusRewrite:
		recordRewrite(resp.TableRewrites, tt, o.PhysicalDB, o.NewTable)
		return engine.TableDecision{Action: engine.ActionRename, NewDB: o.PhysicalDB, NewTable: o.NewTable}, true
	case nameresolve.StatusRemote, nameresolve.StatusRemoteUnsupported:
		rejectUnsupported(resp, kind+" target maps to a remote upstream; remote() can only appear as a SELECT-side table function")
		return engine.TableDecision{}, false
	case nameresolve.StatusInvalid:
		rejectInvalid(resp, o.RejectReason)
		return engine.TableDecision{}, false
	default: // StatusPassthrough
		return engine.TableDecision{Action: engine.ActionSkip}, true
	}
}

// recordAccessedWrite appends one AccessedTable for a write target (skip when
// Table=="" — db-level ops don't populate this field). Mirrors C++
// recordAccessedTable, appending in encounter order (no dedup/sort — writes have
// 1-2 targets). Reuses nameresolve.ResolveAccessed.
func recordAccessedWrite(resp *pb.RewriteSQLResponse, tt engine.TableTarget, sel nameresolve.Selection) {
	if tt.Table == "" {
		return
	}
	a := nameresolve.ResolveAccessed(tt.DB, tt.Table, sel)
	resp.OriginalAccessedTables = append(resp.OriginalAccessedTables, &pb.AccessedTable{
		OriginalDatabase: tt.DB, OriginalTable: tt.Table,
		LogicalDatabase: a.LogicalDB, PhysicalDatabase: a.PhysicalDB, IsRemote: a.IsRemote,
	})
}

// dispatchSingle handles a one-slot structured write (UPDATE/DELETE): strict-decide
// the single target, rewrite the AST, regenerate.
func dispatchSingle(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection, stmt pb.StatementType) (*pb.RewriteSQLResponse, bool, error) {
	resp := newWriteResp(stmt)
	ok := true
	rewritten, err := engine.RewriteWriteTargets(ast, func(s engine.WriteSlot) engine.TableDecision {
		d, good := decideWriteTarget(s.Target, info.Kind, sel, resp)
		if !good {
			ok = false
		}
		return d
	})
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return resp, true, nil // reject already populated
	}
	sql, err := e.Generate(rewritten)
	if err != nil {
		return nil, false, err
	}
	resp.SqlAfterRewrite = sql
	return resp, true, nil
}
```

> **Decision-callback reject caveat:** `RewriteWriteTargets` keeps walking after a slot rejects (it has no early-exit). That is fine: once `ok=false`, the caller ignores `rewritten` and returns the populated reject `resp`. But a *later* slot must not overwrite an *earlier* reject's message. Since `decideWriteTarget` only writes a reject when `Status` is remote/invalid and the first such hit flips `ok=false`, and the response code is monotonic toward the first reject, guard it: in `decideWriteTarget`, only set the reject when `resp.Code == pb.RewriteCode_Success` (don't clobber a prior reject). Add that guard to `rejectUnsupported`/`rejectInvalid` call sites or inside the helpers.

Implement `dispatchDropLike`, `dispatchAlter`, `dispatchCreateTable`:

```go
func dispatchDropLike(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	stmt := pb.StatementType_STATEMENT_TYPE_DROP_TABLE
	kindName := "DROP TABLE"
	switch info.Kind {
	case engine.NodeDropView:
		stmt, kindName = pb.StatementType_STATEMENT_TYPE_DROP_VIEW, "DROP VIEW"
	case engine.NodeTruncate:
		stmt, kindName = pb.StatementType_STATEMENT_TYPE_TRUNCATE_TABLE, "TRUNCATE TABLE"
	}
	resp := newWriteResp(stmt)
	if info.Multi {
		rejectUnsupported(resp, "multi-table DROP/TRUNCATE is not supported")
		return resp, true, nil
	}
	_ = kindName
	return finishStructured(e, ast, info, sel, resp)
}

func dispatchAlter(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_ALTER_TABLE)
	if info.CrossTable {
		rejectUnsupported(resp, "ALTER TABLE with cross-table reference (ATTACH/REPLACE/FETCH PARTITION FROM, MOVE PARTITION TO TABLE) is not supported")
		return resp, true, nil
	}
	return finishStructured(e, ast, info, sel, resp)
}

func dispatchCreateTable(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_CREATE_TABLE)
	if info.AsTableFunction {
		rejectUnsupported(resp, "CREATE TABLE AS table_function(...) is not supported")
		return resp, true, nil
	}
	return finishStructured(e, ast, info, sel, resp)
}

// finishStructured strict-decides every slot, rewrites, regenerates. Shared by the
// structured multi-slot kinds (create_table name+clone_source, drop, alter, ...).
func finishStructured(e engine.Engine, ast engine.AST, info engine.WriteInfo, sel nameresolve.Selection, resp *pb.RewriteSQLResponse) (*pb.RewriteSQLResponse, bool, error) {
	ok := true
	rewritten, err := engine.RewriteWriteTargets(ast, func(s engine.WriteSlot) engine.TableDecision {
		d, good := decideWriteTarget(s.Target, info.Kind, sel, resp)
		if !good {
			ok = false
		}
		return d
	})
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return resp, true, nil
	}
	sql, err := e.Generate(rewritten)
	if err != nil {
		return nil, false, err
	}
	resp.SqlAfterRewrite = sql
	return resp, true, nil
}
```

> `recordRewrite` and `qualify` already exist in `handlers/select.go` — reuse them. Confirm `decideWriteTarget`'s reject helpers respect the "don't clobber prior reject" guard.

- [ ] **Step 4: Run to verify pass.** `POLYGLOT_SQL_FFI_PATH=… go test ./internal/handlers/ -run TestRewriteWrite -v` → PASS.

- [ ] **Step 5: Vet + commit**

```bash
git add internal/handlers/writes.go internal/handlers/writes_test.go
git commit -m "feat(handlers): write spine + strict decide + structured-kind dispatch"
```

---

### Task 8: Handlers — views (name + MV TO target + embedded body SELECT via the Phase-1 pipeline)

CREATE VIEW / MATERIALIZED VIEW: strict-rewrite the view name (and MV `TO` target), then run the embedded body SELECT through the **exact Phase-1 pipeline** and merge its `table_rewrites` / `original_accessed_tables` / `failed_cte_aliases` into the view response. This requires extracting an AST-returning core from `RewriteSelect`.

**Files:**
- Modify: `internal/handlers/select.go` (extract `rewriteSelectCore`)
- Modify: `internal/handlers/writes.go`, `internal/handlers/writes_test.go`

- [ ] **Step 1: Refactor — extract `rewriteSelectCore` from `RewriteSelect`**

Split `RewriteSelect` so the body pipeline (CTE inject + collect accessed + rewrite tables + options + GLOBAL) returns the rewritten **AST** plus the populated response, and `RewriteSelect` becomes a thin wrapper that also Generates:

```go
// rewriteSelectCore runs the full SELECT rewrite pipeline and returns the rewritten
// AST + a response carrying table_rewrites/original_accessed_tables/failed_cte_aliases
// (SqlAfterRewrite left empty — caller Generates or splices). Shared by RewriteSelect
// (top-level) and the view-body path.
func rewriteSelectCore(e engine.Engine, ast engine.AST, opts []*pb.RewriteOption) (engine.AST, *pb.RewriteSQLResponse, error) {
	// … body of current RewriteSelect from the seed line through ForceGlobalForRemoteAsymmetry,
	//   minus the final Generate; return (rewritten, resp, nil). …
}

func RewriteSelect(e engine.Engine, ast engine.AST, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, error) {
	rewritten, resp, err := rewriteSelectCore(e, ast, opts)
	if err != nil {
		return nil, err
	}
	sql, err := e.Generate(rewritten)
	if err != nil {
		return nil, err
	}
	resp.SqlAfterRewrite = sql
	return resp, nil
}
```

Run the existing Phase-1 SELECT tests to confirm the refactor is behavior-preserving:
`POLYGLOT_SQL_FFI_PATH=… go test ./internal/handlers/ -run 'TestRewriteSelect|TestSelect' -v` → PASS (no behavior change). Commit this refactor separately:

```bash
git add internal/handlers/select.go
git commit -m "refactor(handlers): extract rewriteSelectCore (AST-returning) for view-body reuse"
```

- [ ] **Step 2: Write failing view tests**

```go
func TestRewriteWrite_createViewBodyRewritten(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("CREATE VIEW db.v AS SELECT * FROM db.s")
	opts := staticTableMap(map[string]string{"db.v": "v_phys", "db.s": "s_phys"})
	resp, handled, err := RewriteWrite(e, ast, opts)
	if err != nil || !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v err=%v", handled, resp.GetCode(), err)
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_VIEW {
		t.Errorf("stmt=%v", resp.GetStatementType())
	}
	if !semanticSQL(t, e, resp.GetSqlAfterRewrite(), "CREATE VIEW db.v_phys AS SELECT * FROM db.s_phys") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
	// Both the view name and the body table appear in table_rewrites.
	if resp.GetTableRewrites()["db.v"] != "db.v_phys" || resp.GetTableRewrites()["db.s"] != "db.s_phys" {
		t.Errorf("table_rewrites=%v", resp.GetTableRewrites())
	}
}

func TestRewriteWrite_createMVToTarget(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("CREATE MATERIALIZED VIEW db.mv TO db.dst AS SELECT * FROM db.s")
	opts := staticTableMap(map[string]string{"db.mv": "mv2", "db.dst": "dst2", "db.s": "s2"})
	resp, _, _ := RewriteWrite(e, ast, opts)
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_MATERIALIZED_VIEW {
		t.Errorf("stmt=%v", resp.GetStatementType())
	}
	if !semanticSQL(t, e, resp.GetSqlAfterRewrite(), "CREATE MATERIALIZED VIEW db.mv2 TO db.dst2 AS SELECT * FROM db.s2") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
}
```

- [ ] **Step 3: Implement `dispatchView`**

```go
func dispatchView(e engine.Engine, ast engine.AST, info engine.WriteInfo, opts []*pb.RewriteOption, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	stmt := pb.StatementType_STATEMENT_TYPE_CREATE_VIEW
	if info.Materialized {
		stmt = pb.StatementType_STATEMENT_TYPE_CREATE_MATERIALIZED_VIEW
	}
	resp := newWriteResp(stmt)

	// 1+2. View name + MV TO target — strict single-target rewrites on the AST.
	ok := true
	rewritten, err := engine.RewriteWriteTargets(ast, func(s engine.WriteSlot) engine.TableDecision {
		d, good := decideWriteTarget(s.Target, "CREATE VIEW", sel, resp)
		if !good {
			ok = false
		}
		return d
	})
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return resp, true, nil
	}

	// 3. Body SELECT — full Phase-1 pipeline; merge its bookkeeping into resp.
	if info.HasViewBody {
		body, has, err := engine.ExtractViewBody(rewritten)
		if err != nil {
			return nil, false, err
		}
		if has {
			newBody, bodyResp, err := rewriteSelectCore(e, body, opts)
			if err != nil {
				return nil, false, err
			}
			mergeViewBody(resp, bodyResp)
			if rewritten, err = engine.SetViewBody(rewritten, newBody); err != nil {
				return nil, false, err
			}
		}
	}

	sql, err := e.Generate(rewritten)
	if err != nil {
		return nil, false, err
	}
	resp.SqlAfterRewrite = sql
	return resp, true, nil
}

// mergeViewBody folds the body SELECT's bookkeeping into the view response:
// table_rewrites (union), original_accessed_tables (append), failed_cte_aliases
// (append). Mirrors C++ rewriteEmbeddedViewBody merging into the same response.
func mergeViewBody(dst, body *pb.RewriteSQLResponse) {
	for k, v := range body.GetTableRewrites() {
		dst.TableRewrites[k] = v
	}
	dst.OriginalAccessedTables = append(dst.OriginalAccessedTables, body.GetOriginalAccessedTables()...)
	dst.FailedCteAliases = append(dst.FailedCteAliases, body.GetFailedCteAliases()...)
}
```

> **Parity note:** confirm against `select.cc`/`writes.cc` whether the view name's accessed-table entry precedes or follows the body's entries, and whether the body uses the same `sel`/options. C++ `handleViewCreate` rewrites name+TO first (via `rewriteOneTarget`, which records accessed), then the body (which records its own). This ordering (name, then TO, then body tables) is what the append sequence above produces. The Task-12 differential corpus validates it.

- [ ] **Step 4: Run to verify pass.**

- [ ] **Step 5: Vet + commit**

```bash
git add internal/handlers/writes.go internal/handlers/writes_test.go
git commit -m "feat(handlers): CREATE VIEW/MATERIALIZED VIEW — name/TO + body pipeline"
```

---

### Task 9: Handlers — INSERT dispatch (+ payload splice)

INSERT: reject `INSERT INTO FUNCTION(...)` and missing-table, strict-rewrite the `.table` target, then build the final SQL via `engine.GenerateInsert` (preserves a FORMAT payload tail). The embedded `.query` SELECT (INSERT…SELECT) is **left untouched**, matching C++.

**Files:**
- Modify: `internal/handlers/writes.go`, `internal/handlers/writes_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestRewriteWrite_insertValues(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("INSERT INTO db.t (x) VALUES (1)")
	opts := staticTableMap(map[string]string{"db.t": "t_phys"})
	resp, handled, err := RewriteWrite(e, ast, opts)
	if err != nil || !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v err=%v", handled, resp.GetCode(), err)
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_INSERT {
		t.Errorf("stmt=%v", resp.GetStatementType())
	}
	if !semanticSQL(t, e, resp.GetSqlAfterRewrite(), "INSERT INTO db.t_phys (x) VALUES (1)") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
}

func TestRewriteWrite_insertFormatPreservesPayload(t *testing.T) {
	e := newTestEngine(t)
	orig := "INSERT INTO db.t FORMAT JSONEachRow {\"x\":1}\n{\"x\":2}"
	ast, _ := e.ParseOne(orig)
	opts := staticTableMap(map[string]string{"db.t": "t_phys"})
	resp, _, _ := RewriteWrite(e, ast, opts)
	got := resp.GetSqlAfterRewrite()
	if !strings.HasPrefix(got, "INSERT INTO db.t_phys FORMAT JSONEachRow") || !strings.Contains(got, "{\"x\":1}\n{\"x\":2}") {
		t.Errorf("payload not preserved: %q", got)
	}
}

func TestRewriteWrite_insertSelectSourceUntouched(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("INSERT INTO db.t SELECT * FROM db.s")
	opts := staticTableMap(map[string]string{"db.t": "t_phys", "db.s": "s_phys"})
	resp, _, _ := RewriteWrite(e, ast, opts)
	// Only the INSERT target is rewritten; the embedded SELECT source stays db.s (C++ parity).
	if !semanticSQL(t, e, resp.GetSqlAfterRewrite(), "INSERT INTO db.t_phys SELECT * FROM db.s") {
		t.Errorf("sql=%q (embedded SELECT must be untouched)", resp.GetSqlAfterRewrite())
	}
	if _, hit := resp.GetTableRewrites()["db.s"]; hit {
		t.Errorf("embedded SELECT source must not be in table_rewrites")
	}
}

func TestRewriteWrite_insertFunctionRejected(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("INSERT INTO FUNCTION remote('h', d, t) VALUES (1)")
	resp, _, _ := RewriteWrite(e, ast, nil)
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("code=%v", resp.GetCode())
	}
}
```

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement `dispatchInsert`** — note it needs the original SQL. `RewriteWrite` has the AST, not the source string. Thread the original SQL through: change `RewriteWrite`'s signature to accept `sql string` (native.go already has it), or have the engine expose `Generate`-from-original. Simplest: add `sql string` param to `RewriteWrite` and pass to `dispatchInsert`/`dispatchCommand`.

Update `RewriteWrite(e, ast, opts)` → `RewriteWrite(e, ast, sql, opts)` and thread `sql` to the INSERT and command dispatchers (the structured kinds ignore it).

```go
func dispatchInsert(e engine.Engine, ast engine.AST, sql string, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_INSERT)
	if info.AsTableFunction {
		rejectUnsupported(resp, "INSERT INTO FUNCTION(...) is not supported")
		return resp, true, nil
	}
	if info.MissingTable {
		rejectUnsupported(resp, "INSERT target table is missing")
		return resp, true, nil
	}
	ok := true
	rewritten, err := engine.RewriteWriteTargets(ast, func(s engine.WriteSlot) engine.TableDecision {
		d, good := decideWriteTarget(s.Target, "INSERT", sel, resp)
		if !good {
			ok = false
		}
		return d
	})
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return resp, true, nil
	}
	out, err := engine.GenerateInsert(e, sql, rewritten)
	if err != nil {
		return nil, false, err
	}
	resp.SqlAfterRewrite = out
	return resp, true, nil
}
```

- [ ] **Step 4: Run to verify pass.**

- [ ] **Step 5: Vet + commit**

```bash
git add internal/handlers/writes.go internal/handlers/writes_test.go
git commit -m "feat(handlers): INSERT dispatch with FORMAT-payload splice"
```

---

### Task 10: Handlers — tier-C raw (RENAME/EXCHANGE/ALTER…UPDATE), bare-rejects, and db-level out-of-phase

Dispatch `command` nodes: strict-rewrite the raw table refs of RENAME/EXCHANGE/ALTER…UPDATE via `engine.RawTableRefs` + `engine.SpliceRawTables`; bare-reject OPTIMIZE/UNDROP/MOVE/BACKUP/RESTORE/KILL; pass through non-write commands (USE/SHOW/GRANT/EXISTS → `handled=false`). Also reject no-table CREATE/DROP DATABASE as out-of-phase (Phase 3 replaces this).

**Files:**
- Modify: `internal/handlers/writes.go`, `internal/handlers/writes_test.go`

- [ ] **Step 1: Write failing tests**

```go
func TestRewriteWrite_renameStrictRewrite(t *testing.T) {
	e := newTestEngine(t)
	ast, _ := e.ParseOne("RENAME TABLE db.a TO db.b")
	opts := staticTableMap(map[string]string{"db.a": "a_phys", "db.b": "b_phys"})
	resp, handled, err := RewriteWrite(e, ast, "RENAME TABLE db.a TO db.b", opts)
	if err != nil || !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v err=%v", handled, resp.GetCode(), err)
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_RENAME_TABLE {
		t.Errorf("stmt=%v", resp.GetStatementType())
	}
	if !semanticSQL(t, e, resp.GetSqlAfterRewrite(), "RENAME TABLE db.a_phys TO db.b_phys") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
	if len(resp.GetOriginalAccessedTables()) != 2 {
		t.Errorf("accessed=%+v want 2", resp.GetOriginalAccessedTables())
	}
}

func TestRewriteWrite_alterUpdateRaw(t *testing.T) {
	e := newTestEngine(t)
	const s = "ALTER TABLE db.t UPDATE x = 1 WHERE y = 2"
	ast, _ := e.ParseOne(s)
	opts := staticTableMap(map[string]string{"db.t": "t_phys"})
	resp, _, _ := RewriteWrite(e, ast, s, opts)
	if !semanticSQL(t, e, resp.GetSqlAfterRewrite(), "ALTER TABLE db.t_phys UPDATE x = 1 WHERE y = 2") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
}

func TestRewriteWrite_optimizeBareReject(t *testing.T) {
	e := newTestEngine(t)
	const s = "OPTIMIZE TABLE db.t"
	ast, _ := e.ParseOne(s)
	resp, handled, _ := RewriteWrite(e, ast, s, nil)
	if !handled || resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("handled=%v code=%v", handled, resp.GetCode())
	}
}

func TestRewriteWrite_useNotAWrite(t *testing.T) {
	e := newTestEngine(t)
	const s = "USE db"
	ast, _ := e.ParseOne(s)
	_, handled, _ := RewriteWrite(e, ast, s, nil)
	if handled {
		t.Errorf("USE must not be handled by RewriteWrite (handled=true)")
	}
}

func TestRewriteWrite_createDatabaseOutOfPhase(t *testing.T) {
	e := newTestEngine(t)
	const s = "CREATE DATABASE db"
	ast, _ := e.ParseOne(s)
	resp, handled, _ := RewriteWrite(e, ast, s, nil)
	if !handled || resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("handled=%v code=%v", handled, resp.GetCode())
	}
}
```

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement `dispatchCommand` + `dispatchDatabaseOutOfPhase`**

```go
func dispatchCommand(e engine.Engine, ast engine.AST, sql string, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	switch info.Sub {
	case engine.CmdRename, engine.CmdExchange, engine.CmdAlterUpdate:
		return dispatchRawTables(e, ast, sql, info, sel)
	case engine.CmdBareReject:
		resp := newWriteResp(pb.StatementType_STATEMENT_TYPE_UNSPECIFIED)
		rejectUnsupported(resp, "statement is not supported")
		return resp, true, nil
	default: // CmdNone: USE/SHOW/GRANT/REVOKE/EXISTS — not a write this phase handles
		return nil, false, nil
	}
}

func dispatchRawTables(e engine.Engine, ast engine.AST, sql string, info engine.WriteInfo, sel nameresolve.Selection) (*pb.RewriteSQLResponse, bool, error) {
	stmt := pb.StatementType_STATEMENT_TYPE_RENAME_TABLE
	kind := "RENAME TABLE"
	if info.Sub == engine.CmdExchange {
		kind = "EXCHANGE TABLES"
	} else if info.Sub == engine.CmdAlterUpdate {
		stmt, kind = pb.StatementType_STATEMENT_TYPE_ALTER_TABLE, "ALTER TABLE"
	}
	resp := newWriteResp(stmt)

	targets, _, err := engine.RawTableRefs(e, ast)
	if err != nil {
		return nil, false, err
	}
	// Strict-decide each target (records accessed + table_rewrites), build the
	// splice map of qualify(orig) → quoted new qualified name. Reject short-circuits.
	rewrites := map[string]string{}
	for _, tt := range targets {
		recordAccessedWrite(resp, tt, sel)
		o := nameresolve.Resolve(tt.DB, tt.Table, sel)
		switch o.Status {
		case nameresolve.StatusRewrite:
			recordRewrite(resp.TableRewrites, tt, o.PhysicalDB, o.NewTable)
			rewrites[qualify(tt.DB, tt.Table)] = engine.QuoteQualified(o.PhysicalDB, o.NewTable)
		case nameresolve.StatusRemote, nameresolve.StatusRemoteUnsupported:
			rejectUnsupported(resp, kind+" target maps to a remote upstream; remote() can only appear as a SELECT-side table function")
			return resp, true, nil
		case nameresolve.StatusInvalid:
			rejectInvalid(resp, o.RejectReason)
			return resp, true, nil
		}
	}
	out, err := engine.SpliceRawTables(e, sql, rewrites)
	if err != nil {
		return nil, false, err
	}
	resp.SqlAfterRewrite = out
	return resp, true, nil
}

// dispatchDatabaseOutOfPhase rejects CREATE/DROP DATABASE as not-yet-supported.
// Phase 3 replaces this with the synthetic-SELECT debug rewrite + db_map validation.
func dispatchDatabaseOutOfPhase(info engine.WriteInfo) (*pb.RewriteSQLResponse, bool, error) {
	stmt := pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE
	if info.Kind == engine.NodeDropDB {
		stmt = pb.StatementType_STATEMENT_TYPE_DROP_DATABASE
	}
	resp := newWriteResp(stmt)
	rejectUnsupported(resp, "database-level DDL (CREATE/DROP DATABASE) is handled in Phase 3")
	return resp, true, nil
}
```

> **Note:** the bare-reject message is intentionally generic; the C++ emits per-kind messages (OPTIMIZE/UNDROP/MOVE/…). If the differential corpus compares `message` exactly for these (it does — §7 lists `code` exact but `message` is not in the exact list; verify), keep generic. The `code` (UnsupportedStatement) is what's compared exact.

- [ ] **Step 4: Run to verify pass.**

- [ ] **Step 5: Vet + commit**

```bash
git add internal/handlers/writes.go internal/handlers/writes_test.go
git commit -m "feat(handlers): tier-C raw (rename/exchange/alter-update) + bare-reject + db out-of-phase"
```

---

### Task 11: Native routing — dispatch writes before SELECT/pass-through

Wire `handlers.RewriteWrite` into `NativeRewriter.Rewrite`: after parse+classify, try the write handler; if `handled`, return its response; if `err`, fail-open (Go error); otherwise fall through to the existing SELECT/pass-through logic.

**Files:**
- Modify: `native.go`, `native_test.go`

- [ ] **Step 1: Write failing tests**

```go
// native_test.go
func TestNativeRewrite_dropTableRouted(t *testing.T) {
	e := newTestEngine(t)
	r := New(e, WithOptions(func(string) []*pb.RewriteOption {
		return staticTableMap(map[string]string{"db.t": "t_phys"})
	}))
	defer r.Close()
	res, err := r.Rewrite(context.Background(), "DROP TABLE db.t", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success || res.StatementType != pb.StatementType_STATEMENT_TYPE_DROP_TABLE {
		t.Fatalf("code=%v stmt=%v", res.Code, res.StatementType)
	}
	if !semanticSQL(t, e, res.SQL, "DROP TABLE db.t_phys") {
		t.Errorf("sql=%q", res.SQL)
	}
}

func TestNativeRewrite_selectStillWorks(t *testing.T) {
	e := newTestEngine(t)
	r := New(e)
	defer r.Close()
	res, err := r.Rewrite(context.Background(), "SELECT 1", "acct")
	if err != nil || res.Code != pb.RewriteCode_Success {
		t.Fatalf("SELECT regressed: code=%v err=%v", res.Code, err)
	}
}

func TestNativeRewrite_unsupportedWriteSurfacesCode(t *testing.T) {
	e := newTestEngine(t)
	r := New(e)
	defer r.Close()
	res, _ := r.Rewrite(context.Background(), "DROP TABLE a, b", "acct")
	if res.Code != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("code=%v", res.Code)
	}
}
```

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement routing in `native.go`**

Replace the Phase-1 SELECT-only branch with write-first dispatch:

```go
	resp.StatementType = r.classify(ast)

	// Phase 2: route writes (CREATE/DROP/ALTER/INSERT/UPDATE/DELETE/RENAME/views) first.
	var opts []*pb.RewriteOption
	if r.options != nil {
		opts = r.options(account)
	}
	if wresp, handled, werr := handlers.RewriteWrite(r.engine, ast, sql, opts); werr != nil {
		return RewriteResult{}, werr // unexpected/internal → fail-open
	} else if handled {
		r.mu.Lock()
		r.last = &callContext{sql: sql, account: account}
		r.mu.Unlock()
		return resultFromPB(wresp), nil
	}

	// Phase 1: SELECT.
	if kind, _ := engine.NodeKind(ast); kind == engine.NodeSelect {
		hresp, herr := handlers.RewriteSelect(r.engine, ast, opts)
		// … unchanged …
	}
	// … pass-through unchanged …
```

> Note `RewriteWrite` now takes `sql`. Confirm the `opts` are computed once and shared by both the write and SELECT paths (they are — same account policy).

- [ ] **Step 4: Run to verify pass.** Full suite: `POLYGLOT_SQL_FFI_PATH=… go test ./...` → PASS.

- [ ] **Step 5: Vet + commit**

```bash
git add native.go native_test.go
git commit -m "feat(native): route writes through RewriteWrite before SELECT/pass-through"
```

---

### Task 12: Differential harness — writes golden corpus + oracle gate

Mirror `internal/harness/select_golden_test.go`: a `writes_cases.json` corpus (structured-exact + sql-semantic) plus `TestWritesGolden` and the env-gated `REWRITER_ORACLE_ADDR` differential. This is the Phase-2 parity gate.

**Files:**
- Create: `internal/harness/writes_cases.json`, `internal/harness/writes_golden_test.go`

- [ ] **Step 1: Read the Phase-1 harness**

Read `internal/harness/select_golden_test.go` + `select_cases.json` for the exact corpus schema (`{sql, opts, want_code, want_stmt, want_sql, want_table_rewrites, want_accessed, …}`), the `semanticSQLEq` helper, and the `DialOracle`/`Compare` gate. Match them.

- [ ] **Step 2: Author the writes corpus**

Cover at least: CREATE TABLE (static rename), CREATE TABLE AS (both targets), CREATE TABLE IF NOT EXISTS, CREATE TABLE AS table_function (reject), DROP TABLE, DROP TABLE IF EXISTS, DROP TABLE multi (reject), DROP VIEW, TRUNCATE, ALTER TABLE ADD, ALTER cross-table (reject), INSERT VALUES, INSERT FORMAT (payload preserved), INSERT…SELECT (source untouched), INSERT INTO FUNCTION (reject), UPDATE, DELETE, RENAME (single+multi), EXCHANGE, ALTER…UPDATE, CREATE VIEW (body rewritten), CREATE MATERIALIZED VIEW TO, OPTIMIZE (reject), CREATE DATABASE (out-of-phase reject), a dynamic-mode rename, a dynamic-invalid reject, and a remote-hit-on-write reject.

Each case encodes our engine's output as `want_*` (the true oracle gate runs via `REWRITER_ORACLE_ADDR`). Use `semanticSQLEq` for `want_sql`, exact-string for the INSERT-FORMAT payload tail, exact for `code`/`stmt`/`table_rewrites`/`accessed`.

- [ ] **Step 3: Implement `TestWritesGolden`** mirroring `TestSelectGolden` — drive each case through a `New(e, WithOptions(...))` rewriter (or directly `handlers.RewriteWrite`), compare structured fields exactly and `sql` semantically; when `REWRITER_ORACLE_ADDR` is set, also `Compare` against the live C++ oracle.

- [ ] **Step 4: Run** with and without engine + (if available) with the oracle:
`POLYGLOT_SQL_FFI_PATH=… go test ./internal/harness/ -run TestWritesGolden -v` → PASS.
`go test ./internal/harness/` (no engine) → PASS (gated).

- [ ] **Step 5: Vet + commit**

```bash
git add internal/harness/writes_cases.json internal/harness/writes_golden_test.go
git commit -m "test(harness): writes golden corpus + C++ oracle differential (Phase 2 gate)"
```

---

## Self-Review

**Spec coverage (writes.cc → tasks):**
- CREATE TABLE (+AS source, IF NOT EXISTS, AS table_function reject) → T3, T7. ✓
- DROP TABLE / DROP VIEW / TRUNCATE (+IF EXISTS, multi reject, view/materialized) → T2, T7. ✓
- ALTER TABLE (+cross-table reject) → T3, T7. ✓
- INSERT (+VALUES kept, FORMAT splice, INTO FUNCTION reject, missing-table reject, embedded SELECT untouched) → T5, T9. ✓
- UPDATE / DELETE → T2, T7. ✓
- RENAME / EXCHANGE / ALTER…UPDATE (tier-C raw splice) → T6, T10. ✓
- CREATE VIEW / MATERIALIZED VIEW (name + TO + body pipeline) → T4, T8. ✓
- Bare-rejects (OPTIMIZE/UNDROP/MOVE/BACKUP/RESTORE/KILL) → T6, T10. ✓
- Strict decide (remote→Unsupported, invalid→InvalidRequest, passthrough) → T7 `decideWriteTarget`. ✓
- `table_rewrites` / `original_accessed_tables` population → T7 `recordRewrite`/`recordAccessedWrite`. ✓
- Native routing + fail-open → T11. ✓
- Differential gate → T12. ✓
- CREATE/DROP DATABASE → **deferred to Phase 3** (out-of-phase reject, T10) per §9. ✓ (intentional scope boundary)

**Type consistency:** `WriteRole`/`WriteSlot`/`WriteInfo`/`CommandSub` defined once (T2), extended (not redefined) in T3-T6. `InspectWrite`/`RewriteWriteTargets`/`RawTableRefs`/`SpliceRawTables`/`GenerateInsert`/`ExtractViewBody`/`SetViewBody`/`QuoteQualified` engine API used consistently by handlers. `decideWriteTarget` returns `(engine.TableDecision, bool)` everywhere. `RewriteWrite` signature settles at `(e, ast, sql, opts) → (*resp, handled, err)` after T9 adds `sql` — **T7 introduces it already taking `sql`** to avoid a mid-plan signature churn (adjust T7's `RewriteWrite` to include the `sql string` param from the start, threaded but unused until T9).

**Placeholder scan:** reject messages are concrete; the only deliberately-deferred specifics are (a) Phase-1 test-helper names (`newTestEngine`/`semanticEq`/`semanticSQL`/`staticTableMap`) — the implementer reuses the actual Phase-1 helpers, named in each task's "read first" step; (b) the `INSERT INTO FUNCTION` shape + tokenizer type strings for quoted idents — flagged as **verify-then-implement** sub-steps (the Phase-1 pattern for empirically-confirmed shapes). No `TODO`/`TBD`/"add error handling" placeholders.

**Open parity risks (validated by T12 against the oracle):**
1. `rawActionIsCrossTable` keyword heuristic (ALTER cross-table) — corpus must include all four partition forms.
2. INSERT `FORMAT Values (tuples)` mangle — documented allow-listed divergence, not special-cased in v1.
3. Tier-C raw splice with backtick-quoted identifiers — `scanTableRefs` `isName` token-type set must be confirmed against the live tokenizer.
4. Bare-reject `message` text differs from C++ per-kind messages — only `code` is compared exact (§7).

---

## Execution Handoff

Plan saved to `docs/superpowers/plans/2026-06-08-native-go-rewriter-phase-2-writes.md`. Per the user's choice (one PR for all of Phase 2, subagent-per-task execution): use **superpowers:subagent-driven-development** — fresh implementer subagent per task + two-stage review (spec compliance, then code quality) — then **superpowers:finishing-a-development-branch** (push + create PR).
