package engine

import (
	"os"
	"strings"
	"testing"
)

func newTestEngine(t *testing.T) Engine {
	t.Helper()
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("POLYGLOT_SQL_FFI_PATH not set; run via `make test`")
	}
	e, err := NewPolyglot("")
	if err != nil {
		t.Fatalf("NewPolyglot: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func TestPolyglotParseGenerateRoundTrip(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("SELECT a FROM db.t")
	if err != nil {
		t.Fatalf("ParseOne: %v", err)
	}
	sql, err := e.Generate(ast)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if sql == "" {
		t.Fatal("Generate returned empty SQL")
	}
	t.Logf("round-tripped: %s", sql)
}

func TestPolyglotRenameTables(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("SELECT a FROM old_table")
	if err != nil {
		t.Fatalf("ParseOne: %v", err)
	}
	renamed, err := e.RenameTables(ast, map[string]string{"old_table": "new_table"})
	if err != nil {
		t.Fatalf("RenameTables: %v", err)
	}
	sql, err := e.Generate(renamed)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Logf("rename result: %s", sql)
	if !strings.Contains(sql, "new_table") {
		t.Fatalf("expected new_table in %q", sql)
	}
}

func TestPolyglotTokenizeAndDiff(t *testing.T) {
	e := newTestEngine(t)
	if _, err := e.Tokenize("SELECT 1"); err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	if _, err := e.DiffSQL("SELECT a FROM t", "SELECT b FROM t"); err != nil {
		t.Fatalf("DiffSQL: %v", err)
	}
}
