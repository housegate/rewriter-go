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

// sortTargets sorts a TableTarget slice by DB+Table+Alias for stable comparison.
func sortTargets(s []TableTarget) {
	sort.Slice(s, func(i, j int) bool {
		ki := fmt.Sprintf("%s\x00%s\x00%s", s[i].DB, s[i].Table, s[i].Alias)
		kj := fmt.Sprintf("%s\x00%s\x00%s", s[j].DB, s[j].Table, s[j].Alias)
		return ki < kj
	})
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
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("refs = %#v, want %#v", got, tc.want)
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
