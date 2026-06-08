package engine

import "testing"

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
