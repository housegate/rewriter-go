package engine

import (
	"errors"
	"strings"
	"testing"
)

func TestStreamedValuesSQL(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql, standIn, spelling string
		ok                     bool
	}{
		{"INSERT INTO db1.o FORMAT Values", "INSERT INTO db1.o FORMAT Native", "Values", true},
		{"INSERT INTO db1.o (a, b) FORMAT values\n", "INSERT INTO db1.o (a, b) FORMAT Native\n", "values", true},
		{"INSERT INTO db1.o FORMAT VALUES -- c", "INSERT INTO db1.o FORMAT Native -- c", "VALUES", true},
		{"INSERT INTO db1.o FORMAT Values (1)", "", "", false},
		{"INSERT INTO db1.o VALUES", "", "", false},
		{"INSERT INTO db1.o FORMAT `Values`", "", "", false},
		{"INSERT INTO db1.o FORMAT CSV", "", "", false},
		{"SELECT 1 FORMAT Values", "", "", false},
	} {
		standIn, spelling, ok := streamedValuesSQL(e, tc.sql)
		if ok != tc.ok || standIn != tc.standIn || spelling != tc.spelling {
			t.Errorf("streamedValuesSQL(%q) = %q, %q, %v; want %q, %q, %v",
				tc.sql, standIn, spelling, ok, tc.standIn, tc.spelling, tc.ok)
		}
	}
}

// TestParseOneStreamedFormatValues: the streamed form parses to the same
// FORMAT data clause as any other streamed format, naming the client's format,
// and regenerates to the input; text after the name fails the whole-statement
// gate, since ClickHouse would read it as Values data.
func TestParseOneStreamedFormatValues(t *testing.T) {
	e := newTestEngine(t)
	for _, sql := range []string{
		"INSERT INTO db1.o FORMAT Values",
		"INSERT INTO db1.o (a, b) FORMAT values",
		"INSERT INTO db1.o SETTINGS max_threads = 1 FORMAT VALUES",
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("ParseOne(%q): %v", sql, err)
		}
		if !insertHasFormatClause(ast) {
			t.Fatalf("ParseOne(%q): no FORMAT data clause in %s", sql, ast)
		}
		if err := CheckParsedInFull(e, sql, ast); err != nil {
			t.Fatalf("CheckParsedInFull(%q): %v", sql, err)
		}
		out, err := GenerateInsert(e, sql, ast)
		if err != nil {
			t.Fatalf("GenerateInsert(%q): %v", sql, err)
		}
		if out != sql {
			t.Fatalf("GenerateInsert(%q) = %q", sql, out)
		}
		if err := CheckRegenerated(e, sql, ast); err != nil {
			t.Fatalf("CheckRegenerated(%q): %v", sql, err)
		}
	}
	for _, sql := range []string{
		"INSERT INTO db1.o FORMAT Values -- c",
		"INSERT INTO db1.o FORMAT Values /* c */",
		"INSERT INTO db1.o FORMAT Values\n-- c",
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("ParseOne(%q): %v", sql, err)
		}
		err = CheckParsedInFull(e, sql, ast)
		if !errors.Is(err, ErrNotParsedInFull) {
			t.Fatalf("CheckParsedInFull(%q) = %v, want ErrNotParsedInFull", sql, err)
		}
	}
	if _, err := e.ParseOne("INSERT INTO db1.o FORMAT Values;"); err == nil ||
		!strings.Contains(err.Error(), "Expected LParen") {
		t.Fatalf("ParseOne(FORMAT Values;) = %v, want Polyglot's own parse error", err)
	}
}

func TestDiffSQLStreamedFormatValues(t *testing.T) {
	e := newTestEngine(t)
	raw, err := e.DiffSQL(`INSERT INTO phys."db1.o" FORMAT Values`, "INSERT INTO phys.`db1.o` FORMAT Values")
	if err != nil {
		t.Fatalf("DiffSQL: %v", err)
	}
	if s := strings.TrimSpace(string(raw)); s != "[]" && s != "" {
		t.Fatalf("DiffSQL = %s, want no changes", s)
	}
	if _, err := e.DiffSQL("INSERT INTO t FORMAT Values", "INSERT INTO t FORMAT values"); err == nil {
		t.Fatal("DiffSQL of differently spelt format names: want the parse error")
	}
}
