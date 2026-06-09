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
