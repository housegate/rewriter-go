package engine

import (
	"reflect"
	"strings"
	"testing"
)

type mutationGenerateOverrideEngine struct {
	Engine
	generated string
}

func (e mutationGenerateOverrideEngine) Generate(AST) (string, error) {
	return e.generated, nil
}

func TestCollectMutationReadSurface_StructuredGrammarOrder(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		name            string
		sql             string
		wantAssignments []TableTarget
		wantPredicate   []TableTarget
	}{
		{
			name: "update assignments before predicate",
			sql: "UPDATE other.u " +
				"SET x = (SELECT count() FROM db1.t), y = (SELECT count() FROM other.assign_two) " +
				"WHERE id IN (SELECT id FROM hg_safe.db1__t)",
			wantAssignments: []TableTarget{{DB: "db1", Table: "t"}, {DB: "other", Table: "assign_two"}},
			wantPredicate:   []TableTarget{{DB: "hg_safe", Table: "db1__t"}},
		},
		{
			name:          "delete predicate only",
			sql:           "DELETE FROM other.u WHERE id IN (SELECT id FROM hg_unsafe.db1__t)",
			wantPredicate: []TableTarget{{DB: "hg_unsafe", Table: "db1__t"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatalf("ParseOne: %v", err)
			}
			got, err := CollectMutationReadSurface(e, ast, tc.sql)
			if err != nil {
				t.Fatalf("CollectMutationReadSurface: %v", err)
			}
			if !reflect.DeepEqual(got.Assignments.Tables, tc.wantAssignments) {
				t.Fatalf("assignments = %+v, want %+v", got.Assignments.Tables, tc.wantAssignments)
			}
			if !reflect.DeepEqual(got.Predicate.Tables, tc.wantPredicate) {
				t.Fatalf("predicate = %+v, want %+v", got.Predicate.Tables, tc.wantPredicate)
			}
		})
	}
}

func TestCollectMutationReadSurface_AlterSentinelAdapters(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		name            string
		sql             string
		wantAssignments []TableTarget
		wantPredicate   []TableTarget
	}{
		{
			name:            "alter update assignment",
			sql:             "ALTER TABLE other.u UPDATE x = (SELECT count() FROM db1.t) WHERE id = 1",
			wantAssignments: []TableTarget{{DB: "db1", Table: "t"}},
		},
		{
			name:          "alter update predicate",
			sql:           "ALTER TABLE other.u UPDATE x = 1 WHERE id IN (SELECT id FROM hg_safe.db1__t)",
			wantPredicate: []TableTarget{{DB: "hg_safe", Table: "db1__t"}},
		},
		{
			name:          "alter delete predicate",
			sql:           "ALTER TABLE other.u DELETE WHERE id IN (SELECT id FROM db1.t)",
			wantPredicate: []TableTarget{{DB: "db1", Table: "t"}},
		},
		{
			name:          "alter modifiers stay outside the probe",
			sql:           "ALTER TABLE IF EXISTS other.u ON CLUSTER cluster_a DELETE WHERE id IN (SELECT id FROM db1.t)",
			wantPredicate: []TableTarget{{DB: "db1", Table: "t"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatalf("ParseOne: %v", err)
			}
			got, err := CollectMutationReadSurface(e, ast, tc.sql)
			if err != nil {
				t.Fatalf("CollectMutationReadSurface: %v", err)
			}
			if !reflect.DeepEqual(got.Assignments.Tables, tc.wantAssignments) {
				t.Fatalf("assignments = %+v, want %+v", got.Assignments.Tables, tc.wantAssignments)
			}
			if !reflect.DeepEqual(got.Predicate.Tables, tc.wantPredicate) {
				t.Fatalf("predicate = %+v, want %+v", got.Predicate.Tables, tc.wantPredicate)
			}
		})
	}
}

func TestCollectMutationReadSurface_AlterProbeRejectsTruncation(t *testing.T) {
	e := newTestEngine(t)
	sql := "ALTER TABLE other.u UPDATE x = (SELECT count() FROM db1.t) WHERE id = 1 GARBAGE"
	ast, err := e.ParseOne(sql)
	if err != nil {
		t.Fatalf("outer opaque statement must parse for the adaptation test: %v", err)
	}
	if _, err := CollectMutationReadSurface(e, ast, sql); err == nil {
		t.Fatal("CollectMutationReadSurface error = nil, want an unproven/truncated probe error")
	}
}

func TestCollectMutationReadSurface_NonMutationDoesNotProbe(t *testing.T) {
	e := newTestEngine(t)
	for _, sql := range []string{
		"ALTER TABLE other.u ADD COLUMN x UInt64",
		"INSERT INTO other.u SELECT * FROM db1.t",
		"SELECT * FROM db1.t",
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("ParseOne(%q): %v", sql, err)
		}
		got, err := CollectMutationReadSurface(e, ast, sql)
		if err != nil {
			t.Fatalf("CollectMutationReadSurface(%q): %v", sql, err)
		}
		if len(got.Assignments.Tables)+len(got.Predicate.Tables)+
			len(got.Assignments.Namespaces)+len(got.Predicate.Namespaces) != 0 {
			t.Fatalf("CollectMutationReadSurface(%q) = %+v, want empty", sql, got)
		}
	}
}

func TestMutationProbeRoundTripsExactly_DetectsDroppedSuffix(t *testing.T) {
	e := newTestEngine(t)
	probe := "UPDATE __hg_si_probe SET x = 1 WHERE id = 1 GARBAGE"
	ast, err := e.ParseOne(probe)
	if err != nil {
		t.Fatalf("ParseOne: %v", err)
	}
	if err := mutationProbeRoundTripsExactly(e, probe, ast); err == nil {
		t.Fatal("mutationProbeRoundTripsExactly error = nil, want dropped-suffix detection")
	}
}

func TestMutationProbeRoundTripsExactly_DetectsIdentifierCaseDrift(t *testing.T) {
	e := newTestEngine(t)
	probe := "UPDATE __hg_si_probe SET mixedCase = 1 WHERE id = 1"
	ast, err := e.ParseOne(probe)
	if err != nil {
		t.Fatalf("ParseOne: %v", err)
	}
	changed := strings.Replace(probe, "mixedCase", "mixedcase", 1)
	if err := mutationProbeRoundTripsExactly(
		mutationGenerateOverrideEngine{Engine: e, generated: changed}, probe, ast,
	); err == nil {
		t.Fatal("mutationProbeRoundTripsExactly error = nil, want identifier-case drift detection")
	}
}
