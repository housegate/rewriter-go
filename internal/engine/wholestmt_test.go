package engine

import (
	"errors"
	"testing"
)

func TestCheckParsedInFull(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want string // "" = parsed in full; otherwise the exact error text
	}{
		// Parsed in full.
		{"select literal", "SELECT 1", ""},
		{"order by asc", "SELECT * FROM t ORDER BY a ASC", ""},
		{"nested calls end in brackets", "SELECT f(g(a)) FROM t", ""},
		{"window ends in bracket", "SELECT * FROM t WINDOW w AS (ORDER BY a)", ""},
		{"brackets of every kind", "SELECT {p:String}, [1, 2], {'a': 1}, (1)", ""},
		{"alias after comment", "SELECT * FROM t /* c */ XYZ", ""},
		{"trailing semicolons", "SELECT 1;;", ""},
		{"trailing line comment", "SELECT * FROM t -- c", ""},
		{"values", "INSERT INTO t VALUES (1), (2)", ""},
		{"format without payload", "INSERT INTO t FORMAT CSV", ""},
		{"format payload is data", "INSERT INTO t FORMAT CSV 1,(2", ""},
		{"json payload is data", `INSERT INTO t FORMAT JSONEachRow {"a":1}`, ""},
		{"table settings", "CREATE TABLE t (a Int32) ENGINE = Memory SETTINGS max_threads = 1", ""},
		{"select settings then format", "SELECT * FROM t SETTINGS max_threads = 1 FORMAT JSON", ""},
		{"command keeps its text", "RENAME TABLE a TO b XYZ", ""},
		{"insert select format", "INSERT INTO t SELECT * FROM u FORMAT JSON", ""},
		{"insert select format line comment", "INSERT INTO t SELECT * FROM u FORMAT JSON -- c", ""},
		{"insert select format block comment", "INSERT INTO t SELECT * FROM u FORMAT JSON /* c */", ""},

		// Not parsed in full: the parser stopped early.
		{"engine then junk", "CREATE TABLE db1.n ENGINE = Memory XYZ AS SELECT * FROM phys.x",
			`engine: parse: statement was not parsed in full: the parser stopped before "XYZ AS SELECT * FROM phys.x"`},
		{"order by then junk", "CREATE TABLE db1.n (a Int32) ENGINE = MergeTree ORDER BY a XYZ SETTINGS storage_policy = 'x'",
			`engine: parse: statement was not parsed in full: the parser stopped before "XYZ SETTINGS storage_policy = 'x'"`},
		{"clone with empty", "CREATE TABLE db1.n AS db1.src ENGINE = Memory EMPTY AS SELECT * FROM phys.x",
			`engine: parse: statement was not parsed in full: the parser stopped before "EMPTY AS SELECT * FROM phys.x"`},
		{"two selects", "SELECT * FROM t SELECT * FROM phys.x",
			`engine: parse: statement was not parsed in full: the parser stopped before "SELECT * FROM phys.x"`},
		{"junk before brackets", "SELECT * FROM t AS a XYZ WHERE f(b)",
			`engine: parse: statement was not parsed in full: the parser stopped before "XYZ WHERE f(b)"`},
		{"with ties", "SELECT * FROM t LIMIT 1 WITH TIES",
			`engine: parse: statement was not parsed in full: the parser stopped before "WITH TIES"`},
		{"statement settings discarded", "DROP TABLE t SETTINGS max_threads = 1",
			`engine: parse: statement was not parsed in full: the parser stopped before "SETTINGS max_threads = 1"`},
		{"long tail is cut", "CREATE TABLE db1.n ENGINE = Memory XYZ AS SELECT a, b, c, d, e, f FROM phys.x",
			`engine: parse: statement was not parsed in full: the parser stopped before "XYZ AS SELECT a, b, c, d, e, f FROM phys…"`},

		// Not parsed in full: Polyglot closed a bracket the input left open.
		{"unclosed subquery", "SELECT * FROM (SELECT 1",
			`engine: parse: statement was not parsed in full: "(" is never closed in "(SELECT 1"`},
		{"unclosed in list", "SELECT * FROM t WHERE a IN (1, 2",
			`engine: parse: statement was not parsed in full: "(" is never closed in "(1, 2"`},
		{"stray closer", "SELECT (1))",
			`engine: parse: statement was not parsed in full: unmatched ")" before ")"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatalf("ParseOne: %v", err)
			}
			err = CheckParsedInFull(e, tc.sql, ast)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
			if !errors.Is(err, ErrNotParsedInFull) {
				t.Fatalf("err = %v does not wrap ErrNotParsedInFull", err)
			}
		})
	}
}
