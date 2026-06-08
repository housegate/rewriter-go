# Native Go SQL Rewriter — Phase 1 (SELECT) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the Phase-0 pass-through for `SELECT` statements with a full behavioral port of the C++ `handleSelectQuery` path: name resolution (static 3-map + dynamic), table rewriting (incl. `remote()`), the CTE/option pipeline, the GLOBAL cross-shard pass, and full response population — all gated by the differential oracle harness.

**Architecture:** Three layers. (1) `internal/engine` grows a **pure-Go AST node layer** (`nodes.go`, `global.go`) that knows polyglot's JSON shape and exposes typed walk/mutate/global primitives — the only code that understands the AST. (2) `internal/nameresolve` is the **policy layer** (logical→physical `(db,table)` resolution), shared by every future handler, with **zero** polyglot knowledge — it consumes `pb.*` and returns plain outcomes. (3) `internal/handlers` orchestrates: it drives the engine walk with a `nameresolve`-backed decision callback, runs the option pipeline, calls the GLOBAL pass, and assembles `pb.RewriteSQLResponse`. `native.go` routes `select`-kind ASTs to the handler and keeps pass-through for everything else (Phases 2–5).

**Tech Stack:** Go 1.x, `encoding/json` over `json.RawMessage` (in-place mutation via decode-to-`map[string]any`), the `polyglot` ClickHouse FFI (Parse/Generate only), generated `gen/pb` contract types, the existing `internal/harness` differential comparator.

**Ground truth — the C++ oracle & research reports.** Every behavior below is sourced from `rewriter-grpc/src/handlers/select.cc`, `name_rewrite.{cc,h}`, `ASTTransformers.{cc,h}`, and the polyglot AST shapes characterized 2026-06-08. Where this plan deliberately diverges from the C++ control flow (e.g. CTE-inject-then-single-walk vs. C++'s erase-and-reapply), it is called out and the differential harness (Task 11) is the arbiter.

---

## Key polyglot AST facts (locked 2026-06-08)

All confirmed empirically (see Task 1 golden files). Paths are JSON paths under the single top-level `select` key.

- **Table identifier** `db.t`: `…table.name.name = "t"`, `…table.schema.name = "db"` (`schema` is **null** when unqualified), `…table.alias.name` (null when none). DB and table are **independent, nullable** sibling objects. To set a db on an unqualified table, replace `schema: null` with `{"name":"db","quoted":false,"trailing_comments":[]}`.
- **FROM tables:** `select.from.expressions[i].table`. **Joined tables:** `select.joins[i].this.table` (these can carry `schema`). **FROM subquery:** `select.from.expressions[i].subquery.this.select…` (recurse). **Table function** (e.g. `remote(...)`, `numbers(...)`): `select.from.expressions[i].function` (no `table` key).
- **CTE:** `select.with.ctes[i].alias.name` (the alias) and `select.with.ctes[i].this.select…` (the body — recurse). `with` is null when absent.
- **IN node:** `{"in":{"this":<expr>,"expressions":[…literals],"query":<subselect|null>,"not":<bool>,"global":<bool, absent⇒false>}}`. Setting `global:true` regenerates `GLOBAL IN`. `not:true` ⇒ `NOT IN`.
- **GLOBAL JOIN:** polyglot has **no** join-locality field. `a GLOBAL JOIN b` parses as `from.expressions[0].table.alias.name = "GLOBAL"` (with `alias_explicit_as:false`) and `joins[0].kind:"Inner"`. **Injecting** `alias={"name":"GLOBAL",...}` onto a join's left table (when it has no real alias) regenerates `… GLOBAL JOIN …`. A real alias (`alias_explicit_as:true`, or any other name) regenerates as `AS x`. This quirk is how the GLOBAL pass introduces join locality (Task 10).
- **LIMIT/OFFSET:** `select.limit.this.literal.value = "10"`, `select.offset.this.literal.value = "5"` (string-encoded numbers). **SETTINGS:** `select.settings` = array of `{"eq":{"left":{"column":{"name":{"name":"k"}}},"right":{"literal":{"literal_type":…,"value":…}}}}`.
- **remote() table fn:** `{"function":{"name":"remote","args":[{literal|column}…],"distinct":false,"trailing_comments":[],"use_bracket_syntax":false,"no_parens":false,"quoted":false}}`. Args render verbatim: string args as `'x'` literals, bare identifiers (db/table) as `column` nodes.

---

## File Structure

**Create:**
- `internal/engine/nodes.go` — typed node structs + table walk/mutate (`TableTarget`, `TableDecision`, `CollectSelectTables`, `RewriteSelectTables`, `InjectCTEs`, `SetLimit`/`GetLimit`/`SetOffset`/`SetSettings`). Pure Go, no FFI.
- `internal/engine/nodes_test.go`
- `internal/engine/global.go` — `ForceGlobalForRemoteAsymmetry` + `subtreeHasRemoteSource`/`subtreeHasLocalSource`. Pure Go.
- `internal/engine/global_test.go`
- `internal/nameresolve/resolve.go` — `Selection`/`Mode`/`Outcome`/`Accessed`/`Status`, `FindActive`, `LookupStatic`, `ApplyDynamic`, `Resolve`, `ResolveAccessed`, helpers (`qualify`, `resolvePhysicalDatabase`, `buildDynamicTableName`).
- `internal/nameresolve/resolve_test.go`
- `internal/handlers/select.go` — `RewriteSelect(e, ast, opts)` orchestration + decision callback + response assembly.
- `internal/handlers/options.go` — option pipeline (`applyOptions`, pending Limit/Offset/Settings + flush).
- `internal/handlers/select_test.go`, `internal/handlers/options_test.go`

**Modify:**
- `internal/engine/testdata/ast-shapes/` — add golden files for the new shapes (Task 1).
- `internal/engine/characterize_test.go` — add the new characterization cases.
- `native.go` — add injected `options` func; route `select`-kind to `handlers.RewriteSelect`; keep pass-through otherwise.
- `native_test.go` — adjust `New` calls if needed (functional options keep them compiling).
- `internal/harness/` — Task 11 golden + differential SELECT corpus.

**Boundary rules (enforced by `go vet` + review):** `internal/nameresolve` MUST NOT import `internal/engine` or `polyglot`. `internal/engine` MUST NOT import `internal/handlers` or `internal/nameresolve`. Only `internal/engine` imports the polyglot SDK.

---

## Canonical shared types (defined once; referenced verbatim by later tasks)

**`internal/engine/nodes.go`:**

```go
// TableTarget is the read view of one real table reference in a SELECT AST.
type TableTarget struct {
	DB    string // schema.name.name; "" if unqualified
	Table string // name.name
	Alias string // alias.name; "" if none
}

// TableAction is the rewrite a caller chose for a TableTarget.
type TableAction int

const (
	ActionSkip   TableAction = iota // leave the node untouched
	ActionRename                    // set table name (+ optionally schema/db)
	ActionRemote                    // replace the table expr with remote(...)
)

// RemoteSpec are the five positional args of a remote() table function.
type RemoteSpec struct{ Addr, DB, Table, User, Password string }

// TableDecision is what a caller returns for a TableTarget.
type TableDecision struct {
	Action   TableAction
	NewDB    string      // ActionRename: new schema; "" keeps the existing schema untouched
	NewTable string      // ActionRename: new table name
	Remote   *RemoteSpec // ActionRemote: the remote() args
}
```

**`internal/nameresolve/resolve.go`:**

```go
type Mode int

const (
	ModeNone    Mode = iota // no TableNameRewrite option had either sub-message
	ModeStatic              // static_args present (even if all maps empty)
	ModeDynamic             // dynamic_args present, static_args absent
)

// Selection is the active rewrite policy for one statement (last-wins across options).
type Selection struct {
	Mode    Mode
	Static  *pb.RewriteTableStaticArgs
	Dynamic *pb.RewriteTableDynamicArgs
}

type Status int

const (
	StatusPassthrough       Status = iota // no match; leave as written (no error)
	StatusRewrite                         // set physical_db.new_table
	StatusRemote                          // wrap in remote(addr, physical_db, new_table, user, pw)
	StatusRemoteUnsupported               // remote hit on a context that forbids it (non-SELECT)
	StatusInvalid                         // caller policy/request is wrong → InvalidRewriteRequest
)

// Outcome is the result of resolving one (db, table) target. Mirrors the C++
// DynamicRewriteOutcome + the static lookup result, unified.
type Outcome struct {
	Status         Status
	PhysicalDB     string // schema to set on the node (Rewrite/Remote)
	NewTable       string // table name to set (Rewrite/Remote); "" for db-only (USE)
	LogicalDB      string // for table_rewrites / accessed bookkeeping
	RemoteAddr     string
	RemoteUser     string
	RemotePassword string
	RejectReason   string // Status==StatusInvalid only
}

// Accessed is the best-effort resolution used to populate original_accessed_tables.
type Accessed struct {
	LogicalDB  string
	PhysicalDB string
	IsRemote   bool
}
```

---

## Task 1: Lock the new AST shapes as golden contracts

Phase 0 only characterized 12 shapes. Phase 1 depends on a dozen more (CTE, options, subqueries, IN-family, GLOBAL synthesis). Lock them so every later task asserts against frozen JSON, and so a polyglot version bump that changes a shape fails loudly here.

**Files:**
- Modify: `internal/engine/characterize_test.go:14-27` (add cases)
- Create (generated by the test): `internal/engine/testdata/ast-shapes/{select_in_list,select_in_subquery,select_global_in,select_not_in,select_cte,select_cte_join,select_subquery_from,select_limit,select_offset,select_settings,select_remote_fn,select_three_join}.json`

- [ ] **Step 1: Add the new characterization cases**

In `internal/engine/characterize_test.go`, extend the `characterizeCases` map (keep the existing 12) with:

```go
	"select_in_list":      "SELECT x FROM db.t WHERE y IN (1, 2)",
	"select_in_subquery":  "SELECT x FROM db.t WHERE y IN (SELECT z FROM db.u)",
	"select_global_in":    "SELECT x FROM db.t WHERE y GLOBAL IN (SELECT z FROM db.u)",
	"select_not_in":       "SELECT x FROM db.t WHERE y NOT IN (1, 2)",
	"select_cte":          "WITH c AS (SELECT 1) SELECT * FROM c",
	"select_cte_join":     "WITH c AS (SELECT * FROM db.t) SELECT * FROM c JOIN db.u ON c.x = db.u.x",
	"select_subquery_from": "SELECT * FROM (SELECT a FROM db.t) sub",
	"select_limit":        "SELECT a FROM db.t LIMIT 10",
	"select_offset":       "SELECT a FROM db.t LIMIT 10 OFFSET 5",
	"select_settings":     "SELECT a FROM db.t SETTINGS max_threads = 4",
	"select_remote_fn":    "SELECT * FROM remote('addr', db, t, 'u', 'p')",
	"select_three_join":   "SELECT * FROM a JOIN b ON a.x = b.x JOIN c ON b.y = c.y",
```

- [ ] **Step 2: Generate the golden files**

Run: `make ffi >/dev/null 2>&1; POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine -run TestCharacterizeAST -v`
Expected: PASS; 24 `wrote testdata/ast-shapes/*.json` log lines. New files appear under `internal/engine/testdata/ast-shapes/`.

- [ ] **Step 3: Spot-check the two load-bearing shapes**

Confirm by reading the files:
- `select_global_in.json` contains `"global": true` inside the `in` node.
- `select_cte.json` contains `select.with.ctes[0].alias.name == "c"`.
Expected: both present. (If `global` is absent or named differently, STOP — the GLOBAL pass design in Task 10 depends on it.)

- [ ] **Step 4: Commit**

```bash
git add internal/engine/characterize_test.go internal/engine/testdata/ast-shapes/
git commit -m "test(engine): characterize Phase-1 SELECT AST shapes (CTE, IN-family, options, GLOBAL)"
```

---

## Task 2: Engine node layer — read-only table walk

Port `collectAccessedTablePairsFromAST` + `collectInlineCTEAliases` (select.cc:40-113). Walk every real table reference in document order, recursing into JOINs, FROM-subqueries, and CTE bodies, while tracking the CTE-alias scope so a bare reference matching an in-scope alias is **not** reported.

**Files:**
- Create: `internal/engine/nodes.go`
- Test: `internal/engine/nodes_test.go`

- [ ] **Step 1: Write the failing test**

```go
package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func load(t *testing.T, name string) AST {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ast-shapes", name+".json"))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return AST(b)
}

func TestCollectSelectTables_simpleQualified(t *testing.T) {
	got, err := CollectSelectTables(load(t, "select"))
	if err != nil {
		t.Fatal(err)
	}
	want := []TableTarget{{DB: "db", Table: "t"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestCollectSelectTables_joinAndSubquery(t *testing.T) {
	got, err := CollectSelectTables(load(t, "select_subquery_from"))
	if err != nil {
		t.Fatal(err)
	}
	// Recurses into the FROM subquery → db.t.
	want := []TableTarget{{DB: "db", Table: "t"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestCollectSelectTables_cteAliasSkipped(t *testing.T) {
	// WITH c AS (SELECT * FROM db.t) SELECT * FROM c JOIN db.u ON ...
	// `c` is a CTE alias → skipped; db.t (CTE body) and db.u (join) are real.
	got, err := CollectSelectTables(load(t, "select_cte_join"))
	if err != nil {
		t.Fatal(err)
	}
	want := []TableTarget{{DB: "db", Table: "t"}, {DB: "db", Table: "u"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
```

- [ ] **Step 2: Run it to verify it fails to compile**

Run: `go test ./internal/engine -run TestCollectSelectTables`
Expected: FAIL — `undefined: CollectSelectTables`.

- [ ] **Step 3: Implement the read-only walk**

Create `internal/engine/nodes.go`:

```go
package engine

import (
	"encoding/json"
	"fmt"
)

// CollectSelectTables returns every real table reference in a SELECT AST, in
// document order, recursing into JOINs, FROM-subqueries, and CTE bodies. Bare
// references whose name matches an in-scope CTE alias are skipped (they are not
// physical tables). Mirrors collectAccessedTablePairsFromAST (select.cc:67-106).
func CollectSelectTables(ast AST) ([]TableTarget, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode select: %w", err)
	}
	var out []TableTarget
	walkTables(root, nil, &out)
	return out, nil
}

// walkTables recurses any decoded JSON node. `scope` is the set of CTE aliases
// visible here (copy-on-fork at each `select` node, like select.cc).
func walkTables(node any, scope map[string]bool, out *[]TableTarget) {
	switch n := node.(type) {
	case map[string]any:
		// Fork the CTE scope when entering a `select` node.
		if sel, ok := n["select"].(map[string]any); ok {
			scope = forkCTEScope(sel, scope)
			// Walk the select's children under the extended scope.
			for k, v := range sel {
				walkTables(v, scope, out)
			}
			return
		}
		// A `table` node is a concrete table reference.
		if tbl, ok := n["table"].(map[string]any); ok {
			tt := decodeTableTarget(tbl)
			if !(tt.DB == "" && scope[tt.Table]) { // skip in-scope CTE alias refs
				*out = append(*out, tt)
			}
			return // a table node has no table-bearing descendants
		}
		for _, v := range n {
			walkTables(v, scope, out)
		}
	case []any:
		for _, v := range n {
			walkTables(v, scope, out)
		}
	}
}

// forkCTEScope copies the parent scope and adds this select's CTE aliases.
func forkCTEScope(sel map[string]any, parent map[string]bool) map[string]bool {
	with, ok := sel["with"].(map[string]any)
	if !ok {
		return parent
	}
	ctes, ok := with["ctes"].([]any)
	if !ok || len(ctes) == 0 {
		return parent
	}
	extended := make(map[string]bool, len(parent)+len(ctes))
	for k := range parent {
		extended[k] = true
	}
	for _, c := range ctes {
		if cm, ok := c.(map[string]any); ok {
			if name := identName(cm["alias"]); name != "" {
				extended[name] = true
			}
		}
	}
	return extended
}

// decodeTableTarget reads {name:{name}, schema:{name}, alias:{name}} from a table node.
func decodeTableTarget(tbl map[string]any) TableTarget {
	return TableTarget{
		DB:    identName(tbl["schema"]),
		Table: identName(tbl["name"]),
		Alias: identName(tbl["alias"]),
	}
}

// identName extracts the .name string from an Identifier-shaped node ({"name":"x",...}).
// Returns "" for null/missing/malformed.
func identName(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["name"].(string)
	return s
}
```

> **Note on map iteration order.** Ranging a Go map is non-deterministic, which would scramble document order. The C++ collects into a `std::map` keyed by name (lexicographic), so order is name-sorted, not document order. The tests above use single- or naturally-ordered cases; **Task 7 dedups+sorts** the originals by `qualify(db,table)` to match the C++ `std::map`, so walk order does not leak into the response. Where a later test needs determinism on the *raw* walk (Task 3 mutation), it asserts on a set, not a slice.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/engine -run TestCollectSelectTables -v`
Expected: PASS (3 tests). If `TestCollectSelectTables_joinAndSubquery` or `_cteAliasSkipped` fail on ordering, convert the assertion to a multiset compare — the contract is *set* membership here.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/nodes.go internal/engine/nodes_test.go
git commit -m "feat(engine): read-only SELECT table walk with CTE-scope + subquery recursion"
```

---

## Task 3: Engine node layer — table mutation + combined walk-and-rewrite

Port `ASTReplaceTransformer::transform` (ASTTransformers.cc:142-206): a single pass that, for each real table, asks a caller-supplied callback for a `TableDecision` and applies it in place (rename, set-db, or replace-with-`remote()`), preserving aliases.

**Files:**
- Modify: `internal/engine/nodes.go`
- Test: `internal/engine/nodes_test.go`

- [ ] **Step 1: Write the failing test**

```go
func genOf(t *testing.T, ast AST) string {
	t.Helper()
	e, err := NewPolyglot("")
	if err != nil {
		t.Skipf("engine unavailable: %v", err)
	}
	defer e.Close()
	out, err := e.Generate(ast)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return out
}

func TestRewriteSelectTables_renameAndSetDB(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	out, err := RewriteSelectTables(load(t, "select"), func(tt TableTarget) TableDecision {
		// db.t -> phys.t_x
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "t_x"}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := genOf(t, out)
	want := "SELECT a FROM phys.t_x WHERE x IN (1, 2, 3)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRewriteSelectTables_remote(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	out, err := RewriteSelectTables(load(t, "select"), func(tt TableTarget) TableDecision {
		return TableDecision{Action: ActionRemote, Remote: &RemoteSpec{
			Addr: "h:9000", DB: "phys", Table: "t_x", User: "u", Password: "p",
		}}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := genOf(t, out)
	want := "SELECT a FROM remote('h:9000', phys, t_x, 'u', 'p') WHERE x IN (1, 2, 3)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
```

> The `select` golden is `SELECT a FROM db.t WHERE x IN (1, 2, 3)` (Phase 0 corpus). Confirm the exact `want` strings by reading `select.json`'s source SQL; if the literal list renders as `(1, 2, 3)`, the wants above match. Adjust spacing to polyglot's generator output if it differs (run once, copy the exact string).

- [ ] **Step 2: Run to verify failure**

Run: `make test ARGS="-run TestRewriteSelectTables"` or
`POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine -run TestRewriteSelectTables`
Expected: FAIL — `undefined: RewriteSelectTables`.

- [ ] **Step 3: Implement mutation + combined walk**

Append to `internal/engine/nodes.go`:

```go
// RewriteSelectTables walks every real table reference (same traversal as
// CollectSelectTables) and applies the TableDecision returned by decide. The
// AST is decoded once, mutated in place (Go maps are references), and re-encoded.
// Mirrors ASTReplaceTransformer::transform (ASTTransformers.cc:142-206).
func RewriteSelectTables(ast AST, decide func(TableTarget) TableDecision) (AST, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode select: %w", err)
	}
	rewriteWalk(root, nil, decide)
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode select: %w", err)
	}
	return AST(out), nil
}

// rewriteWalk is walkTables with mutation. It mutates the maps in `tableExpr`
// (the {table|subquery|function} wrapper) in place.
func rewriteWalk(node any, scope map[string]bool, decide func(TableTarget) TableDecision) {
	switch n := node.(type) {
	case map[string]any:
		if sel, ok := n["select"].(map[string]any); ok {
			scope = forkCTEScope(sel, scope)
			for _, v := range sel {
				rewriteWalk(v, scope, decide)
			}
			return
		}
		// `n` may be a table-expression wrapper carrying a "table" key.
		if tbl, ok := n["table"].(map[string]any); ok {
			tt := decodeTableTarget(tbl)
			if tt.DB == "" && scope[tt.Table] {
				return // CTE alias ref — leave untouched
			}
			applyDecision(n, tbl, tt, decide(tt))
			return
		}
		for _, v := range n {
			rewriteWalk(v, scope, decide)
		}
	case []any:
		for _, v := range n {
			rewriteWalk(v, scope, decide)
		}
	}
}

// applyDecision mutates the table-expression wrapper `expr` (which currently has
// expr["table"] == tbl) according to d.
func applyDecision(expr, tbl map[string]any, tt TableTarget, d TableDecision) {
	switch d.Action {
	case ActionSkip:
		return
	case ActionRename:
		tbl["name"] = ident(d.NewTable)
		if d.NewDB != "" {
			tbl["schema"] = ident(d.NewDB)
		}
		preserveAlias(tbl, tt)
	case ActionRemote:
		// Replace the whole table expr: drop "table", add a remote() function.
		delete(expr, "table")
		expr["function"] = remoteFunc(d.Remote)
		// Aliases on a table-function expr live on the wrapper; preserve if any.
		if tt.Alias != "" {
			// (table functions keep their alias on the function's parent; for the
			// SELECT FROM position polyglot renders `remote(...) AS alias`.)
			tbl["alias"] = ident(tt.Alias)
			expr["function"].(map[string]any)["_alias_unused"] = nil
			delete(expr["function"].(map[string]any), "_alias_unused")
		}
	}
}

// preserveAlias keeps the user's alias, or sets the original name as alias when
// the rewrite changed the visible table name and there was no explicit alias —
// matching ASTReplaceTransformer (keeps result-set column names stable).
func preserveAlias(tbl map[string]any, tt TableTarget) {
	if tt.Alias != "" {
		tbl["alias"] = ident(tt.Alias)
		return
	}
	// No explicit alias: leave alias null. (The C++ sets alias=origin_name; we
	// only need that when the *short name* changed in a way that breaks column
	// refs. Differential harness (Task 11) catches any case that needs it; start
	// minimal to avoid spurious `AS` noise.)
}

// ident builds an Identifier node {"name":s,"quoted":false,"trailing_comments":[]}.
func ident(s string) map[string]any {
	return map[string]any{"name": s, "quoted": false, "trailing_comments": []any{}}
}

// litStr / litBare build remote() argument nodes.
func litStr(s string) map[string]any {
	return map[string]any{"literal": map[string]any{"literal_type": "string", "value": s}}
}
func colBare(s string) map[string]any {
	return map[string]any{"column": map[string]any{
		"name": ident(s), "table": nil, "join_mark": false, "trailing_comments": []any{},
	}}
}

// remoteFunc builds {"function":{"name":"remote","args":[...]}} with the 5 positional
// args: addr (string lit), db (bare ident), table (bare ident), user+pw (string lits).
func remoteFunc(r *RemoteSpec) map[string]any {
	return map[string]any{
		"name": "remote",
		"args": []any{
			litStr(r.Addr), colBare(r.DB), colBare(r.Table), litStr(r.User), litStr(r.Password),
		},
		"distinct": false, "trailing_comments": []any{},
		"use_bracket_syntax": false, "no_parens": false, "quoted": false,
	}
}
```

> **Alias-on-remote caveat:** the `_alias_unused` dance above is a no-op guard; delete it and the two lines around it if the differential harness shows `remote(...) AS x` is required — preserve the alias by setting `tbl["alias"]` before `delete(expr,"table")`. Keep the minimal form first and let Task 11 dictate.

Clean up `applyDecision`'s `ActionRemote` branch to the minimal correct form:

```go
	case ActionRemote:
		alias := tt.Alias
		delete(expr, "table")
		fn := remoteFunc(d.Remote)
		if alias != "" {
			fn["alias"] = ident(alias)
		}
		expr["function"] = fn
```

(Replace the earlier `ActionRemote` block with this; the `_alias_unused` version was illustrative.)

- [ ] **Step 4: Run the tests**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine -run TestRewriteSelectTables -v`
Expected: PASS. If `want` strings differ in spacing, copy polyglot's exact output into the test (the generator is the source of truth for formatting).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/nodes.go internal/engine/nodes_test.go
git commit -m "feat(engine): in-place SELECT table rewrite (rename / set-db / remote)"
```

---

## Task 4: nameresolve — static 3-map lookup

Port `lookupStaticTableRewrite` + `planTableRewrite` (name_rewrite.cc:71-94, ASTTransformers.cc:107-131). Pure policy: `(db, table)` + `*pb.RewriteTableStaticArgs` → `Outcome`. Precedence: `table_map` → `remote_table_map` → `table_with_database_map` → passthrough.

**Files:**
- Create: `internal/nameresolve/resolve.go`
- Test: `internal/nameresolve/resolve_test.go`

- [ ] **Step 1: Write the failing test**

```go
package nameresolve

import (
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
)

func TestLookupStatic_precedence(t *testing.T) {
	args := &pb.RewriteTableStaticArgs{
		TableMap:             map[string]string{"db.t": "t_phys"},
		RemoteTableMap:       map[string]*pb.RewriteTableStaticArgs_RemoteTable{"db.r": {Addr: "h", Database: "pd", Table: "rt", User: "u", Password: "p"}},
		TableWithDatabaseMap: map[string]*pb.RewriteTableStaticArgs_TableWithDatabase{"db.w": {Database: "pw", Table: "wt"}},
	}
	cases := []struct {
		db, table string
		want      Outcome
	}{
		// table_map: rename only, db preserved.
		{"db", "t", Outcome{Status: StatusRewrite, PhysicalDB: "db", NewTable: "t_phys", LogicalDB: "db"}},
		// remote_table_map.
		{"db", "r", Outcome{Status: StatusRemote, PhysicalDB: "pd", NewTable: "rt", LogicalDB: "db", RemoteAddr: "h", RemoteUser: "u", RemotePassword: "p"}},
		// table_with_database_map: set both.
		{"db", "w", Outcome{Status: StatusRewrite, PhysicalDB: "pw", NewTable: "wt", LogicalDB: "db"}},
		// no match → passthrough.
		{"db", "x", Outcome{Status: StatusPassthrough}},
	}
	for _, c := range cases {
		got := LookupStatic(c.db, c.table, args)
		if got != c.want {
			t.Errorf("LookupStatic(%q,%q) = %+v, want %+v", c.db, c.table, got, c.want)
		}
	}
}

func TestLookupStatic_withDatabaseEmptyKeepsOriginDB(t *testing.T) {
	args := &pb.RewriteTableStaticArgs{
		TableWithDatabaseMap: map[string]*pb.RewriteTableStaticArgs_TableWithDatabase{"db.w": {Database: "", Table: "wt"}},
	}
	got := LookupStatic("db", "w", args)
	want := Outcome{Status: StatusRewrite, PhysicalDB: "db", NewTable: "wt", LogicalDB: "db"} // empty database keeps origin
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/nameresolve -run TestLookupStatic`
Expected: FAIL — package/symbols undefined.

- [ ] **Step 3: Implement**

Create `internal/nameresolve/resolve.go` (start with the shared types from the canonical block above, then):

```go
package nameresolve

import (
	"strings"

	"github.com/housegate/rewriter-go/gen/pb"
)

// qualify builds the map lookup key: "db.table" when db is set, else "table".
// The separator is always a literal "." (name_rewrite.cc:11-15) — never delim.
func qualify(db, table string) string {
	if db == "" {
		return table
	}
	return db + "." + table
}

// LookupStatic resolves (db, table) through the three static maps in precedence
// order: table_map → remote_table_map → table_with_database_map → passthrough.
func LookupStatic(db, table string, a *pb.RewriteTableStaticArgs) Outcome {
	key := qualify(db, table)
	if nt, ok := a.GetTableMap()[key]; ok {
		// Rename only; db qualifier preserved (new_db = origin_db).
		return Outcome{Status: StatusRewrite, PhysicalDB: db, NewTable: nt, LogicalDB: db}
	}
	if rt, ok := a.GetRemoteTableMap()[key]; ok {
		return Outcome{
			Status: StatusRemote, PhysicalDB: rt.GetDatabase(), NewTable: rt.GetTable(), LogicalDB: db,
			RemoteAddr: rt.GetAddr(), RemoteUser: rt.GetUser(), RemotePassword: rt.GetPassword(),
		}
	}
	if wd, ok := a.GetTableWithDatabaseMap()[key]; ok {
		newDB := wd.GetDatabase()
		if newDB == "" { // empty database keeps the origin db
			newDB = db
		}
		return Outcome{Status: StatusRewrite, PhysicalDB: newDB, NewTable: wd.GetTable(), LogicalDB: db}
	}
	return Outcome{Status: StatusPassthrough}
}

var _ = strings.TrimSpace // strings used later (ApplyDynamic)
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/nameresolve -run TestLookupStatic -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/nameresolve/resolve.go internal/nameresolve/resolve_test.go
git commit -m "feat(nameresolve): static 3-map table resolution with precedence"
```

---

## Task 5: nameresolve — dynamic resolution

Port `applyDynamicRewrite` + `resolvePhysicalDatabase` + `buildDynamicTableName` (name_rewrite.cc:97-220). Resolve logical→physical DB, build the prefixed table name, and detect remote-upstream routing.

**Files:**
- Modify: `internal/nameresolve/resolve.go`
- Test: `internal/nameresolve/resolve_test.go`

- [ ] **Step 1: Write the failing test**

```go
func dynArgs() *pb.RewriteTableDynamicArgs {
	return &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"tenant1": "testnet"},
		KnownPhysicalDatabases: []string{"system"},
		Delim:                  "_",
	}
}

func TestApplyDynamic_basic(t *testing.T) {
	got := ApplyDynamic("tenant1", "events", dynArgs())
	want := Outcome{Status: StatusRewrite, PhysicalDB: "testnet", NewTable: "tenant1.events", LogicalDB: "tenant1"}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestApplyDynamic_extraArguments(t *testing.T) {
	a := dynArgs()
	a.ExtraArguments = []string{"shard0"}
	got := ApplyDynamic("tenant1", "events", a)
	if got.NewTable != "tenant1_shard0.events" {
		t.Fatalf("new_table = %q, want tenant1_shard0.events", got.NewTable)
	}
}

func TestApplyDynamic_knownPhysicalPassthrough(t *testing.T) {
	got := ApplyDynamic("system", "tables", dynArgs())
	want := Outcome{Status: StatusRewrite, PhysicalDB: "system", NewTable: "system.tables", LogicalDB: "system"}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestApplyDynamic_unqualifiedNoContext_invalid(t *testing.T) {
	got := ApplyDynamic("", "events", dynArgs()) // no upstream_logical_database_in_context
	if got.Status != StatusInvalid {
		t.Fatalf("status = %v, want StatusInvalid", got.Status)
	}
}

func TestApplyDynamic_unqualifiedUsesContext(t *testing.T) {
	a := dynArgs()
	a.UpstreamLogicalDatabaseInContext = "tenant1"
	got := ApplyDynamic("", "events", a)
	if got.PhysicalDB != "testnet" || got.NewTable != "tenant1.events" {
		t.Fatalf("got %+v", got)
	}
}

func TestApplyDynamic_unknownLogical_invalid(t *testing.T) {
	got := ApplyDynamic("nope", "events", dynArgs())
	if got.Status != StatusInvalid {
		t.Fatalf("status = %v, want StatusInvalid", got.Status)
	}
}

func TestApplyDynamic_remoteUpstream(t *testing.T) {
	a := dynArgs()
	a.LogicalDatabaseToRemoteUpstreamIndex = map[string]string{"tenant1": "us"}
	a.RemoteUpstreams = map[string]*pb.RewriteTableDynamicArgs_RemoteUpstream{
		"us": {Addr: "h:9000", User: "ru", Password: "rp"},
	}
	got := ApplyDynamic("tenant1", "events", a)
	want := Outcome{
		Status: StatusRemote, PhysicalDB: "testnet", NewTable: "tenant1.events", LogicalDB: "tenant1",
		RemoteAddr: "h:9000", RemoteUser: "ru", RemotePassword: "rp",
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestApplyDynamic_remoteUpstreamMissingKey_invalid(t *testing.T) {
	a := dynArgs()
	a.LogicalDatabaseToRemoteUpstreamIndex = map[string]string{"tenant1": "ghost"} // not in RemoteUpstreams
	got := ApplyDynamic("tenant1", "events", a)
	if got.Status != StatusInvalid {
		t.Fatalf("status = %v, want StatusInvalid", got.Status)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/nameresolve -run TestApplyDynamic`
Expected: FAIL — `undefined: ApplyDynamic`.

- [ ] **Step 3: Implement**

Append to `internal/nameresolve/resolve.go`:

```go
// resolvePhysicalDatabase maps a logical DB to its physical name, or returns
// ok=false when unresolvable. Order: database_map, then known_physical (passthrough).
func resolvePhysicalDatabase(logical string, a *pb.RewriteTableDynamicArgs) (string, bool) {
	if logical == "" {
		return "", false
	}
	if phys, ok := a.GetDatabaseMap()[logical]; ok {
		return phys, true
	}
	for _, k := range a.GetKnownPhysicalDatabases() {
		if k == logical {
			return logical, true
		}
	}
	return "", false
}

// buildDynamicTableName constructs "<logical>[<delim><extra>...].<original_table>".
// The separator before original_table is ALWAYS a literal "." (not delim).
// Returns "" when origin_table is "" (the USE sentinel). name_rewrite.cc:112-146.
func buildDynamicTableName(logical, originTable string, a *pb.RewriteTableDynamicArgs) string {
	if originTable == "" {
		return ""
	}
	delim := a.GetDelim()
	if delim == "" {
		delim = "_"
	}
	var b strings.Builder
	b.WriteString(logical)
	for _, extra := range a.GetExtraArguments() {
		b.WriteString(delim)
		b.WriteString(extra)
	}
	b.WriteString(".")
	b.WriteString(originTable)
	return b.String()
}

// ApplyDynamic resolves (db, table) under dynamic args. Mirrors applyDynamicRewrite
// (name_rewrite.cc:157-220). On any policy failure it returns StatusInvalid; the
// SELECT caller treats StatusInvalid as "skip silently" (lenient), non-SELECT as reject.
func ApplyDynamic(db, table string, a *pb.RewriteTableDynamicArgs) Outcome {
	// Step 1: resolve logical DB.
	logical := db
	if logical == "" {
		logical = a.GetUpstreamLogicalDatabaseInContext()
	}
	if logical == "" {
		return Outcome{Status: StatusInvalid, RejectReason: "unqualified target and no upstream_logical_database_in_context"}
	}
	// Step 2: logical → physical.
	physical, ok := resolvePhysicalDatabase(logical, a)
	if !ok {
		return Outcome{Status: StatusInvalid, RejectReason: "logical db " + logical + " not in database_map and not a known physical database"}
	}
	// Step 3: build table name.
	newTable := buildDynamicTableName(logical, table, a)
	// Step 4: remote-upstream routing?
	if key, ok := a.GetLogicalDatabaseToRemoteUpstreamIndex()[logical]; ok {
		up, ok := a.GetRemoteUpstreams()[key]
		if !ok {
			return Outcome{Status: StatusInvalid, RejectReason: "remote upstream key " + key + " not in remote_upstreams"}
		}
		return Outcome{
			Status: StatusRemote, PhysicalDB: physical, NewTable: newTable, LogicalDB: logical,
			RemoteAddr: up.GetAddr(), RemoteUser: up.GetUser(), RemotePassword: up.GetPassword(),
		}
	}
	return Outcome{Status: StatusRewrite, PhysicalDB: physical, NewTable: newTable, LogicalDB: logical}
}
```

Remove the temporary `var _ = strings.TrimSpace` line from Task 4 (strings is now used).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/nameresolve -run TestApplyDynamic -v`
Expected: PASS (8 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/nameresolve/resolve.go internal/nameresolve/resolve_test.go
git commit -m "feat(nameresolve): dynamic resolution (physical-db map, name build, remote upstream)"
```

---

## Task 6: nameresolve — mode selection, unified Resolve, accessed-table resolution

Port `findActiveTableRewrite` (last-wins; static beats dynamic within an option) and `resolveAccessedTable` (best-effort, never rejects). Add the `Resolve` dispatcher the handler calls per table.

**Files:**
- Modify: `internal/nameresolve/resolve.go`
- Test: `internal/nameresolve/resolve_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestFindActive_lastWinsStaticBeatsDynamic(t *testing.T) {
	opts := []*pb.RewriteOption{
		{Op: pb.RewriteOp_LimitRewrite}, // ignored by FindActive
		{Op: pb.RewriteOp_TableNameRewrite, Value: &pb.RewriteOption_TableNameArgs{
			TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: dynArgs()}}},
		{Op: pb.RewriteOp_TableNameRewrite, Value: &pb.RewriteOption_TableNameArgs{
			TableNameArgs: &pb.RewriteTableNameArgs{StaticArgs: &pb.RewriteTableStaticArgs{}}}},
	}
	sel := FindActive(opts)
	if sel.Mode != ModeStatic || sel.Static == nil {
		t.Fatalf("got mode %v static=%v, want ModeStatic non-nil", sel.Mode, sel.Static)
	}
}

func TestFindActive_none(t *testing.T) {
	sel := FindActive([]*pb.RewriteOption{{Op: pb.RewriteOp_LimitRewrite}})
	if sel.Mode != ModeNone {
		t.Fatalf("got %v want ModeNone", sel.Mode)
	}
}

func TestResolve_dispatch(t *testing.T) {
	// Static selection routes to LookupStatic.
	st := Selection{Mode: ModeStatic, Static: &pb.RewriteTableStaticArgs{TableMap: map[string]string{"db.t": "t2"}}}
	if got := Resolve("db", "t", st); got.NewTable != "t2" {
		t.Fatalf("static dispatch: %+v", got)
	}
	// Dynamic selection routes to ApplyDynamic.
	dy := Selection{Mode: ModeDynamic, Dynamic: dynArgs()}
	if got := Resolve("tenant1", "events", dy); got.PhysicalDB != "testnet" {
		t.Fatalf("dynamic dispatch: %+v", got)
	}
	// None → passthrough.
	if got := Resolve("db", "t", Selection{Mode: ModeNone}); got.Status != StatusPassthrough {
		t.Fatalf("none dispatch: %+v", got)
	}
}

func TestResolveAccessed_dynamic(t *testing.T) {
	sel := Selection{Mode: ModeDynamic, Dynamic: dynArgs()}
	got := ResolveAccessed("tenant1", "events", sel)
	want := Accessed{LogicalDB: "tenant1", PhysicalDB: "testnet", IsRemote: false}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestResolveAccessed_staticLeavesLogicalEmpty(t *testing.T) {
	sel := Selection{Mode: ModeStatic, Static: &pb.RewriteTableStaticArgs{TableMap: map[string]string{"db.t": "t2"}}}
	got := ResolveAccessed("db", "t", sel)
	// static mode: physical_database = origin_db, logical empty (name_rewrite.cc:245-249)
	want := Accessed{LogicalDB: "", PhysicalDB: "db", IsRemote: false}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/nameresolve -run 'TestFindActive|TestResolve'`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement**

Append to `internal/nameresolve/resolve.go`:

```go
// FindActive scans options for the active TableNameRewrite policy. Last-wins
// across options; within an option, static_args beats dynamic_args. Options with
// neither sub-message do not reset a prior selection. name_rewrite.cc:37-61.
func FindActive(opts []*pb.RewriteOption) Selection {
	sel := Selection{Mode: ModeNone}
	for _, o := range opts {
		if o.GetOp() != pb.RewriteOp_TableNameRewrite {
			continue
		}
		na := o.GetTableNameArgs()
		if na == nil {
			continue
		}
		switch {
		case na.GetStaticArgs() != nil:
			sel = Selection{Mode: ModeStatic, Static: na.GetStaticArgs()}
		case na.GetDynamicArgs() != nil:
			sel = Selection{Mode: ModeDynamic, Dynamic: na.GetDynamicArgs()}
		}
	}
	return sel
}

// Resolve dispatches one (db, table) through the active selection.
func Resolve(db, table string, sel Selection) Outcome {
	switch sel.Mode {
	case ModeStatic:
		return LookupStatic(db, table, sel.Static)
	case ModeDynamic:
		return ApplyDynamic(db, table, sel.Dynamic)
	default:
		return Outcome{Status: StatusPassthrough}
	}
}

// ResolveAccessed computes the best-effort (logical, physical, is_remote) for
// original_accessed_tables. Never rejects; leaves fields empty when unresolvable.
// name_rewrite.cc:229-288.
func ResolveAccessed(db, table string, sel Selection) Accessed {
	switch sel.Mode {
	case ModeStatic:
		// Static mode tracks no logical/physical distinction; physical = origin_db.
		return Accessed{LogicalDB: "", PhysicalDB: db, IsRemote: false}
	case ModeDynamic:
		logical := db
		if logical == "" {
			logical = sel.Dynamic.GetUpstreamLogicalDatabaseInContext()
		}
		phys, _ := resolvePhysicalDatabase(logical, sel.Dynamic)
		_, isRemote := sel.Dynamic.GetLogicalDatabaseToRemoteUpstreamIndex()[logical]
		return Accessed{LogicalDB: logical, PhysicalDB: phys, IsRemote: isRemote}
	default:
		return Accessed{}
	}
}
```

- [ ] **Step 4: Run the full nameresolve suite**

Run: `go test ./internal/nameresolve -v`
Expected: PASS (all tasks 4–6).

- [ ] **Step 5: Commit**

```bash
git add internal/nameresolve/resolve.go internal/nameresolve/resolve_test.go
git commit -m "feat(nameresolve): mode selection, unified Resolve, accessed-table resolution"
```

---

## Task 7: handlers — SELECT spine (walk + resolve + response), wired into NativeRewriter

Join the engine walk to nameresolve via a decision callback; collect `table_rewrites` + `original_accessed_tables`; assemble `pb.RewriteSQLResponse`; route `select`-kind ASTs from `NativeRewriter.Rewrite` to the handler. No options/CTE/GLOBAL yet (Tasks 8–10).

**Files:**
- Create: `internal/handlers/select.go`
- Test: `internal/handlers/select_test.go`
- Modify: `native.go`, `native_test.go`

- [ ] **Step 1: Write the failing test (handler unit, engine-gated)**

```go
package handlers

import (
	"os"
	"sort"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

func newEngine(t *testing.T) engine.Engine {
	t.Helper()
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func dynOpt(a *pb.RewriteTableDynamicArgs) []*pb.RewriteOption {
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: a}}}}
}

func TestRewriteSelect_dynamicRename(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT a FROM tenant1.events WHERE x IN (1, 2)")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{"tenant1": "testnet"}, Delim: "_",
	})
	resp, err := RewriteSelect(e, ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s)", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SELECT {
		t.Fatalf("stmt type = %v", resp.GetStatementType())
	}
	// table_rewrites: tenant1.events -> testnet.`tenant1.events`
	want := map[string]string{"tenant1.events": "testnet.tenant1.events"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	// SQL backtick-quotes the dotted table name.
	if resp.GetSqlAfterRewrite() == "" {
		t.Fatal("empty sql")
	}
	// original_accessed_tables: one entry, tenant1/events resolved.
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalDatabase() != "tenant1" || ats[0].GetPhysicalDatabase() != "testnet" {
		t.Fatalf("accessed = %+v", ats)
	}
}

func mapEq(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestRewriteSelect_invalidUnqualified_skipsLeniently(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT a FROM events") // unqualified, no context
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, err := RewriteSelect(e, ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	// SELECT is lenient: StatusInvalid → table left as written, Success code.
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code = %v, want Success (lenient)", resp.GetCode())
	}
	if len(resp.GetTableRewrites()) != 0 {
		t.Fatalf("expected no rewrites, got %v", resp.GetTableRewrites())
	}
	_ = sort.Strings
}
```

- [ ] **Step 2: Run to verify failure**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/handlers -run TestRewriteSelect`
Expected: FAIL — `undefined: RewriteSelect`.

- [ ] **Step 3: Implement the handler spine**

Create `internal/handlers/select.go`:

```go
// Package handlers ports the C++ rewriter-grpc statement handlers. Phase 1: SELECT.
package handlers

import (
	"sort"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
)

// RewriteSelect ports handleSelectQuery (select.cc:754-824): resolve every table,
// rewrite the AST, populate table_rewrites + original_accessed_tables, regenerate.
// (Options/CTE/GLOBAL are layered in Tasks 8-10.)
func RewriteSelect(e engine.Engine, ast engine.AST, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, error) {
	resp := &pb.RewriteSQLResponse{
		Code:          pb.RewriteCode_Success,
		Message:       "success",
		StatementType: pb.StatementType_STATEMENT_TYPE_SELECT,
		TableRewrites: map[string]string{},
	}
	sel := nameresolve.FindActive(opts)

	// Collect originals (pre-rewrite) for original_accessed_tables, deduped by key.
	originals, err := engine.CollectSelectTables(ast)
	if err != nil {
		return nil, err
	}
	resp.OriginalAccessedTables = buildAccessed(originals, sel)

	// Single walk: resolve each real table, mutate, record table_rewrites.
	rewritten, err := engine.RewriteSelectTables(ast, func(tt engine.TableTarget) engine.TableDecision {
		return decideTable(tt, sel, resp.TableRewrites)
	})
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

// decideTable maps a nameresolve.Outcome to an engine.TableDecision and records
// the table_rewrites entry. SELECT is lenient: StatusInvalid → skip (no error).
func decideTable(tt engine.TableTarget, sel nameresolve.Selection, rewrites map[string]string) engine.TableDecision {
	o := nameresolve.Resolve(tt.DB, tt.Table, sel)
	switch o.Status {
	case nameresolve.StatusRewrite:
		recordRewrite(rewrites, tt, o.PhysicalDB, o.NewTable)
		return engine.TableDecision{Action: engine.ActionRename, NewDB: o.PhysicalDB, NewTable: o.NewTable}
	case nameresolve.StatusRemote:
		recordRewrite(rewrites, tt, o.PhysicalDB, o.NewTable)
		return engine.TableDecision{Action: engine.ActionRemote, Remote: &engine.RemoteSpec{
			Addr: o.RemoteAddr, DB: o.PhysicalDB, Table: o.NewTable, User: o.RemoteUser, Password: o.RemotePassword,
		}}
	default: // StatusPassthrough, StatusInvalid (lenient skip), StatusRemoteUnsupported
		return engine.TableDecision{Action: engine.ActionSkip}
	}
}

// recordRewrite adds an entry to table_rewrites unless the name is unchanged.
// Key/value are "db.table" (or bare "table"). select.cc:815-822.
func recordRewrite(rewrites map[string]string, tt engine.TableTarget, newDB, newTable string) {
	from := qualify(tt.DB, tt.Table)
	to := qualify(newDB, newTable)
	if from != to {
		rewrites[from] = to
	}
}

// buildAccessed produces deduped, key-sorted AccessedTable entries (matches the
// C++ std::map iteration order).
func buildAccessed(targets []engine.TableTarget, sel nameresolve.Selection) []*pb.AccessedTable {
	seen := map[string]bool{}
	keys := make([]string, 0, len(targets))
	byKey := map[string]engine.TableTarget{}
	for _, tt := range targets {
		k := qualify(tt.DB, tt.Table)
		if seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
		byKey[k] = tt
	}
	sort.Strings(keys)
	out := make([]*pb.AccessedTable, 0, len(keys))
	for _, k := range keys {
		tt := byKey[k]
		a := nameresolve.ResolveAccessed(tt.DB, tt.Table, sel)
		out = append(out, &pb.AccessedTable{
			OriginalDatabase: tt.DB, OriginalTable: tt.Table,
			LogicalDatabase: a.LogicalDB, PhysicalDatabase: a.PhysicalDB, IsRemote: a.IsRemote,
		})
	}
	return out
}

// qualify mirrors nameresolve.qualify (kept local to avoid exporting it).
func qualify(db, table string) string {
	if db == "" {
		return table
	}
	return db + "." + table
}
```

- [ ] **Step 4: Run the handler tests**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/handlers -run TestRewriteSelect -v`
Expected: PASS (2 tests). If `table_rewrites` value differs (e.g. polyglot emits backticks in the *generated SQL* but the rewrites map stores the bare `testnet.tenant1.events`), confirm the map stores the unquoted `qualify` form — backticking is only in `sql_after_rewrite`.

- [ ] **Step 5: Wire into NativeRewriter — write the failing routing test**

In `native_test.go`, add:

```go
func TestNativeRewrite_selectDynamic(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, _ := engine.NewPolyglot("")
	defer e.Close()
	r := New(e, WithOptions(func(account string) []*pb.RewriteOption {
		return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
				DynamicArgs: &pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}, Delim: "_"}}}}}
	}))
	res, err := r.Rewrite(context.Background(), "SELECT a FROM tenant1.events", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success || len(res.TableRewrites) != 1 {
		t.Fatalf("res = %+v", res)
	}
}
```

- [ ] **Step 6: Run to verify failure**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test . -run TestNativeRewrite_selectDynamic`
Expected: FAIL — `undefined: WithOptions` and `New` arity.

- [ ] **Step 7: Add options injection + SELECT routing to native.go**

In `native.go`, modify the struct and `New`, and route SELECT:

```go
// NativeRewriter is the in-process Rewriter. Phase 1: SELECT handled; others pass-through.
type NativeRewriter struct {
	engine  engine.Engine
	options func(account string) []*pb.RewriteOption // injected account-derived policy
	mu      sync.Mutex
	last    *callContext
}

// Option configures a NativeRewriter.
type Option func(*NativeRewriter)

// WithOptions injects the account-derived RewriteOption builder (buildDatabaseMap
// in the consumer). When unset, SELECT runs with no rewrite policy (round-trip).
func WithOptions(fn func(account string) []*pb.RewriteOption) Option {
	return func(r *NativeRewriter) { r.options = fn }
}

// New builds a NativeRewriter over the given engine.
func New(e engine.Engine, opts ...Option) *NativeRewriter {
	r := &NativeRewriter{engine: e}
	for _, o := range opts {
		o(r)
	}
	return r
}
```

Then in `Rewrite`, after `resp.StatementType = r.classify(ast)`, replace the pass-through generate block with routing:

```go
	// Phase 1: route SELECT to the real handler; everything else stays pass-through.
	kind, _ := engine.NodeKind(ast)
	if kind == engine.NodeSelect {
		var opts []*pb.RewriteOption
		if r.options != nil {
			opts = r.options(account)
		}
		hresp, herr := handlers.RewriteSelect(r.engine, ast, opts)
		if herr != nil {
			return RewriteResult{}, herr // unexpected/internal → fail-open Go error
		}
		r.mu.Lock()
		r.last = &callContext{sql: sql, account: account}
		r.mu.Unlock()
		return resultFromPB(hresp), nil
	}
	// Pass-through (Phases 2-5): regenerate, fall back to input.
	if gen, gerr := r.engine.Generate(ast); gerr == nil && gen != "" {
		resp.SqlAfterRewrite = gen
	}
```

Add the import `"github.com/housegate/rewriter-go/internal/handlers"`.

- [ ] **Step 8: Run the full root + handler suite**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test . ./internal/handlers -v`
Expected: PASS (routing test + handler tests + existing pass-through tests still green).

- [ ] **Step 9: Commit**

```bash
git add internal/handlers/select.go internal/handlers/select_test.go native.go native_test.go
git commit -m "feat(handlers): SELECT spine — resolve+rewrite tables, populate response; route in NativeRewriter"
```

---

## Task 8: handlers — option pipeline (Limit / Offset / Settings)

Port `applyRewriteOptions` + `applyPendingToSelect` (select.cc:322-387, 572-752) for the non-CTE option types: accumulate Limit/Offset/Settings, then apply once to the outermost select. (TableNameRewrite is already applied in Task 7's walk; CTE is Task 9.)

**Files:**
- Modify: `internal/engine/nodes.go` (add `SetLimit`, `GetLimit`, `SetOffset`, `SetSettings`)
- Create: `internal/handlers/options.go`
- Test: `internal/engine/nodes_test.go`, `internal/handlers/options_test.go`

- [ ] **Step 1: Write the failing engine test**

```go
func TestLimitOps(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	base := load(t, "select_limit") // SELECT a FROM db.t LIMIT 10
	if v, ok, _ := GetLimit(base); !ok || v != 10 {
		t.Fatalf("GetLimit = %d,%v want 10,true", v, ok)
	}
	out, err := SetLimit(load(t, "select"), 5) // select has no LIMIT
	if err != nil {
		t.Fatal(err)
	}
	if genOf(t, out) != "SELECT a FROM db.t WHERE x IN (1, 2, 3) LIMIT 5" {
		t.Fatalf("got %q", genOf(t, out))
	}
	off, _ := SetOffset(out, 3)
	if genOf(t, off) != "SELECT a FROM db.t WHERE x IN (1, 2, 3) LIMIT 5 OFFSET 3" {
		t.Fatalf("offset got %q", genOf(t, off))
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine -run TestLimitOps`
Expected: FAIL — undefined `GetLimit`/`SetLimit`/`SetOffset`.

- [ ] **Step 3: Implement the engine option primitives**

Append to `internal/engine/nodes.go`:

```go
// numLiteral builds {"literal":{"literal_type":"number","value":"<n>"}}.
func numLiteral(n int64) map[string]any {
	return map[string]any{"literal": map[string]any{"literal_type": "number", "value": itoa(n)}}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// outerSelect decodes the AST and returns the outermost select object (the value
// under the top-level "select" key) for in-place mutation, plus a re-encode func.
func outerSelect(ast AST) (sel map[string]any, encode func() (AST, error), err error) {
	var root map[string]any
	if err = json.Unmarshal(ast, &root); err != nil {
		return nil, nil, fmt.Errorf("engine: decode select: %w", err)
	}
	sel, ok := root["select"].(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("engine: not a select node")
	}
	encode = func() (AST, error) {
		b, e := json.Marshal(root)
		if e != nil {
			return nil, fmt.Errorf("engine: encode select: %w", e)
		}
		return AST(b), nil
	}
	return sel, encode, nil
}

// GetLimit returns the outer select's LIMIT literal value, if present and numeric.
func GetLimit(ast AST) (val int64, ok bool, err error) {
	sel, _, err := outerSelect(ast)
	if err != nil {
		return 0, false, err
	}
	lim, ok := sel["limit"].(map[string]any)
	if !ok {
		return 0, false, nil
	}
	this, ok := lim["this"].(map[string]any)
	if !ok {
		return 0, false, nil
	}
	lit, ok := this["literal"].(map[string]any)
	if !ok {
		return 0, false, nil
	}
	s, _ := lit["value"].(string)
	n, e := strconv.ParseInt(s, 10, 64)
	if e != nil {
		return 0, false, nil // non-literal limit (expression) → treat as absent
	}
	return n, true, nil
}

// SetLimit sets the outer select's LIMIT to n.
func SetLimit(ast AST, n int64) (AST, error) {
	sel, encode, err := outerSelect(ast)
	if err != nil {
		return nil, err
	}
	sel["limit"] = map[string]any{"this": numLiteral(n)}
	return encode()
}

// SetOffset sets the outer select's OFFSET to n (only meaningful when > 0).
func SetOffset(ast AST, n int64) (AST, error) {
	sel, encode, err := outerSelect(ast)
	if err != nil {
		return nil, err
	}
	sel["offset"] = map[string]any{"this": numLiteral(n)}
	return encode()
}

// Setting is one SETTINGS key=value to render. ValueLiteral is the polyglot
// literal_type ("number"|"string") and Value the encoded value.
type Setting struct {
	Key         string
	LiteralType string
	Value       string
}

// SetSettings appends settings to the outer select's SETTINGS array (creating it
// if absent). Each renders as {"eq":{"left":{"column":...},"right":{"literal":...}}}.
func SetSettings(ast AST, settings []Setting) (AST, error) {
	sel, encode, err := outerSelect(ast)
	if err != nil {
		return nil, err
	}
	arr, _ := sel["settings"].([]any)
	for _, s := range settings {
		arr = append(arr, map[string]any{"eq": map[string]any{
			"left":  colBare(s.Key),
			"right": map[string]any{"literal": map[string]any{"literal_type": s.LiteralType, "value": s.Value}},
			"left_comments": []any{}, "operator_comments": []any{}, "trailing_comments": []any{},
		}})
	}
	sel["settings"] = arr
	return encode()
}
```

Add `"strconv"` to the `internal/engine/nodes.go` import block.

- [ ] **Step 4: Run the engine option tests**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine -run 'TestLimitOps' -v`
Expected: PASS. Copy exact generator spacing into `want` if needed.

- [ ] **Step 5: Write the failing handler option test**

In `internal/handlers/options_test.go`:

```go
package handlers

import (
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
)

func TestApplyOptions_forceLimit(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT a FROM db.t LIMIT 100")
	opts := []*pb.RewriteOption{{Op: pb.RewriteOp_LimitRewrite,
		Value: &pb.RewriteOption_LimitArgs{LimitArgs: &pb.RewriteLimitArgs{
			Value: &pb.RewriteLimitArgs_ForceLimit{ForceLimit: 10}}}}}
	out, err := applyOptions(ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	if got != "SELECT a FROM db.t LIMIT 10" {
		t.Fatalf("force limit got %q", got)
	}
}

func TestApplyOptions_replaceLimitThreshold(t *testing.T) {
	e := newEngine(t)
	// replace_to=10, threshold=50: original 100 > 50 → replace; original 5 ≤ 50 → keep.
	mk := func() *pb.RewriteOption {
		return &pb.RewriteOption{Op: pb.RewriteOp_LimitRewrite,
			Value: &pb.RewriteOption_LimitArgs{LimitArgs: &pb.RewriteLimitArgs{
				Value: &pb.RewriteLimitArgs_ReplaceLimit_{ReplaceLimit: &pb.RewriteLimitArgs_ReplaceLimit{Threshold: 50, ReplaceTo: 10}}}}}
	}
	ast1, _ := e.ParseOne("SELECT a FROM db.t LIMIT 100")
	out1, _ := applyOptions(ast1, []*pb.RewriteOption{mk()})
	if g, _ := e.Generate(out1); g != "SELECT a FROM db.t LIMIT 10" {
		t.Fatalf("over threshold got %q", g)
	}
	ast2, _ := e.ParseOne("SELECT a FROM db.t LIMIT 5")
	out2, _ := applyOptions(ast2, []*pb.RewriteOption{mk()})
	if g, _ := e.Generate(out2); g != "SELECT a FROM db.t LIMIT 5" {
		t.Fatalf("under threshold got %q", g)
	}
}
```

- [ ] **Step 6: Run to verify failure**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/handlers -run TestApplyOptions`
Expected: FAIL — `undefined: applyOptions`.

- [ ] **Step 7: Implement the option pipeline**

Create `internal/handlers/options.go`:

```go
package handlers

import (
	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// applyOptions applies LIMIT/OFFSET/SETTINGS options to the AST. Limit/Offset/
// Settings accumulate and apply to the outermost select (select.cc:378-387 only
// touches the first select). TableNameRewrite is handled by the Task-7 walk and
// is ignored here; CommonTableExprRewrite is handled in Task 9.
func applyOptions(ast engine.AST, opts []*pb.RewriteOption) (engine.AST, error) {
	var (
		forceLimit   *int64
		replaceLimit *struct{ threshold, to int64 }
		offset       *int64
		settings     []engine.Setting
	)
	for _, o := range opts {
		switch o.GetOp() {
		case pb.RewriteOp_LimitRewrite:
			switch v := o.GetLimitArgs().GetValue().(type) {
			case *pb.RewriteLimitArgs_ForceLimit:
				n := int64(v.ForceLimit)
				forceLimit = &n
			case *pb.RewriteLimitArgs_ReplaceLimit_:
				replaceLimit = &struct{ threshold, to int64 }{int64(v.ReplaceLimit.GetThreshold()), int64(v.ReplaceLimit.GetReplaceTo())}
			}
		case pb.RewriteOp_OffsetRewrite:
			n := int64(o.GetOffsetArgs().GetOffset())
			offset = &n
		case pb.RewriteOp_SettingsRewrite:
			for _, s := range o.GetSettingsArgs().GetSettings() {
				settings = append(settings, settingToEngine(s))
			}
		}
	}

	// Apply limit (force wins if both present — last-wins is per-op; force is unconditional).
	if forceLimit != nil {
		var err error
		if ast, err = engine.SetLimit(ast, *forceLimit); err != nil {
			return nil, err
		}
	} else if replaceLimit != nil {
		cur, ok, err := engine.GetLimit(ast)
		if err != nil {
			return nil, err
		}
		// Replace iff no limit, original is 0, or original exceeds threshold.
		if !ok || cur == 0 || cur > replaceLimit.threshold {
			if ast, err = engine.SetLimit(ast, replaceLimit.to); err != nil {
				return nil, err
			}
		}
	}
	if offset != nil && *offset > 0 {
		var err error
		if ast, err = engine.SetOffset(ast, *offset); err != nil {
			return nil, err
		}
	}
	if len(settings) > 0 {
		var err error
		if ast, err = engine.SetSettings(ast, settings); err != nil {
			return nil, err
		}
	}
	return ast, nil
}

// settingToEngine maps a proto Setting to an engine.Setting, choosing the literal
// type from the value oneof.
func settingToEngine(s *pb.RewriteSettingsArgs_Setting) engine.Setting {
	switch s.GetValue().(type) {
	case *pb.RewriteSettingsArgs_Setting_StringValue:
		return engine.Setting{Key: s.GetKey(), LiteralType: "string", Value: s.GetStringValue()}
	case *pb.RewriteSettingsArgs_Setting_BoolValue:
		v := "0"
		if s.GetBoolValue() {
			v = "1"
		}
		return engine.Setting{Key: s.GetKey(), LiteralType: "number", Value: v}
	case *pb.RewriteSettingsArgs_Setting_IntValue:
		return engine.Setting{Key: s.GetKey(), LiteralType: "number", Value: itoa64(int64(s.GetIntValue()))}
	case *pb.RewriteSettingsArgs_Setting_Uint64Value:
		return engine.Setting{Key: s.GetKey(), LiteralType: "number", Value: utoa64(s.GetUint64Value())}
	default:
		return engine.Setting{Key: s.GetKey(), LiteralType: "string", Value: ""}
	}
}
```

Add a tiny `internal/handlers/util.go` with `itoa64`/`utoa64` (or use `strconv` inline):

```go
package handlers

import "strconv"

func itoa64(n int64) string  { return strconv.FormatInt(n, 10) }
func utoa64(n uint64) string { return strconv.FormatUint(n, 10) }
```

Then wire `applyOptions` into `RewriteSelect` (Task 7) — after `RewriteSelectTables`, before `Generate`:

```go
	rewritten, err = applyOptions(rewritten, opts)
	if err != nil {
		return nil, err
	}
```

- [ ] **Step 8: Run the option tests + full suite**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine ./internal/handlers . -v`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/engine/nodes.go internal/handlers/options.go internal/handlers/util.go internal/handlers/options_test.go internal/handlers/select.go internal/engine/nodes_test.go
git commit -m "feat(handlers): LIMIT/OFFSET/SETTINGS option pipeline (force/replace/threshold)"
```

---

## Task 9: handlers — CTE injection, scope, failed_cte_aliases

Port the `CommonTableExprRewrite` path (select.cc:713-784): parse each injected CTE body, inject it into the outer select's `with.ctes`, record un-parseable aliases in `failed_cte_aliases`. The injected CTE bodies' tables are then rewritten by the same Task-7 walk (we inject **before** the walk).

**Design divergence (intentional):** C++ applies TableName, then on CTE inlines and *re-applies* TableName. We inject CTEs **first**, then run the single table walk once over the combined tree (the walk recurses into `with.ctes[*].this.select`). Net effect on table rewriting is identical; Task 11's differential harness verifies. This avoids porting the flush/re-apply ordering machinery.

**Files:**
- Modify: `internal/engine/nodes.go` (add `InjectCTEs`)
- Modify: `internal/handlers/select.go` (CTE pre-step), `internal/handlers/options.go` (skip CTE op there)
- Test: `internal/engine/nodes_test.go`, `internal/handlers/select_test.go`

- [ ] **Step 1: Write the failing engine test**

```go
func TestInjectCTEs(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, _ := NewPolyglot("")
	defer e.Close()
	body, _ := e.ParseOne("SELECT * FROM db.src")
	out, err := InjectCTEs(load(t, "select"), map[string]AST{"c": body})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	// select golden = SELECT a FROM db.t WHERE x IN (1,2,3); now prefixed WITH c AS (...).
	want := "WITH c AS (SELECT * FROM db.src) SELECT a FROM db.t WHERE x IN (1, 2, 3)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine -run TestInjectCTEs`
Expected: FAIL — `undefined: InjectCTEs`.

- [ ] **Step 3: Implement InjectCTEs**

Append to `internal/engine/nodes.go`:

```go
// InjectCTEs prepends named CTEs (alias → body select AST) to the outer select's
// WITH clause, creating the clause if absent. Insertion order is sorted by alias
// for determinism. Mirrors ASTRewriteCTETransformer (ASTTransformers.cc:208-356).
func InjectCTEs(ast AST, bodies map[string]AST) (AST, error) {
	if len(bodies) == 0 {
		return ast, nil
	}
	sel, encode, err := outerSelect(ast)
	if err != nil {
		return nil, err
	}
	with, _ := sel["with"].(map[string]any)
	if with == nil {
		with = map[string]any{"ctes": []any{}, "recursive": false, "leading_comments": []any{}}
	}
	ctes, _ := with["ctes"].([]any)

	aliases := make([]string, 0, len(bodies))
	for a := range bodies {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		var bodyNode any
		if err := json.Unmarshal(bodies[alias], &bodyNode); err != nil {
			return nil, fmt.Errorf("engine: decode cte %q: %w", alias, err)
		}
		ctes = append(ctes, map[string]any{
			"alias":        ident(alias),
			"this":         bodyNode,
			"columns":      []any{},
			"materialized": nil,
			"alias_first":  true,
		})
	}
	with["ctes"] = ctes
	sel["with"] = with
	return encode()
}
```

Add `"sort"` to `internal/engine/nodes.go` imports.

- [ ] **Step 4: Run engine test**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine -run TestInjectCTEs -v`
Expected: PASS (adjust `want` spacing to the generator).

- [ ] **Step 5: Write the failing handler CTE test**

In `internal/handlers/select_test.go`:

```go
func TestRewriteSelect_cteInjectAndFailedAliases(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT * FROM c")
	opts := []*pb.RewriteOption{{Op: pb.RewriteOp_CommonTableExprRewrite,
		Value: &pb.RewriteOption_CommonTableExprArgs{CommonTableExprArgs: &pb.RewriteCommonTableExprArgs{
			CteMap: map[string]*pb.RewriteCommonTableExprArgs_CommonTableExpr{
				"c":   {Alias: "c", Sql: "SELECT 1"},
				"bad": {Alias: "bad", Sql: "SELECT FROM WHERE )("}, // unparseable
			}}}}}
	resp, err := RewriteSelect(e, ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	// `c` injected → `c` is now an in-scope alias, not rewritten as a table.
	if got := resp.GetSqlAfterRewrite(); got == "" {
		t.Fatal("empty sql")
	}
	// `bad` failed to parse → recorded.
	if len(resp.GetFailedCteAliases()) != 1 || resp.GetFailedCteAliases()[0] != "bad" {
		t.Fatalf("failed_cte_aliases = %v, want [bad]", resp.GetFailedCteAliases())
	}
}
```

- [ ] **Step 6: Run to verify failure**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/handlers -run TestRewriteSelect_cte`
Expected: FAIL (failed_cte_aliases empty; CTE not injected).

- [ ] **Step 7: Add the CTE pre-step to RewriteSelect**

In `internal/handlers/select.go`, insert **before** `CollectSelectTables` (so injected bodies are walked + their aliases scoped):

```go
	// CTE injection (CommonTableExprRewrite): parse + inject bodies, record failures.
	bodies := map[string]engine.AST{}
	for _, o := range opts {
		if o.GetOp() != pb.RewriteOp_CommonTableExprRewrite {
			continue
		}
		for alias, cte := range o.GetCommonTableExprArgs().GetCteMap() {
			body, perr := e.ParseOne(cte.GetSql())
			if perr != nil {
				resp.FailedCteAliases = append(resp.FailedCteAliases, alias)
				continue
			}
			bodies[alias] = body
		}
	}
	if len(bodies) > 0 {
		var ierr error
		if ast, ierr = engine.InjectCTEs(ast, bodies); ierr != nil {
			return nil, ierr
		}
	}
	sort.Strings(resp.FailedCteAliases) // deterministic order
```

(`resp` is already constructed above; ensure `FailedCteAliases` ordering is stable.) Also ensure `applyOptions` ignores `CommonTableExprRewrite` (it already only matches Limit/Offset/Settings — confirm no default branch touches it).

- [ ] **Step 8: Run the CTE + full suite**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine ./internal/handlers . -v`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/engine/nodes.go internal/handlers/select.go internal/handlers/select_test.go internal/engine/nodes_test.go
git commit -m "feat(handlers): CTE injection + scope + failed_cte_aliases"
```

---

## Task 10: engine — GLOBAL cross-shard pass

Port `forceGlobalForRemoteAsymmetry` (select.cc:425-563): bottom-up, per-select. (A) Promote `JOIN`→`GLOBAL JOIN` (via the left-table alias quirk) when accumulated locality is remote/local-asymmetric. (B) When a select's FROM has a remote source, promote `IN`→`GLOBAL IN` (set `in.global=true`) where the IN's RHS has a local source. Runs **after** table rewriting (so introduced `remote()` calls are visible).

**Files:**
- Create: `internal/engine/global.go`
- Test: `internal/engine/global_test.go`
- Modify: `internal/handlers/select.go` (call after options)

- [ ] **Step 1: Write the failing tests (synthesis-backed)**

```go
package engine

import (
	"os"
	"testing"
)

func TestForceGlobal_joinRemoteLocalAsymmetry(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, _ := NewPolyglot("")
	defer e.Close()
	// remote() (left) JOIN local (right) → asymmetric → GLOBAL JOIN.
	ast, _ := e.ParseOne("SELECT * FROM remote('h', d, t, 'u', 'p') JOIN local_tbl ON x = y")
	out, err := ForceGlobalForRemoteAsymmetry(ast)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	if !contains(got, "GLOBAL JOIN") {
		t.Fatalf("expected GLOBAL JOIN, got %q", got)
	}
}

func TestForceGlobal_inWithRemoteFrom(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, _ := NewPolyglot("")
	defer e.Close()
	// FROM is remote; IN's RHS is a local table → GLOBAL IN.
	ast, _ := e.ParseOne("SELECT x FROM remote('h', d, t, 'u', 'p') WHERE x IN (SELECT z FROM local_tbl)")
	out, err := ForceGlobalForRemoteAsymmetry(ast)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	if !contains(got, "GLOBAL IN") {
		t.Fatalf("expected GLOBAL IN, got %q", got)
	}
}

func TestForceGlobal_noRemoteNoChange(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, _ := NewPolyglot("")
	defer e.Close()
	ast, _ := e.ParseOne("SELECT * FROM a JOIN b ON a.id = b.id")
	out, err := ForceGlobalForRemoteAsymmetry(ast)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	if contains(got, "GLOBAL") {
		t.Fatalf("no remote source → no GLOBAL, got %q", got)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
```

(Add `"strings"` import to the test file.)

- [ ] **Step 2: Run to verify failure**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine -run TestForceGlobal`
Expected: FAIL — `undefined: ForceGlobalForRemoteAsymmetry`.

- [ ] **Step 3: Implement the GLOBAL pass**

Create `internal/engine/global.go`:

```go
package engine

import (
	"encoding/json"
	"fmt"
)

var remoteFuncNames = map[string]bool{
	"remote": true, "remoteSecure": true, "cluster": true, "clusterAllReplicas": true,
}
var inFamily = map[string]bool{"in": true, "notIn": true, "nullIn": true, "notNullIn": true}

// ForceGlobalForRemoteAsymmetry promotes JOIN→GLOBAL JOIN and IN→GLOBAL IN where a
// remote/local asymmetry exists. Bottom-up, per select. select.cc:505-563.
func ForceGlobalForRemoteAsymmetry(ast AST) (AST, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode select: %w", err)
	}
	globalWalk(root, nil)
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode select: %w", err)
	}
	return AST(out), nil
}

// globalWalk recurses bottom-up; at each `select` it runs the two promotion passes.
func globalWalk(node any, scope map[string]bool) {
	switch n := node.(type) {
	case map[string]any:
		if sel, ok := n["select"].(map[string]any); ok {
			scope = forkCTEScope(sel, scope)
			for _, v := range sel { // children first (bottom-up)
				globalWalk(v, scope)
			}
			promoteJoins(sel, scope)
			promoteINs(sel, scope)
			return
		}
		for _, v := range n {
			globalWalk(v, scope)
		}
	case []any:
		for _, v := range n {
			globalWalk(v, scope)
		}
	}
}

// promoteJoins walks FROM+joins left→right; sets a join GLOBAL when accumulated
// locality is asymmetric with the current element. select.cc:526-547.
func promoteJoins(sel map[string]any, scope map[string]bool) {
	from, _ := sel["from"].(map[string]any)
	joins, _ := sel["joins"].([]any)
	if from == nil || len(joins) == 0 {
		return
	}
	fexprs, _ := from["expressions"].([]any)
	accRemote, accLocal := false, false
	for _, fe := range fexprs { // the FROM element(s) seed the accumulators
		accRemote = accRemote || subtreeHasRemoteSource(fe)
		accLocal = accLocal || subtreeHasLocalSource(fe, scope)
	}
	for _, j := range joins {
		jm, ok := j.(map[string]any)
		if !ok {
			continue
		}
		thisRemote := subtreeHasRemoteSource(jm["this"])
		thisLocal := subtreeHasLocalSource(jm["this"], scope)
		mixed := (accRemote && thisLocal) || (accLocal && thisRemote)
		if mixed {
			markJoinGlobal(jm, sel)
		}
		accRemote = accRemote || thisRemote
		accLocal = accLocal || thisLocal
	}
}

// markJoinGlobal sets GLOBAL on a join by giving its LEFT table the alias "GLOBAL"
// (polyglot's representation), but only when that table has no real alias. The
// "left table" of join j is the preceding FROM/join element. To keep it simple and
// match the common single-join case, we tag the FROM table for the first join and
// the previous join's table otherwise. select.cc sets locality on the ASTTableJoin;
// polyglot lacks that field, so we use the alias quirk (verified 2026-06-08).
func markJoinGlobal(join, sel map[string]any) {
	left := leftTableOf(join, sel)
	if left == nil {
		return
	}
	if left["alias"] != nil { // never clobber a real alias
		return
	}
	left["alias"] = map[string]any{"name": "GLOBAL", "quoted": false, "trailing_comments": []any{},
		// alias_explicit_as=false is the default; GLOBAL renders as the join modifier.
	}
}

// leftTableOf returns the table node immediately to the left of `join`. For the
// first join that is the FROM table; otherwise the previous join's table.
func leftTableOf(join, sel map[string]any) map[string]any {
	joins, _ := sel["joins"].([]any)
	// find index of join
	idx := -1
	for i, j := range joins {
		if jm, ok := j.(map[string]any); ok && sameMap(jm, join) {
			idx = i
			break
		}
	}
	if idx <= 0 {
		from, _ := sel["from"].(map[string]any)
		fexprs, _ := from["expressions"].([]any)
		if len(fexprs) > 0 {
			if fe, ok := fexprs[0].(map[string]any); ok {
				if tbl, ok := fe["table"].(map[string]any); ok {
					return tbl
				}
			}
		}
		return nil
	}
	if prev, ok := joins[idx-1].(map[string]any); ok {
		if this, ok := prev["this"].(map[string]any); ok {
			if tbl, ok := this["table"].(map[string]any); ok {
				return tbl
			}
		}
	}
	return nil
}

func sameMap(a, b map[string]any) bool { return &a != nil && fmt.Sprintf("%p", a) == fmt.Sprintf("%p", b) }

// promoteINs: if the select's FROM has a remote source, set in.global=true on any
// IN-family node (anywhere except FROM/WITH) whose RHS has a local source.
// select.cc:550-562 + forceGlobalInWalk:474-497.
func promoteINs(sel map[string]any, scope map[string]bool) {
	from := sel["from"]
	if !subtreeHasRemoteSource(from) {
		return
	}
	for k, v := range sel {
		if k == "from" || k == "with" {
			continue
		}
		globalInWalk(v, scope)
	}
}

// globalInWalk sets in.global=true where the IN's RHS subtree has a local source.
// Stops at nested selects/subqueries (handled at their own scope by globalWalk).
func globalInWalk(node any, scope map[string]bool) {
	switch n := node.(type) {
	case map[string]any:
		if _, ok := n["select"]; ok {
			return // a nested select — its scope is handled elsewhere
		}
		if in, ok := n["in"].(map[string]any); ok {
			rhsLocal := false
			if exprs, ok := in["expressions"].([]any); ok {
				for _, ex := range exprs {
					rhsLocal = rhsLocal || subtreeHasLocalSource(ex, scope)
				}
			}
			if q := in["query"]; q != nil {
				rhsLocal = rhsLocal || subtreeHasLocalSource(q, scope)
			}
			if rhsLocal {
				in["global"] = true
				n["in"] = in
			}
		}
		for _, v := range n {
			globalInWalk(v, scope)
		}
	case []any:
		for _, v := range n {
			globalInWalk(v, scope)
		}
	}
}

// subtreeHasRemoteSource reports whether any descendant is a remote/cluster function.
func subtreeHasRemoteSource(node any) bool {
	found := false
	visit(node, func(m map[string]any) {
		if fn, ok := m["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); remoteFuncNames[name] {
				found = true
			}
		}
	})
	return found
}

// subtreeHasLocalSource reports whether any descendant is a real (non-CTE) table.
func subtreeHasLocalSource(node any, scope map[string]bool) bool {
	found := false
	visit(node, func(m map[string]any) {
		if tbl, ok := m["table"].(map[string]any); ok {
			tt := decodeTableTarget(tbl)
			if !(tt.DB == "" && scope[tt.Table]) {
				found = true
			}
		}
	})
	return found
}

// visit calls fn on every map node in the subtree (pre-order).
func visit(node any, fn func(map[string]any)) {
	switch n := node.(type) {
	case map[string]any:
		fn(n)
		for _, v := range n {
			visit(v, fn)
		}
	case []any:
		for _, v := range n {
			visit(v, fn)
		}
	}
}
```

> **`sameMap` correctness note:** decoded JSON `map[string]any` values are reference types, so two `joins[i]` and the `join` arg passed to `markJoinGlobal` are the **same** underlying map; pointer-identity via `fmt.Sprintf("%p")` is a pragmatic identity check. If review prefers, thread the index through `promoteJoins`→`markJoinGlobal` instead of re-finding it (cleaner; do this if `sameMap` reads as hacky). Replace `leftTableOf(join,...)` with `leftTableByIndex(sel, idx)` and pass `idx` from the `promoteJoins` loop.

**Preferred cleanup (apply during implementation):** thread the index, drop `sameMap`:

```go
// in promoteJoins:
	for i, j := range joins {
		...
		if mixed {
			markJoinGlobalAt(sel, i)
		}
	}

func markJoinGlobalAt(sel map[string]any, joinIdx int) {
	left := leftTableByIndex(sel, joinIdx)
	if left == nil || left["alias"] != nil {
		return
	}
	left["alias"] = map[string]any{"name": "GLOBAL", "quoted": false, "trailing_comments": []any{}}
}

func leftTableByIndex(sel map[string]any, joinIdx int) map[string]any {
	if joinIdx == 0 {
		from, _ := sel["from"].(map[string]any)
		fexprs, _ := from["expressions"].([]any)
		if len(fexprs) > 0 {
			if fe, ok := fexprs[0].(map[string]any); ok {
				if tbl, ok := fe["table"].(map[string]any); ok {
					return tbl
				}
			}
		}
		return nil
	}
	joins, _ := sel["joins"].([]any)
	if prev, ok := joins[joinIdx-1].(map[string]any); ok {
		if this, ok := prev["this"].(map[string]any); ok {
			if tbl, ok := this["table"].(map[string]any); ok {
				return tbl
			}
		}
	}
	return nil
}
```

Use the index-threaded version; delete `markJoinGlobal`, `leftTableOf`, and `sameMap`.

- [ ] **Step 4: Run the GLOBAL tests**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine -run TestForceGlobal -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Wire into the handler**

In `internal/handlers/select.go`, after `applyOptions`, before `Generate`:

```go
	rewritten, err = engine.ForceGlobalForRemoteAsymmetry(rewritten)
	if err != nil {
		return nil, err
	}
```

- [ ] **Step 6: Run full suite**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./... -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/engine/global.go internal/engine/global_test.go internal/handlers/select.go
git commit -m "feat(engine): GLOBAL cross-shard pass (JOIN locality + IN-family promotion)"
```

---

## Task 11: Differential harness — golden SELECT corpus + oracle gating

Port the SELECT cases from `rewriter-grpc/tests/rewriter_test.cc` as Go table tests, and gate the native output against the C++ oracle (env-gated via `REWRITER_ORACLE_ADDR`, already scaffolded in `internal/harness`). This is the parity gate for Phase 1.

**Files:**
- Create: `internal/harness/select_golden_test.go`
- Create: `internal/harness/testdata/select_cases.json` (ported cases: input SQL + options + expected response fields)
- Modify: `internal/harness/compare.go` if a field comparator is missing (e.g. allow-list for `IN`-split, §6f — N/A for SELECT unless a case hits it)

- [ ] **Step 1: Extract the SELECT cases from the C++ gtest**

Read `rewriter-grpc/tests/rewriter_test.cc`; collect every `TEST*` whose input is a SELECT (or whose statement_type is SELECT). For each, capture: input SQL, the `RewriteOption`s built, and the expected `sql_after_rewrite` / `table_rewrites` / `original_accessed_tables` / `code`. Encode as JSON in `internal/harness/testdata/select_cases.json`:

```json
[
  {
    "name": "dynamic_qualified_rename",
    "sql": "SELECT a FROM tenant1.events",
    "dynamic": {"database_map": {"tenant1": "testnet"}, "delim": "_"},
    "want_sql": "SELECT a FROM testnet.`tenant1.events`",
    "want_table_rewrites": {"tenant1.events": "testnet.tenant1.events"},
    "want_code": "Success"
  }
]
```

(Start with ~10 representative cases spanning: qualified/unqualified dynamic rename, static table_map, remote_table_map, JOIN, CTE, LIMIT, GLOBAL JOIN, GLOBAL IN. Expand to the full set as the suite goes green.)

- [ ] **Step 2: Write the golden table test**

```go
package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/handlers"
)

type selectCase struct {
	Name              string             `json:"name"`
	SQL               string             `json:"sql"`
	Dynamic           *dynJSON           `json:"dynamic"`
	WantSQL           string             `json:"want_sql"`
	WantTableRewrites map[string]string  `json:"want_table_rewrites"`
	WantCode          string             `json:"want_code"`
}
type dynJSON struct {
	DatabaseMap map[string]string `json:"database_map"`
	Delim       string            `json:"delim"`
}

func TestSelectGolden(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	b, err := os.ReadFile(filepath.Join("testdata", "select_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []selectCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	e, _ := engine.NewPolyglot("")
	defer e.Close()
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			ast, err := e.ParseOne(c.SQL)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var opts []*pb.RewriteOption
			if c.Dynamic != nil {
				opts = []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
					Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
						DynamicArgs: &pb.RewriteTableDynamicArgs{DatabaseMap: c.Dynamic.DatabaseMap, Delim: c.Dynamic.Delim}}}}}
			}
			resp, err := handlers.RewriteSelect(e, ast, opts)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if c.WantSQL != "" && resp.GetSqlAfterRewrite() != c.WantSQL {
				t.Errorf("sql:\n got %q\nwant %q", resp.GetSqlAfterRewrite(), c.WantSQL)
			}
			for k, v := range c.WantTableRewrites {
				if resp.GetTableRewrites()[k] != v {
					t.Errorf("table_rewrites[%q] = %q want %q", k, resp.GetTableRewrites()[k], v)
				}
			}
		})
	}
}
```

- [ ] **Step 3: Run; reconcile `want_sql` with polyglot's generator**

Run: `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/harness -run TestSelectGolden -v`
Expected: Initially some FAIL where `want_sql` (copied from C++/ClickHouse formatter) differs cosmetically from polyglot's formatter (spacing, backtick rules). For each: decide if it's **semantic** (real bug — fix the code) or **cosmetic** (formatter difference — update `want_sql` to polyglot's output, since §7 compares SELECT SQL *semantically*). Document any intentional divergence inline in the JSON via a `"note"` field.

- [ ] **Step 4: Add the oracle differential (env-gated)**

Extend the test: when `REWRITER_ORACLE_ADDR` is set, also call the C++ oracle (via the existing `internal/harness/oracle.go` client) with the same SQL+options, and compare field-by-field using `internal/harness/compare.go`. Skip when unset so CI without the oracle still runs the golden half.

```go
	if addr := os.Getenv("REWRITER_ORACLE_ADDR"); addr != "" {
		oracleResp := callOracle(t, addr, c.SQL, opts) // existing harness client
		if diffs := Compare(resp, oracleResp); len(diffs) > 0 {
			t.Errorf("oracle divergence: %v", diffs)
		}
	}
```

- [ ] **Step 5: Run the whole suite (pure-Go + engine)**

Run: `go test ./...` (pure units) then `make test` (engine).
Expected: PASS. With the oracle running locally: `REWRITER_ORACLE_ADDR=localhost:50051 make test` → SELECT parity green.

- [ ] **Step 6: Update the Phase-0 status docs**

Update `README.md:14-18` (Status) to note Phase 1 (SELECT) complete and harness-gated. Add a one-paragraph "Phase 1 notes" pointing at this plan and the GLOBAL alias-quirk.

- [ ] **Step 7: Commit**

```bash
git add internal/harness/select_golden_test.go internal/harness/testdata/select_cases.json README.md
git commit -m "test(harness): SELECT golden corpus + env-gated C++ oracle differential (Phase 1 parity gate)"
```

---

## Self-Review (run by the plan author; fixes applied inline above)

**1. Spec coverage (design §4-§7 SELECT row + §6 hazards):**
- dispatch (classify→route) → Task 7 (native.go routing). ✓
- nameresolve (static 3-map + dynamic + precedence) → Tasks 4–6. ✓
- handlers/select (table walk, CTE scope, option pipeline) → Tasks 7–9. ✓
- globalpass (forceGlobalForRemoteAsymmetry) → Task 10. ✓
- response (table_rewrites, original_accessed_tables, statement_type, failed_cte_aliases) → Tasks 7, 9. ✓ (database_rewrites/privileges_deltas: N/A for SELECT, per research §6.2/§6 — correctly omitted.)
- §6a INSERT VALUES splice → Phase 2 (not SELECT). Out of scope. ✓
- §6b synthetic SQL → Phase 3/4. Out of scope. ✓
- §6c GLOBAL pass → Task 10. ✓
- §6d remote() construction → Task 3 (`remoteFunc`) + Tasks 5/7. ✓
- §6e formatting parity → Task 11 semantic compare + cosmetic `want_sql` reconciliation. ✓
- §6f IN-split → intentionally skipped (allow-list); no SELECT case should need it; Task 11 note. ✓
- §6g CTE scope + failed_cte_aliases → Task 9. ✓
- `RewriteErrorMessage` → Phase 5. Out of scope. ✓
- Session overlay (`upstream_logical_database_in_context` from USE) → Phase 3; Phase 1 reads it from the option directly. Noted in Task 7. ✓

**2. Placeholder scan:** No "TBD"/"implement later". The remaining "copy polyglot's exact spacing into `want`" notes are explicit reconciliation steps (Tasks 3, 8, 9, 11), not hand-waving — they exist because polyglot's generator formatting is authoritative and must be observed once. The `sameMap`/`_alias_unused` illustrative code is explicitly replaced by the preferred index-threaded / minimal forms in the same task.

**3. Type consistency:**
- `engine.TableTarget{DB,Table,Alias}`, `engine.TableDecision{Action,NewDB,NewTable,Remote}`, `engine.RemoteSpec{Addr,DB,Table,User,Password}`, `engine.Setting{Key,LiteralType,Value}` — defined once, used identically in Tasks 2,3,7,8.
- `nameresolve.Outcome{Status,PhysicalDB,NewTable,LogicalDB,RemoteAddr,RemoteUser,RemotePassword,RejectReason}`, `Selection{Mode,Static,Dynamic}`, `Accessed{LogicalDB,PhysicalDB,IsRemote}`, `Status*`, `Mode*` — defined in the canonical block, used in Tasks 4–7.
- `engine.RewriteSelectTables(ast, func(TableTarget) TableDecision)` — signature stable across Tasks 3 and 7.
- pb identifiers verified against `gen/pb/rewriter.pb.go` 2026-06-08: `RewriteOp_{TableName,Limit,Offset,Settings,CommonTableExpr}Rewrite`, `RewriteCode_{Success,…}`, `StatementType_STATEMENT_TYPE_SELECT`, `RewriteOption_{TableNameArgs,LimitArgs,…}` oneof wrappers, `RewriteLimitArgs_{ForceLimit,ReplaceLimit_}`, `RewriteSettingsArgs_Setting_{String,Bool,Int,Uint64}Value`, `RewriteTableStaticArgs_{RemoteTable,TableWithDatabase}`, `RewriteTableDynamicArgs_RemoteUpstream`, `AccessedTable{OriginalDatabase,…,IsRemote}`. ✓

**4. Sequencing:** engine primitives (Tasks 1–3) → policy (4–6) → orchestration (7) → layered features (8–10) → parity gate (11). Each task ends green and committed; the harness (11) can run partial after Task 7. ✓
