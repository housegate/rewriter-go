package engine

import "testing"

func TestParseObjectTarget(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql       string
		verb      ObjectVerb
		temporary bool
		objType   string
		db, table string
	}{
		{"EXISTS TABLE db.t", VerbExists, false, "TABLE", "db", "t"},
		{"EXISTS db.t", VerbExists, false, "TABLE", "db", "t"},
		{"EXISTS t", VerbExists, false, "TABLE", "", "t"},
		{"EXISTS TEMPORARY TABLE t", VerbExists, true, "TABLE", "", "t"},
		{"EXISTS DATABASE db", VerbExists, false, "DATABASE", "", "db"},
		{"EXISTS VIEW v", VerbExists, false, "VIEW", "", "v"},
		{"EXISTS DICTIONARY d", VerbExists, false, "DICTIONARY", "", "d"},
		{"SHOW CREATE TABLE db.t", VerbShowCreate, false, "TABLE", "db", "t"},
		{"SHOW CREATE t", VerbShowCreate, false, "TABLE", "", "t"},
		{"SHOW CREATE DATABASE db", VerbShowCreate, false, "DATABASE", "", "db"},
		{"SHOW CREATE VIEW v", VerbShowCreate, false, "VIEW", "", "v"},
		{"SHOW CREATE `weird.tbl`", VerbShowCreate, false, "TABLE", "", "weird.tbl"},
		// Not ours: SHOW TABLES/DATABASES are db-level; SELECT/USE are other handlers.
		{"SHOW TABLES", VerbNone, false, "", "", ""},
		{"SHOW DATABASES", VerbNone, false, "", "", ""},
		{"USE db", VerbNone, false, "", "", ""},
		{"SELECT 1", VerbNone, false, "", "", ""},
	}
	for _, c := range cases {
		got, err := ParseObjectTarget(e, c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		if got.Verb != c.verb || got.Temporary != c.temporary || got.ObjType != c.objType ||
			got.DB != c.db || got.Table != c.table {
			t.Errorf("%q: got %+v", c.sql, got)
		}
	}
}
