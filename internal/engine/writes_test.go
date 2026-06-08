package engine

import (
	"strings"
	"testing"
)

// normalizeSQL parses and regenerates sql through the engine, yielding the
// engine's canonical rendering. Two SQL strings are semantically equal iff their
// normalized forms match.
func normalizeSQL(t *testing.T, e Engine, sql string) string {
	t.Helper()
	ast, err := e.ParseOne(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	out, err := e.Generate(ast)
	if err != nil {
		t.Fatalf("gen %q: %v", sql, err)
	}
	return out
}

// sqlEq reports whether a and b are semantically equal after normalization.
func sqlEq(t *testing.T, e Engine, a, b string) bool {
	t.Helper()
	na := normalizeSQL(t, e, a)
	nb := normalizeSQL(t, e, b)
	return na == nb
}

// inspectSQL parses sql and runs InspectWrite, failing the test (not panicking)
// on either error — the standard pattern for the engine-integration tests here.
func inspectSQL(t *testing.T, e Engine, sql string) WriteInfo {
	t.Helper()
	ast, err := e.ParseOne(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	info, err := InspectWrite(ast)
	if err != nil {
		t.Fatalf("InspectWrite %q: %v", sql, err)
	}
	return info
}

func TestInspectWrite_simpleTargets(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		name     string
		sql      string
		kind     string
		role     WriteRole
		db       string
		table    string
		ifExists bool
	}{
		{"drop_table", "DROP TABLE db.t", NodeDropTable, RoleDrop, "db", "t", false},
		{"drop_table_ife", "DROP TABLE IF EXISTS db.t", NodeDropTable, RoleDrop, "db", "t", true},
		{"drop_view", "DROP VIEW db.v", NodeDropView, RoleDrop, "db", "v", false},
		{"truncate", "TRUNCATE TABLE db.t", NodeTruncate, RoleTruncate, "db", "t", false},
		{"update", "UPDATE db.t SET x=1 WHERE y=2", NodeUpdate, RoleUpdate, "db", "t", false},
		{"delete", "DELETE FROM db.t WHERE x=1", NodeDelete, RoleDelete, "db", "t", false},
		{"drop_table_unqualified", "DROP TABLE t", NodeDropTable, RoleDrop, "", "t", false},
		{"drop_table_ife_unqualified", "DROP TABLE IF EXISTS t", NodeDropTable, RoleDrop, "", "t", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.sql, err)
			}
			info, err := InspectWrite(ast)
			if err != nil {
				t.Fatalf("InspectWrite: %v", err)
			}
			if info.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", info.Kind, tc.kind)
			}
			if len(info.Slots) != 1 {
				t.Fatalf("len(Slots) = %d, want 1: %+v", len(info.Slots), info.Slots)
			}
			s := info.Slots[0]
			if s.Role != tc.role {
				t.Errorf("Slot.Role = %q, want %q", s.Role, tc.role)
			}
			if s.Target.DB != tc.db {
				t.Errorf("Slot.Target.DB = %q, want %q", s.Target.DB, tc.db)
			}
			if s.Target.Table != tc.table {
				t.Errorf("Slot.Target.Table = %q, want %q", s.Target.Table, tc.table)
			}
			if info.IfExists != tc.ifExists {
				t.Errorf("IfExists = %v, want %v", info.IfExists, tc.ifExists)
			}
		})
	}
}

func TestInspectWrite_dropMultiTable(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("DROP TABLE db.a, db.b")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info, err := InspectWrite(ast)
	if err != nil {
		t.Fatalf("InspectWrite: %v", err)
	}
	if !info.Multi {
		t.Errorf("Multi = false, want true for multi-table DROP")
	}
	// writeSlots visits only names[0] — the handler rejects multi-table DROP before
	// rewriting, but InspectWrite must still expose exactly one coherent slot.
	if len(info.Slots) != 1 {
		t.Errorf("len(Slots) = %d, want 1 (first name only)", len(info.Slots))
	}
}

func TestRewriteWriteTargets_rename(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("DROP TABLE db.t")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := RewriteWriteTargets(ast, func(WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "t_renamed"}
	})
	if err != nil {
		t.Fatalf("RewriteWriteTargets: %v", err)
	}
	got, err := e.Generate(out)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !sqlEq(t, e, got, "DROP TABLE phys.t_renamed") {
		t.Errorf("got %q, want semantically DROP TABLE phys.t_renamed", got)
	}
}

func TestRewriteWriteTargets_renameKeepsDBWhenNewDBEmpty(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("DROP TABLE db.t")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := RewriteWriteTargets(ast, func(WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewTable: "t2"}
	})
	if err != nil {
		t.Fatalf("RewriteWriteTargets: %v", err)
	}
	got, err := e.Generate(out)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !sqlEq(t, e, got, "DROP TABLE db.t2") {
		t.Errorf("got %q, want semantically DROP TABLE db.t2 (schema preserved)", got)
	}
}

// --- Task 3: create_table ---

func TestInspectWrite_createTable(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "CREATE TABLE db.t (x Int32) ENGINE=Memory")
	if info.Kind != NodeCreateTable || len(info.Slots) != 1 || info.Slots[0].Role != RoleCreate {
		t.Fatalf("got %+v", info)
	}
	if info.Slots[0].Target.DB != "db" || info.Slots[0].Target.Table != "t" {
		t.Errorf("target=%+v", info.Slots[0].Target)
	}
}

func TestInspectWrite_createTableAs(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "CREATE TABLE db.t AS db2.src")
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
	e := newTestEngine(t)
	info := inspectSQL(t, e, "CREATE TABLE IF NOT EXISTS db.t (x Int32) ENGINE=Memory")
	if !info.IfNotExists {
		t.Errorf("IfNotExists=false want true")
	}
}

// TestInspectWrite_createTableAsFunction pins the EMPIRICAL shape of
// `CREATE TABLE x AS table_function(...)`. The Phase-2 plan ASSUMED a
// top-level `create_table.as_table_function` key (mirroring the C++ DB AST
// field `ASTCreateQuery::as_table_function`, writes.cc:346). Polyglot does NOT
// expose that: it reuses the `clone_source` slot but with an EMPTY name and a
// nested `identifier_func.function`. So AsTableFunction is detected by the
// presence of `clone_source.identifier_func`, not a dedicated key.
func TestInspectWrite_createTableAsFunction(t *testing.T) {
	e := newTestEngine(t)
	for _, sql := range []string{
		"CREATE TABLE db.t AS remote('h', d, tbl)",
		"CREATE TABLE db.t AS numbers(10)",
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		info, err := InspectWrite(ast)
		if err != nil {
			t.Fatalf("InspectWrite %q: %v", sql, err)
		}
		if !info.AsTableFunction {
			t.Errorf("%q: AsTableFunction=false want true", sql)
		}
		// The clone_source slot (the function node, empty name) must be dropped:
		// only the real RoleCreate target remains.
		if len(info.Slots) != 1 || info.Slots[0].Role != RoleCreate {
			t.Errorf("%q: slots=%+v want exactly 1 RoleCreate", sql, info.Slots)
		}
	}
}

// TestInspectWrite_createTableAsPlainTableNotFunction guards the boundary: a
// plain `AS db2.src` clone source must NOT be flagged AsTableFunction (only the
// identifier_func form is a table function).
func TestInspectWrite_createTableAsPlainTableNotFunction(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "CREATE TABLE db.t AS db2.src")
	if info.AsTableFunction {
		t.Errorf("plain AS db2.src flagged AsTableFunction")
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
	if !sqlEq(t, e, got, "CREATE TABLE p1.t2 AS p2.src2") {
		t.Errorf("got %q", got)
	}
}

// --- Task 3: alter_table ---

func TestInspectWrite_alterTable(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "ALTER TABLE db.t ADD COLUMN y Int32")
	if info.Kind != NodeAlterTable || len(info.Slots) != 1 || info.Slots[0].Role != RoleAlter {
		t.Fatalf("got %+v", info)
	}
	if info.CrossTable {
		t.Errorf("plain ALTER ADD flagged CrossTable")
	}
}

func TestInspectWrite_alterTableIfExists(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "ALTER TABLE IF EXISTS db.t ADD COLUMN y Int32")
	if !info.IfExists {
		t.Errorf("IfExists=false want true")
	}
}

func TestInspectWrite_alterCrossTable(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "ALTER TABLE db.t ATTACH PARTITION 1 FROM db.src")
	if !info.CrossTable {
		t.Errorf("ATTACH ... FROM not flagged CrossTable")
	}
}

// TestInspectWrite_alterCrossTableStructured covers REPLACE PARTITION ... FROM,
// which Polyglot models as a STRUCTURED action ({"ReplacePartition":{"source":
// {"table":...}}}) rather than a Raw SQL action. The C++ reference
// (alterHasCrossTableRef, writes.cc:160-168) flags this via the structured
// from_table field and the dedicated test rejects it
// (rewriter_test.cc:2389). Go must catch the structured form too, not just Raw.
func TestInspectWrite_alterCrossTableStructured(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "ALTER TABLE db.t REPLACE PARTITION 1 FROM db.src")
	if !info.CrossTable {
		t.Errorf("REPLACE PARTITION ... FROM (structured) not flagged CrossTable")
	}
}

// TestInspectWrite_alterCrossTableMoveToTable covers MOVE PARTITION ... TO TABLE
// (a Raw action carrying " TO TABLE ").
func TestInspectWrite_alterCrossTableMoveToTable(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "ALTER TABLE db.t MOVE PARTITION 1 TO TABLE db.dst")
	if !info.CrossTable {
		t.Errorf("MOVE PARTITION ... TO TABLE not flagged CrossTable")
	}
}

// TestInspectWrite_alterCrossTableGranularity guards both directions of the Raw
// keyword scan against the bugs found in review: the singular PART forms
// (ATTACH PART ... FROM, MOVE PART ... TO TABLE) ARE cross-table (C++ sets
// from_table/to_table for PART and PARTITION alike), while FETCH PARTITION ...
// FROM '<zk-path>' and MOVE PARTITION ... TO DISK/VOLUME are NOT (FETCH's FROM is
// a ZooKeeper path C++ accepts; TO DISK/VOLUME is single-table).
func TestInspectWrite_alterCrossTableGranularity(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql  string
		want bool
	}{
		{"ALTER TABLE db.t ATTACH PART '1' FROM db.src", true},        // singular PART, was false-negative
		{"ALTER TABLE db.t MOVE PART '1' TO TABLE db.dst", true},      // singular PART, was false-negative
		{"ALTER TABLE db.t FETCH PARTITION 1 FROM '/zk/path'", false}, // ZK path FROM, was false-positive
		{"ALTER TABLE db.t MOVE PARTITION 1 TO VOLUME 'v'", false},    // single-table
		{"ALTER TABLE db.t MOVE PARTITION 1 TO DISK 'd'", false},      // single-table
		{"ALTER TABLE db.t ADD COLUMN y Int32", false},                // plain
	}
	for _, c := range cases {
		ast, err := e.ParseOne(c.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", c.sql, err)
		}
		info, err := InspectWrite(ast)
		if err != nil {
			t.Fatalf("InspectWrite %q: %v", c.sql, err)
		}
		if info.CrossTable != c.want {
			t.Errorf("%q: CrossTable=%v want %v", c.sql, info.CrossTable, c.want)
		}
	}
}

// --- Task 4: create_view (name + MV TO target + body extract/set) ---

func TestInspectWrite_createView(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "CREATE VIEW db.v AS SELECT * FROM db.s")
	if info.Kind != NodeCreateView || !info.IsView || info.Materialized {
		t.Fatalf("got %+v", info)
	}
	if len(info.Slots) != 1 || info.Slots[0].Role != RoleCreate {
		t.Fatalf("slots=%+v", info.Slots)
	}
	if info.Slots[0].Target.DB != "db" || info.Slots[0].Target.Table != "v" {
		t.Errorf("create target=%+v", info.Slots[0].Target)
	}
	if !info.HasViewBody {
		t.Errorf("HasViewBody=false want true")
	}
}

func TestInspectWrite_createMVTo(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "CREATE MATERIALIZED VIEW db.mv TO db.dst AS SELECT * FROM db.s")
	if !info.Materialized {
		t.Fatalf("Materialized=false")
	}
	if !info.IsView {
		t.Errorf("IsView=false want true")
	}
	if len(info.Slots) != 2 || info.Slots[1].Role != RoleViewTo ||
		info.Slots[1].Target.DB != "db" || info.Slots[1].Target.Table != "dst" {
		t.Fatalf("slots=%+v", info.Slots)
	}
}

func TestInspectWrite_createViewIfNotExists(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "CREATE VIEW IF NOT EXISTS db.v AS SELECT * FROM db.s")
	if !info.IfNotExists {
		t.Errorf("IfNotExists=false want true")
	}
}

func TestViewBody_extractRewriteSet(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("CREATE VIEW db.v AS SELECT * FROM db.s")
	if err != nil {
		t.Fatal(err)
	}
	body, ok, err := ExtractViewBody(ast)
	if err != nil || !ok {
		t.Fatalf("extract: ok=%v err=%v", ok, err)
	}
	// body is a {"select":...} AST: CollectSelectTables sees db.s.
	tts, err := CollectSelectTables(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(tts) != 1 || tts[0].Table != "s" {
		t.Fatalf("body tables=%+v", tts)
	}
	body2, err := RewriteSelectTables(body, func(tt TableTarget) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "s2"}
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := SetViewBody(ast, body2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Generate(out)
	if err != nil {
		t.Fatal(err)
	}
	// DEVIATION FROM PLAN STRING: the plan asserted
	//   "CREATE VIEW db.v AS SELECT * FROM phys.s2"
	// but RewriteSelectTables (the Phase-1 SELECT layer) intentionally adds a
	// back-alias of the ORIGINAL qualified name on every rename, so unqualified
	// column refs in the body still resolve to the renamed table. See
	// TestRewriteSelectTables_renameAndSetDB (nodes_test.go), which pins
	//   SELECT a FROM phys.t_x "db.t" ...
	// That alias is semantically load-bearing (sqlEq treats it as distinct from the
	// alias-free form), so the faithful expectation is the back-aliased SQL below.
	// This is NOT introduced by ExtractViewBody/SetViewBody — the body splice is
	// lossless (see TestViewBody_roundTripNoChange); the alias comes from the
	// SELECT rewrite the body went through.
	if !sqlEq(t, e, got, `CREATE VIEW db.v AS SELECT * FROM phys.s2 "db.s"`) {
		t.Errorf("got %q", got)
	}
}

// TestViewBody_roundTripNoChange confirms extract -> (no change) -> SetViewBody ->
// Generate is semantically identical to the original: the {"select":...} body
// splices back without loss.
func TestViewBody_roundTripNoChange(t *testing.T) {
	e := newTestEngine(t)
	const src = "CREATE VIEW db.v AS SELECT * FROM db.s"
	ast, err := e.ParseOne(src)
	if err != nil {
		t.Fatal(err)
	}
	body, ok, err := ExtractViewBody(ast)
	if err != nil || !ok {
		t.Fatalf("extract: ok=%v err=%v", ok, err)
	}
	out, err := SetViewBody(ast, body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Generate(out)
	if err != nil {
		t.Fatal(err)
	}
	if !sqlEq(t, e, got, src) {
		t.Errorf("round-trip got %q want semantically %q", got, src)
	}
}

// TestViewBody_materializedRewrite confirms SetViewBody reattaches correctly for a
// MATERIALIZED VIEW with a TO target: the body SELECT is rewritten while the
// create name and TO target are untouched by the body splice.
func TestViewBody_materializedRewrite(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("CREATE MATERIALIZED VIEW db.mv TO db.dst AS SELECT * FROM db.s")
	if err != nil {
		t.Fatal(err)
	}
	body, ok, err := ExtractViewBody(ast)
	if err != nil || !ok {
		t.Fatalf("extract: ok=%v err=%v", ok, err)
	}
	body2, err := RewriteSelectTables(body, func(tt TableTarget) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "s2"}
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := SetViewBody(ast, body2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Generate(out)
	if err != nil {
		t.Fatal(err)
	}
	// Back-alias on the body's renamed table, as in TestViewBody_extractRewriteSet
	// (the Phase-1 SELECT rewrite contract). The create name (db.mv) and TO target
	// (db.dst) are untouched by the body splice — confirming SetViewBody reattaches
	// the rewritten body to a MATERIALIZED VIEW without disturbing its other slots.
	if !sqlEq(t, e, got, `CREATE MATERIALIZED VIEW db.mv TO db.dst AS SELECT * FROM phys.s2 "db.s"`) {
		t.Errorf("got %q", got)
	}
}

// TestSetViewBody_nonViewRejected confirms SetViewBody guards its kind: calling it
// on a non-create_view AST is an error, not a silent splice.
func TestSetViewBody_nonViewRejected(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("DROP TABLE db.t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetViewBody(ast, AST(`{"select":{}}`)); err == nil {
		t.Errorf("SetViewBody on non-view kind: err=nil want error")
	}
}

// --- Task 5: insert (target flags + FORMAT-payload-preserving GenerateInsert) ---

func TestInspectWrite_insert(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "INSERT INTO db.t (x) VALUES (1)")
	if info.Kind != NodeInsert || len(info.Slots) != 1 || info.Slots[0].Role != RoleInsert {
		t.Fatalf("got %+v", info)
	}
	if info.Slots[0].Target.DB != "db" || info.Slots[0].Target.Table != "t" {
		t.Errorf("target=%+v", info.Slots[0].Target)
	}
	if info.AsTableFunction || info.MissingTable {
		t.Errorf("plain INSERT flagged AsTableFunction=%v MissingTable=%v", info.AsTableFunction, info.MissingTable)
	}
}

// TestInspectWrite_insertFunctionRejectFlags pins the EMPIRICAL shape of
// `INSERT INTO FUNCTION remote(...)`. The Phase-2 plan ASSUMED a top-level
// `insert.table_function` key (mirroring the C++ guard insert_query->table_function,
// writes.cc:504). Polyglot does NOT expose that key. The real shape (verified):
//   - body["function_target"] is populated (a {"function":{…}} object), AND
//   - body["table"] is STILL present but carries an EMPTY name ("name":"").
//
// So AsTableFunction is detected by the presence of `function_target`, and the
// empty-named table means the table-absence MissingTable check does NOT double-fire
// (table IS a map). The handler (Task 6) rejects on AsTableFunction first.
func TestInspectWrite_insertFunctionRejectFlags(t *testing.T) {
	e := newTestEngine(t)
	info := inspectSQL(t, e, "INSERT INTO FUNCTION remote('h', db, t) VALUES (1)")
	if !info.AsTableFunction {
		t.Errorf("INSERT INTO FUNCTION: AsTableFunction=false")
	}
}

func TestGenerateInsert_valuesKept(t *testing.T) {
	e := newTestEngine(t)
	orig := "INSERT INTO db.t (x, y) VALUES (1, 'a'), (2, 'b')"
	ast, err := e.ParseOne(orig)
	if err != nil {
		t.Fatal(err)
	}
	rw, err := RewriteWriteTargets(ast, func(s WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "t2"}
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := GenerateInsert(e, orig, rw)
	if err != nil {
		t.Fatal(err)
	}
	if !sqlEq(t, e, got, "INSERT INTO phys.t2 (x, y) VALUES (1, 'a'), (2, 'b')") {
		t.Errorf("got %q", got)
	}
}

// TestGenerateInsert_valuesSingleRow is the minimal rename: a single-row VALUES
// insert has no FORMAT token, so GenerateInsert returns Generate()'s output with
// no splice.
func TestGenerateInsert_valuesSingleRow(t *testing.T) {
	e := newTestEngine(t)
	orig := "INSERT INTO db.t (x) VALUES (1)"
	ast, err := e.ParseOne(orig)
	if err != nil {
		t.Fatal(err)
	}
	rw, err := RewriteWriteTargets(ast, func(s WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewTable: "t"} // keep DB
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := GenerateInsert(e, orig, rw)
	if err != nil {
		t.Fatal(err)
	}
	if !sqlEq(t, e, got, "INSERT INTO phys.t (x) VALUES (1)") && !sqlEq(t, e, got, "INSERT INTO db.t (x) VALUES (1)") {
		t.Errorf("got %q", got)
	}
}

func TestGenerateInsert_formatPayloadPreserved(t *testing.T) {
	e := newTestEngine(t)
	orig := "INSERT INTO db.t FORMAT JSONEachRow {\"x\":1}\n{\"x\":2}"
	ast, err := e.ParseOne(orig)
	if err != nil {
		t.Fatal(err)
	}
	rw, err := RewriteWriteTargets(ast, func(s WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "t2"}
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := GenerateInsert(e, orig, rw)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "INSERT INTO phys.t2 FORMAT JSONEachRow"
	wantTail := "{\"x\":1}\n{\"x\":2}"
	if !strings.HasPrefix(got, wantPrefix) || !strings.Contains(got, wantTail) {
		t.Errorf("got %q\n want prefix %q + tail %q", got, wantPrefix, wantTail)
	}
}

// TestGenerateInsert_formatCSV covers a FORMAT payload whose tail begins with a
// newline (CSV rows). The splice boundary is the format-name token's byte end, so
// the leading "\n" before the first CSV row is preserved.
func TestGenerateInsert_formatCSV(t *testing.T) {
	e := newTestEngine(t)
	orig := "INSERT INTO db.t FORMAT CSV\n1,2\n3,4"
	ast, err := e.ParseOne(orig)
	if err != nil {
		t.Fatal(err)
	}
	rw, err := RewriteWriteTargets(ast, func(s WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "t2"}
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := GenerateInsert(e, orig, rw)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "INSERT INTO phys.t2 FORMAT CSV"
	wantTail := "\n1,2\n3,4"
	if !strings.HasPrefix(got, wantPrefix) || !strings.Contains(got, wantTail) {
		t.Errorf("got %q\n want prefix %q + tail %q", got, wantPrefix, wantTail)
	}
}

// TestInsertFormatTail_noFormatNoSplice confirms a VALUES insert (no FORMAT token)
// yields ok=false, so GenerateInsert returns the plain Generate() output unspliced.
func TestInsertFormatTail_noFormatNoSplice(t *testing.T) {
	e := newTestEngine(t)
	tail, ok, err := insertFormatTail(e, "INSERT INTO db.t (x) VALUES (1)")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("VALUES insert: ok=true tail=%q want ok=false (no FORMAT clause)", tail)
	}
}

// TestGenerateInsert_formatAsIdentifierNotSpliced guards the parity bug found in
// review: `FORMAT` used as a column identifier (here a column named FORMAT in the
// embedded SELECT) tokenizes as token_type=="FORMAT", but it is NOT a FORMAT data
// clause. The AST gate (insert.query is a select, not a {"command":"FORMAT …"})
// must suppress the splice so the SQL is not corrupted by a spurious tail.
func TestGenerateInsert_formatAsIdentifierNotSpliced(t *testing.T) {
	e := newTestEngine(t)
	orig := "INSERT INTO db.t (x) SELECT FORMAT FROM db.s"
	ast, err := e.ParseOne(orig)
	if err != nil {
		t.Fatal(err)
	}
	rw, err := RewriteWriteTargets(ast, func(s WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "t2"}
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := GenerateInsert(e, orig, rw)
	if err != nil {
		t.Fatal(err)
	}
	// Only the INSERT target is rewritten; the embedded SELECT (FORMAT column,
	// db.s source) is untouched and NO payload tail is appended.
	if !sqlEq(t, e, got, "INSERT INTO phys.t2 (x) SELECT FORMAT FROM db.s") {
		t.Errorf("FORMAT-as-identifier corrupted: got %q", got)
	}
}

// TestInsertHasFormatClause_gate directly pins the AST gate across forms.
func TestInsertHasFormatClause_gate(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql  string
		want bool
	}{
		{"INSERT INTO db.t FORMAT JSONEachRow {\"x\":1}", true},
		{"INSERT INTO db.t FORMAT CSV\n1,2", true},
		{"INSERT INTO db.t (x) VALUES (1)", false},
		{"INSERT INTO db.t SELECT * FROM db.s", false},
		{"INSERT INTO db.t (x) SELECT FORMAT FROM db.s", false},
	}
	for _, c := range cases {
		ast, err := e.ParseOne(c.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", c.sql, err)
		}
		if got := insertHasFormatClause(ast); got != c.want {
			t.Errorf("%q: insertHasFormatClause=%v want %v", c.sql, got, c.want)
		}
	}
}

// ---- Task 6: tier-C raw-SQL command sub-classification + table-span splice ----

func TestInspectWrite_commandSubKinds(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql string
		sub CommandSub
	}{
		{"RENAME TABLE db.a TO db.b", CmdRename},
		{"RENAME TABLE db.a TO db.b, db.c TO db.d", CmdRename},
		{"EXCHANGE TABLES db.a AND db.b", CmdExchange},
		{"ALTER TABLE db.t UPDATE x = 1 WHERE y = 2", CmdAlterUpdate},
		{"OPTIMIZE TABLE db.t", CmdBareReject},
		{"USE db", CmdNone},
		{"EXISTS TABLE db.t", CmdNone},
	}
	for _, c := range cases {
		info := inspectSQL(t, e, c.sql)
		if info.Kind != NodeCommand || info.Sub != c.sub {
			t.Errorf("%q sub=%q want %q (kind=%q)", c.sql, info.Sub, c.sub, info.Kind)
		}
	}
}

func TestClassifyWriteCommand(t *testing.T) {
	cases := []struct {
		sql string
		sub CommandSub
	}{
		{"RENAME TABLE db.a TO db.b", CmdRename},
		{"  rename table db.a to db.b ", CmdRename},
		{"EXCHANGE TABLES db.a AND db.b", CmdExchange},
		{"ALTER TABLE db.t UPDATE x = 1 WHERE y = 2", CmdAlterUpdate},
		{"ALTER TABLE db.t ADD COLUMN x Int32", CmdNone}, // ALTER without UPDATE word → not alter_update
		{"OPTIMIZE TABLE db.t", CmdBareReject},
		{"UNDROP TABLE db.t", CmdBareReject},
		{"MOVE db.t TO SHARD '...'", CmdBareReject},
		{"BACKUP TABLE db.t TO Disk('d','f')", CmdBareReject},
		{"RESTORE TABLE db.t FROM Disk('d','f')", CmdBareReject},
		{"KILL QUERY WHERE 1", CmdBareReject},
		{"USE db", CmdNone},
		{"SHOW TABLES", CmdNone},
		{"GRANT SELECT ON db.t TO user", CmdNone},
		{"EXISTS TABLE db.t", CmdNone},
		{"SHOW CREATE TABLE db.t", CmdNone},
	}
	for _, c := range cases {
		if got := classifyWriteCommand(c.sql); got != c.sub {
			t.Errorf("classifyWriteCommand(%q)=%q want %q", c.sql, got, c.sub)
		}
	}
}

func TestRawTableRefs_rename(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("RENAME TABLE db.a TO db.b, c TO d")
	if err != nil {
		t.Fatal(err)
	}
	got, sub, err := RawTableRefs(e, ast)
	if err != nil {
		t.Fatal(err)
	}
	if sub != CmdRename {
		t.Errorf("sub=%q", sub)
	}
	want := []TableTarget{{DB: "db", Table: "a"}, {DB: "db", Table: "b"}, {Table: "c"}, {Table: "d"}}
	if len(got) != len(want) {
		t.Fatalf("refs=%+v want %+v", got, want)
	}
	for i := range want {
		if got[i].DB != want[i].DB || got[i].Table != want[i].Table {
			t.Errorf("refs[%d]=%+v want %+v", i, got[i], want[i])
		}
	}
}

func TestRawTableRefs_alterUpdateSingleTarget(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("ALTER TABLE db.t UPDATE x = 1 WHERE y = 2")
	if err != nil {
		t.Fatal(err)
	}
	got, sub, err := RawTableRefs(e, ast)
	if err != nil {
		t.Fatal(err)
	}
	if sub != CmdAlterUpdate {
		t.Errorf("sub=%q", sub)
	}
	// Only the single name-run after TABLE is a table ref; x/y are columns and
	// must NOT be picked up.
	if len(got) != 1 || got[0].DB != "db" || got[0].Table != "t" {
		t.Errorf("refs=%+v want [{db t}]", got)
	}
}

func TestRawTableRefs_nonWriteCommand(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("USE db")
	if err != nil {
		t.Fatal(err)
	}
	got, sub, err := RawTableRefs(e, ast)
	if err != nil {
		t.Fatal(err)
	}
	if sub != CmdNone || got != nil {
		t.Errorf("USE db: refs=%+v sub=%q want nil/CmdNone", got, sub)
	}
}

func TestSpliceRawTables_rename(t *testing.T) {
	e := newTestEngine(t)
	orig := "RENAME TABLE db.a TO db.b"
	out, err := SpliceRawTables(e, orig, map[string]string{"db.a": "phys.a1", "db.b": "phys.b1"})
	if err != nil {
		t.Fatal(err)
	}
	if !sqlEq(t, e, out, "RENAME TABLE phys.a1 TO phys.b1") {
		t.Errorf("got %q", out)
	}
}

func TestSpliceRawTables_alterUpdate(t *testing.T) {
	e := newTestEngine(t)
	orig := "ALTER TABLE db.t UPDATE x = 1 WHERE y = 2"
	out, err := SpliceRawTables(e, orig, map[string]string{"db.t": "phys.t1"})
	if err != nil {
		t.Fatal(err)
	}
	if !sqlEq(t, e, out, "ALTER TABLE phys.t1 UPDATE x = 1 WHERE y = 2") {
		t.Errorf("got %q", out)
	}
}

func TestSpliceRawTables_exchange(t *testing.T) {
	e := newTestEngine(t)
	orig := "EXCHANGE TABLES db.a AND db.b"
	out, err := SpliceRawTables(e, orig, map[string]string{"db.a": "p.a", "db.b": "p.b"})
	if err != nil {
		t.Fatal(err)
	}
	if !sqlEq(t, e, out, "EXCHANGE TABLES p.a AND p.b") {
		t.Errorf("got %q", out)
	}
}

func TestSpliceRawTables_partialRewriteOnly(t *testing.T) {
	// A rewrite map missing one of the refs leaves that ref untouched (handler
	// validation pass may rewrite only some targets).
	e := newTestEngine(t)
	orig := "RENAME TABLE db.a TO db.b"
	out, err := SpliceRawTables(e, orig, map[string]string{"db.a": "phys.a1"})
	if err != nil {
		t.Fatal(err)
	}
	if !sqlEq(t, e, out, "RENAME TABLE phys.a1 TO db.b") {
		t.Errorf("got %q", out)
	}
}

func TestSpliceRawTables_bareNameKey(t *testing.T) {
	// Unqualified names key by bare table name.
	e := newTestEngine(t)
	orig := "RENAME TABLE a TO b"
	out, err := SpliceRawTables(e, orig, map[string]string{"a": "phys.a1", "b": "phys.b1"})
	if err != nil {
		t.Fatal(err)
	}
	if !sqlEq(t, e, out, "RENAME TABLE phys.a1 TO phys.b1") {
		t.Errorf("got %q", out)
	}
}

func TestSpliceRawTables_quotedSourceKeyMatches(t *testing.T) {
	// The tokenizer strips backticks from QUOTED_IDENTIFIER text, so a
	// backtick-quoted source name keys by its UNQUOTED identifier text.
	e := newTestEngine(t)
	orig := "RENAME TABLE `db`.`weird.name` TO plain"
	out, err := SpliceRawTables(e, orig, map[string]string{
		"db.weird.name": QuoteQualified("phys", "x"),
		"plain":         QuoteQualified("phys", "y"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sqlEq(t, e, out, "RENAME TABLE phys.x TO phys.y") {
		t.Errorf("got %q", out)
	}
}

func TestQuoteQualified(t *testing.T) {
	cases := []struct {
		db, table, want string
	}{
		{"", "t", "t"},
		{"db", "t", "db.t"},
		{"", "tenant1.a", "`tenant1.a`"},                // dotted single name must quote
		{"testnet", "tenant1.a", "testnet.`tenant1.a`"}, // db plain, table dotted
		{"a-b", "t", "`a-b`.t"},                         // hyphen forces quoting
		{"", "1x", "`1x`"},                              // leading digit forces quoting
	}
	for _, c := range cases {
		if got := QuoteQualified(c.db, c.table); got != c.want {
			t.Errorf("QuoteQualified(%q,%q)=%q want %q", c.db, c.table, got, c.want)
		}
	}
}

func TestSpliceRawTables_dottedDynamicNameQuoted(t *testing.T) {
	// A dynamic rename produces a dotted table name that MUST splice as a quoted
	// single identifier, else it re-parses as a 3-part name.
	//
	// Faithful-behavior note (verified empirically): the source is
	// `RENAME TABLE db.a TO db.b`, which has exactly TWO table refs (a, b) — one
	// rename pair. After rewriting both to dotted dynamic names the statement is
	// still one rename pair, so RawTableRefs returns 2 refs (NOT 4). The
	// load-bearing assertion is that each rewritten dotted name survives as a
	// SINGLE quoted identifier: QuoteQualified("testnet","tenant1.a") splices as
	// `testnet.`tenant1.a``, which re-tokenizes with the backtick-quoted run as one
	// QUOTED_IDENTIFIER → Table=="tenant1.a" (with backticks stripped), DB=="testnet".
	// Were it spliced unquoted (`testnet.tenant1.a`) it would mis-parse as a 3-part
	// name and refs[0].Table would collapse to "tenant1".
	e := newTestEngine(t)
	orig := "RENAME TABLE db.a TO db.b"
	out, err := SpliceRawTables(e, orig, map[string]string{
		"db.a": QuoteQualified("testnet", "tenant1.a"),
		"db.b": QuoteQualified("testnet", "tenant1.b"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Re-parse: the rewritten names must survive as single identifiers, not split.
	reparsed, perr := e.ParseOne(out)
	if perr != nil {
		t.Fatalf("re-parse %q: %v", out, perr)
	}
	refs, sub, err := RawTableRefs(e, reparsed)
	if err != nil {
		t.Fatal(err)
	}
	if sub != CmdRename {
		t.Errorf("sub=%q want %q", sub, CmdRename)
	}
	if len(refs) != 2 ||
		refs[0].DB != "testnet" || refs[0].Table != "tenant1.a" ||
		refs[1].DB != "testnet" || refs[1].Table != "tenant1.b" {
		t.Errorf("dotted dynamic name not preserved as single identifier: out=%q refs=%+v", out, refs)
	}
}
