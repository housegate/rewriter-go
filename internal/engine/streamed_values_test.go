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

// TestInlineFormatValues: an INSERT … FORMAT Values <rows> parses into the
// VALUES form's AST, is found by its FORMAT keyword, and passes both gates
// unless a comment follows the format name (housegate/rewriter-go#75).
func TestInlineFormatValues(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql    string
		format int // index of the FORMAT token, -1 = not the inline form
		full   bool
	}{
		{"INSERT INTO db1.o FORMAT Values (1)", 5, true},
		{"INSERT INTO db1.o FORMAT values\n(1), (2);", 5, true},
		{"INSERT INTO db1.o (a, b) FORMAT VALUES (1, 'x')", 10, true},
		{"INSERT INTO db1.o SETTINGS max_threads = 1 FORMAT Values (1)", 9, true},
		{"INSERT INTO db1.o FORMAT Values ('-- x', '/* y */')", 5, true},
		{"INSERT INTO db1.o FORMAT Values (1) -- c", 5, false},
		{"INSERT INTO db1.o FORMAT Values /* c */ (1)", 5, false},
		{"INSERT INTO db1.o FORMAT Values (1 /* c */)", 5, false},
		{"INSERT INTO db1.o FORMAT Values (1);\n-- c", 5, false},
		// The rows start at the last VALUES token outside brackets, so a
		// table named values does not hide the format name.
		{"INSERT INTO db1.values FORMAT Values (1)", 5, true},
		{"INSERT INTO db1.values FORMAT Values (1) -- c", 5, false},
		{"INSERT INTO db1.o VALUES (1)", -1, true},
		{"INSERT INTO db1.o (format, values) VALUES (1, 2)", -1, true},
		{"INSERT INTO db1.o FORMAT Values", -1, true},
		{"INSERT INTO db1.o FORMAT CSV 1,2", -1, true},
		{"INSERT INTO db1.o SELECT 1 FORMAT Values", -1, true},
	} {
		ast, err := e.ParseOne(tc.sql)
		if err != nil {
			t.Fatalf("ParseOne(%q): %v", tc.sql, err)
		}
		toks, err := tokenizeRaw(e, tc.sql)
		if err != nil {
			t.Fatalf("tokenizeRaw(%q): %v", tc.sql, err)
		}
		if got := inlineFormatValues(ast, toks); got != tc.format {
			t.Errorf("inlineFormatValues(%q) = %d, want %d", tc.sql, got, tc.format)
			continue
		}
		err = CheckParsedInFull(e, tc.sql, ast)
		if tc.full {
			if err != nil {
				t.Errorf("CheckParsedInFull(%q): %v", tc.sql, err)
			}
		} else if !errors.Is(err, ErrNotParsedInFull) {
			t.Errorf("CheckParsedInFull(%q) = %v, want ErrNotParsedInFull", tc.sql, err)
		}
		if tc.format >= 0 && tc.full {
			if err := CheckRegenerated(e, tc.sql, ast); err != nil {
				t.Errorf("CheckRegenerated(%q): %v", tc.sql, err)
			}
		}
	}
}

// TestPlainValuesComments: the inline rows of a plain INSERT … VALUES are
// Values data to ClickHouse from the first row's '(' on, so only whitespace may
// separate their tokens; a comment before the first row is skipped by
// ClickHouse's lexer and stays accepted (housegate/rewriter-go#77).
func TestPlainValuesComments(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql  string
		rows int // index of the first row's '(' token, -1 = not plain VALUES rows
		full bool
	}{
		{"INSERT INTO db1.o VALUES (1)", 6, true},
		{"INSERT INTO db1.o VALUES (1), (2);", 6, true},
		{"INSERT INTO db1.o VALUES (1)\t\n", 6, true},
		{"INSERT INTO db1.o VALUES ('-- x', '/* y */', '# z')", 6, true},
		{"INSERT INTO db1.o VALUES /* c */ (1)", 6, true},
		{"INSERT INTO db1.o VALUES -- c\n(1)", 6, true},
		{"INSERT INTO db1.o /* c */ VALUES (1)", 6, true},
		{"INSERT INTO db1.o (a /* c */) VALUES (1)", 9, true},
		{"INSERT INTO db1.o (format, values) VALUES (1, 2)", 11, true},
		{"INSERT INTO db1.values VALUES (1)", 6, true},
		{"INSERT INTO values (a) VALUES (1)", 7, true},
		// An INSERT whose column list is followed by SETTINGS keeps the rest
		// as opaque text, checked from SETTINGS on (opaqueInsertTextStart,
		// TestOpaqueInsertTextComments).
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES (1)", -1, true},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 FORMAT Values (1)", -1, true},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES (1 /* c */)", -1, false},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 FORMAT Values (1) -- c", -1, false},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 FORMAT Values /* c */ (1)", -1, false},
		{"INSERT INTO db1.o VALUES (1) -- c", 6, false},
		{"INSERT INTO db1.o VALUES (1) /* c */", 6, false},
		{"INSERT INTO db1.o VALUES (1) # c", 6, false},
		{"INSERT INTO db1.o VALUES (1)\n-- c", 6, false},
		{"INSERT INTO db1.o VALUES (1 /* c */)", 6, false},
		{"INSERT INTO db1.o VALUES (1 -- c\n)", 6, false},
		{"INSERT INTO db1.o VALUES (/* c */ 1)", 6, false},
		{"INSERT INTO db1.o VALUES (1), /* c */ (2)", 6, false},
		{"INSERT INTO db1.o VALUES (1) /* c */ ;", 6, false},
		{"INSERT INTO db1.o VALUES (1); -- c", 6, false},
		{"INSERT INTO db1.o VALUES (1); /* c */", 6, false},
		{"INSERT INTO db1.values VALUES (1) -- c", 6, false},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES (1) -- c", -1, false},
		{"INSERT INTO db1.o SETTINGS max_threads = 1 VALUES (1) -- c", 10, false},
		{"INSERT INTO db1.o SETTINGS max_threads = 1 VALUES (1)", 10, true},
		{"INSERT INTO db1.o FORMAT Values (1)", -1, true},
		{"INSERT INTO db1.o SELECT 1 -- c", -1, true},
		{"INSERT INTO db1.o FORMAT CSV 1,2", -1, true},
	} {
		ast, err := e.ParseOne(tc.sql)
		if err != nil {
			t.Fatalf("ParseOne(%q): %v", tc.sql, err)
		}
		toks, err := tokenizeRaw(e, tc.sql)
		if err != nil {
			t.Fatalf("tokenizeRaw(%q): %v", tc.sql, err)
		}
		if got := plainValuesRows(ast, toks); got != tc.rows {
			t.Errorf("plainValuesRows(%q) = %d, want %d", tc.sql, got, tc.rows)
			continue
		}
		err = CheckParsedInFull(e, tc.sql, ast)
		if tc.full {
			if err != nil {
				t.Errorf("CheckParsedInFull(%q): %v", tc.sql, err)
			}
		} else if !errors.Is(err, ErrNotParsedInFull) {
			t.Errorf("CheckParsedInFull(%q) = %v, want ErrNotParsedInFull", tc.sql, err)
		}
	}
}

// TestOpaqueInsertTextComments: the text of an INSERT whose column list is
// followed by SETTINGS is opaque to Polyglot and checked from that keyword on;
// only whitespace may separate its tokens, as rewriter-grpc refuses any comment
// in it in dynamic mode (housegate/rewriter-go#84).
func TestOpaqueInsertTextComments(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql   string
		start int // byte offset of the SETTINGS keyword, -1 = not opaque
		full  bool
	}{
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES (1)", 22, true},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT 1", 22, true},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 FORMAT CSV", 22, true},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 FORMAT CSV\n1,2", 22, true},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES ('-- x', '/* y */')", 22, true},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = '/* x */' VALUES (1)", 22, true},
		{"INSERT INTO db1.o (a) /* c */ SETTINGS max_threads = 1 VALUES (1)", 30, true},
		{"INSERT INTO db1.settings (a) SETTINGS max_threads = 1 VALUES (1)", 29, true},
		{"INSERT INTO db1.o (a) SETTINGS /* c */ max_threads = 1 VALUES (1)", 22, false},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 /* c */ VALUES (1)", 22, false},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 -- c\nVALUES (1)", 22, false},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 # c\nVALUES (1)", 22, false},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES /* c */ (1)", 22, false},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES (1) -- c", 22, false},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT 1 -- c", 22, false},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT /* c */ 1", 22, false},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 /* c */ FORMAT CSV", 22, false},
		// The data after another format name is not SQL and is not read.
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 FORMAT CSV 1 -- x", 22, true},
		{"INSERT INTO db1.o SETTINGS max_threads = 1 VALUES (1)", -1, true},
		{"INSERT INTO db1.o (a) VALUES (1)", -1, true},
		{"INSERT INTO db1.o (a) SELECT 1 SETTINGS max_threads = 1", -1, true},
	} {
		ast, err := e.ParseOne(tc.sql)
		if err != nil {
			t.Fatalf("ParseOne(%q): %v", tc.sql, err)
		}
		toks, err := tokenizeRaw(e, tc.sql)
		if err != nil {
			t.Fatalf("tokenizeRaw(%q): %v", tc.sql, err)
		}
		start, _, ok := opaqueInsertTextStart(tc.sql, ast, toks)
		if !ok {
			start = -1
		}
		if start != tc.start {
			t.Errorf("opaqueInsertTextStart(%q) = %d, want %d", tc.sql, start, tc.start)
			continue
		}
		err = CheckParsedInFull(e, tc.sql, ast)
		if tc.full {
			if err != nil {
				t.Errorf("CheckParsedInFull(%q): %v", tc.sql, err)
			}
		} else if !errors.Is(err, ErrNotParsedInFull) {
			t.Errorf("CheckParsedInFull(%q) = %v, want ErrNotParsedInFull", tc.sql, err)
		}
	}
}
