package handlers

import (
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
