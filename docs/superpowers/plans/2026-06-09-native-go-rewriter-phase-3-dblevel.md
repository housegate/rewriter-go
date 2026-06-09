# Native Go Rewriter — Phase 3 (db-level) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port the database-level handlers — `USE`, `SHOW TABLES`, `SHOW DATABASES`, `CREATE DATABASE`, `DROP DATABASE` — from `rewriter-grpc` (`src/handlers/{use,show_tables,show_databases}.cc` + the CREATE/DROP DATABASE branches of `writes.cc`) to native Go with full behavioral parity.

**Architecture:** db-level statements are mostly **synthetic SQL** (§6b) — `USE <physical>`, a `SELECT … FROM system.tables` enumeration, a `SELECT '<logical>' AS name` UNION, or a `SELECT '<original>' AS cdstmt/ddstmt` debug echo. None round-trip the input, so the parity risk is low: port the C++ string construction + single-quote escaping **exactly**. USE/SHOW parse to opaque `command` nodes under ClickHouse; rather than the §3.1 generic-dialect re-parse (which polyglot **fails** to parse for `NOT LIKE`/`NOT ILIKE`/`IN`), we extract structure from the **clickhouse `Tokenize` stream** (verified to handle every form), reusing the Phase-2 tier-C token-scanning approach. CREATE/DROP DATABASE are already structured `create_database`/`drop_database` nodes (flat `name.name`).

**Tech Stack:** Go (CGO_ENABLED=0), `tobilg/polyglot` via PureGo FFI, `gen/pb` protobuf types, TDD with `go test`.

**Scope:** USE + SHOW TABLES + SHOW DATABASES + CREATE DATABASE + DROP DATABASE. This **completes** the CREATE/DROP DATABASE work deferred from Phase 2 (the `dispatchDatabaseOutOfPhase` stub is removed). GRANT/REVOKE/EXISTS/SHOW CREATE remain Phase 4.

---

## Empirically verified shapes (2026-06-09, against the live engine)

**USE/SHOW — clickhouse `Tokenize` stream** (token = `{token_type, text, span}`; identifiers `VAR`/`QUOTED_IDENTIFIER` (backticks stripped), keywords own types):
- `USE mydb` → `USE VAR(mydb)`; `USE \`weird db\`` → `USE QUOTED_IDENTIFIER(weird db)`.
- `SHOW TABLES FROM mydb LIKE 'a%'` → `SHOW VAR(TABLES) FROM(FROM) VAR(mydb) LIKE(LIKE) STRING(a%)`.
- `SHOW TABLES IN mydb NOT LIKE 'b%'` → `SHOW VAR(TABLES) IN(IN) VAR(mydb) NOT(NOT) LIKE(LIKE) STRING(b%)`.
- `SHOW DATABASES NOT ILIKE 'x%'` → `SHOW VAR(DATABASES) NOT(NOT) I_LIKE(ILIKE) STRING(x%)`.
- `SHOW CLUSTERS` → `SHOW VAR(CLUSTERS)`; `SHOW DICTIONARIES` → `SHOW VAR(DICTIONARIES)`.
- The SHOW-kind keyword (TABLES/DATABASES/CLUSTERS/DICTIONARIES/SETTINGS/MERGES/CACHES) lexes as **`VAR`** immediately after `SHOW`. The FROM/IN db is the `VAR`/`QUOTED_IDENTIFIER` after `FROM`/`IN`. The LIKE clause is `[NOT] (LIKE|I_LIKE) STRING`.

**CREATE/DROP DATABASE — structured nodes** (Phase-2 probe):
- `create_database`: `{ "name": {"name":"db","quoted":false}, "if_not_exists": bool, … }` — db = `create_database.name.name`.
- `drop_database`: `{ "name": {"name":"db"}, "if_exists": bool, "sync": bool }` — db = `drop_database.name.name`.

---

## C++ behavior reference (the parity target)

**USE** (`use.cc`): no dynamic_args → passthrough (`USE <db>`, type USE). Else `resolvePhysicalDatabase(origin)` → unresolvable → **InvalidRewriteRequest**. `isLogicalRemoteMapped(origin)` → **UnsupportedStatement** (USE has no remote analog). physical != origin → `USE <physical>`, type USE, **recordDatabaseRewrite(origin→physical)**. else → passthrough.

**SHOW TABLES** (`show_tables.cc`): SHOW CLUSTERS/DICTIONARIES/SETTINGS/MERGES/CACHES → passthrough (type SHOW_TABLES). No dynamic → passthrough. from_logical = FROM-db (db part before any dot); logical = from_logical or `upstream_logical_database_in_context`; empty → **InvalidRewriteRequest**. `resolvePhysicalDatabase` → unresolvable → **InvalidRewriteRequest**. `system_tables_source` = `system.tables`, OR `remote('<addr>', system, tables, '<user>', '<password>')` when logical is remote-mapped (missing upstream key → **InvalidRewriteRequest**). prefix = `buildDynamicTablePrefix(logical)`. Output:
```
SELECT multiIf(startsWith(name, '<prefix>'), substring(name, length('<prefix>') + 1), name) AS name FROM (SELECT name FROM <source> WHERE database = '<physical>' AND startsWith(name, '<prefix>'))
```
type SHOW_TABLES, **recordDatabaseRewrite(logical→physical)**. (NOTE: C++ **ignores** the original LIKE clause for SHOW TABLES.)

**SHOW DATABASES** (`show_databases.cc`): no dynamic → passthrough. Sort `database_map` by logical; for each whose physical ∈ `known_physical_databases`: subquery `SELECT '<logical>' AS name` + **recordDatabaseRewrite(logical→physical)**. body = subqueries joined by ` UNION ALL `, or `SELECT '' AS name WHERE 0` when none. Output: `SELECT name FROM (<body>)<like> ORDER BY name`, where `<like>` = ` WHERE name <op> '<pat>'` (op ∈ LIKE/NOT LIKE/ILIKE/NOT ILIKE) or "" when no LIKE. type SHOW_DATABASES.

**CREATE DATABASE** (`writes.cc:290-344`): no dynamic → **UnsupportedStatement** ("CREATE DATABASE requires a TableNameRewrite/dynamic_args option to validate against"). `recordAccessedDatabase(target_db)`. db_map hit && !if_not_exists → **InvalidRewriteRequest** ("CREATE DATABASE target '<db>' already exists (mapped to physical '<phys>'); use IF NOT EXISTS to suppress this error"). If `has_upstream_physical_database_in_context` && phys != "" && phys ∉ known_physical → **InvalidRewriteRequest** ("CREATE DATABASE: upstream_physical_database_in_context '<phys>' is not in known_physical_databases"). Else debug rewrite `SELECT '<escaped formatAst>' AS cdstmt`, type CREATE_DATABASE.

**DROP DATABASE** (`writes.cc:423-461`): no dynamic → **UnsupportedStatement** ("DROP DATABASE requires a TableNameRewrite/dynamic_args option to validate against"). `recordAccessedDatabase(target_db)`. db_map MISS && !if_exists → **InvalidRewriteRequest** ("DROP DATABASE target '<db>' is not in database_map; use IF EXISTS to suppress this error"). On db_map HIT → **recordDatabaseRewrite(target→physical)**. Debug rewrite `SELECT '<escaped formatAst>' AS ddstmt`, type DROP_DATABASE.

**`recordAccessedDatabase`** (name_rewrite.h:254-276): append one `AccessedTable{ original_database=target_db, original_table="", logical_database=target_db, physical_database=resolvePhysicalDatabase(target_db) or "", is_remote=false }`.

**`recordDatabaseRewrite`** (name_rewrite.h:291): append `{origin_db → new_db}` to `database_rewrites`; no-op when origin == new.

---

## File Structure

**Create:**
- `internal/engine/dblevel.go` + `dblevel_test.go` — Tokenize-based `ParseDBLevel` (USE/SHOW extraction) + `DatabaseTarget` (create/drop-db accessor).
- `internal/handlers/dblevel.go` + `dblevel_test.go` — `RewriteDBLevel` dispatch + the five handlers + `recordDatabaseRewrite`/`recordAccessedDatabase` + synthetic-SQL builders + `escapeSQLLiteral`.
- `internal/harness/testdata/dblevel_cases.json` + `internal/harness/dblevel_golden_test.go` — differential corpus + oracle gate.

**Modify:**
- `internal/nameresolve/resolve.go` — add `FindDynamicArgs`, `ResolvePhysicalDatabase` (exported), `IsLogicalRemoteMapped`, `BuildDynamicTablePrefix`.
- `internal/handlers/writes.go` — remove `dispatchDatabaseOutOfPhase` + the `NodeCreateDB/NodeDropDB` switch case (they now route to `RewriteDBLevel`).
- `native.go` — route `handlers.RewriteDBLevel` after `RewriteWrite`, before SELECT.

## Test conventions
Reuse the Phase-1/2 helpers: engine pkg `newTestEngine(t)` + `sqlEq(t,e,a,b)`; handlers pkg `newEngine(t)`, `mustParse(t,e,sql)`, `statOpt(...)`, `dynOpt(...)`, `sqlSemEq(t,e,got,want)`, `mapEq`. The harness mirrors `select_golden_test.go`/`writes_golden_test.go`.

---

### Task 1: Engine — db-level extraction (`ParseDBLevel` + `DatabaseTarget`)

**Files:** Create `internal/engine/dblevel.go`, `internal/engine/dblevel_test.go`.

- [ ] **Step 1: Write failing tests** (engine pkg; reuse `newTestEngine`)

```go
func TestParseDBLevel(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql      string
		kind     DBLevelKind
		showWhat string
		db       string
		hasLike  bool
		like     string
		likeNot  bool
		likeCI   bool
	}{
		{"USE mydb", DBUse, "", "mydb", false, "", false, false},
		{"USE `weird db`", DBUse, "", "weird db", false, "", false, false},
		{"SHOW TABLES", DBShow, "TABLES", "", false, "", false, false},
		{"SHOW TABLES FROM mydb", DBShow, "TABLES", "mydb", false, "", false, false},
		{"SHOW TABLES IN mydb", DBShow, "TABLES", "mydb", false, "", false, false},
		{"SHOW TABLES FROM mydb LIKE 'a%'", DBShow, "TABLES", "mydb", true, "a%", false, false},
		{"SHOW DATABASES", DBShow, "DATABASES", "", false, "", false, false},
		{"SHOW DATABASES LIKE 'pre%'", DBShow, "DATABASES", "", true, "pre%", false, false},
		{"SHOW DATABASES NOT LIKE 'x%'", DBShow, "DATABASES", "", true, "x%", true, false},
		{"SHOW DATABASES NOT ILIKE 'y%'", DBShow, "DATABASES", "", true, "y%", true, true},
		{"SHOW DATABASES ILIKE 'z%'", DBShow, "DATABASES", "", true, "z%", false, true},
		{"SHOW CLUSTERS", DBShow, "CLUSTERS", "", false, "", false, false},
		{"SHOW DICTIONARIES", DBShow, "DICTIONARIES", "", false, "", false, false},
		{"SELECT 1", DBNone, "", "", false, "", false, false},
	}
	for _, c := range cases {
		got, err := ParseDBLevel(e, c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		if got.Kind != c.kind || got.ShowWhat != c.showWhat || got.DB != c.db ||
			got.HasLike != c.hasLike || got.Like != c.like || got.LikeNot != c.likeNot || got.LikeCaseInsensitive != c.likeCI {
			t.Errorf("%q: got %+v", c.sql, got)
		}
	}
}

func TestDatabaseTarget(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql         string
		db          string
		ifNotExists bool
		ifExists    bool
	}{
		{"CREATE DATABASE db", "db", false, false},
		{"CREATE DATABASE IF NOT EXISTS db", "db", true, false},
		{"DROP DATABASE db", "db", false, false},
		{"DROP DATABASE IF EXISTS db", "db", false, true},
	}
	for _, c := range cases {
		ast, err := e.ParseOne(c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		db, ine, ie, err := DatabaseTarget(ast)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		if db != c.db || ine != c.ifNotExists || ie != c.ifExists {
			t.Errorf("%q: db=%q ine=%v ie=%v", c.sql, db, ine, ie)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure** (undefined `ParseDBLevel`/`DatabaseTarget`/`DBLevelKind`).

- [ ] **Step 3: Implement `internal/engine/dblevel.go`**

```go
package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DBLevelKind classifies a database-level statement.
type DBLevelKind int

const (
	DBNone DBLevelKind = iota // not a USE/SHOW statement
	DBUse                     // USE <db>
	DBShow                    // SHOW <what> [FROM/IN <db>] [[NOT] (I)LIKE '<pat>']
)

// DBLevelInfo is the extracted structure of a USE/SHOW statement.
type DBLevelInfo struct {
	Kind                DBLevelKind
	ShowWhat            string // SHOW: "TABLES"/"DATABASES"/"CLUSTERS"/... (uppercased); "" otherwise
	DB                  string // USE db, or SHOW's FROM/IN db; "" when absent
	HasLike             bool
	Like                string // LIKE pattern (unquoted)
	LikeNot             bool   // NOT (I)LIKE
	LikeCaseInsensitive bool   // ILIKE
}

type dbToken struct {
	TokenType string `json:"token_type"`
	Text      string `json:"text"`
}

// ParseDBLevel extracts USE/SHOW structure from the clickhouse Tokenize stream.
// Returns Kind==DBNone for anything that isn't a leading USE/SHOW. Robust to the
// forms polyglot's generic parser rejects (NOT LIKE / NOT ILIKE / IN).
func ParseDBLevel(e Engine, sql string) (DBLevelInfo, error) {
	toksAST, err := e.Tokenize(sql)
	if err != nil {
		return DBLevelInfo{}, err
	}
	var toks []dbToken
	if err := json.Unmarshal(toksAST, &toks); err != nil {
		return DBLevelInfo{}, fmt.Errorf("engine: decode tokens: %w", err)
	}
	if len(toks) == 0 {
		return DBLevelInfo{}, nil
	}
	isName := func(tt string) bool { return tt == "VAR" || tt == "QUOTED_IDENTIFIER" || tt == "IDENTIFIER" }
	head := strings.ToUpper(toks[0].Text)

	switch head {
	case "USE":
		info := DBLevelInfo{Kind: DBUse}
		if len(toks) >= 2 && isName(toks[1].TokenType) {
			info.DB = toks[1].Text
		}
		return info, nil
	case "SHOW":
		info := DBLevelInfo{Kind: DBShow}
		i := 1
		if i < len(toks) && isName(toks[i].TokenType) {
			info.ShowWhat = strings.ToUpper(toks[i].Text)
			i++
		}
		for i < len(toks) {
			tt := toks[i].TokenType
			switch {
			case tt == "FROM" || tt == "IN":
				if i+1 < len(toks) && isName(toks[i+1].TokenType) {
					info.DB = toks[i+1].Text
					i += 2
					continue
				}
			case tt == "NOT":
				info.LikeNot = true
			case tt == "LIKE" || tt == "I_LIKE":
				info.HasLike = true
				info.LikeCaseInsensitive = tt == "I_LIKE"
				if i+1 < len(toks) && toks[i+1].TokenType == "STRING" {
					info.Like = toks[i+1].Text
					i += 2
					continue
				}
			}
			i++
		}
		return info, nil
	default:
		return DBLevelInfo{Kind: DBNone}, nil
	}
}

// DatabaseTarget reads the db name + IF [NOT] EXISTS flags of a create_database /
// drop_database node. Errors if the AST is neither.
func DatabaseTarget(ast AST) (db string, ifNotExists, ifExists bool, err error) {
	kind, body, _, err := bodyOf(ast)
	if err != nil {
		return "", false, false, err
	}
	if body == nil || (kind != NodeCreateDB && kind != NodeDropDB) {
		return "", false, false, fmt.Errorf("engine: not a create/drop database node (%q)", kind)
	}
	db = identName(body["name"])
	ifNotExists, _ = body["if_not_exists"].(bool)
	ifExists, _ = body["if_exists"].(bool)
	return db, ifNotExists, ifExists, nil
}
```

> **Implementer notes:** (1) `bodyOf`/`identName` already exist in `internal/engine/writes.go`/`nodes.go` — reuse, don't duplicate. (2) Verify the `STRING` token's `text` is the **unquoted** pattern (the probe showed `STRING(a%)` for `'a%'`); if polyglot keeps escaping, unescape doubled quotes. (3) Verify `IDENTIFIER` is or isn't emitted (the Phase-2 tier-C scan used `VAR`/`QUOTED_IDENTIFIER`); keep `isName` aligned with the real tokenizer. (4) `SHOW DATABASES` — `ShowWhat=="DATABASES"`; the handler dispatch keys off this.

- [ ] **Step 4: Run to verify pass.** `POLYGLOT_SQL_FFI_PATH=… go test ./internal/engine/ -run 'TestParseDBLevel|TestDatabaseTarget' -v` → PASS.

- [ ] **Step 5: Vet + full engine suite + commit**

```bash
git add internal/engine/dblevel.go internal/engine/dblevel_test.go
git commit -m "feat(engine): db-level extraction — ParseDBLevel (USE/SHOW tokens) + DatabaseTarget"
```

---

### Task 2: nameresolve — db-level helpers

**Files:** Modify `internal/nameresolve/resolve.go`, `internal/nameresolve/resolve_test.go`.

- [ ] **Step 1: Write failing tests**

```go
func TestFindDynamicArgs(t *testing.T) {
	a := &pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"l": "p"}}
	opts := dynOpt(a)
	if got := FindDynamicArgs(opts); got != a {
		t.Errorf("FindDynamicArgs = %v, want the dynamic args", got)
	}
	// static-only option → nil
	if got := FindDynamicArgs([]*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{StaticArgs: &pb.RewriteTableStaticArgs{}}}}}); got != nil {
		t.Errorf("static-only: FindDynamicArgs = %v, want nil", got)
	}
}

func TestResolvePhysicalDatabaseExported(t *testing.T) {
	a := &pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}, KnownPhysicalDatabases: []string{"prod"}}
	if p, ok := ResolvePhysicalDatabase("tenant1", a); !ok || p != "testnet" {
		t.Errorf("database_map hit: %q ok=%v", p, ok)
	}
	if p, ok := ResolvePhysicalDatabase("prod", a); !ok || p != "prod" {
		t.Errorf("known_physical passthrough: %q ok=%v", p, ok)
	}
	if _, ok := ResolvePhysicalDatabase("nope", a); ok {
		t.Errorf("unresolvable: ok=true want false")
	}
}

func TestIsLogicalRemoteMapped(t *testing.T) {
	a := &pb.RewriteTableDynamicArgs{LogicalDatabaseToRemoteUpstreamIndex: map[string]string{"tenant1": "up0"}}
	if !IsLogicalRemoteMapped("tenant1", a) {
		t.Errorf("tenant1 should be remote-mapped")
	}
	if IsLogicalRemoteMapped("other", a) {
		t.Errorf("other should not be remote-mapped")
	}
}

func TestBuildDynamicTablePrefix(t *testing.T) {
	// prefix = "<logical>[<delim><extra>...]." — everything buildDynamicTableName
	// produces before original_table, INCLUDING the trailing dot.
	a := &pb.RewriteTableDynamicArgs{Delim: "_"}
	if got := BuildDynamicTablePrefix("tenant1", a); got != "tenant1." {
		t.Errorf("no-extra: %q want tenant1.", got)
	}
	a2 := &pb.RewriteTableDynamicArgs{Delim: "_", ExtraArguments: []string{"x", "y"}}
	if got := BuildDynamicTablePrefix("tenant1", a2); got != "tenant1_x_y." {
		t.Errorf("with-extra: %q want tenant1_x_y.", got)
	}
}
```

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement** (add to `resolve.go`)

```go
// FindDynamicArgs returns the dynamic_args from the last TableNameRewrite option
// that carries them, or nil. Unlike FindActive, it ignores static_args entirely —
// db-level handlers (USE/SHOW/CREATE-DB/DROP-DB) only consult dynamic_args.
// Mirrors C++ findDynamicArgs.
func FindDynamicArgs(opts []*pb.RewriteOption) *pb.RewriteTableDynamicArgs {
	var found *pb.RewriteTableDynamicArgs
	for _, o := range opts {
		if o.GetOp() != pb.RewriteOp_TableNameRewrite {
			continue
		}
		if d := o.GetTableNameArgs().GetDynamicArgs(); d != nil {
			found = d
		}
	}
	return found
}

// ResolvePhysicalDatabase is the exported wrapper over resolvePhysicalDatabase
// (database_map, then known_physical passthrough). ok=false when unresolvable.
func ResolvePhysicalDatabase(logical string, a *pb.RewriteTableDynamicArgs) (string, bool) {
	return resolvePhysicalDatabase(logical, a)
}

// IsLogicalRemoteMapped reports whether logical is in
// logical_database_to_remote_upstream_index. Mirrors C++ isLogicalRemoteMapped.
func IsLogicalRemoteMapped(logical string, a *pb.RewriteTableDynamicArgs) bool {
	if logical == "" {
		return false
	}
	_, ok := a.GetLogicalDatabaseToRemoteUpstreamIndex()[logical]
	return ok
}

// BuildDynamicTablePrefix returns "<logical>[<delim><extra>...]." — the physical
// table-name prefix (everything buildDynamicTableName emits before original_table,
// including the trailing "."). NOTE: buildDynamicTableName short-circuits to "" on
// an empty original_table (the USE sentinel), so this builds the prefix directly.
// Mirrors C++ buildDynamicTablePrefix.
func BuildDynamicTablePrefix(logical string, a *pb.RewriteTableDynamicArgs) string {
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
	return b.String()
}
```

> **Implementer note:** confirm `resolvePhysicalDatabase`'s existing semantics match (database_map first, then known_physical returns logical itself). The exported wrapper must not change behavior.

- [ ] **Step 4: Run to verify pass.**

- [ ] **Step 5: Vet + commit**

```bash
git add internal/nameresolve/resolve.go internal/nameresolve/resolve_test.go
git commit -m "feat(nameresolve): db-level helpers — FindDynamicArgs/ResolvePhysicalDatabase/IsLogicalRemoteMapped/BuildDynamicTablePrefix"
```

---

### Task 3: Handlers — `RewriteDBLevel` spine + USE

**Files:** Create `internal/handlers/dblevel.go`, `internal/handlers/dblevel_test.go`.

- [ ] **Step 1: Write failing tests**

```go
func TestRewriteDBLevel_usePhysicalRewrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "USE tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_USE {
		t.Fatalf("code=%v stmt=%v", resp.GetCode(), resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "USE testnet") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_usePassthroughNoDynamic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE tenant1")
	resp, handled, _ := RewriteDBLevel(e, ast, "USE tenant1", nil)
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v", handled, resp.GetCode())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "USE tenant1") || len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("sql=%q rewrites=%v", resp.GetSqlAfterRewrite(), resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_useSamePhysicalPassthrough(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE prod")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{KnownPhysicalDatabases: []string{"prod"}})
	resp, _, _ := RewriteDBLevel(e, ast, "USE prod", opts)
	// physical == origin → passthrough, no database_rewrites entry.
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "USE prod") || len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("sql=%q rewrites=%v", resp.GetSqlAfterRewrite(), resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_useUnresolvableInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE nope")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, _, _ := RewriteDBLevel(e, ast, "USE nope", opts)
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v", resp.GetCode())
	}
}

func TestRewriteDBLevel_useRemoteMappedUnsupported(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap:                          map[string]string{"tenant1": "testnet"},
		LogicalDatabaseToRemoteUpstreamIndex: map[string]string{"tenant1": "up0"},
	})
	resp, _, _ := RewriteDBLevel(e, ast, "USE tenant1", opts)
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("code=%v", resp.GetCode())
	}
}

func TestRewriteDBLevel_notDBLevel(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SELECT 1")
	_, handled, _ := RewriteDBLevel(e, ast, "SELECT 1", nil)
	if handled {
		t.Errorf("SELECT must not be handled by RewriteDBLevel")
	}
}
```

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement the spine + USE**

```go
// internal/handlers/dblevel.go
package handlers

import (
	"strings"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
)

// RewriteDBLevel ports the database-level handlers (USE / SHOW TABLES / SHOW
// DATABASES / CREATE DATABASE / DROP DATABASE). Returns (resp, handled, err) with
// the same contract as RewriteWrite. native.go calls it after RewriteWrite and
// before the SELECT/pass-through fallback.
func RewriteDBLevel(e engine.Engine, ast engine.AST, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, bool, error) {
	kind, err := engine.NodeKind(ast)
	if err != nil {
		return nil, false, err
	}
	dyn := nameresolve.FindDynamicArgs(opts)

	switch kind {
	case engine.NodeCreateDB:
		return dispatchCreateDatabase(e, ast, dyn) // Task 6
	case engine.NodeDropDB:
		return dispatchDropDatabase(e, ast, dyn) // Task 6
	case engine.NodeCommand:
		info, perr := engine.ParseDBLevel(e, sql)
		if perr != nil {
			return nil, false, perr
		}
		switch info.Kind {
		case engine.DBUse:
			return dispatchUse(e, ast, info, dyn)
		case engine.DBShow:
			if info.ShowWhat == "DATABASES" {
				return dispatchShowDatabases(e, ast, info, dyn) // Task 5
			}
			return dispatchShowTables(e, ast, info, dyn) // Task 4
		}
	}
	return nil, false, nil // not a db-level statement → caller falls through
}

func newDBResp(stmt pb.StatementType) *pb.RewriteSQLResponse {
	return &pb.RewriteSQLResponse{
		Code: pb.RewriteCode_Success, Message: "success",
		StatementType: stmt, DatabaseRewrites: map[string]string{},
	}
}

func rejectDBInvalid(resp *pb.RewriteSQLResponse, msg string) {
	resp.Code, resp.Message = pb.RewriteCode_InvalidRewriteRequest, msg
}
func rejectDBUnsupported(resp *pb.RewriteSQLResponse, msg string) {
	resp.Code, resp.Message = pb.RewriteCode_UnsupportedStatement, msg
}

// recordDatabaseRewrite appends {origin → new} to database_rewrites (no-op when
// equal). Mirrors C++ recordDatabaseRewrite.
func recordDatabaseRewrite(resp *pb.RewriteSQLResponse, origin, newDB string) {
	if origin == newDB {
		return
	}
	if resp.DatabaseRewrites == nil {
		resp.DatabaseRewrites = map[string]string{}
	}
	resp.DatabaseRewrites[origin] = newDB
}

// passthroughDB regenerates the original AST (canonical form) for a db-level
// passthrough; on a generate hiccup it echoes the original sql.
func passthroughDB(e engine.Engine, ast engine.AST, sql string, resp *pb.RewriteSQLResponse) (*pb.RewriteSQLResponse, bool, error) {
	if gen, gerr := e.Generate(ast); gerr == nil && gen != "" {
		resp.SqlAfterRewrite = gen
	} else {
		resp.SqlAfterRewrite = sql
	}
	return resp, true, nil
}

func dispatchUse(e engine.Engine, ast engine.AST, info engine.DBLevelInfo, dyn *pb.RewriteTableDynamicArgs) (*pb.RewriteSQLResponse, bool, error) {
	resp := newDBResp(pb.StatementType_STATEMENT_TYPE_USE)
	origin := info.DB
	if dyn == nil {
		return passthroughDB(e, ast, "USE "+origin, resp)
	}
	physical, ok := nameresolve.ResolvePhysicalDatabase(origin, dyn)
	if !ok {
		rejectDBInvalid(resp, "USE target '"+origin+"' is not in database_map and not a known physical database; user does not have this database")
		return resp, true, nil
	}
	if nameresolve.IsLogicalRemoteMapped(origin, dyn) {
		rejectDBUnsupported(resp, "USE target '"+origin+"' is mapped to a remote upstream via dynamic_args.logical_database_to_remote_upstream_index; USE has no remote analog")
		return resp, true, nil
	}
	if physical != origin {
		resp.SqlAfterRewrite = "USE " + physical
		recordDatabaseRewrite(resp, origin, physical)
		return resp, true, nil
	}
	return passthroughDB(e, ast, "USE "+origin, resp)
}
```

> **Implementer notes:** (1) the `sql` passed to `passthroughDB` for USE is reconstructed; prefer passing the real original `sql` thread (adjust `dispatchUse` to take `sql` if cleaner). (2) C++ USE passthrough uses `formatAst(ast)` = `Generate(ast)` — but the original is a `command` node; confirm `Generate` of a `USE` command round-trips to `USE <db>`. If `Generate` on the command node misbehaves, build `"USE " + ident(origin)`-style output to match. Verify empirically and pick the faithful path. (3) `escapeSQLLiteral` is added in Task 6 — USE doesn't need it.

- [ ] **Step 4: Run to verify pass.**

- [ ] **Step 5: Vet + commit**

```bash
git add internal/handlers/dblevel.go internal/handlers/dblevel_test.go
git commit -m "feat(handlers): db-level spine + USE rewrite"
```

---

### Task 4: Handlers — SHOW TABLES

**Files:** Modify `internal/handlers/dblevel.go`, `internal/handlers/dblevel_test.go`.

- [ ] **Step 1: Write failing tests**

```go
func TestRewriteDBLevel_showTablesSynthetic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW TABLES FROM tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}, Delim: "_"})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW TABLES FROM tenant1", opts)
	if err != nil || !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v err=%v", handled, resp.GetCode(), err)
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_TABLES {
		t.Errorf("stmt=%v", resp.GetStatementType())
	}
	want := "SELECT multiIf(startsWith(name, 'tenant1.'), substring(name, length('tenant1.') + 1), name) AS name FROM (SELECT name FROM system.tables WHERE database = 'testnet' AND startsWith(name, 'tenant1.'))"
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("sql=\n got %q\n want %q", resp.GetSqlAfterRewrite(), want)
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_showTablesUpstreamContext(t *testing.T) {
	e := newEngine(t)
	// No FROM clause → uses upstream_logical_database_in_context.
	ast := mustParse(t, e, "SHOW TABLES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{"tenant1": "testnet"}, Delim: "_",
		UpstreamLogicalDatabaseInContext: "tenant1",
	})
	resp, _, _ := RewriteDBLevel(e, ast, "SHOW TABLES", opts)
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("code=%v rewrites=%v", resp.GetCode(), resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_showTablesNoContextInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW TABLES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, _, _ := RewriteDBLevel(e, ast, "SHOW TABLES", opts)
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v", resp.GetCode())
	}
}

func TestRewriteDBLevel_showClustersPassthrough(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW CLUSTERS")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, _ := RewriteDBLevel(e, ast, "SHOW CLUSTERS", opts)
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v", handled, resp.GetCode())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_TABLES {
		t.Errorf("stmt=%v", resp.GetStatementType()) // C++ types SHOW CLUSTERS as SHOW_TABLES
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SHOW CLUSTERS") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
}

func TestRewriteDBLevel_showTablesRemoteSource(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW TABLES FROM tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{"tenant1": "testnet"}, Delim: "_",
		LogicalDatabaseToRemoteUpstreamIndex: map[string]string{"tenant1": "up0"},
		RemoteUpstreams:                      map[string]*pb.RewriteTableDynamicArgs_RemoteUpstream{"up0": {Addr: "h:9000", User: "u", Password: "p"}},
	})
	resp, _, _ := RewriteDBLevel(e, ast, "SHOW TABLES FROM tenant1", opts)
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
	}
	// system_tables_source becomes remote('h:9000', system, tables, 'u', 'p').
	if !strings.Contains(resp.GetSqlAfterRewrite(), "remote('h:9000', system, tables, 'u', 'p')") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
}
```

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement `dispatchShowTables`** (port `show_tables.cc` exactly; verify the `pb.RemoteUpstream` field names — `Addr/User/Password` — and the `UpstreamLogicalDatabaseInContext` getter)

```go
func dispatchShowTables(e engine.Engine, ast engine.AST, info engine.DBLevelInfo, dyn *pb.RewriteTableDynamicArgs) (*pb.RewriteSQLResponse, bool, error) {
	resp := newDBResp(pb.StatementType_STATEMENT_TYPE_SHOW_TABLES)
	// Only SHOW TABLES proper is rewritten; SHOW CLUSTERS/DICTIONARIES/SETTINGS/
	// MERGES/CACHES (and a no-dynamic request) pass through.
	if info.ShowWhat != "TABLES" || dyn == nil {
		return passthroughDB(e, ast, "", resp)
	}
	fromLogical := info.DB
	if dot := strings.IndexByte(fromLogical, '.'); dot >= 0 {
		fromLogical = fromLogical[:dot]
	}
	logical := fromLogical
	if logical == "" {
		logical = dyn.GetUpstreamLogicalDatabaseInContext()
	}
	if logical == "" {
		rejectDBInvalid(resp, "SHOW TABLES has no FROM clause and no upstream_logical_database_in_context is set; caller must send `USE <db>` or use `SHOW TABLES FROM <db>`")
		return resp, true, nil
	}
	physical, ok := nameresolve.ResolvePhysicalDatabase(logical, dyn)
	if !ok {
		rejectDBInvalid(resp, "SHOW TABLES target logical database '"+logical+"' is not in database_map and not a known physical database; user does not have this database")
		return resp, true, nil
	}
	source := "system.tables"
	if key, ok := dyn.GetLogicalDatabaseToRemoteUpstreamIndex()[logical]; ok {
		up, ok := dyn.GetRemoteUpstreams()[key]
		if !ok {
			rejectDBInvalid(resp, "SHOW TABLES target logical database '"+logical+"' is mapped to remote upstream key '"+key+"' but that key is not in remote_upstreams")
			return resp, true, nil
		}
		source = "remote('" + escapeSQLLiteral(up.GetAddr()) + "', system, tables, '" + escapeSQLLiteral(up.GetUser()) + "', '" + escapeSQLLiteral(up.GetPassword()) + "')"
	}
	prefix := nameresolve.BuildDynamicTablePrefix(logical, dyn)
	ep := escapeSQLLiteral(prefix)
	ephys := escapeSQLLiteral(physical)
	resp.SqlAfterRewrite = "SELECT multiIf(startsWith(name, '" + ep + "'), substring(name, length('" + ep + "') + 1), name) AS name FROM (SELECT name FROM " + source + " WHERE database = '" + ephys + "' AND startsWith(name, '" + ep + "'))"
	recordDatabaseRewrite(resp, logical, physical)
	return resp, true, nil
}
```

> Note: `escapeSQLLiteral` is defined in Task 6 — if implementing Task 4 first, add it now (it's a 6-line helper: double every `'`). Reuse across SHOW/CREATE/DROP.

- [ ] **Step 4: Run to verify pass.** **Verify the SHOW CLUSTERS passthrough SQL** — confirm `Generate` of a `SHOW CLUSTERS` command round-trips; if not, echo the original `sql`.

- [ ] **Step 5: Vet + commit**

```bash
git add internal/handlers/dblevel.go internal/handlers/dblevel_test.go
git commit -m "feat(handlers): SHOW TABLES → synthetic system.tables enumeration"
```

---

### Task 5: Handlers — SHOW DATABASES

**Files:** Modify `internal/handlers/dblevel.go`, `internal/handlers/dblevel_test.go`.

- [ ] **Step 1: Write failing tests**

```go
func TestRewriteDBLevel_showDatabasesSynthetic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"tenant1": "testnet", "tenant2": "mainnet", "ghost": "unknown"},
		KnownPhysicalDatabases: []string{"testnet", "mainnet"}, // "unknown" not trusted → ghost skipped
	})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW DATABASES", opts)
	if err != nil || !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v err=%v", handled, resp.GetCode(), err)
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_DATABASES {
		t.Errorf("stmt=%v", resp.GetStatementType())
	}
	// Alphabetical by logical; ghost (untrusted physical) skipped.
	want := "SELECT name FROM (SELECT 'tenant1' AS name UNION ALL SELECT 'tenant2' AS name) ORDER BY name"
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("sql=\n got %q\n want %q", resp.GetSqlAfterRewrite(), want)
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" || resp.GetDatabaseRewrites()["tenant2"] != "mainnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
	if _, ghost := resp.GetDatabaseRewrites()["ghost"]; ghost {
		t.Errorf("ghost (untrusted) must not appear in database_rewrites")
	}
}

func TestRewriteDBLevel_showDatabasesLike(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES NOT ILIKE 'x%'")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}, KnownPhysicalDatabases: []string{"testnet"}})
	resp, _, _ := RewriteDBLevel(e, ast, "SHOW DATABASES NOT ILIKE 'x%'", opts)
	want := "SELECT name FROM (SELECT 'tenant1' AS name) WHERE name NOT ILIKE 'x%' ORDER BY name"
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("sql=\n got %q\n want %q", resp.GetSqlAfterRewrite(), want)
	}
}

func TestRewriteDBLevel_showDatabasesEmpty(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{}) // empty database_map
	resp, _, _ := RewriteDBLevel(e, ast, "SHOW DATABASES", opts)
	want := "SELECT name FROM (SELECT '' AS name WHERE 0) ORDER BY name"
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
}

func TestRewriteDBLevel_showDatabasesPassthroughNoDynamic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES")
	resp, handled, _ := RewriteDBLevel(e, ast, "SHOW DATABASES", nil)
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v", handled, resp.GetCode())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SHOW DATABASES") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
}
```

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement `dispatchShowDatabases`** (port `show_databases.cc` exactly)

```go
func dispatchShowDatabases(e engine.Engine, ast engine.AST, info engine.DBLevelInfo, dyn *pb.RewriteTableDynamicArgs) (*pb.RewriteSQLResponse, bool, error) {
	resp := newDBResp(pb.StatementType_STATEMENT_TYPE_SHOW_DATABASES)
	if dyn == nil {
		return passthroughDB(e, ast, "", resp)
	}
	// Sort database_map by logical (protobuf map order is unspecified).
	type ent struct{ logical, physical string }
	var entries []ent
	for l, p := range dyn.GetDatabaseMap() {
		entries = append(entries, ent{l, p})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].logical < entries[j].logical })

	known := map[string]bool{}
	for _, p := range dyn.GetKnownPhysicalDatabases() {
		known[p] = true
	}
	var subqueries []string
	for _, en := range entries {
		if !known[en.physical] {
			continue // trust anchor: physical must be declared known
		}
		subqueries = append(subqueries, "SELECT '"+escapeSQLLiteral(en.logical)+"' AS name")
		recordDatabaseRewrite(resp, en.logical, en.physical)
	}
	body := "SELECT '' AS name WHERE 0"
	if len(subqueries) > 0 {
		body = strings.Join(subqueries, " UNION ALL ")
	}
	resp.SqlAfterRewrite = "SELECT name FROM (" + body + ")" + buildLikeClause(info) + " ORDER BY name"
	return resp, true, nil
}

// buildLikeClause renders " WHERE name <op> '<pat>'" or "" when no LIKE.
func buildLikeClause(info engine.DBLevelInfo) string {
	if !info.HasLike {
		return ""
	}
	op := "LIKE"
	if info.LikeCaseInsensitive {
		op = "ILIKE"
	}
	if info.LikeNot {
		op = "NOT " + op
	}
	return " WHERE name " + op + " '" + escapeSQLLiteral(info.Like) + "'"
}
```

Add `"sort"` to the import block.

- [ ] **Step 4: Run to verify pass.**

- [ ] **Step 5: Vet + commit**

```bash
git add internal/handlers/dblevel.go internal/handlers/dblevel_test.go
git commit -m "feat(handlers): SHOW DATABASES → synthetic logical-db enumeration"
```

---

### Task 6: Handlers — CREATE / DROP DATABASE (replaces the Phase-2 stub)

**Files:** Modify `internal/handlers/dblevel.go`, `internal/handlers/dblevel_test.go`, `internal/handlers/writes.go`.

- [ ] **Step 1: Remove the Phase-2 out-of-phase stub**

In `internal/handlers/writes.go`: delete `dispatchDatabaseOutOfPhase` and remove the `case engine.NodeCreateDB, engine.NodeDropDB:` line from `RewriteWrite`'s switch (so those kinds hit `default → nil, false, nil` and fall through to `RewriteDBLevel`). Update/delete the Phase-2 tests asserting the out-of-phase reject (`TestRewriteWrite_createDatabaseOutOfPhase`, `_dropDatabaseOutOfPhase`) — they're superseded by the Task-6 db-level tests.

- [ ] **Step 2: Write failing tests**

```go
func TestRewriteDBLevel_createDatabaseDebugRewrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE newdb")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "CREATE DATABASE newdb", opts)
	if err != nil || !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v err=%v", handled, resp.GetCode(), err)
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE {
		t.Errorf("stmt=%v", resp.GetStatementType())
	}
	// Debug rewrite: SELECT '<canonical original>' AS cdstmt
	if !strings.HasPrefix(resp.GetSqlAfterRewrite(), "SELECT '") || !strings.HasSuffix(resp.GetSqlAfterRewrite(), "' AS cdstmt") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
	// recordAccessedDatabase: 1 entry, original_table empty, physical empty (newdb not in map).
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalDatabase() != "newdb" || ats[0].GetOriginalTable() != "" {
		t.Errorf("accessed=%+v", ats)
	}
}

func TestRewriteDBLevel_createDatabaseNoDynamicUnsupported(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE newdb")
	resp, _, _ := RewriteDBLevel(e, ast, "CREATE DATABASE newdb", nil)
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("code=%v", resp.GetCode())
	}
}

func TestRewriteDBLevel_createDatabaseAlreadyExistsInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, _, _ := RewriteDBLevel(e, ast, "CREATE DATABASE tenant1", opts)
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v", resp.GetCode())
	}
}

func TestRewriteDBLevel_createDatabaseIfNotExistsSuppresses(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE IF NOT EXISTS tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, _, _ := RewriteDBLevel(e, ast, "CREATE DATABASE IF NOT EXISTS tenant1", opts)
	if resp.GetCode() != pb.RewriteCode_Success { // IF NOT EXISTS → falls through to debug rewrite
		t.Errorf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
	}
}

func TestRewriteDBLevel_dropDatabaseDebugRewrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, _ := RewriteDBLevel(e, ast, "DROP DATABASE tenant1", opts)
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v", handled, resp.GetCode())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DROP_DATABASE {
		t.Errorf("stmt=%v", resp.GetStatementType())
	}
	if !strings.HasSuffix(resp.GetSqlAfterRewrite(), "' AS ddstmt") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
	// db_map HIT → recordDatabaseRewrite(tenant1→testnet); accessed physical=testnet.
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetPhysicalDatabase() != "testnet" {
		t.Errorf("accessed=%+v", ats)
	}
}

func TestRewriteDBLevel_dropDatabaseNotManagedInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE nope")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, _, _ := RewriteDBLevel(e, ast, "DROP DATABASE nope", opts)
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v", resp.GetCode())
	}
}

func TestRewriteDBLevel_dropDatabaseIfExistsSuppresses(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE IF EXISTS nope")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, _, _ := RewriteDBLevel(e, ast, "DROP DATABASE IF EXISTS nope", opts)
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Errorf("code=%v", resp.GetCode())
	}
}
```

- [ ] **Step 3: Implement `dispatchCreateDatabase` / `dispatchDropDatabase` + helpers** (port `writes.cc:290-344` / `423-461`)

```go
// escapeSQLLiteral doubles every single quote for embedding inside a single-quoted
// ClickHouse string literal. Mirrors C++ escapeSqlLiteral (common.h).
func escapeSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// recordAccessedDatabase appends one db-level AccessedTable (original_table="").
// physical = ResolvePhysicalDatabase(target) when resolvable, else "". Mirrors C++
// recordAccessedDatabase (name_rewrite.h:254-276).
func recordAccessedDatabase(resp *pb.RewriteSQLResponse, target string, dyn *pb.RewriteTableDynamicArgs) {
	phys := ""
	if dyn != nil {
		if p, ok := nameresolve.ResolvePhysicalDatabase(target, dyn); ok {
			phys = p
		}
	}
	resp.OriginalAccessedTables = append(resp.OriginalAccessedTables, &pb.AccessedTable{
		OriginalDatabase: target, OriginalTable: "",
		LogicalDatabase: target, PhysicalDatabase: phys, IsRemote: false,
	})
}

func dispatchCreateDatabase(e engine.Engine, ast engine.AST, dyn *pb.RewriteTableDynamicArgs) (*pb.RewriteSQLResponse, bool, error) {
	resp := newDBResp(pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE)
	target, ifNotExists, _, err := engine.DatabaseTarget(ast)
	if err != nil {
		return nil, false, err
	}
	if dyn == nil {
		rejectDBUnsupported(resp, "CREATE DATABASE requires a TableNameRewrite/dynamic_args option to validate against")
		return resp, true, nil
	}
	recordAccessedDatabase(resp, target, dyn) // before validation
	if phys, hit := dyn.GetDatabaseMap()[target]; hit && !ifNotExists {
		rejectDBInvalid(resp, "CREATE DATABASE target '"+target+"' already exists (mapped to physical '"+phys+"'); use IF NOT EXISTS to suppress this error")
		return resp, true, nil
	}
	// upstream_physical_database_in_context, when set & non-empty, must be known.
	if phys := dyn.GetUpstreamPhysicalDatabaseInContext(); phys != "" {
		known := false
		for _, p := range dyn.GetKnownPhysicalDatabases() {
			if p == phys {
				known = true
				break
			}
		}
		if !known {
			rejectDBInvalid(resp, "CREATE DATABASE: upstream_physical_database_in_context '"+phys+"' is not in known_physical_databases")
			return resp, true, nil
		}
	}
	gen, gerr := e.Generate(ast)
	if gerr != nil {
		return nil, false, gerr
	}
	resp.SqlAfterRewrite = "SELECT '" + escapeSQLLiteral(gen) + "' AS cdstmt"
	return resp, true, nil
}

func dispatchDropDatabase(e engine.Engine, ast engine.AST, dyn *pb.RewriteTableDynamicArgs) (*pb.RewriteSQLResponse, bool, error) {
	resp := newDBResp(pb.StatementType_STATEMENT_TYPE_DROP_DATABASE)
	target, _, ifExists, err := engine.DatabaseTarget(ast)
	if err != nil {
		return nil, false, err
	}
	if dyn == nil {
		rejectDBUnsupported(resp, "DROP DATABASE requires a TableNameRewrite/dynamic_args option to validate against")
		return resp, true, nil
	}
	recordAccessedDatabase(resp, target, dyn)
	if phys, hit := dyn.GetDatabaseMap()[target]; !hit {
		if !ifExists {
			rejectDBInvalid(resp, "DROP DATABASE target '"+target+"' is not in database_map; use IF EXISTS to suppress this error")
			return resp, true, nil
		}
	} else {
		recordDatabaseRewrite(resp, target, phys)
	}
	gen, gerr := e.Generate(ast)
	if gerr != nil {
		return nil, false, gerr
	}
	resp.SqlAfterRewrite = "SELECT '" + escapeSQLLiteral(gen) + "' AS ddstmt"
	return resp, true, nil
}
```

> **Implementer notes:** (1) verify `pb.RewriteTableDynamicArgs` getter names: `GetUpstreamPhysicalDatabaseInContext()`, `GetUpstreamLogicalDatabaseInContext()`, `GetKnownPhysicalDatabases()`, `GetDatabaseMap()`, `GetDelim()`, `GetExtraArguments()`, `GetRemoteUpstreams()`, `GetLogicalDatabaseToRemoteUpstreamIndex()`. (2) C++ uses `has_upstream_physical_database_in_context()` then checks non-empty; proto3 scalar presence — the Go `GetUpstream…()=="" ` check folds "unset" and "set-empty" together, which matches C++'s `!phys.empty()` guard. (3) the debug literal uses `Generate(ast)` (canonical formatAst), NOT the raw `sql`.

- [ ] **Step 4: Run to verify pass** (incl. the updated writes.go — its Phase-2 db tests now removed; confirm `go test ./internal/handlers/` green).

- [ ] **Step 5: Vet + commit**

```bash
git add internal/handlers/dblevel.go internal/handlers/dblevel_test.go internal/handlers/writes.go internal/handlers/writes_test.go
git commit -m "feat(handlers): CREATE/DROP DATABASE debug rewrite (replaces Phase-2 out-of-phase stub)"
```

---

### Task 7: Native routing — db-level after writes, before SELECT

**Files:** Modify `native.go`, `native_test.go`.

- [ ] **Step 1: Write failing tests**

```go
func TestNativeRewrite_useRouted(t *testing.T) {
	e := newEngine(t)
	r := New(e, WithOptions(func(string) []*pb.RewriteOption {
		return dynOptFn(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	}))
	defer r.Close()
	res, err := r.Rewrite(context.Background(), "USE tenant1", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success || res.StatementType != pb.StatementType_STATEMENT_TYPE_USE {
		t.Fatalf("code=%v stmt=%v", res.Code, res.StatementType)
	}
	if !nativeSQLSemEq(t, e, res.SQL, "USE testnet") || res.DatabaseRewrites["tenant1"] != "testnet" {
		t.Errorf("sql=%q rewrites=%v", res.SQL, res.DatabaseRewrites)
	}
}

func TestNativeRewrite_showDatabasesRouted(t *testing.T) {
	e := newEngine(t)
	r := New(e, WithOptions(func(string) []*pb.RewriteOption {
		return dynOptFn(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}, KnownPhysicalDatabases: []string{"testnet"}})
	}))
	defer r.Close()
	res, _ := r.Rewrite(context.Background(), "SHOW DATABASES", "acct")
	if res.Code != pb.RewriteCode_Success || res.StatementType != pb.StatementType_STATEMENT_TYPE_SHOW_DATABASES {
		t.Fatalf("code=%v stmt=%v", res.Code, res.StatementType)
	}
}

func TestNativeRewrite_createDatabaseRouted(t *testing.T) {
	e := newEngine(t)
	r := New(e, WithOptions(func(string) []*pb.RewriteOption {
		return dynOptFn(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	}))
	defer r.Close()
	res, _ := r.Rewrite(context.Background(), "CREATE DATABASE newdb", "acct")
	if res.Code != pb.RewriteCode_Success || res.StatementType != pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE {
		t.Fatalf("code=%v stmt=%v", res.Code, res.StatementType)
	}
}

func TestNativeRewrite_selectAndWriteStillWork(t *testing.T) {
	e := newEngine(t)
	r := New(e)
	defer r.Close()
	if res, _ := r.Rewrite(context.Background(), "SELECT 1", "acct"); res.Code != pb.RewriteCode_Success {
		t.Errorf("SELECT regressed: %v", res.Code)
	}
	if res, _ := r.Rewrite(context.Background(), "DROP TABLE db.t", "acct"); res.StatementType != pb.StatementType_STATEMENT_TYPE_DROP_TABLE {
		t.Errorf("DROP TABLE regressed: %v", res.StatementType)
	}
}
```

- [ ] **Step 2: Run to verify failure** (USE currently passes through → wrong StatementType / no rewrite).

- [ ] **Step 3: Implement routing in `native.go`**

After the `RewriteWrite` block and before the SELECT branch, insert:

```go
	// Phase 3: route db-level statements (USE / SHOW TABLES / SHOW DATABASES /
	// CREATE DATABASE / DROP DATABASE) after writes, before SELECT.
	if dresp, handled, derr := handlers.RewriteDBLevel(r.engine, ast, sql, opts); derr != nil {
		return RewriteResult{}, derr
	} else if handled {
		if dresp.GetCode() != pb.RewriteCode_Success && dresp.GetSqlAfterRewrite() == "" {
			dresp.SqlAfterRewrite = sql // §8: always-runnable
		}
		r.mu.Lock()
		r.last = &callContext{sql: sql, account: account}
		r.mu.Unlock()
		return resultFromPB(dresp), nil
	}
```

(`opts` is already computed once, shared with the write + SELECT paths.)

- [ ] **Step 4: Run to verify pass.** Full suite: `POLYGLOT_SQL_FFI_PATH=… go test ./...` → PASS.

- [ ] **Step 5: Vet + commit**

```bash
git add native.go native_test.go
git commit -m "feat(native): route db-level (USE/SHOW/CREATE-DB/DROP-DB) after writes"
```

---

### Task 8: Differential harness — db-level golden corpus

**Files:** Create `internal/harness/testdata/dblevel_cases.json`, `internal/harness/dblevel_golden_test.go`.

- [ ] **Step 1: Read the Phase-1/2 harness** (`select_golden_test.go`, `writes_golden_test.go`, `compare.go`, `oracle.go`) and mirror the schema + comparison + env-gated oracle.

- [ ] **Step 2: Author the corpus** (drive each case through `New(e, WithOptions(...)).Rewrite`; freeze the real output as `want_*`). Cover: USE rewrite, USE passthrough, USE unresolvable (invalid), USE remote-mapped (unsupported), SHOW TABLES synthetic, SHOW TABLES no-context (invalid), SHOW TABLES remote-source, SHOW CLUSTERS passthrough, SHOW DATABASES synthetic, SHOW DATABASES + LIKE, SHOW DATABASES empty, CREATE DATABASE debug, CREATE DATABASE no-dynamic (unsupported), CREATE DATABASE already-exists (invalid), DROP DATABASE debug + db-rewrite, DROP DATABASE not-managed (invalid). Compare `code`/`statement_type`/`database_rewrites`/`original_accessed_tables` EXACT, `sql_after_rewrite` SEMANTIC (the synthetic SELECTs re-parse cleanly; the `SELECT '…' AS cdstmt/ddstmt` debug literal compares semantically too).

- [ ] **Step 3: Implement `TestDBLevelGolden`** mirroring `TestWritesGolden` (structured exact + sql semantic; env-gated `REWRITER_ORACLE_ADDR` differential via `Compare`).

- [ ] **Step 4: Run** with + without engine: PASS.

- [ ] **Step 5: Vet + commit**

```bash
git add internal/harness/testdata/dblevel_cases.json internal/harness/dblevel_golden_test.go
git commit -m "test(harness): db-level golden corpus + C++ oracle differential (Phase 3 gate)"
```

---

## Self-Review

**Spec coverage (db-level handlers → tasks):**
- USE (passthrough / rewrite / invalid / remote-unsupported / database_rewrites) → T1, T2, T3. ✓
- SHOW TABLES (synthetic system.tables, remote source, upstream-context, passthrough for SHOW CLUSTERS/etc, no-context invalid) → T1, T4. ✓
- SHOW DATABASES (synthetic UNION, known-physical trust filter, LIKE clause, empty, passthrough) → T1, T5. ✓
- CREATE DATABASE (debug rewrite, no-dynamic unsupported, already-exists invalid, IF NOT EXISTS, upstream-physical check, recordAccessedDatabase) → T1, T6. ✓
- DROP DATABASE (debug rewrite, no-dynamic unsupported, not-managed invalid, IF EXISTS, db-rewrite on hit) → T1, T6. ✓
- `database_rewrites` / `original_accessed_tables` (db-level) → T3/T5 `recordDatabaseRewrite`, T6 `recordAccessedDatabase`. ✓
- Native routing + §8 echo → T7. ✓
- Differential gate → T8. ✓
- Phase-2 `dispatchDatabaseOutOfPhase` stub removed → T6. ✓

**Type consistency:** `DBLevelInfo`/`DBLevelKind` (engine) defined once in T1, consumed by T3-T6. `RewriteDBLevel(e, ast, sql, opts) → (*resp, handled, err)` mirrors `RewriteWrite`. `recordDatabaseRewrite`/`recordAccessedDatabase`/`escapeSQLLiteral`/`newDBResp`/`reject*` defined once (T3/T6), reused. nameresolve `FindDynamicArgs`/`ResolvePhysicalDatabase`/`IsLogicalRemoteMapped`/`BuildDynamicTablePrefix` (T2) used by T3-T6.

**Placeholder scan:** the only deferred specifics are verify-then-implement empirical checks (the `STRING`/`IDENTIFIER` token types in T1; whether `Generate` round-trips a USE/SHOW command for passthrough in T3/T4; proto getter names in T6) — the Phase-2 pattern. No `TODO`/`TBD`/"add validation" placeholders.

**Open parity risks (validated by T8 against the oracle):**
1. Passthrough SQL fidelity — does `Generate` of a `USE`/`SHOW CLUSTERS` command node round-trip, or must we echo the raw `sql`? (T3/T4 verify-then-implement.)
2. The `SELECT '…' AS cdstmt/ddstmt` debug literal embeds `Generate(ast)` (canonical) — confirm C++'s `formatAst` canonicalization matches polyglot's `Generate` closely enough for the semantic compare (else exact-string against our own frozen output, oracle-gated).
3. SHOW TABLES intentionally drops the original LIKE clause (C++ parity) — registered, not a bug.

---

## Execution Handoff

Plan saved to `docs/superpowers/plans/2026-06-09-native-go-rewriter-phase-3-dblevel.md`. Continue with **superpowers:subagent-driven-development** (fresh implementer per task + spec-then-quality review), then **superpowers:finishing-a-development-branch** (push + create PR), mirroring Phases 1-2.
EOF
