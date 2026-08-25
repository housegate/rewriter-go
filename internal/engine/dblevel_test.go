package engine

import "testing"

func TestParseDBLevel(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql      string
		kind     DBLevelKind
		showWhat string
		db       string
		hasLike  bool
		like     string
		likeNot  bool
		likeCI   bool
	}{
		{"USE mydb", DBUse, "", "mydb", false, "", false, false},
		{"USE `weird db`", DBUse, "", "weird db", false, "", false, false},
		{"SHOW TABLES", DBShow, "TABLES", "", false, "", false, false},
		{"SHOW TABLES FROM mydb", DBShow, "TABLES", "mydb", false, "", false, false},
		{"SHOW TABLES IN mydb", DBShow, "TABLES", "mydb", false, "", false, false},
		{"SHOW TABLES FROM mydb LIKE 'a%'", DBShow, "TABLES", "mydb", true, "a%", false, false},
		{"SHOW TABLES IN mydb NOT LIKE 'b%'", DBShow, "TABLES", "mydb", true, "b%", true, false},
		{"SHOW DATABASES", DBShow, "DATABASES", "", false, "", false, false},
		{"SHOW DATABASES LIKE 'pre%'", DBShow, "DATABASES", "", true, "pre%", false, false},
		{"SHOW DATABASES NOT LIKE 'x%'", DBShow, "DATABASES", "", true, "x%", true, false},
		{"SHOW DATABASES NOT ILIKE 'y%'", DBShow, "DATABASES", "", true, "y%", true, true},
		{"SHOW DATABASES ILIKE 'z%'", DBShow, "DATABASES", "", true, "z%", false, true},
		{"SHOW CLUSTERS", DBShow, "CLUSTERS", "", false, "", false, false},
		{"SHOW DICTIONARIES", DBShow, "DICTIONARIES", "", false, "", false, false},
		{"SHOW DICTIONARIES FROM hg_safe", DBShow, "DICTIONARIES", "hg_safe", false, "", false, false},
		{"SHOW DICTIONARIES IN `db1`", DBShow, "DICTIONARIES", "db1", false, "", false, false},
		// The kind word after SHOW lexes as a keyword (not VAR) for these; ShowWhat
		// must still capture it so the handler can distinguish SHOW CREATE (a separate
		// ClickHouse AST) from the ASTShowTablesQuery family (CLUSTER/SETTINGS/...).
		{"SHOW CREATE TABLE db.t", DBShow, "CREATE", "", false, "", false, false},
		{"SHOW CLUSTER 'x'", DBShow, "CLUSTER", "", false, "", false, false},
		{"SHOW SETTINGS LIKE 'a%'", DBShow, "SETTINGS", "", true, "a%", false, false},
		// LIKE pattern with an embedded single quote written as a doubled quote.
		// The extractor must hold the LOGICAL (unescaped) value O'Brien% so the
		// handler can re-escape it when emitting synthetic SQL.
		{"SHOW DATABASES LIKE 'O''Brien%'", DBShow, "DATABASES", "", true, "O'Brien%", false, false},
		{"SELECT 1", DBNone, "", "", false, "", false, false},
	}
	for _, c := range cases {
		got, err := ParseDBLevel(e, c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		if got.Kind != c.kind || got.ShowWhat != c.showWhat || got.DB != c.db ||
			got.HasLike != c.hasLike || got.Like != c.like || got.LikeNot != c.likeNot || got.LikeCaseInsensitive != c.likeCI {
			t.Errorf("%q: got %+v", c.sql, got)
		}
	}
}

func TestParseDBLevel_distinguishesUnresolvedDatabaseClause(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql          string
		wantClause   bool
		wantResolved bool
		wantDB       string
	}{
		{sql: "SHOW DICTIONARIES", wantClause: false, wantResolved: false},
		{sql: "SHOW DICTIONARIES FROM db1", wantClause: true, wantResolved: true, wantDB: "db1"},
		{sql: "SHOW DICTIONARIES FROM {db:Identifier}", wantClause: true, wantResolved: false},
		{sql: "SHOW DICTIONARIES IN {db:Identifier}", wantClause: true, wantResolved: false},
		{sql: "SHOW DICTIONARIES FROM hg_safe WHERE name IN other", wantClause: true, wantResolved: true, wantDB: "hg_safe"},
		{sql: "SHOW DICTIONARIES FROM other WHERE name IN hg_safe", wantClause: true, wantResolved: true, wantDB: "other"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			got, err := ParseDBLevel(e, tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if got.HasDBClause != tc.wantClause || got.DBResolved != tc.wantResolved || got.DB != tc.wantDB {
				t.Fatalf("got %+v, want clause=%v resolved=%v db=%q", got, tc.wantClause, tc.wantResolved, tc.wantDB)
			}
		})
	}
}

func TestParseDBLevel_showPrefixesPrecedeKindAndDatabaseClause(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql           string
		wantShow      string
		wantFull      bool
		wantTemporary bool
		wantClause    bool
		wantResolved  bool
		wantDB        string
	}{
		{sql: "SHOW FULL DICTIONARIES FROM hg_safe", wantShow: "DICTIONARIES", wantFull: true, wantClause: true, wantResolved: true, wantDB: "hg_safe"},
		{sql: "SHOW TEMPORARY DICTIONARIES IN db1", wantShow: "DICTIONARIES", wantTemporary: true, wantClause: true, wantResolved: true, wantDB: "db1"},
		{sql: "SHOW FULL TEMPORARY DICTIONARIES FROM {db:Identifier}", wantShow: "DICTIONARIES", wantFull: true, wantTemporary: true, wantClause: true},
		{sql: "SHOW FULL TABLES FROM hg_safe", wantShow: "TABLES", wantFull: true, wantClause: true, wantResolved: true, wantDB: "hg_safe"},
		{sql: "SHOW TEMPORARY TABLES IN db1", wantShow: "TABLES", wantTemporary: true, wantClause: true, wantResolved: true, wantDB: "db1"},
		{sql: "SHOW TEMPORARY FULL DICTIONARIES FROM hg_safe", wantShow: "FULL", wantTemporary: true},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			got, err := ParseDBLevel(e, tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if got.ShowWhat != tc.wantShow || got.ShowFull != tc.wantFull || got.ShowTemporary != tc.wantTemporary ||
				got.HasDBClause != tc.wantClause ||
				got.DBResolved != tc.wantResolved || got.DB != tc.wantDB {
				t.Fatalf("got %+v, want show=%q full=%v temporary=%v clause=%v resolved=%v db=%q", got, tc.wantShow, tc.wantFull, tc.wantTemporary, tc.wantClause, tc.wantResolved, tc.wantDB)
			}
		})
	}
}

func TestParseDBLevel_showDatabaseClauseUsesParserIdentifierAuthority(t *testing.T) {
	e := newTestEngine(t)
	for _, db := range []string{"system", "default", "select", "from", "table", "settings", "123db"} {
		sql := "SHOW DICTIONARIES FROM " + db
		t.Run(db, func(t *testing.T) {
			got, err := ParseDBLevel(e, sql)
			if err != nil {
				t.Fatal(err)
			}
			if got.ShowWhat != "DICTIONARIES" || !got.HasDBClause || !got.DBResolved || got.DB != db {
				t.Fatalf("got %+v, want resolved database %q", got, db)
			}
		})
	}

	got, err := ParseDBLevel(e, "SHOW DICTIONARIES FROM {db:Identifier}")
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasDBClause || got.DBResolved || got.DB != "" {
		t.Fatalf("parameterized target got %+v, want explicit unresolved clause", got)
	}
}

func TestDatabaseTarget(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql         string
		db          string
		ifNotExists bool
		ifExists    bool
	}{
		{"CREATE DATABASE db", "db", false, false},
		{"CREATE DATABASE IF NOT EXISTS db", "db", true, false},
		{"DROP DATABASE db", "db", false, false},
		{"DROP DATABASE IF EXISTS db", "db", false, true},
	}
	for _, c := range cases {
		ast, err := e.ParseOne(c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		db, ine, ie, err := DatabaseTarget(ast)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		if db != c.db || ine != c.ifNotExists || ie != c.ifExists {
			t.Errorf("%q: db=%q ine=%v ie=%v", c.sql, db, ine, ie)
		}
	}
}

// TestDatabaseTarget_nonDBNode confirms DatabaseTarget errors (not panics or
// silently returns "") on a node that is neither create_database nor drop_database.
func TestDatabaseTarget_nonDBNode(t *testing.T) {
	e := newTestEngine(t)
	for _, sql := range []string{"SELECT 1", "CREATE TABLE db.t (x Int32) ENGINE = Memory", "DROP TABLE db.t"} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		if _, _, _, err := DatabaseTarget(ast); err == nil {
			t.Errorf("%q: DatabaseTarget err=nil, want non-nil (not a create/drop database node)", sql)
		}
	}
}

// TestParseDBLevel_columnsFamilyBindsTableThenDatabase pins ClickHouse's
// SHOW [EXTENDED] [FULL] COLUMNS {FROM|IN} <table> [{FROM|IN} <database>]
// grammar (and the INDEX/INDEXES/KEYS spelling of the same shape), whose
// clause order is the reverse of the SHOW TABLES family's.
func TestParseDBLevel_columnsFamilyBindsTableThenDatabase(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql             string
		wantShow        string
		wantExtended    bool
		wantFull        bool
		wantTableClause bool
		wantTable       string
		wantDBClause    bool
		wantDBResolved  bool
		wantDB          string
	}{
		{sql: "SHOW COLUMNS FROM db1__t FROM hg_safe", wantShow: "COLUMNS",
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true, wantDBResolved: true, wantDB: "hg_safe"},
		{sql: "SHOW COLUMNS FROM hg_safe.db1__t", wantShow: "COLUMNS",
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true, wantDBResolved: true, wantDB: "hg_safe"},
		{sql: "SHOW COLUMNS FROM db1__t", wantShow: "COLUMNS",
			wantTableClause: true, wantTable: "db1__t"},
		{sql: "SHOW EXTENDED COLUMNS FROM db1__t FROM hg_safe", wantShow: "COLUMNS", wantExtended: true,
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true, wantDBResolved: true, wantDB: "hg_safe"},
		{sql: "SHOW EXTENDED FULL COLUMNS FROM db1__t IN hg_unsafe", wantShow: "COLUMNS", wantExtended: true, wantFull: true,
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true, wantDBResolved: true, wantDB: "hg_unsafe"},
		{sql: "SHOW FULL COLUMNS FROM db1__t IN hg_unsafe", wantShow: "COLUMNS", wantFull: true,
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true, wantDBResolved: true, wantDB: "hg_unsafe"},
		{sql: "SHOW INDEX FROM hg_safe.db1__t", wantShow: "INDEX",
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true, wantDBResolved: true, wantDB: "hg_safe"},
		{sql: "SHOW INDEXES FROM db1__t FROM hg_safe", wantShow: "INDEXES",
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true, wantDBResolved: true, wantDB: "hg_safe"},
		{sql: "SHOW KEYS FROM db1__t FROM hg_safe", wantShow: "KEYS",
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true, wantDBResolved: true, wantDB: "hg_safe"},
		{sql: "SHOW EXTENDED INDEX FROM db1__t FROM hg_safe", wantShow: "INDEX", wantExtended: true,
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true, wantDBResolved: true, wantDB: "hg_safe"},
		// ClickHouse's own precedence: an explicit database clause wins over the
		// qualifier of the table clause.
		{sql: "SHOW COLUMNS FROM hg_unsafe.db1__t FROM hg_safe", wantShow: "COLUMNS",
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true, wantDBResolved: true, wantDB: "hg_safe"},
		// The LIKE/WHERE tail still resumes after both clauses.
		{sql: "SHOW COLUMNS FROM db1__t FROM hg_safe LIKE 'a%'", wantShow: "COLUMNS",
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true, wantDBResolved: true, wantDB: "hg_safe"},
		// An explicit but unresolvable database target stays explicit-and-unresolved,
		// exactly as the TABLES/DICTIONARIES family already does.
		{sql: "SHOW COLUMNS FROM db1__t FROM {db:Identifier}", wantShow: "COLUMNS",
			wantTableClause: true, wantTable: "db1__t", wantDBClause: true},
		// An unresolvable table target keeps the clause explicit without a name.
		{sql: "SHOW COLUMNS FROM {tbl:Identifier} FROM hg_safe", wantShow: "COLUMNS",
			wantTableClause: true, wantDBClause: true, wantDBResolved: true, wantDB: "hg_safe"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			got, err := ParseDBLevel(e, tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if got.ShowWhat != tc.wantShow || got.ShowExtended != tc.wantExtended || got.ShowFull != tc.wantFull ||
				got.HasTableClause != tc.wantTableClause || got.ShowTable != tc.wantTable ||
				got.HasDBClause != tc.wantDBClause || got.DBResolved != tc.wantDBResolved || got.DB != tc.wantDB {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

// TestParseDBLevel_nonColumnsFamilyGrammarIsUnchanged is the regression half:
// the reversed grammar must not leak into the families whose single FROM/IN
// really does name a database.
func TestParseDBLevel_nonColumnsFamilyGrammarIsUnchanged(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct{ sql, want string }{
		{"SHOW TABLES FROM hg_safe", "hg_safe"},
		{"SHOW DICTIONARIES FROM hg_safe", "hg_safe"},
		{"SHOW FULL DICTIONARIES IN db1", "db1"},
		{"SHOW SOMETHINGNEW FROM hg_safe", "hg_safe"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			got, err := ParseDBLevel(e, tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if !got.HasDBClause || !got.DBResolved || got.DB != tc.want || got.HasTableClause {
				t.Fatalf("got %+v, want database-only clause %q", got, tc.want)
			}
		})
	}
}
