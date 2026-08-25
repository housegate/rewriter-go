package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	want := "SELECT a FROM remote('h:9000', 'phys', 't_x', 'u', 'p') AS \"db.t\" WHERE x IN (1, 2)"
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
	want := "SELECT * FROM remote('h:9000', 'phys', 't_x', 'u', 'p') AS x"
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

func TestRewriteSelectTables_subquerySubstitution(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := NewPolyglot("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	body, err := e.ParseOne(`SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db__t`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := RewriteSelectTables(load(t, "select"), func(tt TableTarget) TableDecision {
		return TableDecision{Action: ActionSubquery, Subquery: body}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := genOf(t, out)
	want := `SELECT a FROM (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db__t) AS "db.t" WHERE x IN (1, 2)`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRewriteSelectTables_subqueryKeepsUserAliasInJoin(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := NewPolyglot("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ast, err := e.ParseOne(`SELECT count() FROM db.t AS a JOIN db.u AS b ON a.id = b.id`)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := e.ParseOne(`SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db__t`)
	out, err := RewriteSelectTables(ast, func(tt TableTarget) TableDecision {
		if tt.Table == "t" {
			return TableDecision{Action: ActionSubquery, Subquery: body}
		}
		return TableDecision{Action: ActionSkip}
	})
	if err != nil {
		t.Fatal(err)
	}
	got := genOf(t, out)
	want := `SELECT count() FROM (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db__t) AS a JOIN db.u AS b ON a.id = b.id`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// The substituted body's own table must NOT be re-visited/collected.
	tabs, err := CollectSelectTables(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range tabs {
		if tt.DB == "hg_safe" {
			t.Fatalf("substituted body table leaked into collection: %+v", tabs)
		}
	}
}

func TestReferencesIdentifier(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := NewPolyglot("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	cases := []struct {
		sql  string
		want bool
	}{
		{`SELECT _hg_row_id FROM t`, true},
		{`SELECT a FROM t WHERE _hg_row_id = 'x'`, true},
		{`SELECT a FROM t ORDER BY _hg_row_id`, true},
		{`SELECT lower(t._hg_row_id) FROM t`, true},
		{`SELECT db.t._hg_row_id FROM db.t`, true},
		{`SELECT * EXCEPT (_hg_row_id) FROM t`, true},
		{`SELECT * REPLACE (1 AS _hg_row_id) FROM t`, true},
		{`SELECT * RENAME (a AS _hg_row_id) FROM t`, true},
		{`SELECT * RENAME (_hg_row_id AS x) FROM t`, true},
		{`SELECT 1 AS _hg_row_id FROM t`, true},
		{`WITH 1 AS _hg_row_id SELECT a FROM t`, true},
		{`SELECT * FROM t AS a JOIN u AS b USING (_hg_row_id)`, true},
		{`SELECT a FROM t WHERE b IN (SELECT _hg_row_id FROM u)`, true},
		{`SELECT a FROM t`, false},
		{`SELECT '_hg_row_id' FROM t`, false},
		{`SELECT hg_row_id FROM t`, false},
	}
	for _, c := range cases {
		ast, err := e.ParseOne(c.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", c.sql, err)
		}
		got, err := ReferencesIdentifier(ast, "_hg_row_id")
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%q: got %v want %v ast=%s", c.sql, got, c.want, ast)
		}
	}
}

func TestCollectTableFunctionTargets(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql  string
		want []TableTarget
	}{
		{`SELECT * FROM merge('hg_safe', 'db1__t')`, []TableTarget{{DB: "hg_safe", Table: "db1__t"}}},
		{`SELECT * FROM remote('127.0.0.1', 'hg_safe', 'db1__t')`, []TableTarget{{DB: "hg_safe", Table: "db1__t"}}},
		{`SELECT * FROM remote('127.0.0.1', 'hg_safe.db1__t')`, []TableTarget{{DB: "hg_safe", Table: "db1__t"}}},
		{`SELECT * FROM cluster('c', hg_unsafe, db1__t)`, []TableTarget{{DB: "hg_unsafe", Table: "db1__t"}}},
		{`SELECT * FROM cluster('c', 'hg_unsafe.db1__t')`, []TableTarget{{DB: "hg_unsafe", Table: "db1__t"}}},
		{`SELECT * FROM numbers(10)`, nil},
	} {
		ast, err := e.ParseOne(tc.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.sql, err)
		}
		got, err := CollectTableFunctionTargets(ast)
		if err != nil {
			t.Fatalf("collect %q: %v", tc.sql, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %+v want %+v ast=%s", tc.sql, got, tc.want, ast)
		}
	}
}

func TestCollectTableFunctionRefs_preservesUnresolvedNamespace(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql  string
		want []TableFunctionRef
	}{
		{`SELECT * FROM remote('h', 'hg_safe', concat('db1', '__t'))`, []TableFunctionRef{{Target: TableTarget{DB: "hg_safe"}}}},
		{`SELECT * FROM cluster('c', 'hg_unsafe', concat('db1', '__t'))`, []TableFunctionRef{{Target: TableTarget{DB: "hg_unsafe"}}}},
		{`SELECT * FROM merge('hg_safe', concat('db1', '__t'))`, []TableFunctionRef{{Target: TableTarget{DB: "hg_safe"}}}},
		{`SELECT * FROM merge('db1__t')`, []TableFunctionRef{{Target: TableTarget{Table: "db1__t"}, UsesCurrentDatabase: true}}},
		{`SELECT merge('db1__t')`, nil},
		{`SELECT * FROM remote('h', concat('hg_', 'safe'), 'db1__t')`, []TableFunctionRef{{Target: TableTarget{Table: "db1__t"}}}},
		{`SELECT * FROM remote('h', 'hg_safe', 'db1__t')`, []TableFunctionRef{{Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}}},
		{`SELECT * FROM numbers(10)`, nil},
	} {
		ast, err := e.ParseOne(tc.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.sql, err)
		}
		got, err := CollectTableFunctionRefs(ast)
		if err != nil {
			t.Fatalf("collect %q: %v", tc.sql, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %+v want %+v ast=%s", tc.sql, got, tc.want, ast)
		}
	}
}

func TestCollectNamespaceRefs_localCatalogSurfaces(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql  string
		want []NamespaceRef
	}{
		{
			`SELECT * FROM other.u WHERE id GLOBAL IN hg_safe.db1__t`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "GLOBAL IN", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT * FROM other.u WHERE id IN db1__t`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "IN", Target: TableTarget{Table: "db1__t"}, UsesCurrentDatabase: true}},
		},
		{
			`SELECT * FROM other.u WHERE id NOT IN hg_safe.db1__t`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "NOT IN", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT * FROM other.u WHERE id GLOBAL NOT IN hg_unsafe.db1__t`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "GLOBAL NOT IN", Target: TableTarget{DB: "hg_unsafe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT in(id, hg_safe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "IN", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT notIn(id, hg_unsafe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "NOT IN", Target: TableTarget{DB: "hg_unsafe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT globalIn(id, hg_safe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "GLOBAL IN", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT globalNotIn(id, hg_unsafe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "GLOBAL NOT IN", Target: TableTarget{DB: "hg_unsafe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT nullIn(id, hg_safe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "NULL IN", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT notNullIn(id, hg_unsafe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "NOT NULL IN", Target: TableTarget{DB: "hg_unsafe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT globalNullIn(id, hg_safe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "GLOBAL NULL IN", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT globalNotNullIn(id, hg_unsafe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "GLOBAL NOT NULL IN", Target: TableTarget{DB: "hg_unsafe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT inIgnoreSet(id, hg_safe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "IN", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT notInIgnoreSet(id, hg_unsafe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "NOT IN", Target: TableTarget{DB: "hg_unsafe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT globalInIgnoreSet(id, hg_safe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "GLOBAL IN", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT globalNotInIgnoreSet(id, hg_unsafe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "GLOBAL NOT IN", Target: TableTarget{DB: "hg_unsafe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT nullInIgnoreSet(id, hg_safe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "NULL IN", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT notNullInIgnoreSet(id, hg_unsafe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "NOT NULL IN", Target: TableTarget{DB: "hg_unsafe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT globalNullInIgnoreSet(id, hg_safe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "GLOBAL NULL IN", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT globalNotNullInIgnoreSet(id, hg_unsafe.db1__t) FROM other.u`,
			[]NamespaceRef{{Source: NamespaceRefInTable, Name: "GLOBAL NOT NULL IN", Target: TableTarget{DB: "hg_unsafe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT * FROM mergeTreeIndex(currentDatabase(), db1__t)`,
			[]NamespaceRef{{Source: NamespaceRefTableFunction, Name: "mergeTreeIndex", Target: TableTarget{Table: "db1__t"}, UsesCurrentDatabase: true}},
		},
		{
			`SELECT * FROM mergeTreeProjection('hg_safe', 'db1__t')`,
			[]NamespaceRef{{Source: NamespaceRefTableFunction, Name: "mergeTreeProjection", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT * FROM mergeTreeCodecBlockCounts('hg_safe', 'db1__t')`,
			[]NamespaceRef{{Source: NamespaceRefTableFunction, Name: "mergeTreeCodecBlockCounts", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT * FROM loop(hg_safe.db1__t)`,
			[]NamespaceRef{{Source: NamespaceRefTableFunction, Name: "loop", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT * FROM timeSeriesSelector(hg_safe.db1__t, 'x', 0, 1)`,
			[]NamespaceRef{{Source: NamespaceRefTableFunction, Name: "timeSeriesSelector", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT * FROM prometheusQuery(hg_safe.db1__t, 'x', 1)`,
			[]NamespaceRef{{Source: NamespaceRefTableFunction, Name: "prometheusQuery", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`SELECT * FROM dictionary('hg_safe.db1__t')`,
			[]NamespaceRef{{Source: NamespaceRefTableFunction, Name: "dictionary", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`CREATE TABLE other.x (a UInt64) ENGINE = Remote('h', 'hg_safe', 'db1__t')`,
			[]NamespaceRef{{Source: NamespaceRefTableEngine, Name: "Remote", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`CREATE TABLE other.x (a UInt64) ENGINE = Merge(currentDatabase(), 'db1__t')`,
			[]NamespaceRef{{Source: NamespaceRefTableEngine, Name: "Merge", Target: TableTarget{Table: "db1__t"}, UsesCurrentDatabase: true}},
		},
		{
			`CREATE DICTIONARY other.d (id UInt64) PRIMARY KEY id SOURCE(CLICKHOUSE(DB 'hg_safe' TABLE 'db1__t')) LAYOUT(HASHED()) LIFETIME(0)`,
			[]NamespaceRef{{Source: NamespaceRefDictionarySource, Name: "CLICKHOUSE", Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true}},
		},
		{
			`CREATE DICTIONARY other.d (id UInt64) PRIMARY KEY id SOURCE(CLICKHOUSE(TABLE 'db1__t')) LAYOUT(HASHED()) LIFETIME(0)`,
			[]NamespaceRef{{Source: NamespaceRefDictionarySource, Name: "CLICKHOUSE", Target: TableTarget{Table: "db1__t"}, UsesCurrentDatabase: true}},
		},
		{
			`CREATE DICTIONARY other.d (id UInt64) PRIMARY KEY id SOURCE(CLICKHOUSE(DB concat('hg_', 'safe') TABLE 'db1__t')) LAYOUT(HASHED()) LIFETIME(0)`,
			[]NamespaceRef{{Source: NamespaceRefDictionarySource, Name: "CLICKHOUSE", Target: TableTarget{Table: "db1__t"}}},
		},
		{
			`CREATE DICTIONARY other.d (id UInt64) PRIMARY KEY id SOURCE(CLICKHOUSE(QUERY 'SELECT id FROM hg_safe.db1__t')) LAYOUT(HASHED()) LIFETIME(0)`,
			[]NamespaceRef{{Source: NamespaceRefDictionarySource, Name: "CLICKHOUSE"}},
		},
		{
			`CREATE DICTIONARY other.d (id UInt64) PRIMARY KEY id SOURCE(CLICKHOUSE(DB 'other' TABLE 'u' WHERE 'id > 0')) LAYOUT(HASHED()) LIFETIME(0)`,
			[]NamespaceRef{{Source: NamespaceRefDictionarySource, Name: "CLICKHOUSE", Target: TableTarget{DB: "other", Table: "u"}}},
		},
		{
			`CREATE DICTIONARY other.d (id UInt64) PRIMARY KEY id SOURCE(CLICKHOUSE(DB 'other' TABLE 'u' INVALIDATE_QUERY 'SELECT max(updated_at) FROM hg_safe.db1__t')) LAYOUT(HASHED()) LIFETIME(0)`,
			[]NamespaceRef{{Source: NamespaceRefDictionarySource, Name: "CLICKHOUSE", Target: TableTarget{DB: "other", Table: "u"}}},
		},
		{
			`CREATE DICTIONARY other.d (id UInt64) PRIMARY KEY id SOURCE(CLICKHOUSE(NAME 'shared_clickhouse')) LAYOUT(HASHED()) LIFETIME(0)`,
			[]NamespaceRef{{Source: NamespaceRefDictionarySource, Name: "CLICKHOUSE"}},
		},
		{
			`CREATE DICTIONARY other.d (id UInt64) PRIMARY KEY id SOURCE(CLICKHOUSE(NAME 'shared_clickhouse' DB 'other' TABLE 'u')) LAYOUT(HASHED()) LIFETIME(0)`,
			[]NamespaceRef{{Source: NamespaceRefDictionarySource, Name: "CLICKHOUSE", Target: TableTarget{DB: "other", Table: "u"}}},
		},
		{
			`CREATE DICTIONARY other.d (id UInt64) PRIMARY KEY id SOURCE(CLICKHOUSE(DB 'other' TABLE 'u' QUERY 'SELECT id FROM hg_safe.db1__t')) LAYOUT(HASHED()) LIFETIME(0)`,
			[]NamespaceRef{{Source: NamespaceRefDictionarySource, Name: "CLICKHOUSE", Target: TableTarget{DB: "other", Table: "u"}}},
		},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			got, err := CollectNamespaceRefs(ast)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(withoutNamespaceOrigins(got), tc.want) {
				t.Fatalf("refs = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestCollectNamespaceRefs_RespectsCTEAndCSEScopes(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NamespaceRef
	}{
		{
			name: "named CTE suppresses infix IN table interpretation",
			sql:  `WITH c AS (SELECT * FROM other.body) SELECT id IN c FROM other.u`,
		},
		{
			name: "named CTE suppresses callable IN table interpretation",
			sql:  `WITH c AS (SELECT * FROM other.body) SELECT in(id, c) FROM other.u`,
		},
		{
			name: "recursive named CTE scope includes self",
			sql:  `WITH RECURSIVE c AS (SELECT id IN c) SELECT id IN c`,
		},
		{
			name: "expression-first CSE suppresses IN but not table sources",
			sql:  `WITH 1 AS t SELECT id IN t FROM other.u`,
		},
		{
			name: "real IN table remains a namespace reference",
			sql:  `WITH c AS (SELECT * FROM other.body) SELECT id IN db1.t FROM other.u`,
			want: []NamespaceRef{{Source: NamespaceRefInTable, Name: "IN", Target: TableTarget{DB: "db1", Table: "t"}, Resolved: true}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			got, err := CollectNamespaceRefs(ast)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(withoutNamespaceOrigins(got), tc.want) {
				t.Fatalf("refs = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func withoutNamespaceOrigins(refs []NamespaceRef) []NamespaceRef {
	out := append([]NamespaceRef(nil), refs...)
	for i := range out {
		out[i].databaseIdentifier = false
		out[i].tableIdentifier = false
	}
	return out
}

func TestCollectNamespaceRefs_PreservesIdentifierOrigins(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name                string
		sql                 string
		wantDB              string
		wantDBIdentifier    bool
		wantTableIdentifier bool
	}{
		{
			name:                "identifier arguments",
			sql:                 "SELECT * FROM remote('h', `hg\\x5Fsafe`, db1__t)",
			wantDB:              "hg_safe",
			wantDBIdentifier:    true,
			wantTableIdentifier: true,
		},
		{
			name:                "string literal arguments",
			sql:                 `SELECT * FROM remote('h', '\\x64b1', 'db1__t')`,
			wantDB:              `\x64b1`,
			wantDBIdentifier:    false,
			wantTableIdentifier: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			refs, err := CollectNamespaceRefs(ast)
			if err != nil {
				t.Fatal(err)
			}
			if len(refs) != 1 {
				t.Fatalf("refs=%#v, want one", refs)
			}
			ref := refs[0]
			if ref.databaseIdentifier != tc.wantDBIdentifier || ref.tableIdentifier != tc.wantTableIdentifier {
				t.Fatalf("origins db=%v table=%v", ref.databaseIdentifier, ref.tableIdentifier)
			}
			semantic, ok := SemanticNamespaceRef(e, ref)
			if !ok || semantic.Target.DB != tc.wantDB || semantic.Target.Table != "db1__t" {
				t.Fatalf("semantic=%#v ok=%v", semantic, ok)
			}
		})
	}
}

func TestCollectEmbeddedSelectSources(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql       string
		wantTable []TableTarget
		wantFn    []TableFunctionRef
	}{
		{`CREATE TABLE other.x AS SELECT * FROM hg_safe.db1__t`, []TableTarget{{DB: "hg_safe", Table: "db1__t"}}, nil},
		{`INSERT INTO other.u SELECT * FROM db1.t`, []TableTarget{{DB: "db1", Table: "t"}}, nil},
		{`INSERT INTO other.u SELECT * FROM remote('h', 'hg_unsafe', concat('db1', '__t'))`, nil, []TableFunctionRef{{Target: TableTarget{DB: "hg_unsafe"}}}},
		{`CREATE TABLE other.x AS SELECT * FROM merge('db1__t')`, nil, []TableFunctionRef{{Target: TableTarget{Table: "db1__t"}, UsesCurrentDatabase: true}}},
		{`CREATE TABLE other.x AS SELECT merge('db1__t')`, nil, nil},
		{`CREATE TABLE other.x (a UInt64) ENGINE = MergeTree ORDER BY a`, nil, nil},
	} {
		ast, err := e.ParseOne(tc.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.sql, err)
		}
		gotTables, gotFns, err := CollectEmbeddedSelectSources(ast)
		if err != nil {
			t.Fatalf("collect %q: %v", tc.sql, err)
		}
		if !reflect.DeepEqual(gotTables, tc.wantTable) || !reflect.DeepEqual(gotFns, tc.wantFn) {
			t.Errorf("%q: tables=%+v functions=%+v, want tables=%+v functions=%+v ast=%s", tc.sql, gotTables, gotFns, tc.wantTable, tc.wantFn, ast)
		}
	}
}

func TestCollectSelectTables_CSEAliasesDoNotHideRealTableSources(t *testing.T) {
	e := newTestEngine(t)
	for _, sql := range []string{
		`WITH 1 AS t SELECT * FROM t`,
		`WITH RECURSIVE 1 AS t SELECT * FROM t`,
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("ParseOne(%q): %v", sql, err)
		}
		got, err := CollectSelectTables(ast)
		if err != nil {
			t.Fatal(err)
		}
		want := []TableTarget{{Table: "t"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("CollectSelectTables(%q) = %#v, want %#v", sql, got, want)
		}
	}
}

type readSourceView struct {
	kind                ReadSourceKind
	target              TableTarget
	resolved            bool
	usesCurrentDatabase bool
}

func collectReadSourceViews(t *testing.T, e Engine, sql string) []readSourceView {
	t.Helper()
	ast, err := e.ParseOne(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	refs, err := CollectEmbeddedReadSources(ast)
	if err != nil {
		t.Fatalf("collect %q: %v", sql, err)
	}
	out := make([]readSourceView, 0, len(refs))
	for _, ref := range refs {
		out = append(out, readSourceView{
			kind: ref.Kind, target: ref.Target, resolved: ref.Resolved,
			usesCurrentDatabase: ref.UsesCurrentDatabase,
		})
	}
	return out
}

func TestCollectEmbeddedReadSources_PreservesSQLOrderAndRoles(t *testing.T) {
	e := newTestEngine(t)
	sql := `WITH c AS (SELECT * FROM db.cte)
		SELECT (SELECT x FROM db.projection)
		FROM c, db.base
		JOIN remote('h', 'db', 'join_fn') AS r
			ON EXISTS (SELECT 1 FROM db.join_on)
		JOIN db.tail ON 1
		WHERE EXISTS (SELECT 1 FROM db.where_late)`
	want := []readSourceView{
		{kind: ReadSourceTable, target: TableTarget{DB: "db", Table: "cte"}, resolved: true},
		{kind: ReadSourceTable, target: TableTarget{DB: "db", Table: "projection"}, resolved: true},
		{kind: ReadSourceTable, target: TableTarget{DB: "db", Table: "base"}, resolved: true},
		{kind: ReadSourceTableFunction, target: TableTarget{DB: "db", Table: "join_fn"}, resolved: true},
		{kind: ReadSourceTable, target: TableTarget{DB: "db", Table: "join_on"}, resolved: true},
		{kind: ReadSourceTable, target: TableTarget{DB: "db", Table: "tail"}, resolved: true},
		{kind: ReadSourceTable, target: TableTarget{DB: "db", Table: "where_late"}, resolved: true},
	}
	for i := 0; i < 100; i++ {
		if got := collectReadSourceViews(t, e, sql); !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d sources = %#v, want %#v", i, got, want)
		}
	}
}

func TestCollectEmbeddedReadSources_TableFunctionsRequireSourceRole(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []readSourceView
	}{
		{
			name: "scalar projection predicate and nested scalar",
			sql: `SELECT remote('h', 'decoy', 'projection'), merge('scalar'),
				(SELECT merge('nested'))
				FROM db.base
				WHERE remote('h', 'decoy', 'predicate') = 1`,
			want: []readSourceView{{kind: ReadSourceTable, target: TableTarget{DB: "db", Table: "base"}, resolved: true}},
		},
		{
			name: "from and alias wrapped join sources",
			sql:  `SELECT 1 FROM remote('h', 'db', 'from_fn') AS r JOIN merge('db', 'join_fn') AS m ON 1`,
			want: []readSourceView{
				{kind: ReadSourceTableFunction, target: TableTarget{DB: "db", Table: "from_fn"}, resolved: true},
				{kind: ReadSourceTableFunction, target: TableTarget{DB: "db", Table: "join_fn"}, resolved: true},
			},
		},
		{
			name: "scalar and aggregate arguments retain nested queries",
			sql: `SELECT
				coalesce((SELECT 1 FROM db.scalar_arg), 0),
				sum((SELECT 1 FROM db.aggregate_arg))`,
			want: []readSourceView{
				{kind: ReadSourceTable, target: TableTarget{DB: "db", Table: "scalar_arg"}, resolved: true},
				{kind: ReadSourceTable, target: TableTarget{DB: "db", Table: "aggregate_arg"}, resolved: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := collectReadSourceViews(t, e, tc.sql); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sources = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestCollectEmbeddedReadSources_SetOperationsAreLeftToRight(t *testing.T) {
	e := newTestEngine(t)
	got := collectReadSourceViews(t, e, `SELECT * FROM db.left_source UNION ALL SELECT * FROM merge('db', 'right_source')`)
	want := []readSourceView{
		{kind: ReadSourceTable, target: TableTarget{DB: "db", Table: "left_source"}, resolved: true},
		{kind: ReadSourceTableFunction, target: TableTarget{DB: "db", Table: "right_source"}, resolved: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %#v, want %#v", got, want)
	}
}

func TestCollectEmbeddedReadSources_ConditionalExpressionsUseSQLOrder(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []readSourceView
	}{
		{
			name: "CASE condition then result then ELSE",
			sql: `SELECT CASE
				WHEN EXISTS(SELECT 1 FROM hg_unsafe.x) THEN (SELECT 1 FROM db1.t)
				ELSE (SELECT 1 FROM hg_safe.y)
			END`,
			want: []readSourceView{
				{kind: ReadSourceTable, target: TableTarget{DB: "hg_unsafe", Table: "x"}, resolved: true},
				{kind: ReadSourceTable, target: TableTarget{DB: "db1", Table: "t"}, resolved: true},
				{kind: ReadSourceTable, target: TableTarget{DB: "hg_safe", Table: "y"}, resolved: true},
			},
		},
		{
			name: "IF condition then true then false",
			sql: `SELECT if(
				EXISTS(SELECT 1 FROM hg_unsafe.x),
				(SELECT 1 FROM db1.t),
				(SELECT 1 FROM hg_safe.y))`,
			want: []readSourceView{
				{kind: ReadSourceTable, target: TableTarget{DB: "hg_unsafe", Table: "x"}, resolved: true},
				{kind: ReadSourceTable, target: TableTarget{DB: "db1", Table: "t"}, resolved: true},
				{kind: ReadSourceTable, target: TableTarget{DB: "hg_safe", Table: "y"}, resolved: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := collectReadSourceViews(t, e, tc.sql); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sources = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestCollectEmbeddedReadSources_WindowClausesUseGrammarOrder(t *testing.T) {
	e := newTestEngine(t)
	got := collectReadSourceViews(t, e, `SELECT sum(x) OVER (
		PARTITION BY (SELECT 1 FROM hg_unsafe.db1__x)
		ORDER BY (SELECT 1 FROM db1.t))`)
	want := []readSourceView{
		{kind: ReadSourceTable, target: TableTarget{DB: "hg_unsafe", Table: "db1__x"}, resolved: true},
		{kind: ReadSourceTable, target: TableTarget{DB: "db1", Table: "t"}, resolved: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %#v, want %#v", got, want)
	}
}

func TestCollectEmbeddedReadSources_WindowAndQualifyFollowSurfaceOrder(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []readSourceView
	}{
		{
			name: "WINDOW before QUALIFY",
			sql: `SELECT row_number() OVER w AS rn
				WINDOW w AS (PARTITION BY (SELECT 1 FROM hg_unsafe.x))
				QUALIFY EXISTS(SELECT 1 FROM db1.t)`,
			want: []readSourceView{
				{kind: ReadSourceTable, target: TableTarget{DB: "hg_unsafe", Table: "x"}, resolved: true},
				{kind: ReadSourceTable, target: TableTarget{DB: "db1", Table: "t"}, resolved: true},
			},
		},
		{
			name: "QUALIFY before WINDOW",
			sql: `SELECT row_number() OVER w AS rn
				QUALIFY EXISTS(SELECT 1 FROM hg_unsafe.x)
				WINDOW w AS (PARTITION BY (SELECT 1 FROM db1.t))`,
			want: []readSourceView{
				{kind: ReadSourceTable, target: TableTarget{DB: "hg_unsafe", Table: "x"}, resolved: true},
				{kind: ReadSourceTable, target: TableTarget{DB: "db1", Table: "t"}, resolved: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := collectReadSourceViews(t, e, tc.sql); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sources = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestCollectEmbeddedReadSources_WithFillUsesGrammarFieldOrder(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne(`SELECT 1 ORDER BY 1 WITH FILL`)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		t.Fatal(err)
	}
	selectNode := root[NodeSelect].(map[string]any)
	orderBy := selectNode["order_by"].(map[string]any)
	ordered := orderBy["expressions"].([]any)[0].(map[string]any)
	withFill := ordered["with_fill"].(map[string]any)
	query := func(table string) any {
		t.Helper()
		parsed, err := e.ParseOne(`(SELECT 1 FROM db.` + table + `)`)
		if err != nil {
			t.Fatal(err)
		}
		var node any
		if err := json.Unmarshal(parsed, &node); err != nil {
			t.Fatal(err)
		}
		return node
	}
	ordered["this"] = query("this_expr")
	for _, field := range []string{"from_", "to", "step", "staleness", "interpolate"} {
		withFill[field] = query(field)
	}
	mutated, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := CollectEmbeddedReadSources(AST(mutated))
	if err != nil {
		t.Fatal(err)
	}
	var got []TableTarget
	for _, ref := range refs {
		got = append(got, ref.Target)
	}
	want := []TableTarget{
		{DB: "db", Table: "this_expr"},
		{DB: "db", Table: "from_"},
		{DB: "db", Table: "to"},
		{DB: "db", Table: "step"},
		{DB: "db", Table: "staleness"},
		{DB: "db", Table: "interpolate"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %#v, want %#v", got, want)
	}
}

func TestCollectEmbeddedReadSources_ParenthesizedQueryIsTransparent(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql   string
		table string
	}{
		{`(SELECT * FROM db.top_level)`, "top_level"},
		{`SELECT * FROM ((SELECT * FROM db.from_level))`, "from_level"},
	} {
		got := collectReadSourceViews(t, e, tc.sql)
		want := []readSourceView{{
			kind: ReadSourceTable, target: TableTarget{DB: "db", Table: tc.table}, resolved: true,
		}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%q sources = %#v, want %#v", tc.sql, got, want)
		}
	}
}

func TestObjectWalker_UnknownReadBearingCarrierFailsClosedForEveryProjection(t *testing.T) {
	e := newTestEngine(t)
	outer, err := e.ParseOne(`SELECT 1`)
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := e.ParseOne(`SELECT * FROM db.hidden`)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	var hiddenNode any
	if err := json.Unmarshal(outer, &root); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(hidden, &hiddenNode); err != nil {
		t.Fatal(err)
	}
	root[NodeSelect].(map[string]any)["future_read_carrier"] = map[string]any{"payload": hiddenNode}
	mutated, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	ast := AST(mutated)
	checks := []struct {
		name string
		run  func() error
	}{
		{"tables", func() error { _, err := CollectSelectTables(ast); return err }},
		{"read sources", func() error { _, err := CollectEmbeddedReadSources(ast); return err }},
		{"split read sources", func() error {
			_, _, err := CollectEmbeddedSelectSources(ast)
			return err
		}},
		{"namespaces", func() error { _, err := CollectNamespaceRefs(ast); return err }},
		{"table functions", func() error { _, err := CollectTableFunctionRefs(ast); return err }},
		{"rewrite", func() error {
			_, err := RewriteSelectTables(ast, func(TableTarget) TableDecision {
				return TableDecision{Action: ActionSkip}
			})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); err == nil {
				t.Fatal("unmodeled read-bearing carrier was accepted")
			}
		})
	}
}

func TestCollectNamespaceRefs_InsertFunctionTargetIsExplicitButNotAReadSource(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne(`INSERT INTO FUNCTION remote('h', 'db1', 'target') SELECT * FROM other.u`)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := CollectNamespaceRefs(ast)
	if err != nil {
		t.Fatal(err)
	}
	wantRefs := []NamespaceRef{{
		Source: NamespaceRefTableFunction, Name: "remote",
		Target: TableTarget{DB: "db1", Table: "target"}, Resolved: true,
	}}
	if !reflect.DeepEqual(refs, wantRefs) {
		t.Fatalf("namespace refs = %#v, want %#v", refs, wantRefs)
	}
	reads, err := CollectEmbeddedReadSources(ast)
	if err != nil {
		t.Fatal(err)
	}
	wantReads := []ReadSourceRef{{
		Kind: ReadSourceTable, Target: TableTarget{DB: "other", Table: "u"},
		Resolved: true, databaseIdentifier: true, tableIdentifier: true,
	}}
	if !reflect.DeepEqual(reads, wantReads) {
		t.Fatalf("read sources = %#v, want %#v", reads, wantReads)
	}
}

func TestCollectNamespaceRefs_CreateAsTableFunctionUsesExactSourceRole(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NamespaceRef
	}{
		{
			name: "clone source table function is a namespace",
			sql:  `CREATE TABLE other.x AS merge('hg_safe', 'db1__t')`,
			want: []NamespaceRef{{
				Source: NamespaceRefTableFunction, Name: "merge",
				Target: TableTarget{DB: "hg_safe", Table: "db1__t"}, Resolved: true,
			}},
		},
		{
			name: "scalar lookalike in select projection is not a namespace",
			sql:  `CREATE TABLE other.x AS SELECT merge('hg_safe', 'db1__t')`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			got, err := CollectNamespaceRefs(ast)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("namespace refs = %#v, want %#v; ast=%s", got, tc.want, ast)
			}
		})
	}
}

func TestCollectEmbeddedReadSources_InTableOperandsAreOrderedReadEvents(t *testing.T) {
	e := newTestEngine(t)
	got := collectReadSourceViews(t, e, `SELECT
		(SELECT 1 FROM other.before),
		id IN db1.t,
		id GLOBAL IN hg_safe.x,
		in(id, hg_unsafe.y),
		equals(id, hg_safe.scalar_decoy)
	FROM other.base`)
	want := []readSourceView{
		{kind: ReadSourceTable, target: TableTarget{DB: "other", Table: "before"}, resolved: true},
		{kind: ReadSourceInTable, target: TableTarget{DB: "db1", Table: "t"}, resolved: true},
		{kind: ReadSourceInTable, target: TableTarget{DB: "hg_safe", Table: "x"}, resolved: true},
		{kind: ReadSourceInTable, target: TableTarget{DB: "hg_unsafe", Table: "y"}, resolved: true},
		{kind: ReadSourceTable, target: TableTarget{DB: "other", Table: "base"}, resolved: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %#v, want %#v", got, want)
	}
}

func TestCollectEmbeddedReadSources_InSubqueryAndScalarArgumentsStayRoleAware(t *testing.T) {
	e := newTestEngine(t)
	got := collectReadSourceViews(t, e, `SELECT
		id IN (SELECT id FROM db.subquery),
		equals(id, hg_safe.not_a_table_operand)
	FROM other.base`)
	want := []readSourceView{
		{kind: ReadSourceTable, target: TableTarget{DB: "db", Table: "subquery"}, resolved: true},
		{kind: ReadSourceTable, target: TableTarget{DB: "other", Table: "base"}, resolved: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %#v, want %#v", got, want)
	}
}

func TestCollectEmbeddedReadSources_InTableOperandsRespectCTEScopeAndOpacity(t *testing.T) {
	e := newTestEngine(t)
	got := collectReadSourceViews(t, e, `WITH t AS (SELECT * FROM other.cte_body)
		SELECT
			id IN t,
			id IN hg_safe.{target:Identifier},
			in(id, {other_target:Identifier})
		FROM other.base`)
	want := []readSourceView{
		{kind: ReadSourceTable, target: TableTarget{DB: "other", Table: "cte_body"}, resolved: true},
		{kind: ReadSourceTable, target: TableTarget{DB: "other", Table: "base"}, resolved: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %#v, want %#v", got, want)
	}
}

func TestCollectEmbeddedReadSources_InTableOperandsRespectOutputAndTableAliases(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []readSourceView
	}{
		{
			name: "output alias hides infix table interpretation",
			sql:  `SELECT tuple(1, 2) AS t, 1 IN t`,
			want: []readSourceView{},
		},
		{
			name: "output alias hides callable table interpretation",
			sql:  `SELECT tuple(1, 2) AS t, in(1, t)`,
			want: []readSourceView{},
		},
		{
			name: "FROM alias is scoped before projection infix IN",
			sql:  `SELECT id IN t FROM other.u AS t`,
			want: []readSourceView{{kind: ReadSourceTable, target: TableTarget{DB: "other", Table: "u", Alias: "t"}, resolved: true}},
		},
		{
			name: "FROM alias is scoped before projection callable IN",
			sql:  `SELECT in(id, t) FROM other.u AS t`,
			want: []readSourceView{{kind: ReadSourceTable, target: TableTarget{DB: "other", Table: "u", Alias: "t"}, resolved: true}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := collectReadSourceViews(t, e, tc.sql); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sources = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestCollectEmbeddedReadSources_CTEDefinitionsUseIncrementalNonrecursiveScope(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []readSourceView
	}{
		{
			name: "self reference is physical only inside its own definition",
			sql:  `WITH t AS (SELECT * FROM t) SELECT * FROM t`,
			want: []readSourceView{
				{kind: ReadSourceTable, target: TableTarget{Table: "t"}, resolved: true},
			},
		},
		{
			name: "later definition sees earlier aliases but not itself",
			sql: `WITH
				a AS (SELECT * FROM other.first),
				b AS (SELECT * FROM a JOIN b ON 1)
			SELECT * FROM b JOIN other.tail ON 1`,
			want: []readSourceView{
				{kind: ReadSourceTable, target: TableTarget{DB: "other", Table: "first"}, resolved: true},
				{kind: ReadSourceTable, target: TableTarget{Table: "b"}, resolved: true},
				{kind: ReadSourceTable, target: TableTarget{DB: "other", Table: "tail"}, resolved: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := collectReadSourceViews(t, e, tc.sql); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sources = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestCollectEmbeddedReadSources_RecursiveCTEsPredeclareTheirCompleteScope(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []readSourceView
	}{
		{
			name: "self reference is recursive scope",
			sql:  `WITH RECURSIVE t AS (SELECT * FROM t) SELECT * FROM t`,
			want: []readSourceView{},
		},
		{
			name: "all recursive aliases are visible to every body",
			sql: `WITH RECURSIVE
				a AS (SELECT * FROM b),
				b AS (SELECT * FROM a)
			SELECT * FROM a JOIN b ON 1`,
			want: []readSourceView{},
		},
		{
			name: "recursive bodies retain real sources",
			sql: `WITH RECURSIVE
				a AS (SELECT * FROM b JOIN other.real ON 1),
				b AS (SELECT * FROM a)
			SELECT * FROM a`,
			want: []readSourceView{{kind: ReadSourceTable, target: TableTarget{DB: "other", Table: "real"}, resolved: true}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := collectReadSourceViews(t, e, tc.sql); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sources = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestRewriteSelectTables_RecursiveCTESelfReferencesStayScoped(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne(`WITH RECURSIVE t AS (SELECT * FROM t) SELECT * FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	var visited []TableTarget
	if _, err := RewriteSelectTables(ast, func(target TableTarget) TableDecision {
		visited = append(visited, target)
		return TableDecision{Action: ActionSkip}
	}); err != nil {
		t.Fatal(err)
	}
	if len(visited) != 0 {
		t.Fatalf("recursive CTE references visited as physical tables: %+v", visited)
	}
}

func TestCollectEmbeddedReadSources_QualifiedOpaqueTableTargetIsNotFabricated(t *testing.T) {
	e := newTestEngine(t)
	got := collectReadSourceViews(t, e,
		`SELECT * FROM hg_safe.{target:Identifier} JOIN hg_unsafe.db1__x ON 1`)
	want := []readSourceView{
		{kind: ReadSourceTable, target: TableTarget{DB: "hg_unsafe", Table: "db1__x"}, resolved: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %#v, want %#v", got, want)
	}
}

func TestRewriteSelectTables_UsesOrderedReadSourceVisitor(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne(`WITH c AS (SELECT * FROM db.cte) SELECT (SELECT x FROM db.projection) FROM c, db.base JOIN db.tail ON EXISTS (SELECT 1 FROM db.join_on)`)
	if err != nil {
		t.Fatal(err)
	}
	var got []TableTarget
	if _, err := RewriteSelectTables(ast, func(target TableTarget) TableDecision {
		got = append(got, target)
		return TableDecision{Action: ActionSkip}
	}); err != nil {
		t.Fatal(err)
	}
	want := []TableTarget{
		{DB: "db", Table: "cte"},
		{DB: "db", Table: "projection"},
		{DB: "db", Table: "base"},
		{DB: "db", Table: "tail"},
		{DB: "db", Table: "join_on"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rewrite visit order = %+v, want %+v", got, want)
	}
}

func TestUnsupportedTableWrapperTargets(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql  string
		want []TableTarget
	}{
		{`SELECT * FROM db1.t FINAL`, []TableTarget{{DB: "db1", Table: "t"}}},
		{`SELECT * FROM db1.t SAMPLE 0.1`, []TableTarget{{DB: "db1", Table: "t"}}},
		{`SELECT * FROM db1.t AS x(a)`, []TableTarget{{DB: "db1", Table: "t", Alias: "x"}}},
		{`SELECT * FROM db1.t AS s JOIN other.u FINAL ON 1`, []TableTarget{{DB: "other", Table: "u"}}},
		{`SELECT * FROM db1.t AS s JOIN other.u SAMPLE 0.1 ON 1`, []TableTarget{{DB: "other", Table: "u"}}},
		{`SELECT * FROM db1.t AS s JOIN other.u AS o(id) ON s.id = o.id`, []TableTarget{{DB: "other", Table: "u", Alias: "o"}}},
	} {
		ast, err := e.ParseOne(tc.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.sql, err)
		}
		got, err := UnsupportedTableWrapperTargets(ast)
		if err != nil {
			t.Fatalf("inspect %q: %v", tc.sql, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %+v want %+v ast=%s", tc.sql, got, tc.want, ast)
		}
	}
}

func TestHasWithOffset(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{"SELECT * FROM t WITH OFFSET AS off", true},
		{"SELECT * FROM t WITH\nOFFSET AS off", true},
		{"SELECT * FROM t WITH\tOFFSET AS off", true},
		{"SELECT 'WITH OFFSET' FROM t", false},
		{"SELECT 'WITH' FROM t OFFSET 1", false},
		{"-- WITH OFFSET\nSELECT * FROM t", false},
	} {
		got, err := HasWithOffset(e, tc.sql)
		if err != nil {
			t.Fatalf("%q: %v", tc.sql, err)
		}
		if got != tc.want {
			t.Errorf("%q: got %v want %v", tc.sql, got, tc.want)
		}
	}
}

func TestWithOffsetTargets(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql  string
		want []TableTarget
	}{
		{`SELECT * FROM db1.t WITH OFFSET AS off`, []TableTarget{{DB: "db1", Table: "t"}}},
		{"SELECT * FROM db1.t WITH\nOFFSET AS off", []TableTarget{{DB: "db1", Table: "t"}}},
		{`SELECT * FROM db1.t AS s JOIN other.u WITH OFFSET AS off ON 1`, []TableTarget{{DB: "other", Table: "u"}}},
		{`SELECT * FROM other.u, db1.t WITH OFFSET AS off`, []TableTarget{{DB: "db1", Table: "t"}}},
		{`SELECT * FROM db1.t, other.u WITH OFFSET AS off`, []TableTarget{{DB: "other", Table: "u"}}},
		{`SELECT 'WITH OFFSET' FROM db1.t`, nil},
	} {
		got, err := WithOffsetTargets(e, tc.sql)
		if err != nil {
			t.Fatalf("%q: %v", tc.sql, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %+v want %+v", tc.sql, got, tc.want)
		}
	}
}

// TestTableFunctionArgValue_DecodesHeredocLiterals pins that the value the
// namespace policy sees equals the value the generator emits. Polyglot encodes
// a tagged ClickHouse heredoc as "<tag>\x00<body>", so reading lit["value"] raw
// made merge($t$hg_safe$t$, 'db1__t') invisible to the storage-integrity gate
// while Generate re-emitted it as merge('hg_safe', 'db1__t').
func TestTableFunctionArgValue_DecodesHeredocLiterals(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct{ sql, wantDB string }{
		{"SELECT * FROM merge('hg_safe', 'db1__t')", "hg_safe"},
		{"SELECT * FROM merge($$hg_safe$$, 'db1__t')", "hg_safe"},
		{"SELECT * FROM merge($tag$hg_safe$tag$, 'db1__t')", "hg_safe"},
		{"SELECT * FROM merge($x$hg_safe$x$, 'db1__t')", "hg_safe"},
		{"SELECT * FROM remote('h', $tag$hg_unsafe$tag$, 'db1__t')", "hg_unsafe"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			refs, err := CollectNamespaceRefs(ast)
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			if len(refs) != 1 || refs[0].Target.DB != tc.wantDB || refs[0].Target.Table != "db1__t" || !refs[0].Resolved {
				t.Fatalf("refs = %+v, want a single resolved ref naming %q.db1__t", refs, tc.wantDB)
			}
		})
	}
}

// polyglotLiteralTypes mirrors polyglot's Literal enum
// (third_party/polyglot-src/crates/polyglot-sql/src/expressions.rs, the
// #[serde(tag = "literal_type", ...)] variants). Spec N D6 requires the class
// of "policy reads a raw AST field the generator interprets differently" to be
// closed, not just its dollar_string instance, so the invariant test below has
// to be driven by the complete tag set rather than by the shapes someone
// happened to think of.
var polyglotLiteralTypes = []string{
	"string", "number", "hex_string", "hex_number", "bit_string", "byte_string",
	"national_string", "date", "time", "timestamp", "datetime",
	"triple_quoted_string", "escape_string", "dollar_string", "raw_string",
}

// literalTypesUnreachableInClickHouseArgs are the polyglot literal variants no
// ClickHouse-dialect spelling produces in a table-function argument position.
// b"..." is BigQuery-only and the ClickHouse lexer splits it into an identifier
// plus a quoted identifier. Keep this list as a tripwire: a variant that
// becomes reachable must gain a probe below, not silently skip the invariant.
var literalTypesUnreachableInClickHouseArgs = map[string]string{
	"byte_string": `b"hg_safe"`,
}

// TestTableFunctionArgValue_PolicyValueMatchesGeneratedValueOrRefuses is Spec N
// D6's closure proof. For every literal kind polyglot can emit in a
// table-function namespace argument, either the value storage-integrity policy
// inspects is exactly the value the generator emits for ClickHouse to execute,
// or policy refuses to resolve the argument at all. The tagged heredoc broke
// the first half (policy saw "tag\x00hg_safe", ClickHouse got 'hg_safe') and
// nothing enforced the second, so an unmodelled encoding silently became an
// opaque, harmless-looking value.
func TestTableFunctionArgValue_PolicyValueMatchesGeneratedValueOrRefuses(t *testing.T) {
	e := newTestEngine(t)
	probes := []string{
		`'hg_safe'`,
		`'''hg_safe'''`,
		`123`,
		`x'6867'`,
		`X'6867'`,
		`0x68675f73616665`,
		`b'0110'`,
		`N'hg_safe'`,
		`DATE '2024-01-15'`,
		`TIME '10:30:00'`,
		`TIMESTAMP '2024-01-15 10:30:00'`,
		`DATETIME '2024-01-15 10:30:00'`,
		`"""hg_safe"""`,
		`e'hg_safe'`,
		`E'hg_safe'`,
		`$$hg_safe$$`,
		`$$$$`,
		`$tag$hg_safe$tag$`,
		`r'hg_safe'`,
		`R'hg_safe'`,
	}
	observed := map[string]bool{}
	for _, probe := range probes {
		t.Run(probe, func(t *testing.T) {
			sql := "SELECT * FROM merge(" + probe + ", 'db1__t')"
			ast, err := e.ParseOne(sql)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			for _, kind := range literalTypesInAST(t, ast) {
				observed[kind] = true
			}
			refs, err := CollectNamespaceRefs(ast)
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			if len(refs) != 1 {
				t.Fatalf("refs = %+v, want exactly one merge() namespace reference", refs)
			}
			policyValue := refs[0].Target.DB
			if policyValue == "" && !refs[0].Resolved {
				return // policy refuses this encoding: fail-closed, nothing to compare
			}
			gen, err := e.Generate(ast)
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			emitted, ok := firstMergeArgument(gen)
			if !ok {
				t.Fatalf("generated %q does not have the canonical merge(<arg>, 'db1__t') shape", gen)
			}
			executed, plain := plainClickHouseStringLiteral(emitted)
			if !plain {
				t.Fatalf("policy resolved %q but ClickHouse executes the non-string argument %s in %q; "+
					"an encoding whose emitted form is not a plain string literal must refuse", policyValue, emitted, gen)
			}
			if executed != policyValue {
				t.Fatalf("policy sees %q but ClickHouse executes %q (emitted %s)", policyValue, executed, emitted)
			}
		})
	}
	for _, kind := range polyglotLiteralTypes {
		spelling, unreachable := literalTypesUnreachableInClickHouseArgs[kind]
		switch {
		case unreachable && observed[kind]:
			t.Errorf("literal_type %q is listed as unreachable (%s) but a probe produced it; give it a probe instead", kind, spelling)
		case !unreachable && !observed[kind]:
			t.Errorf("literal_type %q is not exercised by any probe; add one or record why it is unreachable", kind)
		}
	}
}

// literalTypesInAST returns every literal_type tag present in the AST.
func literalTypesInAST(t *testing.T, ast AST) []string {
	t.Helper()
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		t.Fatalf("decode ast: %v", err)
	}
	var out []string
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case []any:
			for _, child := range n {
				walk(child)
			}
		case map[string]any:
			if lit, ok := n["literal"].(map[string]any); ok {
				if kind, ok := lit["literal_type"].(string); ok {
					out = append(out, kind)
				}
			}
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(root)
	return out
}

// firstMergeArgument extracts the first argument of the canonical
// merge(<arg>, 'db1__t') form the generator emits for the probes above.
func firstMergeArgument(generated string) (string, bool) {
	const prefix = "SELECT * FROM merge("
	const suffix = ", 'db1__t')"
	if !strings.HasPrefix(generated, prefix) || !strings.HasSuffix(generated, suffix) {
		return "", false
	}
	return generated[len(prefix) : len(generated)-len(suffix)], true
}

// plainClickHouseStringLiteral decodes an ordinary single-quoted ClickHouse
// string literal. It deliberately does NOT reuse the AST decode under test, and
// it refuses backslash-bearing bodies rather than guessing ClickHouse's escape
// rules: anything it cannot decode exactly is reported as not plain, which the
// invariant treats as "policy must refuse".
func plainClickHouseStringLiteral(emitted string) (string, bool) {
	if len(emitted) < 2 || emitted[0] != '\'' || emitted[len(emitted)-1] != '\'' {
		return "", false
	}
	body := emitted[1 : len(emitted)-1]
	if strings.Contains(body, `\`) {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\'' {
			b.WriteByte(body[i])
			continue
		}
		if i+1 >= len(body) || body[i+1] != '\'' {
			return "", false // an unpaired quote is not a well-formed single literal
		}
		b.WriteByte('\'')
		i++
	}
	return b.String(), true
}

// TestCollectNamespaceRefs_foreignConnectorFamily pins the Spec N D4 decode.
// ClickHouse ships its own MySQL (9004) and PostgreSQL (9005) wire listeners
// and a JDBC/ODBC datasource can point back at ClickHouse, so these signatures
// carry a (database|schema, table) pair into the protected namespace. Decoding
// is by ARITY, not a flat "pair at index 1": mongodb and jdbc/odbc each have a
// short form whose index 1 is the table, and every one of the five also accepts
// a named-collection form that names nothing statically.
func TestCollectNamespaceRefs_foreignConnectorFamily(t *testing.T) {
	e := newTestEngine(t)
	fn := func(name string, target TableTarget, resolved, current bool) []NamespaceRef {
		return []NamespaceRef{{Source: NamespaceRefTableFunction, Name: name, Target: target, Resolved: resolved, UsesCurrentDatabase: current}}
	}
	for _, tc := range []struct {
		sql  string
		want []NamespaceRef
	}{
		{`SELECT * FROM mysql('127.0.0.1:9004', 'hg_safe', 'db1__t', 'u', 'p')`,
			fn("mysql", TableTarget{DB: "hg_safe", Table: "db1__t"}, true, false)},
		{`SELECT * FROM postgresql('127.0.0.1:9005', 'hg_unsafe', 'db1__t', 'u', 'p')`,
			fn("postgresql", TableTarget{DB: "hg_unsafe", Table: "db1__t"}, true, false)},
		// mongodb(host:port, database, collection, user, password, structure, ...)
		{`SELECT * FROM mongodb('127.0.0.1:27017', 'hg_safe', 'db1__t', 'u', 'p', 'a String')`,
			fn("mongodb", TableTarget{DB: "hg_safe", Table: "db1__t"}, true, false)},
		// mongodb(uri, collection, structure, ...) -- index 1 is the collection.
		{`SELECT * FROM mongodb('mongodb://h:27017/hg_safe', 'db1__t', 'a String')`,
			fn("mongodb", TableTarget{Table: "db1__t"}, false, true)},
		{`SELECT * FROM jdbc('jdbc:clickhouse://127.0.0.1:8123', 'hg_safe', 'db1__t')`,
			fn("jdbc", TableTarget{DB: "hg_safe", Table: "db1__t"}, true, false)},
		{`SELECT * FROM jdbc('jdbc:clickhouse://127.0.0.1:8123', 'db1__t')`,
			fn("jdbc", TableTarget{Table: "db1__t"}, false, true)},
		{`SELECT * FROM odbc('DSN=ch', 'hg_unsafe', 'db1__t')`,
			fn("odbc", TableTarget{DB: "hg_unsafe", Table: "db1__t"}, true, false)},
		{`SELECT * FROM odbc('DSN=ch', 'db1__t')`,
			fn("odbc", TableTarget{Table: "db1__t"}, false, true)},
		// Named-collection forms name no namespace statically. They stay
		// recognized and unresolved so policy refuses them, rather than being
		// invisible the way they were before this decode existed.
		{`SELECT * FROM mysql(creds)`, fn("mysql", TableTarget{}, false, false)},
		{`SELECT * FROM mysql(creds, database = 'hg_safe', table = 'db1__t')`,
			fn("mysql", TableTarget{}, false, false)},
		{`SELECT * FROM postgresql(creds, database = 'hg_safe', table = 'db1__t')`,
			fn("postgresql", TableTarget{}, false, false)},
		{`SELECT * FROM mongodb(creds, database = 'hg_safe', collection = 'db1__t')`,
			fn("mongodb", TableTarget{}, false, true)},
		{`SELECT * FROM jdbc(creds)`, fn("jdbc", TableTarget{}, false, true)},
		{`SELECT * FROM odbc(creds)`, fn("odbc", TableTarget{}, false, true)},
		// Deliberately NOT decoded (Spec N D4, plan deviation D-2): sqlite's
		// second argument is a table inside a SQLite FILE and redis's is a
		// COLUMN name, so neither names a ClickHouse namespace and gating them
		// would only manufacture false positives.
		{`SELECT * FROM sqlite('/tmp/x.db', 'db1__t')`, nil},
		{`SELECT * FROM redis('127.0.0.1:6379', 'hg_safe', 'k String')`, nil},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			got, err := CollectNamespaceRefs(ast)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(withoutNamespaceOrigins(got), tc.want) {
				t.Fatalf("refs = %#v, want %#v", got, tc.want)
			}
		})
	}
}
