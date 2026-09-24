package engine

import "testing"

func TestPlainDropTable(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql  string
		want bool
	}{
		{"DROP TABLE db1.t", true},
		{"drop table if exists `db1`.\"t\", other.u sync;", true},
		{"DROP TABLE IF EXISTS db1.t SYNC", true},
		{"DROP TABLE db1.t NO DELAY", true},
		{"DROP /* c */ TABLE db1.t -- trailing", true},
		{"DROP TABLE t", true},
		{"DROP TABLE sync", true},
		{"DROP TABLE db1.t, other.u", true},
		{"DROP TABLE db1.t ON CLUSTER c", false},
		{"DROP TABLE db1.t ON CLUSTER 'c' SYNC", false},
		{"DROP TEMPORARY TABLE t", false},
		{"DROP TABLE IF EMPTY db1.t", false},
		{"DROP TABLE db1.t FORMAT JSON", false},
		{"DROP TABLE db1.t SETTINGS a = 1", false},
		{"DROP TABLE {tbl:Identifier}", false},
		{"DROP TABLE a.b.c", false},
		{"DROP TABLE db1.t,", false},
		{"DROP TABLE db1.t `sync`", false},
		{"DROP VIEW db1.t", false},
		{"DROP DICTIONARY db1.t", false},
		{"TRUNCATE TABLE db1.t", false},
		{"DROP TABLE db1.t; DROP TABLE other.u", false},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			got, err := PlainDropTable(e, c.sql)
			if err != nil {
				t.Fatalf("PlainDropTable: %v", err)
			}
			if got != c.want {
				t.Fatalf("PlainDropTable(%q) = %v, want %v", c.sql, got, c.want)
			}
		})
	}
}

func TestRewriteWriteTargets_multiDropRenamesEveryName(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("DROP TABLE IF EXISTS db1.t, other.u SYNC")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info, err := InspectWrite(ast)
	if err != nil {
		t.Fatalf("InspectWrite: %v", err)
	}
	if len(info.Slots) != 2 || info.Slots[0].Role != RoleDrop || info.Slots[1].Role != "drop#1" {
		t.Fatalf("Slots = %+v, want roles drop, drop#1", info.Slots)
	}
	out, err := RewriteWriteTargets(ast, func(s WriteSlot) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: s.Target.DB + "." + s.Target.Table}
	})
	if err != nil {
		t.Fatalf("RewriteWriteTargets: %v", err)
	}
	got, err := e.Generate(out)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if want := `DROP TABLE IF EXISTS phys."db1.t", phys."other.u" SYNC`; got != want {
		t.Fatalf("generated %q, want %q", got, want)
	}
}
