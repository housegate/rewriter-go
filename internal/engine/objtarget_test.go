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

func TestParseObjectTarget_trailingAndAccessEntity(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql      string
		table    string
		trailing bool
		entity   bool
	}{
		{"EXISTS TABLE db.t", "t", false, false},
		{"EXISTS TABLE db.t;", "t", false, false},
		{"EXISTS TABLE db.t FORMAT JSON", "t", true, false},
		{"EXISTS TABLE db.t XYZ", "t", true, false},
		{"EXISTS TABLE system.one", "", false, false},
		{"EXISTS", "", false, false},
		{"SHOW CREATE TABLE user", "user", false, false},
		{"SHOW CREATE `user`", "user", false, false},
		{"SHOW CREATE TABLE db.user", "user", false, false},
		{"SHOW CREATE USER", "USER", false, true},
		{"SHOW CREATE USER u1", "USER", true, true},
		{"SHOW CREATE QUOTAS", "QUOTAS", false, true},
		{"SHOW CREATE ROLE r", "ROLE", true, true},
		{"SHOW CREATE PROFILES", "PROFILES", false, true},
		{"SHOW CREATE ROW POLICY p ON db.t", "", false, true},
		{"SHOW CREATE SETTINGS PROFILE p", "", false, true},
		{"SHOW CREATE POLICIES", "POLICIES", false, true},
		{"SHOW CREATE MASKING POLICY p ON db.t", "MASKING", true, true},
		{"SHOW CREATE t", "t", false, false},
		// EXISTS has no access-entity form.
		{"EXISTS USER", "USER", false, false},
	}
	for _, c := range cases {
		got, err := ParseObjectTarget(e, c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		if got.Table != c.table || got.Trailing != c.trailing || got.AccessEntity != c.entity {
			t.Errorf("%q: Table=%q Trailing=%v AccessEntity=%v, want %q %v %v", c.sql, got.Table, got.Trailing, got.AccessEntity, c.table, c.trailing, c.entity)
		}
	}
}
