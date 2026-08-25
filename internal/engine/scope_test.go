package engine

import "testing"

func TestReferencesIdentifierInScope(t *testing.T) {
	e := newTestEngine(t)
	si := func(tt TableTarget) bool { return tt.DB == "db1" && tt.Table == "t" }
	for _, tc := range []struct {
		name string
		sql  string
		want bool
	}{
		{"bare ref in the si block", "SELECT _hg_row_id, a FROM db1.t", true},
		{"where ref in the si block", "SELECT a FROM db1.t WHERE _hg_row_id = 'x'", true},
		{"output alias in the si block", "SELECT 1 AS _hg_row_id FROM db1.t", true},
		{"join using in the si block", "SELECT * FROM db1.t AS a JOIN other.u AS b USING (_hg_row_id)", true},
		{"star except in the si block", "SELECT * EXCEPT (_hg_row_id) FROM db1.t", true},
		{"ordinary table in a nested block", "SELECT a FROM db1.t WHERE a IN (SELECT _hg_row_id FROM other.u)", false},
		{"si alias reached from a nested block", "SELECT a FROM db1.t AS s WHERE a IN (SELECT k FROM other.u WHERE k = s._hg_row_id)", true},
		{"ordinary alias in the si block", "SELECT o._hg_row_id FROM db1.t AS s JOIN other.u AS o ON 1", false},
		{"no si table at all", "SELECT _hg_row_id FROM other.u", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := ReferencesIdentifierInScope(ast, "_hg_row_id", si)
			if err != nil {
				t.Fatalf("ReferencesIdentifierInScope: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCollectNamespaceRefs_SkipsInScopeCTEAliases(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("WITH t AS (SELECT 1 AS id) SELECT a FROM other.u WHERE id IN t")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	refs, err := CollectNamespaceRefs(ast)
	if err != nil {
		t.Fatalf("CollectNamespaceRefs: %v", err)
	}
	for _, ref := range refs {
		if ref.Source == NamespaceRefInTable && ref.Target.DB == "" && ref.Target.Table == "t" {
			t.Fatalf("in-scope CTE alias must not be collected as a namespace ref: %+v", refs)
		}
	}

	ast, err = e.ParseOne("SELECT a FROM other.u WHERE id IN t")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	refs, err = CollectNamespaceRefs(ast)
	if err != nil {
		t.Fatalf("CollectNamespaceRefs: %v", err)
	}
	found := false
	for _, ref := range refs {
		if ref.Source == NamespaceRefInTable && ref.Target.Table == "t" {
			found = true
		}
	}
	if !found {
		t.Fatal("a bare IN target with no CTE in scope must still be collected")
	}
}
