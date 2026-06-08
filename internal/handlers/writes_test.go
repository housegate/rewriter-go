package handlers

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// statOpt wraps static table-rewrite args into a single TableNameRewrite option.
func statOpt(a *pb.RewriteTableStaticArgs) []*pb.RewriteOption {
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{StaticArgs: a}}}}
}

// sqlSemEq reports whether got and want are semantically equal after parsing and
// regenerating both through the engine (mirrors engine.sqlEq, but in handlers).
// Writes are not SELECTs, so a parse+generate canonical compare is the right
// fidelity check (you cannot CollectSelectTables a DROP/UPDATE/...).
func sqlSemEq(t *testing.T, e engine.Engine, got, want string) bool {
	t.Helper()
	ga, err := e.ParseOne(got)
	if err != nil {
		t.Fatalf("parse got %q: %v", got, err)
	}
	gn, err := e.Generate(ga)
	if err != nil {
		t.Fatalf("gen got %q: %v", got, err)
	}
	wa, err := e.ParseOne(want)
	if err != nil {
		t.Fatalf("parse want %q: %v", want, err)
	}
	wn, err := e.Generate(wa)
	if err != nil {
		t.Fatalf("gen want %q: %v", want, err)
	}
	return gn == wn
}

// mustParse parses sql, failing the test on error.
func mustParse(t *testing.T, e engine.Engine, sql string) engine.AST {
	t.Helper()
	ast, err := e.ParseOne(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	return ast
}

// 1. DROP TABLE db.t + static table_map → table renamed, db preserved.
func TestRewriteWrite_dropTableStaticRename(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP TABLE db.t")
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{"db.t": "t_phys"}})

	resp, handled, err := RewriteWrite(e, ast, "DROP TABLE db.t", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s)", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DROP_TABLE {
		t.Fatalf("stmt = %v", resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "DROP TABLE db.t_phys") {
		t.Fatalf("sql = %q, want ≈ DROP TABLE db.t_phys", resp.GetSqlAfterRewrite())
	}
	// table_map renames only the table; db stays → db.t → db.t_phys.
	want := map[string]string{"db.t": "db.t_phys"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 {
		t.Fatalf("accessed = %+v, want 1", ats)
	}
	if ats[0].GetOriginalDatabase() != "db" || ats[0].GetOriginalTable() != "t" {
		t.Fatalf("accessed[0] = %+v, want {db, t}", ats[0])
	}
	// Static-mode table_map hit → physical db = origin db.
	if ats[0].GetPhysicalDatabase() != "db" {
		t.Fatalf("accessed[0].physical = %q, want db", ats[0].GetPhysicalDatabase())
	}
}

// 2. CREATE TABLE rename → stmt=CREATE_TABLE.
func TestRewriteWrite_createTableRename(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE TABLE db.t (x Int32) ENGINE = Memory")
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{"db.t": "t_phys"}})

	resp, handled, err := RewriteWrite(e, ast, "", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_TABLE {
		t.Fatalf("stmt = %v", resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "CREATE TABLE db.t_phys (x Int32) ENGINE = Memory") {
		t.Fatalf("sql = %q", resp.GetSqlAfterRewrite())
	}
	want := map[string]string{"db.t": "db.t_phys"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalTable() != "t" {
		t.Fatalf("accessed = %+v", ats)
	}
}

//  3. CREATE TABLE db.t AS db2.src with table_map for BOTH → both rewritten,
//     2 accessed tables (target first, then clone source).
func TestRewriteWrite_createTableAsBothTargets(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE TABLE db.t AS db2.src")
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{
		"db.t":    "t_phys",
		"db2.src": "src_phys",
	}})

	resp, handled, err := RewriteWrite(e, ast, "", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "CREATE TABLE db.t_phys AS db2.src_phys") {
		t.Fatalf("sql = %q", resp.GetSqlAfterRewrite())
	}
	want := map[string]string{"db.t": "db.t_phys", "db2.src": "db2.src_phys"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 2 {
		t.Fatalf("accessed = %+v, want 2", ats)
	}
	// Encounter order: create target, then clone source.
	if ats[0].GetOriginalTable() != "t" || ats[1].GetOriginalTable() != "src" {
		t.Fatalf("accessed order = %+v, want [t, src]", ats)
	}
}

// 3b. CREATE TABLE db.t AS db2.src where the FIRST slot (db.t) rejects (remote):
// C++ short-circuits — it records ONLY db.t's access and NO table_rewrites (the
// AS-source rewriteOneTarget is never reached). Go must match: 1 accessed, 0
// rewrites — NOT 2 accessed + 1 rewrite (the multi-slot over-record parity bug).
func TestRewriteWrite_createTableAsFirstSlotRejectShortCircuits(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE TABLE db.t AS db2.src")
	opts := statOpt(&pb.RewriteTableStaticArgs{
		RemoteTableMap: map[string]*pb.RewriteTableStaticArgs_RemoteTable{
			"db.t": {Addr: "h", Database: "d", Table: "x"}, // create name → remote → reject
		},
		TableMap: map[string]string{"db2.src": "src_phys"}, // would rewrite, but never reached
	})
	resp, handled, err := RewriteWrite(e, ast, "", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalTable() != "t" {
		t.Errorf("accessed = %+v, want exactly 1 (db.t) — second slot must not be recorded", ats)
	}
	if got := resp.GetTableRewrites(); len(got) != 0 {
		t.Errorf("table_rewrites = %v, want empty (rejected before any rewrite recorded)", got)
	}
}

// 4. CREATE TABLE db.t AS remote(...) → UnsupportedStatement (table function).
func TestRewriteWrite_createTableAsFunctionRejected(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE TABLE db.t AS remote('h', d, x)")

	resp, handled, err := RewriteWrite(e, ast, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("handled = false, want true (reject is handled)")
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("code = %v, want UnsupportedStatement (%s)", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_TABLE {
		t.Fatalf("stmt = %v", resp.GetStatementType())
	}
}

// 5. DROP TABLE db.a, db.b → multi-table → UnsupportedStatement.
func TestRewriteWrite_dropMultiRejected(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP TABLE db.a, db.b")

	resp, handled, err := RewriteWrite(e, ast, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("code = %v, want UnsupportedStatement (%s)", resp.GetCode(), resp.GetMessage())
	}
}

// 6. ALTER TABLE db.t ATTACH PARTITION 1 FROM db.src → cross-table → UnsupportedStatement.
func TestRewriteWrite_alterCrossTableRejected(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "ALTER TABLE db.t ATTACH PARTITION 1 FROM db.src")

	resp, handled, err := RewriteWrite(e, ast, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("code = %v, want UnsupportedStatement (%s)", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_ALTER_TABLE {
		t.Fatalf("stmt = %v", resp.GetStatementType())
	}
}

//  7. DROP TABLE t (unqualified) + dynamic args with NO upstream logical db in
//     context → StatusInvalid → InvalidRewriteRequest (strict reject for writes).
func TestRewriteWrite_dynamicInvalidRejected(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP TABLE t")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{"tenant1": "testnet"},
	})

	resp, handled, err := RewriteWrite(e, ast, "", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Fatalf("code = %v, want InvalidRewriteRequest (%s)", resp.GetCode(), resp.GetMessage())
	}
	// Access is recorded BEFORE the reject: 1 accessed table for `t`.
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalTable() != "t" {
		t.Fatalf("accessed = %+v, want 1 {t} (recorded before reject)", ats)
	}
}

//  8. DROP TABLE db.t + static remote_table_map → writes can't remote →
//     UnsupportedStatement. Access still recorded (with is_remote=true).
func TestRewriteWrite_remoteRejectedForWrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP TABLE db.t")
	opts := statOpt(&pb.RewriteTableStaticArgs{RemoteTableMap: map[string]*pb.RewriteTableStaticArgs_RemoteTable{
		"db.t": {Addr: "h", Database: "d", Table: "x"},
	}})

	resp, handled, err := RewriteWrite(e, ast, "", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("handled = false, want true")
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("code = %v, want UnsupportedStatement (%s)", resp.GetCode(), resp.GetMessage())
	}
	// No rewrite applied (remote reject short-circuits before rename).
	if len(resp.GetTableRewrites()) != 0 {
		t.Fatalf("table_rewrites = %v, want empty", resp.GetTableRewrites())
	}
	// Access recorded BEFORE reject, with is_remote flagged from the static lookup.
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalTable() != "t" {
		t.Fatalf("accessed = %+v, want 1 {t}", ats)
	}
	if !ats[0].GetIsRemote() {
		t.Fatalf("accessed[0].is_remote = false, want true")
	}
}

// 9. UPDATE db.t SET x=1 WHERE y=2 + nil opts → passthrough, sql unchanged.
func TestRewriteWrite_passthroughNoOpts(t *testing.T) {
	e := newEngine(t)
	const src = "UPDATE db.t SET x = 1 WHERE y = 2"
	ast := mustParse(t, e, src)

	resp, handled, err := RewriteWrite(e, ast, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_UPDATE {
		t.Fatalf("stmt = %v", resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), src) {
		t.Fatalf("sql = %q, want ≈ %q", resp.GetSqlAfterRewrite(), src)
	}
	if len(resp.GetTableRewrites()) != 0 {
		t.Fatalf("table_rewrites = %v, want empty", resp.GetTableRewrites())
	}
	// Mode::None still records access (recordAccessedTable runs before the
	// mode switch in C++ rewriteOneTarget).
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalTable() != "t" {
		t.Fatalf("accessed = %+v, want 1 {t}", ats)
	}
}

//  10. SELECT 1 → not a write this phase handles → handled=false (caller falls
//     through to SELECT).
func TestRewriteWrite_notAWrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SELECT 1")

	resp, handled, err := RewriteWrite(e, ast, "SELECT 1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatalf("handled = true, want false (resp=%+v)", resp)
	}
}

// 11. UPDATE + DELETE rename → correct stmt types, both rewritten.
func TestRewriteWrite_updateDeleteRename(t *testing.T) {
	e := newEngine(t)
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{"db.t": "t_phys"}})

	t.Run("update", func(t *testing.T) {
		ast := mustParse(t, e, "UPDATE db.t SET x = 1 WHERE y = 2")
		resp, handled, err := RewriteWrite(e, ast, "", opts)
		if err != nil {
			t.Fatal(err)
		}
		if !handled || resp.GetCode() != pb.RewriteCode_Success {
			t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
		}
		if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_UPDATE {
			t.Fatalf("stmt = %v", resp.GetStatementType())
		}
		if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "UPDATE db.t_phys SET x = 1 WHERE y = 2") {
			t.Fatalf("sql = %q", resp.GetSqlAfterRewrite())
		}
		if got := resp.GetTableRewrites(); !mapEq(got, map[string]string{"db.t": "db.t_phys"}) {
			t.Fatalf("table_rewrites = %v", got)
		}
	})

	t.Run("delete", func(t *testing.T) {
		ast := mustParse(t, e, "DELETE FROM db.t WHERE y = 2")
		resp, handled, err := RewriteWrite(e, ast, "", opts)
		if err != nil {
			t.Fatal(err)
		}
		if !handled || resp.GetCode() != pb.RewriteCode_Success {
			t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
		}
		if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DELETE {
			t.Fatalf("stmt = %v", resp.GetStatementType())
		}
		if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "DELETE FROM db.t_phys WHERE y = 2") {
			t.Fatalf("sql = %q", resp.GetSqlAfterRewrite())
		}
		if got := resp.GetTableRewrites(); !mapEq(got, map[string]string{"db.t": "db.t_phys"}) {
			t.Fatalf("table_rewrites = %v", got)
		}
	})
}

// bodyTableRefs re-parses a CREATE VIEW's output SQL and returns the [db.]table
// refs of its body SELECT. The body rewrite surfaces the SELECT layer's back-alias
// (the renamed table is aliased back to its original qualified name — verified
// parity-correct against C++ rewriteEmbeddedViewBody), so a semantic SQL compare
// must carry that alias. Re-parsing + CollectSelectTables checks the physical
// table ref directly and is robust to the exact alias rendering.
func bodyTableRefs(t *testing.T, e engine.Engine, viewSQL string) []engine.TableTarget {
	t.Helper()
	ast := mustParse(t, e, viewSQL)
	body, has, err := engine.ExtractViewBody(ast)
	if err != nil || !has {
		t.Fatalf("extract view body from %q: has=%v err=%v", viewSQL, has, err)
	}
	refs, err := engine.CollectSelectTables(body)
	if err != nil {
		t.Fatalf("collect body tables from %q: %v", viewSQL, err)
	}
	return refs
}

// Task 8.1. CREATE VIEW db.v AS SELECT * FROM db.s, static {db.v→v_phys,
// db.s→s_phys}. The view NAME and the body table are both rewritten; the body
// table is back-aliased to "db.s" (SELECT-layer behavior, parity-correct).
// Both db.v and db.s appear in table_rewrites.
func TestRewriteWrite_createViewBodyRewritten(t *testing.T) {
	e := newEngine(t)
	const src = "CREATE VIEW db.v AS SELECT * FROM db.s"
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{
		"db.v": "v_phys",
		"db.s": "s_phys",
	}})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_VIEW {
		t.Fatalf("stmt = %v, want CREATE_VIEW", resp.GetStatementType())
	}
	// Back-aliased want (verified empirically against the SELECT transformer):
	// the renamed body table carries an alias back to its original qualified name.
	want := `CREATE VIEW db.v_phys AS SELECT * FROM db.s_phys "db.s"`
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Fatalf("sql = %q, want ≈ %q", resp.GetSqlAfterRewrite(), want)
	}
	// Robust cross-check: re-parse output and inspect the body's physical ref.
	refs := bodyTableRefs(t, e, resp.GetSqlAfterRewrite())
	if len(refs) != 1 || refs[0].DB != "db" || refs[0].Table != "s_phys" {
		t.Fatalf("body refs = %+v, want 1 {db, s_phys}", refs)
	}
	// Both the view name and the body table are recorded in table_rewrites.
	wantRewrites := map[string]string{"db.v": "db.v_phys", "db.s": "db.s_phys"}
	if got := resp.GetTableRewrites(); !mapEq(got, wantRewrites) {
		t.Fatalf("table_rewrites = %v, want %v", got, wantRewrites)
	}
}

// Task 8.2. CREATE MATERIALIZED VIEW db.mv TO db.dst AS SELECT * FROM db.s,
// static {db.mv→mv2, db.dst→dst2, db.s→s2}. stmt=CREATE_MATERIALIZED_VIEW; the
// name, the TO target, AND the body table are all rewritten.
func TestRewriteWrite_createMVToTarget(t *testing.T) {
	e := newEngine(t)
	const src = "CREATE MATERIALIZED VIEW db.mv TO db.dst AS SELECT * FROM db.s"
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{
		"db.mv":  "mv2",
		"db.dst": "dst2",
		"db.s":   "s2",
	}})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_MATERIALIZED_VIEW {
		t.Fatalf("stmt = %v, want CREATE_MATERIALIZED_VIEW", resp.GetStatementType())
	}
	want := `CREATE MATERIALIZED VIEW db.mv2 TO db.dst2 AS SELECT * FROM db.s2 "db.s"`
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Fatalf("sql = %q, want ≈ %q", resp.GetSqlAfterRewrite(), want)
	}
	// Body physical ref cross-check.
	refs := bodyTableRefs(t, e, resp.GetSqlAfterRewrite())
	if len(refs) != 1 || refs[0].DB != "db" || refs[0].Table != "s2" {
		t.Fatalf("body refs = %+v, want 1 {db, s2}", refs)
	}
	// name, TO target, AND body all present in table_rewrites.
	wantRewrites := map[string]string{
		"db.mv":  "db.mv2",
		"db.dst": "db.dst2",
		"db.s":   "db.s2",
	}
	if got := resp.GetTableRewrites(); !mapEq(got, wantRewrites) {
		t.Fatalf("table_rewrites = %v, want %v", got, wantRewrites)
	}
}

// Task 8.3. CREATE MATERIALIZED VIEW where the view NAME rejects (remote): the
// C++ short-circuits at slot 1, so the TO target's access is NOT recorded and the
// body is NOT processed. Exactly 1 accessed table (the name), 0 rewrites,
// UnsupportedStatement.
func TestRewriteWrite_createViewNameRejectShortCircuits(t *testing.T) {
	e := newEngine(t)
	const src = "CREATE MATERIALIZED VIEW db.mv TO db.dst AS SELECT * FROM db.s"
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{
		RemoteTableMap: map[string]*pb.RewriteTableStaticArgs_RemoteTable{
			"db.mv": {Addr: "h", Database: "d", Table: "x"}, // view name → remote → reject
		},
		TableMap: map[string]string{
			"db.dst": "dst2", // would rewrite, but never reached
			"db.s":   "s2",   // body never processed
		},
	})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_MATERIALIZED_VIEW {
		t.Fatalf("stmt = %v, want CREATE_MATERIALIZED_VIEW", resp.GetStatementType())
	}
	// Short-circuit: only the view name is recorded; TO target NOT recorded.
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalTable() != "mv" {
		t.Fatalf("accessed = %+v, want exactly 1 (db.mv) — TO target must not be recorded", ats)
	}
	// No rewrites recorded (rejected before any rename; body never reached).
	if got := resp.GetTableRewrites(); len(got) != 0 {
		t.Fatalf("table_rewrites = %v, want empty (rejected before any rewrite)", got)
	}
}

// Task 8.4. original_accessed_tables includes the view name (and MV TO target)
// PLUS the body's accessed tables, in order: name/TO first (write slots, in
// document order), then the body SELECT's accessed tables.
func TestRewriteWrite_createViewAccessedMergesBody(t *testing.T) {
	e := newEngine(t)
	const src = "CREATE MATERIALIZED VIEW db.mv TO db.dst AS SELECT * FROM db.s"
	ast := mustParse(t, e, src)
	// No rename needed to observe accessed ordering; use a benign map so nothing
	// rejects (all three pass through).
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{
		"db.mv":  "mv2",
		"db.dst": "dst2",
		"db.s":   "s2",
	}})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	ats := resp.GetOriginalAccessedTables()
	// Expected order: write slots first (name, TO), then body (s).
	wantOrder := []string{"mv", "dst", "s"}
	if len(ats) != len(wantOrder) {
		t.Fatalf("accessed = %+v, want %v", ats, wantOrder)
	}
	for i, w := range wantOrder {
		if ats[i].GetOriginalTable() != w {
			t.Fatalf("accessed[%d] = %q, want %q (full=%+v)", i, ats[i].GetOriginalTable(), w, ats)
		}
	}
}

// Task 9.1. INSERT INTO db.t (x) VALUES (1), static {db.t→t_phys}. Target
// rewritten (db preserved); stmt=INSERT; 1 accessed (t); table_rewrites{db.t:db.t_phys}.
// The original sql is threaded as the 3rd arg (used by the FORMAT-payload splice).
func TestRewriteWrite_insertValues(t *testing.T) {
	e := newEngine(t)
	const src = "INSERT INTO db.t (x) VALUES (1)"
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{"db.t": "t_phys"}})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_INSERT {
		t.Fatalf("stmt = %v, want INSERT", resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "INSERT INTO db.t_phys (x) VALUES (1)") {
		t.Fatalf("sql = %q, want ≈ INSERT INTO db.t_phys (x) VALUES (1)", resp.GetSqlAfterRewrite())
	}
	want := map[string]string{"db.t": "db.t_phys"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalTable() != "t" {
		t.Fatalf("accessed = %+v, want 1 {t}", ats)
	}
}

// Task 9.2. INSERT INTO db.t FORMAT JSONEachRow <payload>. The inline data
// payload lives off-AST; GenerateInsert must splice it back verbatim. The output
// starts with the rewritten prelude and contains the literal payload. The payload
// tail is NOT a re-parseable standalone statement, so assert via HasPrefix+Contains
// rather than sqlSemEq.
func TestRewriteWrite_insertFormatPreservesPayload(t *testing.T) {
	e := newEngine(t)
	const payload = `{"x":1}` + "\n" + `{"x":2}`
	const src = "INSERT INTO db.t FORMAT JSONEachRow " + payload
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{"db.t": "t_phys"}})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_INSERT {
		t.Fatalf("stmt = %v, want INSERT", resp.GetStatementType())
	}
	out := resp.GetSqlAfterRewrite()
	if !strings.HasPrefix(out, "INSERT INTO db.t_phys FORMAT JSONEachRow") {
		t.Fatalf("sql = %q, want prefix 'INSERT INTO db.t_phys FORMAT JSONEachRow'", out)
	}
	if !strings.Contains(out, payload) {
		t.Fatalf("sql = %q, want to contain payload %q", out, payload)
	}
	want := map[string]string{"db.t": "db.t_phys"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalTable() != "t" {
		t.Fatalf("accessed = %+v, want 1 {t}", ats)
	}
}

// Task 9.3. INSERT INTO db.t SELECT * FROM db.s, static {db.t→t_phys,
// db.s→s_phys}. C++ parity: only the INSERT target is rewritten — the embedded
// SELECT source db.s is LEFT db.s (the INSERT handler does not run the SELECT
// pipeline on insert.query). So db.s is NOT in table_rewrites and accessed is
// ONLY [t]. Verified empirically: GenerateInsert+applyStructuredSlots produce
// exactly `INSERT INTO db.t_phys SELECT * FROM db.s`.
func TestRewriteWrite_insertSelectSourceUntouched(t *testing.T) {
	e := newEngine(t)
	const src = "INSERT INTO db.t SELECT * FROM db.s"
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{
		"db.t": "t_phys",
		"db.s": "s_phys",
	}})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_INSERT {
		t.Fatalf("stmt = %v, want INSERT", resp.GetStatementType())
	}
	// Target rewritten, embedded SELECT source untouched (db.s stays db.s).
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "INSERT INTO db.t_phys SELECT * FROM db.s") {
		t.Fatalf("sql = %q, want ≈ INSERT INTO db.t_phys SELECT * FROM db.s", resp.GetSqlAfterRewrite())
	}
	// Only the INSERT target is recorded — the SELECT source is NOT walked.
	want := map[string]string{"db.t": "db.t_phys"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v (db.s must NOT be rewritten)", got, want)
	}
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalTable() != "t" {
		t.Fatalf("accessed = %+v, want exactly 1 {t} (embedded SELECT not walked)", ats)
	}
}

// Task 9.4. INSERT INTO FUNCTION remote(...) → table function → UnsupportedStatement
// with the INSERT-FUNCTION message (C++ writes.cc:504-506). Original sql is threaded.
func TestRewriteWrite_insertFunctionRejected(t *testing.T) {
	e := newEngine(t)
	const src = "INSERT INTO FUNCTION remote('h', d, t) VALUES (1)"
	ast := mustParse(t, e, src)

	resp, handled, err := RewriteWrite(e, ast, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("handled = false, want true (reject is handled)")
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("code = %v, want UnsupportedStatement (%s)", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetMessage() != "INSERT INTO FUNCTION(...) is not supported" {
		t.Fatalf("message = %q", resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_INSERT {
		t.Fatalf("stmt = %v, want INSERT", resp.GetStatementType())
	}
}

// Task 9.5. INSERT INTO db.t (x) VALUES (1) with nil opts → passthrough: sql
// unchanged, no table_rewrites, but access still recorded (1 accessed {t}).
func TestRewriteWrite_insertPassthrough(t *testing.T) {
	e := newEngine(t)
	const src = "INSERT INTO db.t (x) VALUES (1)"
	ast := mustParse(t, e, src)

	resp, handled, err := RewriteWrite(e, ast, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_INSERT {
		t.Fatalf("stmt = %v, want INSERT", resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), src) {
		t.Fatalf("sql = %q, want ≈ %q", resp.GetSqlAfterRewrite(), src)
	}
	if len(resp.GetTableRewrites()) != 0 {
		t.Fatalf("table_rewrites = %v, want empty", resp.GetTableRewrites())
	}
	// Passthrough still records access (recordAccessedWrite runs before the mode switch).
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalTable() != "t" {
		t.Fatalf("accessed = %+v, want 1 {t}", ats)
	}
}

// accessedTables returns the OriginalTable of each accessed entry in order
// (used by the multi-target tier-C tests to assert document-order recording).
func accessedTables(resp *pb.RewriteSQLResponse) []string {
	ats := resp.GetOriginalAccessedTables()
	out := make([]string, len(ats))
	for i, a := range ats {
		out[i] = a.GetOriginalTable()
	}
	return out
}

// Task 10.1. RENAME TABLE db.a TO db.b, static {db.a→a_phys, db.b→b_phys}. Both
// sides rewritten via the raw splice; stmt=RENAME_TABLE; 2 accessed [a, b] in
// document order; table_rewrites carry UNQUOTED qualified names.
func TestRewriteWrite_renameStrictRewrite(t *testing.T) {
	e := newEngine(t)
	const src = "RENAME TABLE db.a TO db.b"
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{
		"db.a": "a_phys",
		"db.b": "b_phys",
	}})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_RENAME_TABLE {
		t.Fatalf("stmt = %v, want RENAME_TABLE", resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "RENAME TABLE db.a_phys TO db.b_phys") {
		t.Fatalf("sql = %q, want ≈ RENAME TABLE db.a_phys TO db.b_phys", resp.GetSqlAfterRewrite())
	}
	want := map[string]string{"db.a": "db.a_phys", "db.b": "db.b_phys"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	if got := accessedTables(resp); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("accessed = %v, want [a b] in order", got)
	}
}

// Task 10.2. RENAME TABLE db.a TO db.b, db.c TO db.d with all four mapped → all
// four spliced; 4 accessed in document order [a, b, c, d].
func TestRewriteWrite_renameMultiPair(t *testing.T) {
	e := newEngine(t)
	const src = "RENAME TABLE db.a TO db.b, db.c TO db.d"
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{
		"db.a": "a_phys", "db.b": "b_phys", "db.c": "c_phys", "db.d": "d_phys",
	}})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(),
		"RENAME TABLE db.a_phys TO db.b_phys, db.c_phys TO db.d_phys") {
		t.Fatalf("sql = %q", resp.GetSqlAfterRewrite())
	}
	want := map[string]string{
		"db.a": "db.a_phys", "db.b": "db.b_phys",
		"db.c": "db.c_phys", "db.d": "db.d_phys",
	}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	if got := accessedTables(resp); len(got) != 4 ||
		got[0] != "a" || got[1] != "b" || got[2] != "c" || got[3] != "d" {
		t.Fatalf("accessed = %v, want [a b c d] in order", got)
	}
}

// Task 10.3. EXCHANGE TABLES db.a AND db.b → both rewritten; stmt stays
// RENAME_TABLE (C++ writes.cc:585: EXCHANGE uses STATEMENT_TYPE_RENAME_TABLE).
func TestRewriteWrite_exchangeStrictRewrite(t *testing.T) {
	e := newEngine(t)
	const src = "EXCHANGE TABLES db.a AND db.b"
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{
		"db.a": "a_phys",
		"db.b": "b_phys",
	}})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_RENAME_TABLE {
		t.Fatalf("stmt = %v, want RENAME_TABLE", resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "EXCHANGE TABLES db.a_phys AND db.b_phys") {
		t.Fatalf("sql = %q, want ≈ EXCHANGE TABLES db.a_phys AND db.b_phys", resp.GetSqlAfterRewrite())
	}
	want := map[string]string{"db.a": "db.a_phys", "db.b": "db.b_phys"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	if got := accessedTables(resp); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("accessed = %v, want [a b]", got)
	}
}

// Task 10.4. ALTER TABLE db.t UPDATE x = 1 WHERE y = 2, static {db.t→t_phys}.
// Only the table name splices (stmt=ALTER_TABLE); the UPDATE/WHERE expression is
// untouched (raw byte-span splice never reaches the SET/WHERE clauses).
func TestRewriteWrite_alterUpdateRaw(t *testing.T) {
	e := newEngine(t)
	const src = "ALTER TABLE db.t UPDATE x = 1 WHERE y = 2"
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{"db.t": "t_phys"}})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_ALTER_TABLE {
		t.Fatalf("stmt = %v, want ALTER_TABLE", resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "ALTER TABLE db.t_phys UPDATE x = 1 WHERE y = 2") {
		t.Fatalf("sql = %q, want ≈ ALTER TABLE db.t_phys UPDATE x = 1 WHERE y = 2", resp.GetSqlAfterRewrite())
	}
	want := map[string]string{"db.t": "db.t_phys"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	if got := accessedTables(resp); len(got) != 1 || got[0] != "t" {
		t.Fatalf("accessed = %v, want [t]", got)
	}
}

// Task 10.5. RENAME TABLE db.a TO db.b where db.a → remote upstream (writes can't
// remote) → UnsupportedStatement. Strict decide short-circuits: only db.a is
// accessed (db.b never reached), 0 table_rewrites.
func TestRewriteWrite_renameRejectShortCircuits(t *testing.T) {
	e := newEngine(t)
	const src = "RENAME TABLE db.a TO db.b"
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{RemoteTableMap: map[string]*pb.RewriteTableStaticArgs_RemoteTable{
		"db.a": {Addr: "h", Database: "d", Table: "x"}, // first side → remote → reject
	}})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if got := accessedTables(resp); len(got) != 1 || got[0] != "a" {
		t.Fatalf("accessed = %v, want exactly [a] — db.b must not be recorded", got)
	}
	if got := resp.GetTableRewrites(); len(got) != 0 {
		t.Fatalf("table_rewrites = %v, want empty (short-circuit before any rewrite)", got)
	}
}

// Task 10.6. RENAME TABLE db.a TO db.b with dynamic args (databaseMap{db→testnet},
// delim "_"). The dynamic rename produces NewDB=testnet and NewTable="db.a"/"db.b"
// (the buildDynamicTableName "<logical>.<origtable>" rule, where logical=db). Each
// rewritten name must splice with the dotted table backtick-quoted as a SINGLE
// identifier (db plain, table quoted), so re-parsing + RawTableRefs decodes
// DB=testnet, Table="db.a"/"db.b" (NOT a 3-part name that would collapse Table to
// "db"). table_rewrites carry the UNQUOTED names.
func TestRewriteWrite_renameDynamicDottedNameQuoted(t *testing.T) {
	e := newEngine(t)
	const src = "RENAME TABLE db.a TO db.b"
	ast := mustParse(t, e, src)
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{"db": "testnet"},
		Delim:       "_",
	})

	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_RENAME_TABLE {
		t.Fatalf("stmt = %v, want RENAME_TABLE", resp.GetStatementType())
	}
	// table_rewrites: UNQUOTED qualified names (recordRewrite via qualify).
	want := map[string]string{"db.a": "testnet.db.a", "db.b": "testnet.db.b"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	// Load-bearing: the dotted dynamic names survive as SINGLE quoted identifiers.
	// Re-parse the output and decode via RawTableRefs.
	out := resp.GetSqlAfterRewrite()
	reparsed := mustParse(t, e, out)
	refs, sub, err := engine.RawTableRefs(e, reparsed)
	if err != nil {
		t.Fatal(err)
	}
	if sub != engine.CmdRename {
		t.Fatalf("sub = %q, want %q", sub, engine.CmdRename)
	}
	if len(refs) != 2 ||
		refs[0].DB != "testnet" || refs[0].Table != "db.a" ||
		refs[1].DB != "testnet" || refs[1].Table != "db.b" {
		t.Fatalf("dotted dynamic name not preserved as single identifier: out=%q refs=%+v", out, refs)
	}
	// 2 accessed [a, b]; static-less dynamic mode records logical=db, physical=testnet.
	if got := accessedTables(resp); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("accessed = %v, want [a b]", got)
	}
}

// Task 10.7. OPTIMIZE TABLE db.t → bare-reject → handled, UnsupportedStatement.
func TestRewriteWrite_optimizeBareReject(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "OPTIMIZE TABLE db.t")

	resp, handled, err := RewriteWrite(e, ast, "OPTIMIZE TABLE db.t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("handled = false, want true (bare-reject is handled)")
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("code = %v, want UnsupportedStatement (%s)", resp.GetCode(), resp.GetMessage())
	}
}

// Task 10.8. USE db → not a write this phase handles → handled=false (pass through).
func TestRewriteWrite_useNotAWrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE db")

	resp, handled, err := RewriteWrite(e, ast, "USE db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatalf("handled = true, want false (resp=%+v)", resp)
	}
}

// Task 10.9. EXISTS TABLE db.t → not a write this phase handles → handled=false.
func TestRewriteWrite_existsNotAWrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "EXISTS TABLE db.t")

	resp, handled, err := RewriteWrite(e, ast, "EXISTS TABLE db.t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatalf("handled = true, want false (resp=%+v)", resp)
	}
}

// Task 10.10. CREATE DATABASE db → handled (out-of-phase reject),
// UnsupportedStatement, stmt=CREATE_DATABASE. Phase 3 replaces this.
func TestRewriteWrite_createDatabaseOutOfPhase(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE db")

	resp, handled, err := RewriteWrite(e, ast, "CREATE DATABASE db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("handled = false, want true (out-of-phase reject is handled)")
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("code = %v, want UnsupportedStatement (%s)", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE {
		t.Fatalf("stmt = %v, want CREATE_DATABASE", resp.GetStatementType())
	}
}

// ---- Task 13: reject-guard parity (writes.cc forms polyglot flattens) ----

// wantUnsupported drives a SQL through RewriteWrite (nil opts) and asserts the
// statement is handled + rejected as UnsupportedStatement. Used by the Task-13
// reject parity cases (CREATE DICTIONARY, LIVE/WINDOW VIEW, COPY, DETACH, RENAME/
// EXCHANGE non-table, ALTER non-table, TRUNCATE non-table).
func wantUnsupported(t *testing.T, sql string) *pb.RewriteSQLResponse {
	t.Helper()
	e := newEngine(t)
	ast := mustParse(t, e, sql)
	resp, handled, err := RewriteWrite(e, ast, sql, nil)
	if err != nil {
		t.Fatalf("%q: RewriteWrite err: %v", sql, err)
	}
	if !handled {
		t.Fatalf("%q: handled = false, want true (reject is handled)", sql)
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("%q: code = %v, want UnsupportedStatement (%s)", sql, resp.GetCode(), resp.GetMessage())
	}
	return resp
}

// wantPassthrough drives a SQL through RewriteWrite (nil opts) and asserts it is
// NOT handled by this phase (handled=false) — the caller falls through to SELECT.
// Regression guard that Phase-3/4 statements (USE/SHOW/GRANT/REVOKE/EXISTS) are
// not swallowed by the new reject prefixes.
func wantPassthrough(t *testing.T, sql string) {
	t.Helper()
	e := newEngine(t)
	ast := mustParse(t, e, sql)
	resp, handled, err := RewriteWrite(e, ast, sql, nil)
	if err != nil {
		t.Fatalf("%q: RewriteWrite err: %v", sql, err)
	}
	if handled {
		t.Fatalf("%q: handled = true, want false (must pass through to SELECT) resp=%+v", sql, resp)
	}
}

func TestRewriteWrite_truncateViewRejected(t *testing.T) { wantUnsupported(t, "TRUNCATE VIEW db.v") }
func TestRewriteWrite_truncateAllTablesRejected(t *testing.T) {
	wantUnsupported(t, "TRUNCATE ALL TABLES FROM db")
}
func TestRewriteWrite_truncateDatabaseRejected(t *testing.T) {
	wantUnsupported(t, "TRUNCATE DATABASE db")
}

// REGRESSION guard: a plain TRUNCATE TABLE must still rewrite to Success — the
// new non-table TRUNCATE reject must not break real truncates.
func TestRewriteWrite_plainTruncateStillWorks(t *testing.T) {
	e := newEngine(t)
	const src = "TRUNCATE TABLE db.t"
	ast := mustParse(t, e, src)
	opts := statOpt(&pb.RewriteTableStaticArgs{TableMap: map[string]string{"db.t": "t_phys"}})
	resp, handled, err := RewriteWrite(e, ast, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v (%s)", handled, resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_TRUNCATE_TABLE {
		t.Fatalf("stmt = %v, want TRUNCATE_TABLE", resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "TRUNCATE TABLE db.t_phys") {
		t.Fatalf("sql = %q, want ≈ TRUNCATE TABLE db.t_phys", resp.GetSqlAfterRewrite())
	}
	if got := resp.GetTableRewrites(); !mapEq(got, map[string]string{"db.t": "db.t_phys"}) {
		t.Fatalf("table_rewrites = %v", got)
	}
}

func TestRewriteWrite_createDictionaryRejected(t *testing.T) {
	resp := wantUnsupported(t, "CREATE DICTIONARY db.d (id UInt64) PRIMARY KEY id SOURCE(NULL()) LAYOUT(FLAT()) LIFETIME(0)")
	// Reject lives in dispatchCreateTable → stmt stays CREATE_TABLE (parity with
	// the sibling create_table AS-function reject).
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_TABLE {
		t.Fatalf("stmt = %v, want CREATE_TABLE", resp.GetStatementType())
	}
}

func TestRewriteWrite_liveViewRejected(t *testing.T) {
	wantUnsupported(t, "CREATE LIVE VIEW db.lv AS SELECT 1")
}
func TestRewriteWrite_windowViewRejected(t *testing.T) {
	wantUnsupported(t, "CREATE WINDOW VIEW db.wv AS SELECT 1")
}

func TestRewriteWrite_copyRejected(t *testing.T) { wantUnsupported(t, "COPY foo FROM 'bar'") }

func TestRewriteWrite_detachRejected(t *testing.T) { wantUnsupported(t, "DETACH TABLE db.t") }
func TestRewriteWrite_renameDatabaseRejected(t *testing.T) {
	wantUnsupported(t, "RENAME DATABASE db1 TO db2")
}
func TestRewriteWrite_renameDictionaryRejected(t *testing.T) {
	wantUnsupported(t, "RENAME DICTIONARY db.d1 TO db.d2")
}
func TestRewriteWrite_exchangeDictionariesRejected(t *testing.T) {
	wantUnsupported(t, "EXCHANGE DICTIONARIES db.d1 AND db.d2")
}
func TestRewriteWrite_alterUserRejected(t *testing.T) {
	wantUnsupported(t, "ALTER USER u IDENTIFIED BY 'p'")
}
func TestRewriteWrite_dropDictionaryRejected(t *testing.T) {
	wantUnsupported(t, "DROP DICTIONARY db.d")
}
func TestRewriteWrite_alterDatabaseRejected(t *testing.T) {
	wantUnsupported(t, "ALTER DATABASE db MODIFY SETTING x = 1")
}

// REGRESSION guards: Phase-3/4 pass-throughs must NOT be swallowed by the new
// reject prefixes (handled=false → caller falls through to SELECT).
func TestRewriteWrite_useStillPassthrough(t *testing.T)    { wantPassthrough(t, "USE db") }
func TestRewriteWrite_existsStillPassthrough(t *testing.T) { wantPassthrough(t, "EXISTS TABLE db.t") }
func TestRewriteWrite_grantStillPassthrough(t *testing.T) {
	wantPassthrough(t, "GRANT SELECT ON db.t TO user")
}
func TestRewriteWrite_revokeStillPassthrough(t *testing.T) {
	wantPassthrough(t, "REVOKE SELECT ON db.t FROM user")
}
func TestRewriteWrite_showStillPassthrough(t *testing.T) { wantPassthrough(t, "SHOW TABLES") }

// Task 10.11. DROP DATABASE db → handled (out-of-phase reject),
// UnsupportedStatement, stmt=DROP_DATABASE.
func TestRewriteWrite_dropDatabaseOutOfPhase(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE db")

	resp, handled, err := RewriteWrite(e, ast, "DROP DATABASE db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("handled = false, want true (out-of-phase reject is handled)")
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("code = %v, want UnsupportedStatement (%s)", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DROP_DATABASE {
		t.Fatalf("stmt = %v, want DROP_DATABASE", resp.GetStatementType())
	}
}
