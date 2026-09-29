package engine

import "testing"

func TestStripCreateTableEmpty(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name, sql, want string
		ok              bool
	}{
		{"empty modifier", "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM db1.p",
			"CREATE TABLE db1.n ENGINE = Memory  AS SELECT * FROM db1.p", true},
		{"lower-case modifier", "create table db1.n engine = Memory empty as select 1",
			"create table db1.n engine = Memory  as select 1", true},
		{"table named empty keeps its name", "CREATE TABLE db1.empty ENGINE = Memory EMPTY AS SELECT 1",
			"CREATE TABLE db1.empty ENGINE = Memory  AS SELECT 1", true},
		{"plain ctas is untouched", "CREATE TABLE db1.n ENGINE = Memory AS SELECT 1", "", false},
		{"table named empty without modifier", "CREATE TABLE db1.empty AS SELECT 1", "", false},
		{"clone of a table named empty", "CREATE TABLE db1.n AS db1.empty", "", false},
		{"quoted identifier is not the keyword", "CREATE TABLE db1.n (`EMPTY` UInt8) ENGINE = Memory", "", false},
		{"not a create table", "SELECT 1 AS empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := StripCreateTableEmpty(e, tc.sql)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				if got != tc.sql {
					t.Fatalf("unmatched statement changed: %q", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("stripped = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestInsertCreateTableEmpty(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct{ name, generated, want string }{
		{"parenthesized body", `CREATE TABLE phys."db1.n" ENGINE=Memory AS (SELECT * FROM phys."db1.p" "db1.p")`,
			`CREATE TABLE phys."db1.n" ENGINE=Memory EMPTY AS (SELECT * FROM phys."db1.p" "db1.p")`},
		{"trailing comment", `CREATE TABLE phys."db1.n" ENGINE=Memory AS ((SELECT 1)) COMMENT 'c'`,
			`CREATE TABLE phys."db1.n" ENGINE=Memory EMPTY AS ((SELECT 1)) COMMENT 'c'`},
		{"column list and a body alias", `CREATE TABLE phys."db1.n" (a UInt64 DEFAULT 1) ENGINE=Memory AS SELECT 1 AS a`,
			`CREATE TABLE phys."db1.n" (a UInt64 DEFAULT 1) ENGINE=Memory EMPTY AS SELECT 1 AS a`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := InsertCreateTableEmpty(e, tc.generated)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	for _, generated := range []string{
		`CREATE TABLE phys."db1.n" ENGINE=Memory`,
		`SELECT 1`,
	} {
		if got, err := InsertCreateTableEmpty(e, generated); err == nil {
			t.Fatalf("%q: got %q, want an error", generated, got)
		}
	}
}
