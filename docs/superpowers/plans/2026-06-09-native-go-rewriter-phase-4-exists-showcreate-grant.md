# Phase 4 — EXISTS / SHOW CREATE / GRANT+REVOKE Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the remaining single-target + privilege-delta diff with the C++ `rewriter-grpc` oracle by porting `exists.cc`, `show_create.cc`, and `grant.cc` into the native Go rewriter (design spec §9, Phase 4).

**Architecture:** Three handlers join the dispatch spine after `RewriteDBLevel`, before SELECT (mirroring the C++ server order `exists → show_create → grant`). EXISTS and SHOW CREATE are near-identical *single-target* handlers that reuse the Phase-2 `decideWriteTarget` name-rewrite machinery and emit the canonical `EXISTS TABLE …` / `SHOW CREATE TABLE …` form. GRANT/REVOKE emits structured `privileges_deltas` plus a marker `SELECT '…' AS gstmt|rstmt`. The engine layer keeps the polyglot-shape boundary: it adds a generic-dialect parse plus two tokenize-based extractors; all policy lives in the handlers.

**Tech Stack:** Go (`CGO_ENABLED=0`), `tobilg/polyglot` v0.4.3 via PureGo FFI, `gen/pb` protobuf types, the existing `internal/engine` / `internal/nameresolve` / `internal/handlers` / `internal/harness` packages.

---

## Empirical shape reference (probed against the live FFI lib, 2026-06-09)

Authoritative facts that drive the design. Re-probe with a throwaway `internal/engine/zz_test.go` if needed; **delete any probe before committing.**

**EXISTS** (`EXISTS [TEMPORARY] [TABLE|DATABASE|VIEW|DICTIONARY] [db.]name`):
- clickhouse dialect → opaque `{"command":{"this":"EXISTS TABLE db.t"}}`.
- generic dialect → **parse error for every form** (generic SQL only knows `EXISTS (subquery)`).
- → **tokenize-only.** Token stream is regular: `EXISTS` / optional `TEMPORARY` / optional object keyword (`TABLE`/`DATABASE`/`VIEW`/`DICTIONARY` are their own token types) / name-run `VAR (DOT VAR)?`. Backtick names lex as `QUOTED_IDENTIFIER` (backticks stripped from `.text`, span covers the quotes).

**SHOW CREATE** (`SHOW CREATE [TEMPORARY] [TABLE|DATABASE|VIEW|DICTIONARY] [db.]name`):
- clickhouse → opaque `command`.
- generic → structured `show` node **only when the object keyword is present** (`{"show":{"this":"CREATE TABLE", "target":{"table":{"name":…,"schema":…}}}}`); **bare `SHOW CREATE t` mis-parses** to `this:"CREATE T"` with no target.
- → **tokenize-only**, identical token grammar to EXISTS (the `SHOW CREATE` verb prefix replaces `EXISTS`). This handles the bare form the generic path drops.

**GRANT / REVOKE**:
- clickhouse → opaque `command`.
- generic → rich `{"grant"|"revoke":{...}}` node: `privileges:[{name, columns?}]`, `securable:{name:"db.t"|"db.*"|"*.*"|"t"}` (flat, **manual split**), `principals:[{name:{name}}]`, `grant_option:bool` (covers both `WITH GRANT OPTION` and `GRANT OPTION FOR`). `REVOKE` also has `cascade`/`restrict` (unused). Multi-word privileges group correctly (`ALTER UPDATE` → one privilege). `CURRENT GRANTS` parses as a privilege named `"CURRENT GRANTS"`.
- generic **fails** on three forms the C++ treats specially: `ATTACH GRANT …`, `… WITH REPLACE OPTION`, and the role-membership form (no `ON` clause, e.g. `GRANT role TO u` / `GRANT NONE TO u`). `ON CLUSTER …` also fails generic parse.
- **`Generate(genericGrantNode, "clickhouse")` round-trips to canonical CH SQL** (uppercases keywords, normalizes spacing): `grant select on db.t to u with grant option` → `GRANT SELECT ON db.t TO u WITH GRANT OPTION`. This is what the marker SELECT embeds — the same normalization C++ gets from `formatAst`.
- `ON CLUSTER <name>` lexes as a SECOND `ON` token immediately followed by a `CLUSTER` token + the cluster name token. Tokens carry `span{start,end}` byte offsets → splice it out before generic-parsing (which doubles as the C++ `grant->cluster.clear()`).

## C++ behavior reference (the parity oracle)

**Server dispatch order** (`src/rewriter-server.cc:353-386`): set `existence_clause` from `create->if_not_exists` / `drop->if_exists`; then `USE → SHOW DATABASES → SHOW TABLES → EXISTS → SHOW CREATE → GRANT → WRITE → SELECT`. Phase-4 statements all carry **no** IF [NOT] EXISTS, so `existence_clause` stays `UNSPECIFIED` (native already leaves it unset → match). Go's order is inverted (writes/db-level before these), which is safe: EXISTS/SHOW CREATE/GRANT all arrive as `command` nodes and fall through `RewriteWrite` (`CmdNone`) and `RewriteDBLevel` (`ParseDBLevel`→`DBNone`, or the `SHOW CREATE` defer).

**`exists.cc` / `show_create.cc`** (identical but for the statement type + keyword):
1. Reject `…DATABASE`/`…VIEW`/`…DICTIONARY` as `UnsupportedStatement` (table-oriented pipeline).
2. Accept the `…TABLE` form. `recordAccessedTable(origin_db, origin_table, sel)` **before** the mode switch.
3. Mode `None` → emit `formatAst` unchanged, no table rewrite. `Dynamic`/`Static` → resolve target; on remote → `UnsupportedStatement`, on invalid → `InvalidRewriteRequest`; on hit set db+table and `recordTableRewrite`.
4. `setSuccessResponse(formatAst(ast), STATEMENT_TYPE_EXISTS_TABLE | STATEMENT_TYPE_SHOW_CREATE_TABLE)`.
   - `formatAst` is canonical: always `EXISTS TABLE [db.]t` / `SHOW CREATE TABLE [db.]t` (the bare and TEMPORARY forms normalize to include `TABLE`), identifiers backtick-quoted `WhenNecessary`.

**`grant.cc`** (reject order is load-bearing — it decides which `code` wins when conditions co-occur):
1. `attach_mode` → Unsupported "ATTACH GRANT is not supported".
2. `roles` (role-membership) → Unsupported "<kind> of a role to a user/role (role-membership grant) is not supported".
3. `replace_access||replace_granted_roles` → Unsupported "<kind> WITH REPLACE OPTION is not supported".
4. `current_grants` → Unsupported "GRANT CURRENT GRANTS is not supported".
5. no `dynamic_args` → Unsupported "<kind> requires a TableNameRewrite/dynamic_args option to validate against".
6. `buildGrantees`: `set.all` → Unsupported "<dir>ALL is not supported"; `except_*` → Unsupported "<dir>… EXCEPT is not supported"; names → `Grantee{name}`; `current_user` → `Grantee{is_current_user}`; empty → InvalidRequest "<kind> has no grantees". (`<dir>` = `GRANT TO ` / `REVOKE FROM `.)
7. Per `AccessRightsElement` (ClickHouse splits `SELECT, INSERT` into **one element per privilege**): `anyDatabase()`→Unsup "<kind> ON *.* (global scope) is not supported"; `!columns.empty()`→Unsup "<kind> with column-level granularity is not supported"; `original_database = default_database ? "" : database`; `logical = original_database ?: upstream_logical_database_in_context`; empty→Invalid "<kind> target '<table>' is unqualified and no upstream_logical_database_in_context is set; caller must send \`USE <db>\` or qualify the target"; `physical = resolvePhysicalDatabase(logical)` else Invalid "<kind> target references logical database '<logical>' which is not in database_map and not a known physical database; user does not have this database to grant on". Emit one delta: `action`; `original_database`; `logical_database`; `physical_database`; `grant_option = elem.grant_option || elem.is_partial_revoke`; `anyTable()` → `SCOPE_DATABASE` (no table) else `SCOPE_TABLE` + `original_table=elem.table` + `physical_table=buildDynamicTableName(logical, elem.table)`; `privileges = elem.access_flags.toKeywords()` (verbatim); `grantees` fan out to every delta.
8. `grant->cluster.clear()`; `marker = is_revoke?"rstmt":"gstmt"`; `sql = "SELECT '"+escapeSqlLiteral(formatAst(ast))+"' AS "+marker`; `setSuccessResponse(sql, STATEMENT_TYPE_GRANT|REVOKE)`.

**Harness comparison** (`internal/harness/compare.go`, `select_golden_test.go`): structured fields exact; `sql_after_rewrite` semantic = `ParseOne→Generate` then string-compare. For `command`-blob outputs (EXISTS / SHOW CREATE) `Generate` is identity, so semantic ≈ byte-exact → **string-built output must match C++ `formatAst`**. `Compare` currently diffs code/stmt/existence_clause/table_rewrites/database_rewrites/failed_cte_aliases/sql but **NOT** `privileges_deltas` (Task 6 adds it) and **NOT** `message` (so reject message text is not gated — reuse of `decideWriteTarget`'s generic messages is fine even though exists.cc's wording differs).

## Reusable machinery (already on `main`)

- `nameresolve.FindActive(opts) Selection`; `nameresolve.Resolve` / `ResolveAccessed`; `FindDynamicArgs`; `ResolvePhysicalDatabase`; `BuildDynamicTablePrefix(logical,a)` (== `buildDynamicTableName(logical,t,a)` minus the table, parity-invariant from Phase 3 → use `prefix+originalTable` for `physical_table`).
- `handlers.decideWriteTarget(tt, kind, sel, resp) (TableDecision, ok)` — records accessed + table_rewrites, rejects remote(Unsup)/invalid(Invalid), returns `ActionRename{NewDB,NewTable}` / `ActionSkip`. `newWriteResp`, `rejectUnsupported`, `rejectInvalid`, `escapeSQLLiteral`.
- `engine.NodeKind`, `engine.TableTarget`, `engine.ActionRename`, `engine.QuoteQualified(db,table)` (backtick `WhenNecessary` — quotes a dotted dynamic table as one identifier), `engine.tokenizeRaw` / `rawToken{TokenType,Text,Span{Start,End}}` / `isNameTok` (all in `engine/writes.go`, same package), `newTestEngine(t)` (engine test helper used by `dblevel_test.go`).
- `e.Generate(ast)` targets the clickhouse dialect (used for the GRANT marker).

## File Structure

- **Create** `internal/engine/objtarget.go` (+`_test.go`): `ParseObjectTarget` — EXISTS/SHOW CREATE tokenize extractor (Task 1).
- **Create** `internal/engine/grant.go` (+`_test.go`): `ParseGrant` — GRANT/REVOKE extractor (Task 2).
- **Modify** `internal/engine/engine.go`, `internal/engine/polyglot.go`: add `ParseGeneric` to the Engine seam (Task 2).
- **Create** `internal/handlers/exists.go` (+`_test.go`): `RewriteExistsShowCreate` (Task 3).
- **Create** `internal/handlers/grant.go` (+`_test.go`): `RewriteGrant` (Task 4).
- **Modify** `native.go` (+`native_test.go`), `internal/handlers/dblevel.go` (comment): wire the two handlers (Task 5).
- **Modify** `internal/harness/compare.go`: add `privileges_deltas` diff. **Create** `internal/harness/phase4_golden_test.go` + `internal/harness/testdata/phase4_cases.json` (Task 6).

Each task: TDD (failing test → minimal impl → green), gofmt, `go vet ./...`, `go build ./...`, one commit. Engine/handler/harness tests gate on `POLYGLOT_SQL_FFI_PATH` (skip when unset). Run the FFI-backed suite with:
`POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./...`

---

## Task 1: Engine — `ParseObjectTarget` (EXISTS / SHOW CREATE extractor)

**Files:**
- Create: `internal/engine/objtarget.go`
- Test: `internal/engine/objtarget_test.go`

- [ ] **Step 1: Write the failing test**

```go
package engine

import "testing"

func TestParseObjectTarget(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql       string
		verb      ObjectVerb
		temporary bool
		objType   string
		db, table string
	}{
		{"EXISTS TABLE db.t", VerbExists, false, "TABLE", "db", "t"},
		{"EXISTS db.t", VerbExists, false, "TABLE", "db", "t"},
		{"EXISTS t", VerbExists, false, "TABLE", "", "t"},
		{"EXISTS TEMPORARY TABLE t", VerbExists, true, "TABLE", "", "t"},
		{"EXISTS DATABASE db", VerbExists, false, "DATABASE", "", "db"},
		{"EXISTS VIEW v", VerbExists, false, "VIEW", "", "v"},
		{"EXISTS DICTIONARY d", VerbExists, false, "DICTIONARY", "", "d"},
		{"SHOW CREATE TABLE db.t", VerbShowCreate, false, "TABLE", "db", "t"},
		{"SHOW CREATE t", VerbShowCreate, false, "TABLE", "", "t"},
		{"SHOW CREATE DATABASE db", VerbShowCreate, false, "DATABASE", "", "db"},
		{"SHOW CREATE VIEW v", VerbShowCreate, false, "VIEW", "", "v"},
		{"SHOW CREATE `weird.tbl`", VerbShowCreate, false, "TABLE", "", "weird.tbl"},
		// Not ours: SHOW TABLES/DATABASES are db-level; SELECT/USE are other handlers.
		{"SHOW TABLES", VerbNone, false, "", "", ""},
		{"SHOW DATABASES", VerbNone, false, "", "", ""},
		{"USE db", VerbNone, false, "", "", ""},
		{"SELECT 1", VerbNone, false, "", "", ""},
	}
	for _, c := range cases {
		got, err := ParseObjectTarget(e, c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		if got.Verb != c.verb || got.Temporary != c.temporary || got.ObjType != c.objType ||
			got.DB != c.db || got.Table != c.table {
			t.Errorf("%q: got %+v", c.sql, got)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./internal/engine -run TestParseObjectTarget` → FAIL (`ParseObjectTarget`/`VerbExists` undefined).

- [ ] **Step 3: Write the implementation**

```go
package engine

import "strings"

// ObjectVerb classifies an EXISTS / SHOW CREATE statement. Both are
// "<verb> [TEMPORARY] [<object-type>] [db.]name" in the ClickHouse grammar and
// both reach us as an opaque `command` node, so a shared tokenize-based
// extractor recovers their structure.
type ObjectVerb int

const (
	VerbNone       ObjectVerb = iota // not an EXISTS / SHOW CREATE statement
	VerbExists                       // EXISTS …
	VerbShowCreate                   // SHOW CREATE …
)

// ObjectTarget is the extracted structure of an EXISTS / SHOW CREATE statement.
type ObjectTarget struct {
	Verb      ObjectVerb
	Temporary bool
	ObjType   string // "TABLE" (default) / "DATABASE" / "VIEW" / "DICTIONARY"
	DB        string // "" when the name was bare
	Table     string
}

// ParseObjectTarget extracts EXISTS / SHOW CREATE structure from the clickhouse
// Tokenize stream. Returns Verb==VerbNone for anything else. EXISTS does not parse
// structurally under ANY polyglot dialect, and bare `SHOW CREATE t` (no TABLE
// keyword) mis-parses under the generic dialect, so the tokenizer is the only
// faithful source for both (verified against the live engine).
//
// Grammar recovered: <verb> [TEMPORARY] [TABLE|DATABASE|VIEW|DICTIONARY] <name-run>
// where <name-run> is `name` or `db DOT name`. A missing object-type keyword
// defaults to TABLE (ClickHouse's `EXISTS t` / `SHOW CREATE t` ≡ … TABLE t).
// Backtick-quoted names lex as QUOTED_IDENTIFIER with the backticks stripped from
// .Text, so DB/Table carry the unquoted identifier (matching the rewrite key).
func ParseObjectTarget(e Engine, sql string) (ObjectTarget, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return ObjectTarget{}, err
	}
	if len(toks) == 0 {
		return ObjectTarget{}, nil
	}
	var out ObjectTarget
	i := 0
	switch strings.ToUpper(toks[0].Text) {
	case "EXISTS":
		out.Verb, i = VerbExists, 1
	case "SHOW":
		if len(toks) < 2 || !strings.EqualFold(toks[1].Text, "CREATE") {
			return ObjectTarget{}, nil // SHOW <other> is a db-level statement, not ours
		}
		out.Verb, i = VerbShowCreate, 2
	default:
		return ObjectTarget{}, nil
	}
	if i < len(toks) && strings.EqualFold(toks[i].Text, "TEMPORARY") {
		out.Temporary = true
		i++
	}
	out.ObjType = "TABLE"
	if i < len(toks) {
		switch strings.ToUpper(toks[i].Text) {
		case "TABLE", "DATABASE", "VIEW", "DICTIONARY":
			out.ObjType = strings.ToUpper(toks[i].Text)
			i++
		}
	}
	// Name-run: `db DOT name` or `name`.
	if i < len(toks) && isNameTok(toks[i].TokenType) {
		if i+2 < len(toks) && toks[i+1].TokenType == "DOT" && isNameTok(toks[i+2].TokenType) {
			out.DB, out.Table = toks[i].Text, toks[i+2].Text
		} else {
			out.Table = toks[i].Text
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run to verify it passes** — same command → PASS. Also `go test ./internal/engine` (no FFI) → the test SKIPs cleanly (via `newTestEngine`).

- [ ] **Step 5: gofmt + vet + commit**

```bash
gofmt -w internal/engine/objtarget.go internal/engine/objtarget_test.go
go vet ./... && go build ./...
git add internal/engine/objtarget.go internal/engine/objtarget_test.go
git commit -m "feat(engine): ParseObjectTarget — EXISTS/SHOW CREATE tokenize extractor"
```

---

## Task 2: Engine — `ParseGrant` (GRANT / REVOKE extractor) + `ParseGeneric` seam

**Files:**
- Modify: `internal/engine/engine.go` (add interface method), `internal/engine/polyglot.go` (impl)
- Create: `internal/engine/grant.go`, `internal/engine/grant_test.go`

- [ ] **Step 1: Add `ParseGeneric` to the Engine seam**

In `internal/engine/engine.go`, add to the `Engine` interface (after `ParseOne`):

```go
	// ParseGeneric parses under polyglot's `generic` dialect, used to recover
	// GRANT/REVOKE structure that the clickhouse dialect leaves as an opaque
	// `command` node. Returns the single statement's node JSON.
	ParseGeneric(sql string) (AST, error)
```

In `internal/engine/polyglot.go`, add (after `ParseOne`):

```go
func (e *polyglotEngine) ParseGeneric(sql string) (AST, error) {
	ast, err := e.c.ParseOne(sql, "generic")
	if err != nil {
		return nil, fmt.Errorf("engine: parse(generic): %w", err)
	}
	return AST(ast), nil
}
```

- [ ] **Step 2: Write the failing test** (`internal/engine/grant_test.go`)

```go
package engine

import "testing"

func TestParseGrant(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql         string
		isGrant     bool // IsGrantVerb
		isRevoke    bool
		isAttach    bool
		hasReplace  bool
		hasOn       bool
		structured  bool
		privNames   []string
		securable   string
		principals  []string
		grantOption bool
		marker      string
	}{
		{"GRANT SELECT ON db.t TO u", true, false, false, false, true, true,
			[]string{"SELECT"}, "db.t", []string{"u"}, false, "GRANT SELECT ON db.t TO u"},
		{"REVOKE SELECT ON db.t FROM u", true, true, false, false, true, true,
			[]string{"SELECT"}, "db.t", []string{"u"}, false, "REVOKE SELECT ON db.t FROM u"},
		{"GRANT SELECT, INSERT ON db.t TO u1, u2 WITH GRANT OPTION", true, false, false, false, true, true,
			[]string{"SELECT", "INSERT"}, "db.t", []string{"u1", "u2"}, true, "GRANT SELECT, INSERT ON db.t TO u1, u2 WITH GRANT OPTION"},
		{"GRANT SELECT ON db.* TO u", true, false, false, false, true, true,
			[]string{"SELECT"}, "db.*", []string{"u"}, false, "GRANT SELECT ON db.* TO u"},
		{"GRANT ALTER UPDATE ON db.t TO u", true, false, false, false, true, true,
			[]string{"ALTER UPDATE"}, "db.t", []string{"u"}, false, "GRANT ALTER UPDATE ON db.t TO u"},
		{"GRANT SELECT ON db.t TO CURRENT_USER", true, false, false, false, true, true,
			[]string{"SELECT"}, "db.t", []string{"CURRENT_USER"}, false, "GRANT SELECT ON db.t TO CURRENT_USER"},
		{"REVOKE GRANT OPTION FOR SELECT ON db.t FROM u", true, true, false, false, true, true,
			[]string{"SELECT"}, "db.t", []string{"u"}, true, "REVOKE GRANT OPTION FOR SELECT ON db.t FROM u"},
		// ON CLUSTER stripped from the marker; structure intact.
		{"GRANT SELECT ON db.t ON CLUSTER c TO u", true, false, false, false, true, true,
			[]string{"SELECT"}, "db.t", []string{"u"}, false, "GRANT SELECT ON db.t TO u"},
		// CURRENT GRANTS parses as a privilege (handler rejects it).
		{"GRANT CURRENT GRANTS ON db.t TO u", true, false, false, false, true, true,
			[]string{"CURRENT GRANTS"}, "db.t", []string{"u"}, false, "GRANT CURRENT GRANTS ON db.t TO u"},
		// Unstructured forms — flags set, generic parse skipped.
		{"ATTACH GRANT SELECT ON db.t TO u", true, false, true, false, false, false, nil, "", nil, false, ""},
		{"GRANT SELECT ON db.t TO u WITH REPLACE OPTION", true, false, false, true, true, false, nil, "", nil, false, ""},
		{"GRANT role1 TO u", true, false, false, false, false, false, nil, "", nil, false, ""},
		// Not a grant.
		{"SELECT 1", false, false, false, false, false, false, nil, "", nil, false, ""},
	}
	for _, c := range cases {
		got, err := ParseGrant(e, c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		if got.IsGrantVerb != c.isGrant || got.IsRevoke != c.isRevoke || got.IsAttach != c.isAttach ||
			got.HasReplace != c.hasReplace || got.HasOn != c.hasOn || got.Structured != c.structured ||
			got.Securable != c.securable || got.GrantOption != c.grantOption || got.Marker != c.marker {
			t.Errorf("%q: got %+v", c.sql, got)
		}
		var names []string
		for _, p := range got.Privileges {
			names = append(names, p.Name)
		}
		if !strSliceEq(names, c.privNames) || !strSliceEq(got.Principals, c.principals) {
			t.Errorf("%q: privs=%v principals=%v", c.sql, names, got.Principals)
		}
	}
}

func TestParseGrant_columns(t *testing.T) {
	e := newTestEngine(t)
	got, err := ParseGrant(e, "GRANT SELECT(c1, c2) ON db.t TO u")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Structured || len(got.Privileges) != 1 || got.Privileges[0].Name != "SELECT" ||
		len(got.Privileges[0].Columns) != 2 {
		t.Fatalf("got %+v", got)
	}
}

func strSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 3: Run to verify it fails** — `…go test ./internal/engine -run TestParseGrant` → FAIL (`ParseGrant` undefined).

- [ ] **Step 4: Write the implementation** (`internal/engine/grant.go`)

```go
package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// GrantPrivilege is one privilege in a GRANT/REVOKE, with the columns of a
// column-level grant (non-empty → the handler rejects it).
type GrantPrivilege struct {
	Name    string   // "SELECT", "ALTER UPDATE", "ALL", "CURRENT GRANTS", … (as polyglot emits)
	Columns []string // GRANT SELECT(c1,c2) → ["c1","c2"]; empty otherwise
}

// GrantParse is the recovered structure of a GRANT / REVOKE. The engine extracts
// the shape (token-level flags for the forms polyglot's generic dialect can't
// parse, plus the generic node for the rest); the handler owns every policy
// decision and reject code.
type GrantParse struct {
	IsGrantVerb bool // leading token GRANT / REVOKE / ATTACH GRANT — else not ours
	IsRevoke    bool
	IsAttach    bool // ATTACH GRANT (system-internal form)
	HasReplace  bool // … WITH REPLACE OPTION
	HasOn       bool // an ON <securable> clause exists (absent → role-membership grant)
	Structured  bool // generic-dialect parse succeeded → the fields below are populated

	Privileges  []GrantPrivilege
	Securable   string   // "db.t" / "db.*" / "*.*" / "t" (flat, from the generic node)
	Principals  []string // grantee names in source order ("u", "CURRENT_USER", "ALL")
	GrantOption bool     // WITH GRANT OPTION (GRANT) / GRANT OPTION FOR (REVOKE)
	Marker      string   // canonical CH SQL (ON CLUSTER stripped) for the marker SELECT
}

// ParseGrant recovers GRANT/REVOKE structure. The clickhouse dialect renders
// GRANT/REVOKE as an opaque `command` node; the generic dialect structures them
// but FAILS on ATTACH GRANT, `… WITH REPLACE OPTION`, and the role-membership form
// (no ON clause), and on `ON CLUSTER …`. ParseGrant detects those at the token
// level (so the handler can reject with the right code instead of surfacing a
// generic parse error as a SyntaxError), strips any `ON CLUSTER <name>` fragment
// (which the generic parser rejects and the C++ handler drops anyway), then
// generic-parses the remainder and decodes the node. The marker SQL is the
// (cluster-free) generic AST regenerated under the clickhouse dialect — the same
// canonicalization the C++ handler gets from formatAst.
func ParseGrant(e Engine, sql string) (GrantParse, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return GrantParse{}, err
	}
	if len(toks) == 0 {
		return GrantParse{}, nil
	}
	var gp GrantParse
	switch strings.ToUpper(toks[0].Text) {
	case "GRANT":
		gp.IsGrantVerb = true
	case "REVOKE":
		gp.IsGrantVerb, gp.IsRevoke = true, true
	case "ATTACH":
		if len(toks) >= 2 && strings.EqualFold(toks[1].Text, "GRANT") {
			gp.IsGrantVerb, gp.IsAttach = true, true
			return gp, nil // handler rejects; generic parse would fail
		}
		return gp, nil
	default:
		return gp, nil // not a GRANT/REVOKE
	}

	gp.HasOn = tokensHaveSecurableOn(toks)
	gp.HasReplace = tokensHaveReplaceOption(toks)
	if !gp.HasOn || gp.HasReplace {
		return gp, nil // handler rejects on the flags; generic parse would fail
	}

	cleaned := stripOnCluster(sql, toks)
	node, perr := e.ParseGeneric(cleaned)
	if perr != nil {
		return gp, nil // exotic but valid GRANT; handler rejects as Unsupported (Structured=false)
	}
	if derr := decodeGrantNode(node, &gp); derr != nil {
		return gp, nil
	}
	marker, gerr := e.Generate(node)
	if gerr != nil {
		return gp, nil
	}
	gp.Marker = marker
	gp.Structured = true
	return gp, nil
}

// tokensHaveSecurableOn reports whether an ON token introduces a securable (i.e.
// an ON NOT immediately followed by CLUSTER). Role-membership grants have no ON;
// `ON CLUSTER` alone is not a securable ON.
func tokensHaveSecurableOn(toks []rawToken) bool {
	for i, tk := range toks {
		if tk.TokenType == "ON" && (i+1 >= len(toks) || toks[i+1].TokenType != "CLUSTER") {
			return true
		}
	}
	return false
}

// tokensHaveReplaceOption reports whether the token stream contains `WITH REPLACE`
// (the `… WITH REPLACE OPTION` form polyglot's generic dialect rejects).
func tokensHaveReplaceOption(toks []rawToken) bool {
	for i := 0; i+1 < len(toks); i++ {
		if strings.EqualFold(toks[i].Text, "WITH") && strings.EqualFold(toks[i+1].Text, "REPLACE") {
			return true
		}
	}
	return false
}

// stripOnCluster removes the `ON CLUSTER <name>` byte span (a SECOND ON followed
// by CLUSTER + the cluster name) from sql, using the token spans. Leaves the rest
// verbatim; the resulting double space is harmless (the marker is regenerated).
func stripOnCluster(sql string, toks []rawToken) string {
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].TokenType == "ON" && toks[i+1].TokenType == "CLUSTER" {
			start, end := toks[i].Span.Start, toks[i+2].Span.End
			if start >= 0 && end <= len(sql) && start < end {
				return sql[:start] + sql[end:]
			}
		}
	}
	return sql
}

// decodeGrantNode reads the generic-dialect grant/revoke node into gp.
func decodeGrantNode(node AST, gp *GrantParse) error {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(node, &env); err != nil {
		return err
	}
	body, ok := env["grant"]
	if !ok {
		if body, ok = env["revoke"]; !ok {
			return fmt.Errorf("engine: not a grant/revoke node")
		}
	}
	var raw struct {
		Privileges []struct {
			Name    string   `json:"name"`
			Columns []string `json:"columns"`
		} `json:"privileges"`
		Securable struct {
			Name string `json:"name"`
		} `json:"securable"`
		Principals []struct {
			Name struct {
				Name string `json:"name"`
			} `json:"name"`
		} `json:"principals"`
		GrantOption bool `json:"grant_option"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	for _, p := range raw.Privileges {
		gp.Privileges = append(gp.Privileges, GrantPrivilege{Name: p.Name, Columns: p.Columns})
	}
	gp.Securable = raw.Securable.Name
	for _, pr := range raw.Principals {
		gp.Principals = append(gp.Principals, pr.Name.Name)
	}
	gp.GrantOption = raw.GrantOption
	return nil
}
```

- [ ] **Step 5: Run to verify it passes** — `…go test ./internal/engine -run TestParseGrant` → PASS. Then full `…go test ./internal/engine` (with FFI) → PASS; `go test ./internal/engine` (no FFI) → SKIP.

- [ ] **Step 6: gofmt + vet + commit**

```bash
gofmt -w internal/engine/engine.go internal/engine/polyglot.go internal/engine/grant.go internal/engine/grant_test.go
go vet ./... && go build ./...
git add internal/engine/engine.go internal/engine/polyglot.go internal/engine/grant.go internal/engine/grant_test.go
git commit -m "feat(engine): ParseGrant + generic-dialect parse seam for GRANT/REVOKE"
```

---

## Task 3: Handlers — EXISTS + SHOW CREATE (`RewriteExistsShowCreate`)

**Files:**
- Create: `internal/handlers/exists.go`, `internal/handlers/exists_test.go`

- [ ] **Step 1: Write the failing test** (`internal/handlers/exists_test.go`)

```go
package handlers

import (
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
)

// dynOpts builds a single dynamic-args TableNameRewrite option for handler tests.
func dynOpts(da *pb.RewriteTableDynamicArgs) []*pb.RewriteOption {
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: da}}}}
}

func TestRewriteExistsShowCreate(t *testing.T) {
	e := newEngine(t)
	dyn := &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"logical1": "phys1"},
		KnownPhysicalDatabases: []string{"phys1"},
		Delim:                  "_",
	}

	t.Run("exists none-mode bare", func(t *testing.T) {
		resp, handled, err := RewriteExistsShowCreate(e, parse(t, e, "EXISTS t"), "EXISTS t", nil)
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if resp.Code != pb.RewriteCode_Success || resp.StatementType != pb.StatementType_STATEMENT_TYPE_EXISTS_TABLE {
			t.Fatalf("code=%v stmt=%v", resp.Code, resp.StatementType)
		}
		if resp.SqlAfterRewrite != "EXISTS TABLE t" {
			t.Errorf("sql=%q", resp.SqlAfterRewrite)
		}
		if len(resp.OriginalAccessedTables) != 1 || resp.OriginalAccessedTables[0].GetOriginalTable() != "t" {
			t.Errorf("accessed=%+v", resp.OriginalAccessedTables)
		}
	})

	t.Run("exists dynamic rewrite", func(t *testing.T) {
		resp, handled, err := RewriteExistsShowCreate(e, parse(t, e, "EXISTS logical1.t"), "EXISTS logical1.t", dynOpts(dyn))
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		// physical db = phys1; physical table = buildDynamicTableName("logical1","t") = "logical1.t"
		// → quoted WhenNecessary because of the dot.
		if resp.SqlAfterRewrite != "EXISTS TABLE phys1.`logical1.t`" {
			t.Errorf("sql=%q", resp.SqlAfterRewrite)
		}
		if resp.TableRewrites["logical1.t"] != "phys1.logical1.t" {
			t.Errorf("table_rewrites=%v", resp.TableRewrites)
		}
	})

	t.Run("exists database rejected", func(t *testing.T) {
		resp, handled, _ := RewriteExistsShowCreate(e, parse(t, e, "EXISTS DATABASE db"), "EXISTS DATABASE db", dynOpts(dyn))
		if !handled || resp.Code != pb.RewriteCode_UnsupportedStatement {
			t.Fatalf("handled=%v code=%v", handled, resp.Code)
		}
	})

	t.Run("show create table dynamic", func(t *testing.T) {
		resp, handled, _ := RewriteExistsShowCreate(e, parse(t, e, "SHOW CREATE TABLE logical1.t"), "SHOW CREATE TABLE logical1.t", dynOpts(dyn))
		if !handled || resp.StatementType != pb.StatementType_STATEMENT_TYPE_SHOW_CREATE_TABLE {
			t.Fatalf("handled=%v stmt=%v", handled, resp.StatementType)
		}
		if resp.SqlAfterRewrite != "SHOW CREATE TABLE phys1.`logical1.t`" {
			t.Errorf("sql=%q", resp.SqlAfterRewrite)
		}
	})

	t.Run("show create view rejected", func(t *testing.T) {
		resp, handled, _ := RewriteExistsShowCreate(e, parse(t, e, "SHOW CREATE VIEW v"), "SHOW CREATE VIEW v", dynOpts(dyn))
		if !handled || resp.Code != pb.RewriteCode_UnsupportedStatement {
			t.Fatalf("handled=%v code=%v", handled, resp.Code)
		}
	})

	t.Run("exists unresolvable logical invalid", func(t *testing.T) {
		resp, handled, _ := RewriteExistsShowCreate(e, parse(t, e, "EXISTS unknown.t"), "EXISTS unknown.t", dynOpts(dyn))
		if !handled || resp.Code != pb.RewriteCode_InvalidRewriteRequest {
			t.Fatalf("handled=%v code=%v", handled, resp.Code)
		}
	})

	t.Run("not exists falls through", func(t *testing.T) {
		_, handled, err := RewriteExistsShowCreate(e, parse(t, e, "USE db"), "USE db", dynOpts(dyn))
		if handled || err != nil {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
	})
}
```

Add the shared `parse` helper to `internal/handlers/exists_test.go` ONLY IF it does not already exist in the package's test files (Phase 1-3 tests may already define one — check `select_test.go` / `writes_test.go`; reuse it and drop this definition if present):

```go
func parse(t *testing.T, e engine.Engine, sql string) engine.AST {
	t.Helper()
	ast, err := e.ParseOne(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	return ast
}
```

- [ ] **Step 2: Run to verify it fails** — `…go test ./internal/handlers -run TestRewriteExistsShowCreate` → FAIL (`RewriteExistsShowCreate` undefined).

- [ ] **Step 3: Write the implementation** (`internal/handlers/exists.go`)

```go
package handlers

import (
	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
)

// RewriteExistsShowCreate ports exists.cc + show_create.cc — two near-identical
// single-target handlers (EXISTS TABLE / SHOW CREATE TABLE) sharing a tokenize
// extractor and the write-side name-rewrite machinery. Returns (resp, handled,
// err) with the RewriteWrite contract; native.go calls it after RewriteDBLevel.
//
// Only the TABLE object is accepted; the DATABASE/VIEW/DICTIONARY variants are
// rejected as UnsupportedStatement. The accepted target runs through
// decideWriteTarget (records accessed + table_rewrites; rejects remote/invalid),
// and the output is the canonical `EXISTS TABLE <name>` / `SHOW CREATE TABLE
// <name>` form — always with the TABLE keyword, db-qualified and backtick-quoted
// WhenNecessary, matching C++ formatAst.
func RewriteExistsShowCreate(e engine.Engine, ast engine.AST, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, bool, error) {
	kind, err := engine.NodeKind(ast)
	if err != nil {
		return nil, false, err
	}
	if kind != engine.NodeCommand {
		return nil, false, nil // EXISTS / SHOW CREATE only ever arrive as command nodes
	}
	t, err := engine.ParseObjectTarget(e, sql)
	if err != nil {
		return nil, false, err
	}
	if t.Verb == engine.VerbNone {
		return nil, false, nil // not EXISTS / SHOW CREATE → caller falls through
	}

	stmt := pb.StatementType_STATEMENT_TYPE_EXISTS_TABLE
	keyword := "EXISTS"
	if t.Verb == engine.VerbShowCreate {
		stmt, keyword = pb.StatementType_STATEMENT_TYPE_SHOW_CREATE_TABLE, "SHOW CREATE"
	}
	resp := newWriteResp(stmt)

	if t.ObjType != "TABLE" {
		rejectUnsupported(resp, keyword+" "+t.ObjType+" is not supported; only "+keyword+" TABLE is allowed")
		return resp, true, nil
	}

	sel := nameresolve.FindActive(opts)
	tt := engine.TableTarget{DB: t.DB, Table: t.Table}
	d, ok := decideWriteTarget(tt, keyword+" TABLE", sel, resp)
	if !ok {
		return resp, true, nil // reject populated (accessed recorded first, like C++)
	}
	db, table := t.DB, t.Table
	if d.Action == engine.ActionRename {
		db, table = d.NewDB, d.NewTable
	}
	resp.SqlAfterRewrite = buildObjectSQL(keyword, t.Temporary, db, table)
	return resp, true, nil
}

// buildObjectSQL renders the canonical EXISTS / SHOW CREATE output: the verb, an
// always-present TABLE keyword (ClickHouse normalizes the bare and TEMPORARY
// forms to include it), and the db-qualified, WhenNecessary-backtick-quoted name
// (engine.QuoteQualified — the same quoting the RENAME splice uses, so a dotted
// dynamic table name like `tenant.events` is quoted as one identifier).
func buildObjectSQL(keyword string, temporary bool, db, table string) string {
	s := keyword + " "
	if temporary {
		s += "TEMPORARY "
	}
	return s + "TABLE " + engine.QuoteQualified(db, table)
}
```

- [ ] **Step 4: Run to verify it passes** — `…go test ./internal/handlers -run TestRewriteExistsShowCreate` → PASS.

- [ ] **Step 5: gofmt + vet + commit**

```bash
gofmt -w internal/handlers/exists.go internal/handlers/exists_test.go
go vet ./... && go build ./...
git add internal/handlers/exists.go internal/handlers/exists_test.go
git commit -m "feat(handlers): EXISTS TABLE / SHOW CREATE TABLE single-target rewrite"
```

---

## Task 4: Handlers — GRANT / REVOKE (`RewriteGrant`)

**Files:**
- Create: `internal/handlers/grant.go`, `internal/handlers/grant_test.go`

- [ ] **Step 1: Write the failing test** (`internal/handlers/grant_test.go`)

```go
package handlers

import (
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
)

func grantDyn() *pb.RewriteTableDynamicArgs {
	return &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"logical1": "phys1"},
		KnownPhysicalDatabases: []string{"phys1"},
		Delim:                  "_",
	}
}

func TestRewriteGrant(t *testing.T) {
	e := newEngine(t)
	dyn := grantDyn()

	t.Run("grant select table", func(t *testing.T) {
		resp, handled, err := RewriteGrant(e, parse(t, e, "GRANT SELECT ON logical1.t TO u"), "GRANT SELECT ON logical1.t TO u", dynOpts(dyn))
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if resp.Code != pb.RewriteCode_Success || resp.StatementType != pb.StatementType_STATEMENT_TYPE_GRANT {
			t.Fatalf("code=%v stmt=%v", resp.Code, resp.StatementType)
		}
		if resp.SqlAfterRewrite != "SELECT 'GRANT SELECT ON logical1.t TO u' AS gstmt" {
			t.Errorf("sql=%q", resp.SqlAfterRewrite)
		}
		if len(resp.PrivilegesDeltas) != 1 {
			t.Fatalf("deltas=%+v", resp.PrivilegesDeltas)
		}
		d := resp.PrivilegesDeltas[0]
		if d.GetAction() != pb.PrivilegeDelta_ACTION_GRANT || d.GetScope() != pb.PrivilegeDelta_SCOPE_TABLE ||
			d.GetOriginalDatabase() != "logical1" || d.GetLogicalDatabase() != "logical1" ||
			d.GetPhysicalDatabase() != "phys1" || d.GetOriginalTable() != "t" ||
			d.GetPhysicalTable() != "logical1.t" || d.GetGrantOption() ||
			len(d.GetPrivileges()) != 1 || d.GetPrivileges()[0] != "SELECT" ||
			len(d.GetGrantees()) != 1 || d.GetGrantees()[0].GetName() != "u" {
			t.Errorf("delta=%+v", d)
		}
	})

	t.Run("grant two privs two grantees with option", func(t *testing.T) {
		resp, _, _ := RewriteGrant(e, parse(t, e, "GRANT SELECT, INSERT ON logical1.t TO u1, u2 WITH GRANT OPTION"),
			"GRANT SELECT, INSERT ON logical1.t TO u1, u2 WITH GRANT OPTION", dynOpts(dyn))
		if len(resp.PrivilegesDeltas) != 2 {
			t.Fatalf("deltas=%d", len(resp.PrivilegesDeltas))
		}
		for i, want := range []string{"SELECT", "INSERT"} {
			d := resp.PrivilegesDeltas[i]
			if d.GetPrivileges()[0] != want || !d.GetGrantOption() || len(d.GetGrantees()) != 2 {
				t.Errorf("delta[%d]=%+v", i, d)
			}
		}
	})

	t.Run("grant on db.* is scope database", func(t *testing.T) {
		resp, _, _ := RewriteGrant(e, parse(t, e, "GRANT SELECT ON logical1.* TO u"), "GRANT SELECT ON logical1.* TO u", dynOpts(dyn))
		d := resp.PrivilegesDeltas[0]
		if d.GetScope() != pb.PrivilegeDelta_SCOPE_DATABASE || d.GetOriginalTable() != "" || d.GetPhysicalTable() != "" {
			t.Errorf("delta=%+v", d)
		}
	})

	t.Run("revoke", func(t *testing.T) {
		resp, _, _ := RewriteGrant(e, parse(t, e, "REVOKE SELECT ON logical1.t FROM u"), "REVOKE SELECT ON logical1.t FROM u", dynOpts(dyn))
		if resp.StatementType != pb.StatementType_STATEMENT_TYPE_REVOKE ||
			resp.SqlAfterRewrite != "SELECT 'REVOKE SELECT ON logical1.t FROM u' AS rstmt" ||
			resp.PrivilegesDeltas[0].GetAction() != pb.PrivilegeDelta_ACTION_REVOKE {
			t.Errorf("resp=%+v", resp)
		}
	})

	t.Run("current_user grantee", func(t *testing.T) {
		resp, _, _ := RewriteGrant(e, parse(t, e, "GRANT SELECT ON logical1.t TO CURRENT_USER"), "GRANT SELECT ON logical1.t TO CURRENT_USER", dynOpts(dyn))
		g := resp.PrivilegesDeltas[0].GetGrantees()[0]
		if !g.GetIsCurrentUser() || g.GetName() != "" {
			t.Errorf("grantee=%+v", g)
		}
	})

	t.Run("on cluster stripped in marker", func(t *testing.T) {
		resp, _, _ := RewriteGrant(e, parse(t, e, "GRANT SELECT ON logical1.t ON CLUSTER c TO u"), "GRANT SELECT ON logical1.t ON CLUSTER c TO u", dynOpts(dyn))
		if resp.SqlAfterRewrite != "SELECT 'GRANT SELECT ON logical1.t TO u' AS gstmt" {
			t.Errorf("sql=%q", resp.SqlAfterRewrite)
		}
	})

	// Reject matrix (code is gated; message is not).
	rejects := []struct {
		name, sql string
		opts      []*pb.RewriteOption
		code      pb.RewriteCode
	}{
		{"global scope", "GRANT SELECT ON *.* TO u", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"column level", "GRANT SELECT(c) ON logical1.t TO u", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"role membership", "GRANT role1 TO u", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"attach grant", "ATTACH GRANT SELECT ON logical1.t TO u", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"with replace", "GRANT SELECT ON logical1.t TO u WITH REPLACE OPTION", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"current grants", "GRANT CURRENT GRANTS ON logical1.t TO u", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"to all", "GRANT SELECT ON logical1.t TO ALL", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"no dynamic args", "GRANT SELECT ON logical1.t TO u", nil, pb.RewriteCode_UnsupportedStatement},
		{"unresolvable db", "GRANT SELECT ON unknown.t TO u", dynOpts(dyn), pb.RewriteCode_InvalidRewriteRequest},
		{"unqualified no upstream", "GRANT SELECT ON t TO u", dynOpts(dyn), pb.RewriteCode_InvalidRewriteRequest},
	}
	for _, rc := range rejects {
		t.Run("reject/"+rc.name, func(t *testing.T) {
			resp, handled, err := RewriteGrant(e, parse(t, e, rc.sql), rc.sql, rc.opts)
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if resp.Code != rc.code {
				t.Errorf("code=%v want %v (%s)", resp.Code, rc.code, resp.Message)
			}
		})
	}

	t.Run("unqualified uses upstream", func(t *testing.T) {
		d2 := grantDyn()
		d2.UpstreamLogicalDatabaseInContext = "logical1"
		resp, _, _ := RewriteGrant(e, parse(t, e, "GRANT SELECT ON t TO u"), "GRANT SELECT ON t TO u", dynOpts(d2))
		if resp.Code != pb.RewriteCode_Success {
			t.Fatalf("code=%v (%s)", resp.Code, resp.Message)
		}
		d := resp.PrivilegesDeltas[0]
		if d.GetOriginalDatabase() != "" || d.GetLogicalDatabase() != "logical1" || d.GetPhysicalTable() != "logical1.t" {
			t.Errorf("delta=%+v", d)
		}
	})

	t.Run("not a grant falls through", func(t *testing.T) {
		_, handled, _ := RewriteGrant(e, parse(t, e, "SELECT 1"), "SELECT 1", dynOpts(dyn))
		if handled {
			t.Fatal("handled=true, want false")
		}
	})
}
```

- [ ] **Step 2: Run to verify it fails** — `…go test ./internal/handlers -run TestRewriteGrant` → FAIL (`RewriteGrant` undefined).

- [ ] **Step 3: Write the implementation** (`internal/handlers/grant.go`)

```go
package handlers

import (
	"strings"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
)

// RewriteGrant ports grant.cc. GRANT/REVOKE are never executed against the
// physical ClickHouse (a logical DB shares its physical with sibling tenants via
// prefix-sharing, so a CH-level grant would leak across tenants). The handler
// validates the statement against dynamic_args, emits one PrivilegeDelta per
// privilege (the per-element fan-out C++ gets from ClickHouse's parser, which
// splits `GRANT SELECT, INSERT` into one AccessRightsElement per privilege), and
// rewrites the SQL to a marker `SELECT '<canonical GRANT/REVOKE>' AS gstmt|rstmt`.
// Reject order matches grant.cc exactly (it decides which code wins when several
// conditions hold). Returns (resp, handled, err) with the RewriteWrite contract.
func RewriteGrant(e engine.Engine, ast engine.AST, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, bool, error) {
	kind, err := engine.NodeKind(ast)
	if err != nil {
		return nil, false, err
	}
	if kind != engine.NodeCommand {
		return nil, false, nil
	}
	gp, err := engine.ParseGrant(e, sql)
	if err != nil {
		return nil, false, err
	}
	if !gp.IsGrantVerb {
		return nil, false, nil // not GRANT/REVOKE → caller falls through
	}

	kw := "GRANT"
	stmt := pb.StatementType_STATEMENT_TYPE_GRANT
	if gp.IsRevoke {
		kw, stmt = "REVOKE", pb.StatementType_STATEMENT_TYPE_REVOKE
	}
	resp := newGrantResp(stmt)

	// Statement-level rejects, in grant.cc order.
	if gp.IsAttach {
		rejectUnsupported(resp, "ATTACH GRANT is not supported")
		return resp, true, nil
	}
	if !gp.HasOn {
		rejectUnsupported(resp, kw+" of a role to a user/role (role-membership grant) is not supported")
		return resp, true, nil
	}
	if gp.HasReplace {
		rejectUnsupported(resp, kw+" WITH REPLACE OPTION is not supported")
		return resp, true, nil
	}
	for _, p := range gp.Privileges {
		if strings.EqualFold(p.Name, "CURRENT GRANTS") {
			rejectUnsupported(resp, "GRANT CURRENT GRANTS is not supported")
			return resp, true, nil
		}
	}
	if !gp.Structured {
		// Pre-checks passed but polyglot's generic dialect couldn't structure it
		// (an exotic GRANT form). Parity-safe reject rather than mis-emit.
		rejectUnsupported(resp, kw+" form is not supported")
		return resp, true, nil
	}

	dyn := nameresolve.FindDynamicArgs(opts)
	if dyn == nil {
		rejectUnsupported(resp, kw+" requires a TableNameRewrite/dynamic_args option to validate against")
		return resp, true, nil
	}

	grantees, ok := buildGrantees(resp, kw, gp.Principals)
	if !ok {
		return resp, true, nil
	}

	origDB, origTable, scopeDatabase, anyDatabase := splitSecurable(gp.Securable)
	if anyDatabase {
		rejectUnsupported(resp, kw+" ON *.* (global scope) is not supported")
		return resp, true, nil
	}

	action := pb.PrivilegeDelta_ACTION_GRANT
	if gp.IsRevoke {
		action = pb.PrivilegeDelta_ACTION_REVOKE
	}

	// Per-privilege fan-out. Logical/physical resolution is lazy (resolved at the
	// first privilege) so the per-element reject precedence matches grant.cc:
	// column-level (Unsupported) is checked before logical/physical (Invalid).
	resolved := false
	var logical, physical, prefix string
	for _, p := range gp.Privileges {
		if len(p.Columns) > 0 {
			rejectUnsupported(resp, kw+" with column-level granularity is not supported")
			return resp, true, nil
		}
		if !resolved {
			logical = origDB
			if logical == "" {
				logical = dyn.GetUpstreamLogicalDatabaseInContext()
			}
			if logical == "" {
				rejectInvalid(resp, kw+" target '"+origTable+"' is unqualified and no upstream_logical_database_in_context is set; caller must send `USE <db>` or qualify the target")
				return resp, true, nil
			}
			var pok bool
			physical, pok = nameresolve.ResolvePhysicalDatabase(logical, dyn)
			if !pok {
				rejectInvalid(resp, kw+" target references logical database '"+logical+"' which is not in database_map and not a known physical database; user does not have this database to grant on")
				return resp, true, nil
			}
			prefix = nameresolve.BuildDynamicTablePrefix(logical, dyn)
			resolved = true
		}
		delta := &pb.PrivilegeDelta{
			Action:           action,
			OriginalDatabase: origDB,
			LogicalDatabase:  logical,
			PhysicalDatabase: physical,
			GrantOption:      gp.GrantOption,
			Privileges:       []string{p.Name},
			Grantees:         grantees,
		}
		if scopeDatabase {
			delta.Scope = pb.PrivilegeDelta_SCOPE_DATABASE
		} else {
			delta.Scope = pb.PrivilegeDelta_SCOPE_TABLE
			delta.OriginalTable = origTable
			delta.PhysicalTable = prefix + origTable
		}
		resp.PrivilegesDeltas = append(resp.PrivilegesDeltas, delta)
	}

	marker := "gstmt"
	if gp.IsRevoke {
		marker = "rstmt"
	}
	resp.SqlAfterRewrite = "SELECT '" + escapeSQLLiteral(gp.Marker) + "' AS " + marker
	return resp, true, nil
}

func newGrantResp(stmt pb.StatementType) *pb.RewriteSQLResponse {
	return &pb.RewriteSQLResponse{Code: pb.RewriteCode_Success, Message: "success", StatementType: stmt}
}

// buildGrantees translates principal names to proto Grantees. Rejects the
// variants the privilege-delta path doesn't model — ALL — and an empty list (a
// well-formed GRANT always names a grantee). CURRENT_USER becomes a flagged
// grantee. Mirrors grant.cc buildGrantees (ALL/EXCEPT → Unsupported; the EXCEPT
// form fails the generic parse upstream and is rejected as `<kw> form is not
// supported`, so only ALL is reachable here).
func buildGrantees(resp *pb.RewriteSQLResponse, kw string, principals []string) ([]*pb.PrivilegeDelta_Grantee, bool) {
	dir := "GRANT TO "
	if kw == "REVOKE" {
		dir = "REVOKE FROM "
	}
	var out []*pb.PrivilegeDelta_Grantee
	for _, name := range principals {
		switch {
		case strings.EqualFold(name, "ALL"):
			rejectUnsupported(resp, dir+"ALL is not supported")
			return nil, false
		case strings.EqualFold(name, "CURRENT_USER"):
			out = append(out, &pb.PrivilegeDelta_Grantee{IsCurrentUser: true})
		default:
			out = append(out, &pb.PrivilegeDelta_Grantee{Name: name})
		}
	}
	if len(out) == 0 {
		rejectInvalid(resp, kw+" has no grantees")
		return nil, false
	}
	return out, true
}

// splitSecurable parses polyglot's flat securable ("db.t" / "db.*" / "*.*" / "t")
// into (database, table, scopeDatabase, anyDatabase). The table part "*" means
// ON db.* (SCOPE_DATABASE); the database part "*" means ON *.* (global, rejected).
// Splits on the LAST dot so a single-segment name is a bare table.
func splitSecurable(s string) (db, table string, scopeDatabase, anyDatabase bool) {
	if dot := strings.LastIndexByte(s, '.'); dot >= 0 {
		db, table = s[:dot], s[dot+1:]
	} else {
		table = s
	}
	if db == "*" {
		return db, table, false, true
	}
	if table == "*" {
		return db, "", true, false
	}
	return db, table, false, false
}
```

- [ ] **Step 4: Run to verify it passes** — `…go test ./internal/handlers -run TestRewriteGrant` → PASS.

- [ ] **Step 5: gofmt + vet + commit**

```bash
gofmt -w internal/handlers/grant.go internal/handlers/grant_test.go
go vet ./... && go build ./...
git add internal/handlers/grant.go internal/handlers/grant_test.go
git commit -m "feat(handlers): GRANT/REVOKE privilege-delta extraction + marker SELECT"
```

---

## Task 5: Native routing — wire EXISTS/SHOW CREATE + GRANT after db-level

**Files:**
- Modify: `native.go` (insert two dispatch blocks), `internal/handlers/dblevel.go` (comment refresh)
- Test: `native_test.go`

- [ ] **Step 1: Write the failing test** (append to `native_test.go`; reuse the package's existing engine helper — `newEngine` per `native_test.go`, and the existing options-builder pattern; if helpers differ, adapt to what the file already has)

```go
func TestNativeRewrite_phase4(t *testing.T) {
	e := newEngine(t)
	optFn := func(string) []*pb.RewriteOption {
		return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
				DynamicArgs: &pb.RewriteTableDynamicArgs{
					DatabaseMap:            map[string]string{"logical1": "phys1"},
					KnownPhysicalDatabases: []string{"phys1"},
					Delim:                  "_",
				}}}}}
	}
	r := New(e, WithOptions(optFn))

	t.Run("exists table rewritten", func(t *testing.T) {
		res, err := r.Rewrite(context.Background(), "EXISTS logical1.t", "acct")
		if err != nil {
			t.Fatal(err)
		}
		if res.Code != pb.RewriteCode_Success || res.StatementType != pb.StatementType_STATEMENT_TYPE_EXISTS_TABLE {
			t.Fatalf("code=%v stmt=%v", res.Code, res.StatementType)
		}
		if res.SQL != "EXISTS TABLE phys1.`logical1.t`" {
			t.Errorf("sql=%q", res.SQL)
		}
	})

	t.Run("show create no longer mis-stamped", func(t *testing.T) {
		res, _ := r.Rewrite(context.Background(), "SHOW CREATE TABLE logical1.t", "acct")
		if res.StatementType != pb.StatementType_STATEMENT_TYPE_SHOW_CREATE_TABLE {
			t.Errorf("stmt=%v", res.StatementType)
		}
		if res.SQL != "SHOW CREATE TABLE phys1.`logical1.t`" {
			t.Errorf("sql=%q", res.SQL)
		}
	})

	t.Run("grant produces deltas + marker", func(t *testing.T) {
		res, _ := r.Rewrite(context.Background(), "GRANT SELECT ON logical1.t TO u", "acct")
		if res.Code != pb.RewriteCode_Success || res.StatementType != pb.StatementType_STATEMENT_TYPE_GRANT {
			t.Fatalf("code=%v stmt=%v", res.Code, res.StatementType)
		}
		if res.SQL != "SELECT 'GRANT SELECT ON logical1.t TO u' AS gstmt" || len(res.PrivilegesDeltas) != 1 {
			t.Errorf("sql=%q deltas=%d", res.SQL, len(res.PrivilegesDeltas))
		}
	})

	t.Run("grant reject echoes input (design §8)", func(t *testing.T) {
		res, _ := r.Rewrite(context.Background(), "GRANT SELECT ON *.* TO u", "acct")
		if res.Code != pb.RewriteCode_UnsupportedStatement {
			t.Fatalf("code=%v", res.Code)
		}
		if res.SQL != "GRANT SELECT ON *.* TO u" {
			t.Errorf("sql=%q want input echo", res.SQL)
		}
	})
}
```

(If `RewriteResult` exposes privilege deltas under a different field name than `PrivilegesDeltas`, adjust — check `resultFromPB` / the `RewriteResult` type. The proto field is `PrivilegesDeltas`.)

- [ ] **Step 2: Run to verify it fails** — `…go test . -run TestNativeRewrite_phase4` → FAIL (SHOW CREATE mis-stamped / EXISTS not handled).

- [ ] **Step 3: Insert the dispatch blocks in `native.go`** — directly AFTER the `RewriteDBLevel` block (the `}` closing the `else if handled` at ~line 89), BEFORE the `// Phase 1: route SELECT` comment:

```go
	// Phase 4: EXISTS / SHOW CREATE (single-target), then GRANT / REVOKE
	// (privilege deltas) — after db-level, before SELECT. Both match only
	// `command` nodes and recognize disjoint verbs, so their relative order is
	// irrelevant; this mirrors the C++ server order (exists → show_create → grant).
	if xresp, handled, xerr := handlers.RewriteExistsShowCreate(r.engine, ast, sql, opts); xerr != nil {
		return RewriteResult{}, xerr
	} else if handled {
		if xresp.GetCode() != pb.RewriteCode_Success && xresp.GetSqlAfterRewrite() == "" {
			xresp.SqlAfterRewrite = sql // §8: always-runnable
		}
		r.mu.Lock()
		r.last = &callContext{sql: sql, account: account}
		r.mu.Unlock()
		return resultFromPB(xresp), nil
	}
	if gresp, handled, gerr := handlers.RewriteGrant(r.engine, ast, sql, opts); gerr != nil {
		return RewriteResult{}, gerr
	} else if handled {
		if gresp.GetCode() != pb.RewriteCode_Success && gresp.GetSqlAfterRewrite() == "" {
			gresp.SqlAfterRewrite = sql // §8: always-runnable
		}
		r.mu.Lock()
		r.last = &callContext{sql: sql, account: account}
		r.mu.Unlock()
		return resultFromPB(gresp), nil
	}
```

- [ ] **Step 4: Refresh the SHOW CREATE defer comment in `internal/handlers/dblevel.go`** — the `info.ShowWhat == "CREATE"` branch (~line 46-53). Replace its body comment so it no longer says "until the Phase-4 handler lands":

```go
			if info.ShowWhat == "CREATE" {
				// SHOW CREATE {TABLE|DATABASE|VIEW|...} is NOT an ASTShowTablesQuery in
				// ClickHouse — don't let dispatchShowTables mis-stamp it SHOW_TABLES.
				// Defer so native routes it to RewriteExistsShowCreate (Phase 4), which
				// classifies it as SHOW_CREATE_TABLE and rewrites the single target.
				return nil, false, nil
			}
```

- [ ] **Step 5: Run to verify it passes** — `…go test . -run TestNativeRewrite_phase4` → PASS. Then full `…go test ./...` (with FFI) → all green; `go test ./...` (no FFI) → green via skips.

- [ ] **Step 6: gofmt + vet + commit**

```bash
gofmt -w native.go native_test.go internal/handlers/dblevel.go
go vet ./... && go build ./...
git add native.go native_test.go internal/handlers/dblevel.go
git commit -m "feat(native): route EXISTS/SHOW CREATE + GRANT/REVOKE after db-level"
```

---

## Task 6: Harness — `privileges_deltas` in Compare + Phase-4 golden corpus

**Files:**
- Modify: `internal/harness/compare.go`
- Create: `internal/harness/phase4_golden_test.go`, `internal/harness/testdata/phase4_cases.json`

- [ ] **Step 1: Add `privileges_deltas` to `Compare`** (`internal/harness/compare.go`) — after the `failed_cte_aliases` block, before the `sql_after_rewrite` block:

```go
	if !privilegeDeltasEqual(got.GetPrivilegesDeltas(), want.GetPrivilegesDeltas()) {
		add("privileges_deltas", got.GetPrivilegesDeltas(), want.GetPrivilegesDeltas())
	}
```

And add the comparison helpers (proto messages carry unexported state, so compare field-by-field rather than reflect.DeepEqual on the message):

```go
// privilegeDeltasEqual compares two PrivilegeDelta lists field-by-field (proto
// messages can't be reflect.DeepEqual'd — they carry unexported state). Order is
// significant: the per-privilege fan-out preserves source order.
func privilegeDeltasEqual(a, b []*pb.PrivilegeDelta) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.GetAction() != y.GetAction() || x.GetScope() != y.GetScope() ||
			x.GetOriginalDatabase() != y.GetOriginalDatabase() ||
			x.GetLogicalDatabase() != y.GetLogicalDatabase() ||
			x.GetPhysicalDatabase() != y.GetPhysicalDatabase() ||
			x.GetOriginalTable() != y.GetOriginalTable() ||
			x.GetPhysicalTable() != y.GetPhysicalTable() ||
			x.GetGrantOption() != y.GetGrantOption() ||
			!reflect.DeepEqual(x.GetPrivileges(), y.GetPrivileges()) ||
			!granteesEqual(x.GetGrantees(), y.GetGrantees()) {
			return false
		}
	}
	return true
}

func granteesEqual(a, b []*pb.PrivilegeDelta_Grantee) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].GetName() != b[i].GetName() || a[i].GetIsCurrentUser() != b[i].GetIsCurrentUser() {
			return false
		}
	}
	return true
}
```

(`reflect` is already imported in compare.go.)

- [ ] **Step 2: Add a Compare unit test** to `internal/harness/compare_test.go` (no FFI needed — pure proto):

```go
func TestCompare_privilegeDeltas(t *testing.T) {
	mk := func() *pb.RewriteSQLResponse {
		return &pb.RewriteSQLResponse{PrivilegesDeltas: []*pb.PrivilegeDelta{{
			Action: pb.PrivilegeDelta_ACTION_GRANT, Scope: pb.PrivilegeDelta_SCOPE_TABLE,
			LogicalDatabase: "l", PhysicalDatabase: "p", OriginalTable: "t", PhysicalTable: "l.t",
			Privileges: []string{"SELECT"},
			Grantees:   []*pb.PrivilegeDelta_Grantee{{Name: "u"}},
		}}}
	}
	if d := Compare(mk(), mk(), nil); !d.Equal() {
		t.Errorf("identical deltas should match: %v", d.Mismatches)
	}
	diff := mk()
	diff.PrivilegesDeltas[0].Privileges = []string{"INSERT"}
	if d := Compare(diff, mk(), nil); d.Equal() {
		t.Error("differing privileges should not match")
	}
}
```

- [ ] **Step 3: Run to verify** — `go test ./internal/harness -run TestCompare_privilegeDeltas` (no FFI) → PASS.

- [ ] **Step 4: Create the golden test driver** (`internal/harness/phase4_golden_test.go`) — mirrors `dblevel_golden_test.go`, reusing `dblevelDynamicJSON`, `accessedJSON`, `checkAccessed`, `semanticSQLEq`, `codeByName`, `wantStmtType`, `newWriteRewriter`, `DialOracle`, `pbFromResult`, `Compare`:

```go
package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

type granteeJSON struct {
	Name          string `json:"name"`
	IsCurrentUser bool   `json:"is_current_user"`
}

type privilegeDeltaJSON struct {
	Action           string        `json:"action"` // "GRANT" / "REVOKE"
	Scope            string        `json:"scope"`  // "TABLE" / "DATABASE"
	OriginalDatabase string        `json:"original_database"`
	LogicalDatabase  string        `json:"logical_database"`
	PhysicalDatabase string        `json:"physical_database"`
	OriginalTable    string        `json:"original_table"`
	PhysicalTable    string        `json:"physical_table"`
	Privileges       []string      `json:"privileges"`
	Grantees         []granteeJSON `json:"grantees"`
	GrantOption      bool          `json:"grant_option"`
}

// phase4Case mirrors dblevelCase, swapping database_rewrites for the table-side
// (want_table_rewrites + want_accessed, for EXISTS/SHOW CREATE) and adding
// want_privileges_deltas (for GRANT/REVOKE). SQL is compared semantically by
// default; EXISTS/SHOW CREATE outputs re-parse to a `command` blob (semantic ≈
// exact), so want_sql must match C++ formatAst.
type phase4Case struct {
	Name              string              `json:"name"`
	SQL               string              `json:"sql"`
	Dynamic           *dblevelDynamicJSON `json:"dynamic"`
	WantCode          string              `json:"want_code"`
	WantStmt          string              `json:"want_stmt"`
	WantTableRewrites map[string]string   `json:"want_table_rewrites"`
	WantAccessed      []accessedJSON      `json:"want_accessed"`
	WantDeltas        []privilegeDeltaJSON `json:"want_privileges_deltas"`
	WantSQL           string              `json:"want_sql"`
	SQLExact          bool                `json:"sql_exact"`
	SQLContains       []string            `json:"sql_contains"`
	Reject            bool                `json:"reject"`
}

func (c phase4Case) options() []*pb.RewriteOption {
	if c.Dynamic == nil {
		return nil
	}
	da := &pb.RewriteTableDynamicArgs{
		DatabaseMap:                          c.Dynamic.DatabaseMap,
		KnownPhysicalDatabases:               c.Dynamic.KnownPhysicalDatabases,
		UpstreamLogicalDatabaseInContext:     c.Dynamic.UpstreamLogical,
		Delim:                                c.Dynamic.Delim,
		LogicalDatabaseToRemoteUpstreamIndex: c.Dynamic.LogicalDatabaseToRemoteUpstreamIndex,
	}
	if c.Dynamic.RemoteUpstreams != nil {
		da.RemoteUpstreams = map[string]*pb.RewriteTableDynamicArgs_RemoteUpstream{}
		for k, u := range c.Dynamic.RemoteUpstreams {
			da.RemoteUpstreams[k] = &pb.RewriteTableDynamicArgs_RemoteUpstream{Addr: u.Addr, User: u.User, Password: u.Password}
		}
	}
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: da}}}}
}

func (c phase4Case) wantDeltas() []*pb.PrivilegeDelta {
	if c.WantDeltas == nil {
		return nil
	}
	out := make([]*pb.PrivilegeDelta, 0, len(c.WantDeltas))
	for _, d := range c.WantDeltas {
		pd := &pb.PrivilegeDelta{
			Action:           map[string]pb.PrivilegeDelta_Action{"GRANT": pb.PrivilegeDelta_ACTION_GRANT, "REVOKE": pb.PrivilegeDelta_ACTION_REVOKE}[d.Action],
			Scope:            map[string]pb.PrivilegeDelta_Scope{"TABLE": pb.PrivilegeDelta_SCOPE_TABLE, "DATABASE": pb.PrivilegeDelta_SCOPE_DATABASE}[d.Scope],
			OriginalDatabase: d.OriginalDatabase, LogicalDatabase: d.LogicalDatabase, PhysicalDatabase: d.PhysicalDatabase,
			OriginalTable: d.OriginalTable, PhysicalTable: d.PhysicalTable,
			Privileges: d.Privileges, GrantOption: d.GrantOption,
		}
		for _, g := range d.Grantees {
			pd.Grantees = append(pd.Grantees, &pb.PrivilegeDelta_Grantee{Name: g.Name, IsCurrentUser: g.IsCurrentUser})
		}
		out = append(out, pd)
	}
	return out
}

func loadPhase4Cases(t *testing.T) []phase4Case {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "phase4_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []phase4Case
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// TestPhase4Golden is the Phase-4 parity gate for EXISTS / SHOW CREATE / GRANT /
// REVOKE. Structured fields (code / statement_type / table_rewrites /
// original_accessed_tables / privileges_deltas) are compared EXACTLY;
// sql_after_rewrite SEMANTICALLY. want_* were frozen from the native rewriter's
// real output (verified by the handler unit tests); the REWRITER_ORACLE_ADDR
// differential is the TRUE gate.
func TestPhase4Golden(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	oracle, _ := DialOracle()
	defer oracle.Close()
	semEq := semanticSQLEq(e)

	for _, c := range loadPhase4Cases(t) {
		t.Run(c.Name, func(t *testing.T) {
			r := newWriteRewriter(e, c.options())
			res, err := r.Rewrite(context.Background(), c.SQL, "acct")
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if c.WantCode != "" && res.Code != codeByName[c.WantCode] {
				t.Errorf("code = %v, want %s (%s)", res.Code, c.WantCode, res.Message)
			}
			if c.WantStmt != "" && res.StatementType != wantStmtType(c.WantStmt) {
				t.Errorf("statement_type = %v, want %s", res.StatementType, c.WantStmt)
			}
			if c.WantTableRewrites != nil && !eqStrMap(res.TableRewrites, c.WantTableRewrites) {
				t.Errorf("table_rewrites = %v, want %v", res.TableRewrites, c.WantTableRewrites)
			}
			if c.WantAccessed != nil {
				checkAccessed(t, res.OriginalAccessedTables, c.WantAccessed)
			}
			if c.WantDeltas != nil && !privilegeDeltasEqual(res.PrivilegesDeltas, c.wantDeltas()) {
				t.Errorf("privileges_deltas = %+v, want %+v", res.PrivilegesDeltas, c.wantDeltas())
			}
			checkPhase4SQL(t, c, res.SQL, semEq)

			if oracle != nil {
				want, oerr := oracle.Rewrite(c.SQL, c.options())
				if oerr != nil {
					t.Fatalf("oracle: %v", oerr)
				}
				got := pbFromResult(res)
				cmpEq := semEq
				if c.Reject {
					got.SqlAfterRewrite = want.GetSqlAfterRewrite()
					if got.SqlAfterRewrite == "" {
						cmpEq = nil
					}
				}
				if d := Compare(got, want, cmpEq); !d.Equal() {
					t.Errorf("oracle divergence: %v", d.Mismatches)
				}
			}
		})
	}
}

func checkPhase4SQL(t *testing.T, c phase4Case, got string, semEq SemanticEq) {
	t.Helper()
	switch {
	case len(c.SQLContains) > 0:
		for _, sub := range c.SQLContains {
			if !strings.Contains(got, sub) {
				t.Errorf("sql:\n got %q\nwant contains %q", got, sub)
			}
		}
	case c.SQLExact:
		if got != c.WantSQL {
			t.Errorf("sql (exact):\n got %q\nwant %q", got, c.WantSQL)
		}
	case c.WantSQL != "":
		eq, err := semEq(got, c.WantSQL)
		if err != nil {
			t.Errorf("sql (semantic-error): %v\n got %q\nwant %q", err, got, c.WantSQL)
		} else if !eq {
			t.Errorf("sql (semantic):\n got %q\nwant %q", got, c.WantSQL)
		}
	}
}
```

> NOTE FOR THE IMPLEMENTER: `pbFromResult` must surface `PrivilegesDeltas` for the oracle differential. Check its definition (`internal/harness/oracle.go` or a golden test helper); if it doesn't copy `PrivilegesDeltas` from the `RewriteResult`, add that field to the copy. Same for `RewriteResult`/`resultFromPB` in the root package — verify the deltas survive the `pb → RewriteResult → pb` round trip; extend if needed (and add a root-package assertion).

- [ ] **Step 5: Create the corpus** (`internal/harness/testdata/phase4_cases.json`). All cases use `dynamic.database_map={"logical1":"phys1"}`, `known_physical_databases=["phys1"]`, `delim="_"`. Freeze `want_sql` / `want_privileges_deltas` from the actual native output (run the handler tests first; the values below match this plan's handler code):

```json
[
  {
    "name": "exists_none_bare",
    "sql": "EXISTS t",
    "want_code": "Success", "want_stmt": "EXISTS_TABLE",
    "want_sql": "EXISTS TABLE t",
    "want_accessed": [{"original_table": "t"}]
  },
  {
    "name": "exists_dynamic_rewrite",
    "sql": "EXISTS logical1.t",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "Success", "want_stmt": "EXISTS_TABLE",
    "want_sql": "EXISTS TABLE phys1.`logical1.t`",
    "want_table_rewrites": {"logical1.t": "phys1.logical1.t"},
    "want_accessed": [{"original_database": "logical1", "original_table": "t", "logical_database": "logical1", "physical_database": "phys1"}]
  },
  {
    "name": "exists_database_rejected",
    "sql": "EXISTS DATABASE logical1",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "UnsupportedStatement", "want_stmt": "EXISTS_TABLE", "reject": true
  },
  {
    "name": "show_create_dynamic_rewrite",
    "sql": "SHOW CREATE TABLE logical1.t",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "Success", "want_stmt": "SHOW_CREATE_TABLE",
    "want_sql": "SHOW CREATE TABLE phys1.`logical1.t`",
    "want_table_rewrites": {"logical1.t": "phys1.logical1.t"},
    "want_accessed": [{"original_database": "logical1", "original_table": "t", "logical_database": "logical1", "physical_database": "phys1"}]
  },
  {
    "name": "show_create_view_rejected",
    "sql": "SHOW CREATE VIEW v",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "UnsupportedStatement", "want_stmt": "SHOW_CREATE_TABLE", "reject": true
  },
  {
    "name": "grant_select_table",
    "sql": "GRANT SELECT ON logical1.t TO u",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "Success", "want_stmt": "GRANT", "sql_exact": true,
    "want_sql": "SELECT 'GRANT SELECT ON logical1.t TO u' AS gstmt",
    "want_privileges_deltas": [{"action": "GRANT", "scope": "TABLE", "original_database": "logical1", "logical_database": "logical1", "physical_database": "phys1", "original_table": "t", "physical_table": "logical1.t", "privileges": ["SELECT"], "grantees": [{"name": "u"}], "grant_option": false}]
  },
  {
    "name": "grant_two_privs_two_grantees_option",
    "sql": "GRANT SELECT, INSERT ON logical1.t TO u1, u2 WITH GRANT OPTION",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "Success", "want_stmt": "GRANT", "sql_exact": true,
    "want_sql": "SELECT 'GRANT SELECT, INSERT ON logical1.t TO u1, u2 WITH GRANT OPTION' AS gstmt",
    "want_privileges_deltas": [
      {"action": "GRANT", "scope": "TABLE", "original_database": "logical1", "logical_database": "logical1", "physical_database": "phys1", "original_table": "t", "physical_table": "logical1.t", "privileges": ["SELECT"], "grantees": [{"name": "u1"}, {"name": "u2"}], "grant_option": true},
      {"action": "GRANT", "scope": "TABLE", "original_database": "logical1", "logical_database": "logical1", "physical_database": "phys1", "original_table": "t", "physical_table": "logical1.t", "privileges": ["INSERT"], "grantees": [{"name": "u1"}, {"name": "u2"}], "grant_option": true}
    ]
  },
  {
    "name": "grant_database_scope",
    "sql": "GRANT SELECT ON logical1.* TO u",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "Success", "want_stmt": "GRANT", "sql_exact": true,
    "want_sql": "SELECT 'GRANT SELECT ON logical1.* TO u' AS gstmt",
    "want_privileges_deltas": [{"action": "GRANT", "scope": "DATABASE", "original_database": "logical1", "logical_database": "logical1", "physical_database": "phys1", "privileges": ["SELECT"], "grantees": [{"name": "u"}], "grant_option": false}]
  },
  {
    "name": "revoke_select",
    "sql": "REVOKE SELECT ON logical1.t FROM u",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "Success", "want_stmt": "REVOKE", "sql_exact": true,
    "want_sql": "SELECT 'REVOKE SELECT ON logical1.t FROM u' AS rstmt",
    "want_privileges_deltas": [{"action": "REVOKE", "scope": "TABLE", "original_database": "logical1", "logical_database": "logical1", "physical_database": "phys1", "original_table": "t", "physical_table": "logical1.t", "privileges": ["SELECT"], "grantees": [{"name": "u"}], "grant_option": false}]
  },
  {
    "name": "grant_current_user",
    "sql": "GRANT SELECT ON logical1.t TO CURRENT_USER",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "Success", "want_stmt": "GRANT", "sql_exact": true,
    "want_sql": "SELECT 'GRANT SELECT ON logical1.t TO CURRENT_USER' AS gstmt",
    "want_privileges_deltas": [{"action": "GRANT", "scope": "TABLE", "original_database": "logical1", "logical_database": "logical1", "physical_database": "phys1", "original_table": "t", "physical_table": "logical1.t", "privileges": ["SELECT"], "grantees": [{"is_current_user": true}], "grant_option": false}]
  },
  {
    "name": "grant_on_cluster_stripped",
    "sql": "GRANT SELECT ON logical1.t ON CLUSTER c TO u",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "Success", "want_stmt": "GRANT", "sql_exact": true,
    "want_sql": "SELECT 'GRANT SELECT ON logical1.t TO u' AS gstmt",
    "want_privileges_deltas": [{"action": "GRANT", "scope": "TABLE", "original_database": "logical1", "logical_database": "logical1", "physical_database": "phys1", "original_table": "t", "physical_table": "logical1.t", "privileges": ["SELECT"], "grantees": [{"name": "u"}], "grant_option": false}]
  },
  {
    "name": "grant_global_scope_rejected",
    "sql": "GRANT SELECT ON *.* TO u",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "UnsupportedStatement", "want_stmt": "GRANT", "reject": true
  },
  {
    "name": "grant_column_rejected",
    "sql": "GRANT SELECT(c) ON logical1.t TO u",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "UnsupportedStatement", "want_stmt": "GRANT", "reject": true
  },
  {
    "name": "grant_role_membership_rejected",
    "sql": "GRANT role1 TO u",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "UnsupportedStatement", "want_stmt": "GRANT", "reject": true
  },
  {
    "name": "grant_unresolvable_db_invalid",
    "sql": "GRANT SELECT ON unknown.t TO u",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "want_code": "InvalidRewriteRequest", "want_stmt": "GRANT", "reject": true
  }
]
```

- [ ] **Step 6: Run + freeze** — `…go test ./internal/harness -run 'TestPhase4Golden|TestCompare_privilegeDeltas'` → PASS. If any `want_sql` / delta value disagrees with the real native output, reconcile by FIXING the handler if it's a genuine parity bug, else update the frozen value (it encodes our output until the oracle runs). Confirm `go test ./...` (no FFI) still green via skips.

- [ ] **Step 7: gofmt + vet + commit**

```bash
gofmt -w internal/harness/compare.go internal/harness/compare_test.go internal/harness/phase4_golden_test.go
go vet ./... && go build ./...
git add internal/harness/compare.go internal/harness/compare_test.go internal/harness/phase4_golden_test.go internal/harness/testdata/phase4_cases.json
git commit -m "test(harness): privileges_deltas compare + Phase-4 golden corpus + oracle differential"
```

---

## Self-Review (run against design spec §9 + grant.cc/exists.cc/show_create.cc)

**Spec coverage:**
- EXISTS TABLE (+ DATABASE/VIEW/DICTIONARY rejects) → Task 1 (extract) + Task 3 (handle). ✅
- SHOW CREATE TABLE (+ non-table rejects, bare form) → Task 1 + Task 3; SHOW CREATE defer refreshed Task 5. ✅
- GRANT + REVOKE privilege deltas (every reject in grant.h's list: ON *.*, role-membership, column-level, WITH REPLACE, FROM ALL, CURRENT GRANTS, ATTACH, no-dynamic; Invalid: unresolvable logical, unqualified-no-upstream) → Task 2 (extract) + Task 4 (handle). ✅
- `privileges_deltas` parity gate → Task 6 (Compare + corpus + oracle). ✅
- Native routing in C++ order (exists → show_create → grant), §8 echo, `r.last` stamping → Task 5. ✅

**Type consistency:** `engine.ParseObjectTarget`→`ObjectTarget{Verb,Temporary,ObjType,DB,Table}`; `engine.ParseGrant`→`GrantParse`/`GrantPrivilege`; `engine.ParseGeneric` added to the interface AND `polyglotEngine` (only implementer — no fakes). Handlers use `pb.PrivilegeDelta_ACTION_GRANT/REVOKE`, `pb.PrivilegeDelta_SCOPE_TABLE/DATABASE`, `pb.PrivilegeDelta_Grantee{Name,IsCurrentUser}`, `RewriteSQLResponse.PrivilegesDeltas` (verified against `gen/pb/rewriter.pb.go`). Reused unchanged: `decideWriteTarget`, `newWriteResp`, `rejectUnsupported/Invalid`, `escapeSQLLiteral`, `nameresolve.{FindActive,FindDynamicArgs,ResolvePhysicalDatabase,BuildDynamicTablePrefix}`, `engine.{NodeKind,TableTarget,ActionRename,QuoteQualified,tokenizeRaw,rawToken,isNameTok}`.

**Known parity risks (gated by the live-oracle differential, the true §7 gate — not runnable in-session):**
1. **GRANT marker exact-string:** `Generate(genericAST,"clickhouse")` vs C++ `formatAst`. Probe shows agreement on the common forms; exotic identifier quoting / privilege spelling may diverge → oracle catches, allow-list if intentional.
2. **EXISTS/SHOW CREATE quoting:** `engine.QuoteQualified` (WhenNecessary backticks) vs C++ for edge identifiers (keywords, leading digits) — same risk surface as the Phase-2 RENAME splice.
3. **Securable split:** polyglot's flat `securable.name` loses quoting; a dotted/quoted db name could mis-split on the last `.` (rare). Documented in `splitSecurable`.
4. **Privilege keyword spelling:** preserved verbatim from polyglot (proto contract says "verbatim"); polyglot vs ClickHouse `toKeywords()` agree on probed cases (SELECT/INSERT/ALTER UPDATE/ALL).
5. **`!Structured` residual GRANT** (pre-checks pass but generic still fails) → rejected Unsupported; C++ would handle it. An exotic, parity-safe divergence; `log`-free (no silent cap — it surfaces as Unsupported).

**Pre-existing, out of scope (NOT Phase 4):** the `classifyCommand` `SHOW` catch-all still mis-stamps admin `SHOW GRANTS/PROCESSLIST/...` as `SHOW_TABLES` (all-unsupported surface; a separate cleanup). `RewriteErrorMessage` reverse-mapping is Phase 5.
