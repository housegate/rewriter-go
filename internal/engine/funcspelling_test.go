package engine

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestRestoreFunctionSpellings pins the SQL the generator prints for every
// restored call, and that the drop gate then passes it. Argument order is
// pinned exactly: the gate compares multisets of spellings and cannot see an
// argument swap. Equivalence with the input was measured on ClickHouse 26.8:
// formatQuery of each output equals formatQuery of its input.
func TestRestoreFunctionSpellings(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct{ sql, want string }{
		{"SELECT startsWith(name, 'patch-') FROM system.parts WHERE active AND startsWith(name, 'x')",
			"SELECT startsWith(name, 'patch-') FROM system.parts WHERE active AND startsWith(name, 'x')"},
		{"SELECT toTypeName(a) FROM db1.o", "SELECT toTypeName(a) FROM db1.o"},
		{"SELECT CHAR_LENGTH(s), CHAR_LENGTH(t) FROM db1.o", "SELECT CHAR_LENGTH(s), CHAR_LENGTH(t) FROM db1.o"},
		{"SELECT character_length(s) FROM db1.o", "SELECT character_length(s) FROM db1.o"},
		{"SELECT instr(s, 'x'), instr(s, 'y', 3) FROM db1.o", "SELECT instr(s, 'x'), instr(s, 'y', 3) FROM db1.o"},
		{"SELECT locate('x', s), locate('y', s, 3) FROM db1.o", "SELECT locate('x', s), locate('y', s, 3) FROM db1.o"},
		{"SELECT trim(s, 'x') FROM db1.o", "SELECT trim(s, 'x') FROM db1.o"},
		{"SELECT cume_dist() OVER (ORDER BY a) FROM db1.o", "SELECT cume_dist() OVER (ORDER BY a) FROM db1.o"},
		{"SELECT match(s, '^a') FROM db1.o", "SELECT match(s, '^a') FROM db1.o"},
		{"SELECT toStartOfDay(t) FROM db1.o", "SELECT toStartOfDay(t) FROM db1.o"},
		// Nested, and in a write.
		{"SELECT if(startsWith(lower(s), 'x'), toTypeName(a), 'n') FROM db1.o",
			"SELECT IF(startsWith(LOWER(s), 'x'), toTypeName(a), 'n') FROM db1.o"},
		{"DELETE FROM db1.o WHERE startsWith(s, 'x')", "DELETE FROM db1.o WHERE startsWith(s, 'x')"},
		{"CREATE TABLE db1.n (s String, k UInt8 DEFAULT startsWith(s, 'x')) ENGINE = Memory",
			"CREATE TABLE db1.n (s String, k UInt8 DEFAULT startsWith(s, 'x')) ENGINE=Memory"},
		// Spellings Polyglot already prints as written stay untouched.
		{"SELECT trim(s), TRIM(BOTH 'x' FROM s), length(s), position(s, 'x') FROM db1.o",
			"SELECT TRIM(s), TRIM(BOTH 'x' FROM s), LENGTH(s), POSITION(s, 'x') FROM db1.o"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			got, err := e.Generate(ast)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("generated %q, want %q", got, tc.want)
			}
			if err := CheckRegenerated(e, tc.sql, ast); err != nil {
				t.Fatalf("CheckRegenerated: %v", err)
			}
		})
	}
}

// TestRestoreFunctionSpellingsLeavesAmbiguousCalls pins the cases in which the
// spelling is not restored, so Polyglot's respelling stays and the drop gate
// refuses: two spellings of one kind (a quoted call name counts as one),
// and a spelling that is not ClickHouse's. Where Polyglot's spelling differs
// in case only (MATCH), the UnrestoredSpellingKey mark makes the gate refuse.
func TestRestoreFunctionSpellingsLeavesAmbiguousCalls(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct{ sql, gen string }{
		{"SELECT startsWith(s, 'x') OR STARTSWITH(s, 'y') FROM db1.o", "SELECT STARTS_WITH(s, 'x') OR STARTS_WITH(s, 'y') FROM db1.o"},
		{"SELECT starts_with(s, 'x') FROM db1.o", "SELECT STARTS_WITH(s, 'x') FROM db1.o"},
		{"SELECT instr(s, 'x'), locate('x', s) FROM db1.o", "SELECT POSITION(s, 'x'), POSITION(s, 'x') FROM db1.o"},
		{"SELECT CHAR_LENGTH(s), length(s) FROM db1.o", "SELECT LENGTH(s), LENGTH(s) FROM db1.o"},
		{"SELECT match(s, 'x'), MATCH(s, 'y') FROM db1.o", "SELECT MATCH(s, 'x'), MATCH(s, 'y') FROM db1.o"},
		{"SELECT `STARTSWITH`(a, 'x'), startsWith(b, 'y') FROM db1.o", "SELECT STARTS_WITH(a, 'x'), STARTS_WITH(b, 'y') FROM db1.o"},
		{"SELECT `MATCH`(a, 'x'), match(b, 'y') FROM db1.o", "SELECT MATCH(a, 'x'), MATCH(b, 'y') FROM db1.o"},
		{"SELECT group_concat(s, '-') FROM db1.o", "SELECT GROUP_CONCAT(CONCAT(s, '-')) FROM db1.o"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			got, err := e.Generate(ast)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.gen {
				t.Fatalf("generated %q, want %q", got, tc.gen)
			}
			// starts_with is no ClickHouse spelling: nothing is restored, and
			// the regeneration spells the input, so the gate passes it.
			if err := CheckRegenerated(e, tc.sql, ast); (err == nil) != (tc.sql == "SELECT starts_with(s, 'x') FROM db1.o") {
				t.Fatalf("CheckRegenerated = %v", err)
			}
		})
	}
}

type clickhouseFunction struct {
	caseInsensitive bool
	aliasTo         string
}

func loadClickHouseFunctions(t *testing.T) map[string]clickhouseFunction {
	t.Helper()
	f, err := os.Open("testdata/clickhouse_functions_26_8.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]clickhouseFunction{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 4 {
			t.Fatalf("bad line %q", line)
		}
		out[cols[0]] = clickhouseFunction{caseInsensitive: cols[2] == "1", aliasTo: cols[3]}
	}
	return out
}

// TestFunctionSpellingSweep calls every function of ClickHouse 26.8's
// system.functions with zero to three arguments and pins what the engine
// does with each call: a call the drop gate passes is printed under a name
// ClickHouse resolves to the same function (the same spelling, a case change
// of a case-insensitive name, or a measured alias), and the calls it refuses
// are exactly the respellings left unrestored, each of which ClickHouse
// rejects or which is not a ClickHouse function on every 26.x release. A
// Polyglot bump that respells another function fails here.
func TestFunctionSpellingSweep(t *testing.T) {
	e := newTestEngine(t)
	fns := loadClickHouseFunctions(t)
	resolves := func(in, out string) bool {
		if in == out {
			return true
		}
		if f, ok := fns[in]; ok && f.caseInsensitive && strings.EqualFold(in, out) {
			return true
		}
		// Measured aliases (system.functions alias_to on 26.2 and 26.8) and
		// the two calls ClickHouse's parser reads itself: TRIM(s) and NOT (x).
		switch in + "→" + out {
		case "ceiling→CEIL", "lcase→LOWER", "ucase→UPPER", "log→LN", "pow→POWER",
			"substr→SUBSTRING", "DATE_TRUNC→dateTrunc", "trim→TRIM", "not→NOT":
			return true
		}
		return false
	}
	// Refused: ClickHouse rejects the input (JSON_QUERY / JSON_VALUE need a
	// path; first_value / last_value / ntile take one argument), max_by /
	// min_by are no ClickHouse function on 26.2 (Polyglot prints argMax /
	// argMin), and group_concat(s, sep) is printed GROUP_CONCAT(CONCAT(…)).
	wantRefused := []string{
		"JSON_QUERY(a)", "JSON_VALUE(a)", "first_value(a, b)", "group_concat(a, b)", "group_concat(a, b, c)",
		"last_value(a, b)", "max_by()", "max_by(a)", "max_by(a, b)", "max_by(a, b, c)", "min_by()", "min_by(a)",
		"min_by(a, b)", "min_by(a, b, c)", "ntile(a, b)", "ntile(a, b, c)",
	}
	call := regexp.MustCompile(`^SELECT (NOT|\w+) ?\(`)
	var refused []string
	names := make([]string, 0, len(fns))
	for n := range fns {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, args := range []string{"", "a", "a, b", "a, b, c"} {
			sql := fmt.Sprintf("SELECT %s(%s)", name, args)
			ast, err := e.ParseOne(sql)
			if err != nil || CheckParsedInFull(e, sql, ast) != nil {
				continue // a syntax the engine refuses before generating
			}
			if CheckRegenerated(e, sql, ast) != nil {
				refused = append(refused, fmt.Sprintf("%s(%s)", name, args))
				continue
			}
			gen, err := e.Generate(ast)
			if err != nil {
				t.Fatalf("%s: generate: %v", sql, err)
			}
			m := call.FindStringSubmatch(gen)
			if m == nil {
				t.Errorf("%s: regenerated %q is not a call", sql, gen)
				continue
			}
			if !resolves(name, m[1]) {
				t.Errorf("%s: regenerated as %q, which ClickHouse does not resolve to %s", sql, gen, name)
			}
		}
	}
	if strings.Join(refused, "; ") != strings.Join(wantRefused, "; ") {
		t.Fatalf("refused calls:\n got %v\nwant %v", refused, wantRefused)
	}
}
