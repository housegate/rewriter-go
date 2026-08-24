package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestSICorpusContract is the Spec J D3 gate: the shared corpus must satisfy
// the frozen schema. It needs no engine and no oracle, so it runs in the
// pure-Go CI lane.
func TestSICorpusContract(t *testing.T) {
	cases := LoadSICorpus(t)
	if len(cases) == 0 {
		t.Fatal("corpus is empty; the shared behaviour contract cannot be empty")
	}
	if violations := ValidateSICorpus(cases); len(violations) > 0 {
		t.Fatalf("corpus contract violations (%d):\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

func TestSICorpusIsBytePinned(t *testing.T) {
	raw, err := os.ReadFile(SICorpusPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(raw); got != SICorpusBytes {
		t.Errorf("corpus size = %d bytes, want %d", got, SICorpusBytes)
	}
	if got := siCorpusFingerprint(raw); got != SICorpusFingerprint {
		t.Errorf("corpus fingerprint = %d, want %d\n"+
			"The shared corpus changed. Copy it to rewriter-grpc/tests/testdata/ and update the pinned\n"+
			"constants in BOTH internal/harness/sicorpus_test.go and rewriter-grpc/tests/si_corpus.h.",
			got, SICorpusFingerprint)
	}
	if got := len(LoadSICorpus(t)); got != SICorpusCases {
		t.Errorf("corpus case count = %d, want %d", got, SICorpusCases)
	}
}

func TestLoadSICorpus_RequiresExactlyOneJSONValue(t *testing.T) {
	valid := `[{"name":"one","sql":"SELECT 1","want_code":"Success","want_sql":"SELECT 1"}]`
	tests := []struct {
		name, contents string
		wantErr        string
	}{
		{"trailing whitespace", valid + "\n\t ", ""},
		{"trailing JSON value", valid + ` {"unexpected":true}`, "exactly one JSON value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := filepath.Join(t.TempDir(), "candidate.json")
			if err := os.WriteFile(candidate, []byte(tt.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(SICorpusPathEnv, candidate)
			cases, err := loadSICorpusFile(SICorpusPath())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("load candidate: %v", err)
				}
				if len(cases) != 1 || cases[0].Name != "one" {
					t.Fatalf("loaded cases = %#v, want one named case", cases)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateSICorpus_RejectsVacuousContains(t *testing.T) {
	got := ValidateSICorpus([]SICase{{
		Name:            "vacuous",
		SQL:             "SELECT a FROM other.u",
		WantCode:        "Success",
		WantSQL:         `SELECT a FROM phys."other.u"`,
		WantSQLContains: []string{"other.u"},
	}})
	if len(got) != 1 || !strings.Contains(got[0], "R4") {
		t.Fatalf("want one R4 violation, got %v", got)
	}
}

func TestValidateSICorpus_RejectsDivergenceWithoutPerEnginePins(t *testing.T) {
	got := ValidateSICorpus([]SICase{{
		Name:               "half-pinned",
		SQL:                "SELECT a FROM db1.t",
		WantCode:           "Success",
		AllowSQLDivergence: true,
		WantSQLGo:          `SELECT a FROM phys."db1.t"`,
	}})
	if len(got) != 1 || !strings.Contains(got[0], "want_sql_go and want_sql_cpp") {
		t.Fatalf("want one R3 violation, got %v", got)
	}
}

func TestValidateSICorpus_RejectsSuccessWithoutWantSQL(t *testing.T) {
	got := ValidateSICorpus([]SICase{{Name: "unpinned", SQL: "SELECT 1", WantCode: "Success"}})
	if len(got) != 1 || !strings.Contains(got[0], "must carry want_sql") {
		t.Fatalf("want one R3 violation, got %v", got)
	}
}

func TestValidateSICorpus_RejectsRejectWithoutMessage(t *testing.T) {
	got := ValidateSICorpus([]SICase{{
		Name: "silent-reject", SQL: "OPTIMIZE TABLE db1.t",
		WantCode: "UnsupportedStatement", Reject: true,
	}})
	if len(got) != 1 || !strings.Contains(got[0], "want_message_contains") {
		t.Fatalf("want one R3 violation, got %v", got)
	}
}

func TestValidateSICorpus_RequiresExplicitKnownWantCode(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		got := ValidateSICorpus([]SICase{{
			Name: "missing-code", SQL: "SELECT 1", WantSQL: "SELECT 1",
		}})
		if len(got) != 1 || !strings.Contains(got[0], "R6") || !strings.Contains(got[0], "non-empty") {
			t.Fatalf("want one missing-code R6 violation, got %v", got)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		got := ValidateSICorpus([]SICase{{
			Name: "unknown-code", SQL: "OPTIMIZE TABLE db1.t", WantCode: "NotARewriteCode",
			Reject: true, WantMessageContains: "unsupported",
		}})
		if len(got) != 1 || !strings.Contains(got[0], "R6") || !strings.Contains(got[0], "unknown want_code") {
			t.Fatalf("want one unknown-code R6 violation, got %v", got)
		}
	})
}

func TestValidateSICorpus_RejectForbidsSQLPinsAndDivergence(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*SICase)
		wantDetail string
	}{
		{"want_sql", func(c *SICase) { c.WantSQL = "OPTIMIZE TABLE db1.t" }, "must not pin SQL"},
		{"want_sql_go", func(c *SICase) { c.WantSQLGo = "OPTIMIZE TABLE db1.t" }, "must not pin SQL"},
		{"want_sql_cpp", func(c *SICase) { c.WantSQLCPP = "OPTIMIZE TABLE db1.t" }, "must not pin SQL"},
		{"want_sql_contains", func(c *SICase) { c.WantSQLContains = []string{"rewritten"} }, "must not pin SQL"},
		{"want_sql_not_contains", func(c *SICase) { c.WantSQLNotContains = []string{"forbidden"} }, "must not pin SQL"},
		{"allow_sql_divergence", func(c *SICase) { c.AllowSQLDivergence = true }, "forbids allow_sql_divergence"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := SICase{
				Name: "reject", SQL: "OPTIMIZE TABLE db1.t", WantCode: "UnsupportedStatement",
				Reject: true, WantMessageContains: "unsupported",
			}
			tt.mutate(&c)
			got := ValidateSICorpus([]SICase{c})
			if len(got) != 1 || !strings.Contains(got[0], "R3") || !strings.Contains(got[0], tt.wantDetail) {
				t.Fatalf("want one R3 %q violation, got %v", tt.wantDetail, got)
			}
		})
	}
}

func TestValidateSICorpus_RejectsCanonicalVacuousContains(t *testing.T) {
	tests := []struct {
		name, sql, contains string
	}{
		{"raw backtick containment", "SELECT * FROM `db1.t`", "`db1.t`"},
		{"canonical backtick equivalence", `SELECT * FROM "db1.t"`, "`db1.t`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateSICorpus([]SICase{{
				Name: "vacuous", SQL: tt.sql, WantCode: "Success",
				WantSQL: `SELECT * FROM phys."db1.t"`, WantSQLContains: []string{tt.contains},
			}})
			if len(got) != 1 || !strings.Contains(got[0], "R4") {
				t.Fatalf("want one R4 violation, got %v", got)
			}
		})
	}
}

func TestLegacyCoverageReport_SyntheticAlwaysRuns(t *testing.T) {
	raw := []byte(`[
		{"name":"raw-vacuous","sql":"SELECT * FROM ` + "`db.t`" + `","want_sql":"SELECT * FROM phys.db_t","want_sql_contains":["` + "`db.t`" + `"]},
		{"name":"canonical-vacuous","sql":"SELECT * FROM \"db.t\"","sql_exact":true,"want_sql_contains":["` + "`db.t`" + `"]},
		{"name":"non-vacuous","sql":"SELECT * FROM db.t","sql_exact":true,"want_sql":"SELECT * FROM phys.db_t","want_sql_contains":["phys.db_t"]},
		{"name":"reject-is-not-vacuous","sql":"OPTIMIZE TABLE db.t","reject":true,"want_sql_contains":["db.t"]},
		{"name":"dead-only","sql":"SELECT 1","want_sql":"SELECT 1"},
		{"name":"exact-is-not-dead","sql":"SELECT 2","sql_exact":true,"want_sql":"SELECT 2"}
	]`)
	vacuous, dead, err := LegacyCoverageReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"raw-vacuous", "canonical-vacuous"}; !reflect.DeepEqual(vacuous, want) {
		t.Errorf("vacuous = %v, want %v", vacuous, want)
	}
	if want := []string{"raw-vacuous", "dead-only"}; !reflect.DeepEqual(dead, want) {
		t.Errorf("dead want_sql = %v, want %v", dead, want)
	}
}

// TestSICorpusLegacyCoverageReport pins Spec J §5's acceptance sentence:
// "Running it against the pre-fix corpus must report the 7 vacuous cases and
// the 12 unasserted want_sql's." The pre-fix corpus is recovered from git at
// the spec's baseline commit so this stays reproducible without checking a
// second 145 KB file into the repo.
func TestSICorpusLegacyCoverageReport(t *testing.T) {
	const baseline = "dbac7bc"
	raw, err := exec.Command("git", "show",
		baseline+":internal/harness/testdata/storage_integrity_cases.json").Output()
	if err != nil {
		t.Skipf("pre-fix corpus unavailable at %s: %v", baseline, err)
	}
	vacuous, dead, err := LegacyCoverageReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	wantVacuous := []string{
		"si_insert_rewrites_like_today",
		"non_si_table_unaffected",
		"si_ordinary_callable_in_values_allowed",
		"si_ordinary_callable_in_ignore_set_values_allowed",
		"si_ordinary_in_table_allowed",
		"si_ordinary_local_catalog_function_allowed",
		"si_ordinary_remote_engine_allowed",
	}
	wantDead := []string{
		"si_safe_plain_select",
		"si_safe_alias_join_non_si",
		"si_unsafe_latest_no_excluded",
		"si_unsafe_latest_two_excluded",
		"si_safe_subquery_in_where",
		"si_reserved_keyword_quoted",
		"si_star_hides_rid",
		"si_use_default_database",
		"si_exists_table_safe",
		"si_insert_rewrites_like_today",
		"non_si_table_unaffected",
		"si_absent_args_ordinary_rewrite",
	}
	if !reflect.DeepEqual(vacuous, wantVacuous) {
		t.Errorf("vacuous want_sql_contains cases = %v, want exact ordered %v", vacuous, wantVacuous)
	}
	if !reflect.DeepEqual(dead, wantDead) {
		t.Errorf("want_sql cases dead in the pre-fix C++ runner = %v, want exact ordered %v", dead, wantDead)
	}
	t.Logf("pre-fix vacuous: %v", vacuous)
	t.Logf("pre-fix dead want_sql: %v", dead)
}
