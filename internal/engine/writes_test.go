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
