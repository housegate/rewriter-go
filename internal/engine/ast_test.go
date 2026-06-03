package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNodeKindFromSnapshots(t *testing.T) {
	want := map[string]string{ // snapshot file -> top-level key polyglot emits
		"select":       NodeSelect,
		"select_join":  NodeSelect,
		"insert":       NodeInsert,
		"create_table": NodeCreateTable,
		"drop_table":   NodeDropTable,
		"alter_table":  NodeAlterTable,
		"use":          NodeCommand,
		"grant":        NodeCommand,
		"rename_table": NodeCommand,
		"show_tables":  NodeCommand,
		"show_create":  NodeCommand,
		"exists_table": NodeCommand,
	}
	for name, expect := range want {
		raw, err := os.ReadFile(filepath.Join("testdata", "ast-shapes", name+".json"))
		if err != nil {
			t.Fatalf("read snapshot %s: %v", name, err)
		}
		got, err := NodeKind(AST(raw))
		if err != nil {
			t.Fatalf("%s: NodeKind: %v", name, err)
		}
		if got != expect {
			t.Errorf("%s: NodeKind = %q, want %q", name, got, expect)
		}
	}
}

func TestCommandSQL(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "ast-shapes", "use.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := CommandSQL(AST(raw))
	if err != nil {
		t.Fatalf("CommandSQL: %v", err)
	}
	if got != "USE db" {
		t.Errorf("CommandSQL = %q, want %q", got, "USE db")
	}
}
