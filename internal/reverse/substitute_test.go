package reverse

import "testing"

func TestSubstituteIdent(t *testing.T) {
	cases := []struct{ in, target, repl, want string }{
		{"Database phys1 does not exist", "phys1", "logical1", "Database logical1 does not exist"},
		{"Database `phys1` missing", "phys1", "logical1", "Database logical1 missing"},
		// boundary: must not match inside a larger identifier.
		{"phys1x and xphys1", "phys1", "L", "phys1x and xphys1"},
		// two adjacent occurrences separated by one boundary char: both fire.
		{"phys1 phys1", "phys1", "L", "L L"},
		// no match → unchanged.
		{"nothing here", "phys1", "L", "nothing here"},
	}
	for _, c := range cases {
		if got := substituteIdent(c.in, c.target, c.repl); got != c.want {
			t.Errorf("substituteIdent(%q,%q)=%q want %q", c.in, c.target, got, c.want)
		}
	}
}

func TestSubstituteQualified(t *testing.T) {
	cases := []struct{ in, qualified, repl, want string }{
		// dotted dynamic table → ClickHouse backticks the table half.
		{"Table phys1.`logical1.t` does not exist", "phys1.logical1.t", "logical1.t", "Table logical1.t does not exist"},
		// plain qualified, no dot in table → optional backticks.
		{"Table phys1.events gone", "phys1.events", "logical1.events", "Table logical1.events gone"},
		{"Table `phys1`.`events` gone", "phys1.events", "logical1.events", "Table logical1.events gone"},
		// no dot in the qualified key → falls back to ident match.
		{"db phys1 here", "phys1", "logical1", "db logical1 here"},
	}
	for _, c := range cases {
		if got := substituteQualified(c.in, c.qualified, c.repl); got != c.want {
			t.Errorf("substituteQualified(%q,%q)=%q want %q", c.in, c.qualified, got, c.want)
		}
	}
}

func TestFlexibleSQLReplace(t *testing.T) {
	// verbatim hit → first occurrence replaced.
	got := flexibleSQLReplace("In scope SELECT 1 FROM phys.t", "SELECT 1 FROM phys.t", "SELECT 1 FROM t")
	if got != "In scope SELECT 1 FROM t" {
		t.Errorf("verbatim: %q", got)
	}
	// whitespace-flexible: error collapsed newlines/spaces vs single-spaced rewritten.
	got = flexibleSQLReplace("err: SELECT   1\nFROM phys.t !", "SELECT 1 FROM phys.t", "SELECT 1 FROM t")
	if got != "err: SELECT 1 FROM t !" {
		t.Errorf("flexible: %q", got)
	}
	// no match → unchanged.
	if got := flexibleSQLReplace("unrelated", "SELECT 1", "x"); got != "unrelated" {
		t.Errorf("nomatch: %q", got)
	}
}
