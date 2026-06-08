package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func load(t *testing.T, name string) AST {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ast-shapes", name+".json"))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return AST(b)
}

func TestCollectSelectTables_simpleQualified(t *testing.T) {
	got, err := CollectSelectTables(load(t, "select"))
	if err != nil {
		t.Fatal(err)
	}
	want := []TableTarget{{DB: "db", Table: "t"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestCollectSelectTables_joinAndSubquery(t *testing.T) {
	got, err := CollectSelectTables(load(t, "select_subquery_from"))
	if err != nil {
		t.Fatal(err)
	}
	want := []TableTarget{{DB: "db", Table: "t"}} // recurses into the FROM subquery
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestCollectSelectTables_cteAliasSkipped(t *testing.T) {
	// WITH c AS (SELECT * FROM db.t) SELECT * FROM c JOIN db.u ON ...
	// `c` is a CTE alias → skipped; db.t (CTE body) and db.u (join) are real.
	got, err := CollectSelectTables(load(t, "select_cte_join"))
	if err != nil {
		t.Fatal(err)
	}
	want := []TableTarget{{DB: "db", Table: "t"}, {DB: "db", Table: "u"}}
	// order: set-compare; map iteration is non-deterministic
	sortTargets(got)
	sortTargets(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestCollectSelectTables_columnQualifierNotATable(t *testing.T) {
	// ON-clause column qualifiers like `a.x = b.x` must NOT produce phantom
	// TableTargets — they share the "table" JSON key with real table descriptors
	// but their qualifier name is a flat string, caught by the tt.Table=="" guard.
	got, err := CollectSelectTables(load(t, "select_three_join"))
	if err != nil {
		t.Fatal(err)
	}
	want := []TableTarget{{Table: "a"}, {Table: "b"}, {Table: "c"}}
	sortTargets(got)
	sortTargets(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func genOf(t *testing.T, ast AST) string {
	t.Helper()
	e, err := NewPolyglot("")
	if err != nil {
		t.Skipf("engine unavailable: %v", err)
	}
	defer e.Close()
	out, err := e.Generate(ast)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return out
}

func TestRewriteSelectTables_renameAndSetDB(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	out, err := RewriteSelectTables(load(t, "select"), func(tt TableTarget) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "phys", NewTable: "t_x"}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := genOf(t, out)
	t.Logf("RENAME got: %q", got)
	want := "SELECT a FROM phys.t_x \"db.t\" WHERE x IN (1, 2)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRewriteSelectTables_remote(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	out, err := RewriteSelectTables(load(t, "select"), func(tt TableTarget) TableDecision {
		return TableDecision{Action: ActionRemote, Remote: &RemoteSpec{
			Addr: "h:9000", DB: "phys", Table: "t_x", User: "u", Password: "p",
		}}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := genOf(t, out)
	t.Logf("REMOTE got: %q", got)
	want := "SELECT a FROM remote('h:9000', phys, t_x, 'u', 'p') AS \"db.t\" WHERE x IN (1, 2)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRewriteSelectTables_remoteWithAlias(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := NewPolyglot("")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer e.Close()

	// Parse a source that carries an alias on the table reference.
	// Characterization confirmed: polyglot parses the alias onto tbl["alias"],
	// so decodeTableTarget returns tt.Alias=="x" correctly.
	src, err := e.ParseOne("SELECT * FROM db.t AS x")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	out, err := RewriteSelectTables(src, func(tt TableTarget) TableDecision {
		return TableDecision{Action: ActionRemote, Remote: &RemoteSpec{
			Addr: "h:9000", DB: "phys", Table: "t_x", User: "u", Password: "p",
		}}
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := e.Generate(out)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	t.Logf("REMOTE_WITH_ALIAS got: %q", got)

	// The alias wrapper node causes polyglot to render `remote(...) AS x`.
	want := "SELECT * FROM remote('h:9000', phys, t_x, 'u', 'p') AS x"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRewriteSelectTables_dottedNameIsQuoted(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	out, err := RewriteSelectTables(load(t, "select"), func(tt TableTarget) TableDecision {
		return TableDecision{Action: ActionRename, NewDB: "testnet", NewTable: "tenant1.events"}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := genOf(t, out)
	// Re-parse the output and confirm it is db=testnet, table=tenant1.events
	// (a single quoted identifier), NOT a 3-part name. Style (quotes) is irrelevant.
	e, _ := NewPolyglot("")
	defer e.Close()
	reparsed, _ := e.ParseOne(got)
	refs, _ := CollectSelectTables(reparsed)
	if len(refs) != 1 || refs[0].DB != "testnet" || refs[0].Table != "tenant1.events" {
		t.Fatalf("dotted name not preserved as single identifier; got SQL %q -> refs %+v", got, refs)
	}
}

func TestLimitOps(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	if v, ok, _ := GetLimit(load(t, "select_limit")); !ok || v != 10 {
		t.Fatalf("GetLimit = %d,%v want 10,true", v, ok)
	}
	out, err := SetLimit(load(t, "select"), 5) // `select` golden has no LIMIT
	if err != nil {
		t.Fatal(err)
	}
	wantLimit := "SELECT a FROM db.t WHERE x IN (1, 2) LIMIT 5"
	if got := genOf(t, out); got != wantLimit {
		t.Fatalf("SetLimit got %q want %q", got, wantLimit)
	}
	off, err := SetOffset(out, 3)
	if err != nil {
		t.Fatal(err)
	}
	wantOffset := "SELECT a FROM db.t WHERE x IN (1, 2) LIMIT 5 OFFSET 3"
	if got := genOf(t, off); got != wantOffset {
		t.Fatalf("SetOffset got %q want %q", got, wantOffset)
	}
}

func TestInjectCTEs(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := NewPolyglot("")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	defer e.Close()
	body, err := e.ParseOne("SELECT * FROM db.src")
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	out, err := InjectCTEs(load(t, "select"), map[string]AST{"c": body})
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Generate(out)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	t.Logf("InjectCTEs got: %q", got)
	want := "WITH c AS (SELECT * FROM db.src) SELECT a FROM db.t WHERE x IN (1, 2)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// sortTargets sorts a TableTarget slice by DB+Table+Alias for stable comparison.
func sortTargets(s []TableTarget) {
	sort.Slice(s, func(i, j int) bool {
		ki := fmt.Sprintf("%s\x00%s\x00%s", s[i].DB, s[i].Table, s[i].Alias)
		kj := fmt.Sprintf("%s\x00%s\x00%s", s[j].DB, s[j].Table, s[j].Alias)
		return ki < kj
	})
}
