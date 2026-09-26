package engine

import (
	"fmt"
	"testing"
)

func TestOpaqueTextIsUngoverned(t *testing.T) {
	e := newTestEngine(t)
	for text, want := range map[string]bool{
		"DELETE WHERE a = 1":                                                        false,
		"DELETE WHERE a IN (1, 2)":                                                  false,
		"DELETE WHERE a IN tuple(1, 2)":                                             false,
		"DELETE IN PARTITION tuple() WHERE a = 1":                                   false,
		"MODIFY TTL d + INTERVAL 1 DAY":                                             false,
		"MODIFY COLUMN b UInt8 DEFAULT 2":                                           false,
		"MOVE PARTITION tuple() TO DISK 'd'":                                        false,
		"ATTACH PART 'x'":                                                           false,
		"FREEZE WITH NAME 'x'":                                                      false,
		"MODIFY COMMENT 'select from with'":                                         false,
		"ADD PROJECTION p(SELECT a ORDER BY b)":                                     false,
		"DELETE WHERE a IN(SELECT 1)":                                               true,
		"DELETE WHERE a IN db1.p":                                                   true,
		"DELETE WHERE a IN p":                                                       true,
		`DELETE WHERE a IN "db2.x"`:                                                 true,
		"DELETE WHERE a NOT IN ((db1.p))":                                           true,
		"DELETE WHERE a GLOBAL IN db1.p":                                            true,
		"DELETE WHERE in(a, db1.p)":                                                 true,
		"DELETE WHERE `in`(a, db1.p)":                                               true,
		`DELETE WHERE "notIn"(a, "db2.x")`:                                          true,
		"DELETE WHERE `globalNotIn`(a, (`db2.x`))":                                  true,
		"DELETE WHERE `in`(a, (1, 2))":                                              false,
		"DELETE WHERE a IN {p:Identifier}":                                          true,
		"MODIFY QUERY SELECT * FROM db1.p":                                          true,
		"MODIFY COLUMN b UInt8 DEFAULT(SELECT 1)":                                   true,
		"MODIFY TTL d DELETE WHERE a IN db1.v":                                      true,
		"FETCH PARTITION tuple() FROM '/x'":                                         true,
		"FETCH PART 'p' FROM '/x'":                                                  true,
		"ATTACH PARTITION tuple() FROM db1.p":                                       true,
		"REPLACE PARTITION tuple() FROM db1.p":                                      true,
		"MOVE PARTITION tuple() TO TABLE db1.p":                                     true,
		"ADD PROJECTION p(SELECT a FROM db1.p)":                                     true,
		"ADD PROJECTION p(SELECT a WHERE a IN (SELECT 1))":                          true,
		"ALTER TABLE db1.o UPDATE b = 1 WHERE 1, DELETE WHERE a IN db1.p":           true,
		"ALTER TABLE db1.o UPDATE b = 1 WHERE 1, FETCH PARTITION tuple() FROM '/x'": true,
	} {
		if got := OpaqueTextIsUngoverned(e, text); got != want {
			t.Errorf("OpaqueTextIsUngoverned(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestOpaqueTextDatabases(t *testing.T) {
	e := newTestEngine(t)
	dbs, ok := OpaqueTextDatabases(e, `ALTER TABLE db1.o UPDATE b = (SELECT max(a) FROM hg_promote.x) WHERE a IN phys."db2.x"`)
	if !ok {
		t.Fatal("tokenize failed")
	}
	want := []string{"db1", "hg_promote", "phys"}
	if len(dbs) != len(want) {
		t.Fatalf("dbs = %v, want %v", dbs, want)
	}
	for i := range want {
		if dbs[i] != want[i] {
			t.Fatalf("dbs = %v, want %v", dbs, want)
		}
	}
}

func TestExpressionPositionHasReads(t *testing.T) {
	e := newTestEngine(t)
	for sql, want := range map[string]bool{
		"UPDATE db1.o SET b = 1 WHERE a = 1":                                                   false,
		"UPDATE db1.o SET b = 1 WHERE a IN db1.p":                                              true,
		"DELETE FROM db1.o WHERE a IN (SELECT a FROM p)":                                       true,
		"INSERT INTO db1.o VALUES (1)":                                                         false,
		"INSERT INTO db1.o VALUES ((SELECT 1 FROM db1.p))":                                     true,
		"INSERT INTO db1.o SELECT * FROM db1.p":                                                false,
		"CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM db1.p":                            false,
		"CREATE TABLE db1.n (a UInt64 DEFAULT 1) ENGINE = MergeTree ORDER BY a":                false,
		"CREATE TABLE db1.n (a UInt64 DEFAULT (SELECT 1 FROM db1.p)) ENGINE = Memory":          true,
		"CREATE TABLE db1.n (a UInt64) ENGINE = MergeTree ORDER BY a IN db1.p":                 true,
		"CREATE MATERIALIZED VIEW db1.mv ENGINE = MergeTree ORDER BY a AS SELECT * FROM db1.o": false,
		"ALTER TABLE db1.o ADD COLUMN c UInt8 DEFAULT a IN db1.p":                              true,
		"ALTER TABLE db1.o REPLACE PARTITION tuple() FROM db1.p":                               true,
		"SELECT * FROM db1.o WHERE a IN db1.p":                                                 false,
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("%s: parse: %v", sql, err)
		}
		got, err := ExpressionPositionHasReads(ast)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if got != want {
			t.Errorf("ExpressionPositionHasReads(%s) = %v, want %v", sql, got, want)
		}
	}
}

// TestOpaqueInRuleCoversEverySpelling closes the IN class for every opaque
// scanner (spec 2026-09-26 residual round 3): spelling (infix keyword,
// keyword-callable, every callable IN-family name bare / quoted / mixed case,
// a comment or whitespace before "(") × operand form × position. A table
// operand is refused in every combination; a literal list passes — except in
// a SHOW body, where every named function call is refused by the SHOW rule
// (only the keyword spellings reach the literal-list case there).
func TestOpaqueInRuleCoversEverySpelling(t *testing.T) {
	e := newTestEngine(t)
	type spelling struct {
		form    string // %s = operand
		keyword bool   // infix or keyword-callable (no named call)
	}
	spellings := []spelling{
		{"a IN %s", true}, {"a NOT IN %s", true}, {"a GLOBAL IN %s", true}, {"a GLOBAL NOT IN %s", true},
		{"in(a, %s)", true}, {"IN(a, %s)", true}, {"In /* c */ (a, %s)", true}, {"in  (a, %s)", true},
	}
	for _, name := range []string{"in", "notIn", "globalIn", "globalNotIn", "nullIn", "notNullIn",
		"globalNullIn", "globalNotNullIn", "inIgnoreSet", "NOTIN", "GlobalNotIn"} {
		if name != "in" { // bare in( lexes as the IN keyword: the keyword-callable rows above
			spellings = append(spellings, spelling{name + "(a, %s)", false})
		}
		spellings = append(spellings, spelling{"`" + name + "`(a, %s)", false}, spelling{`"` + name + `"(a, %s)`, false})
	}
	tables := []string{"p", "db1.p", "`db2.x`", "{p:Identifier}", "((db1.p))", "(`db2.x`)", "(SELECT 1)"}
	const literalList = "(1, 2)"
	type position struct {
		name  string
		wrap  string // %s = expression
		check func(string) bool
	}
	positions := []position{
		{"alter update", "ALTER TABLE db1.o UPDATE b = %s WHERE 1", func(s string) bool { return OpaqueTextIsUngoverned(e, s) }},
		{"alter delete", "DELETE WHERE %s", func(s string) bool { return OpaqueTextIsUngoverned(e, s) }},
		{"modify ttl", "MODIFY TTL d + INTERVAL 1 DAY DELETE WHERE %s", func(s string) bool { return OpaqueTextIsUngoverned(e, s) }},
		{"modify column default", "MODIFY COLUMN b UInt8 DEFAULT %s", func(s string) bool { return OpaqueTextIsUngoverned(e, s) }},
		{"insert select", "SETTINGS max_threads = 1 SELECT %s", func(s string) bool { return OpaqueInsertQueryIsUngoverned(e, s) }},
		{"insert values", "SETTINGS max_threads = 1 VALUES (%s)", func(s string) bool { return OpaqueInsertQueryIsUngoverned(e, s) }},
		{"show body", "SHOW DICTIONARIES FROM default WHERE %s", func(s string) bool {
			info, err := ParseDBLevel(e, s)
			if err != nil {
				return true
			}
			return ShowBodyIsUngoverned(e, info, s)
		}},
	}
	for _, pos := range positions {
		for _, sp := range spellings {
			for _, table := range tables {
				text := fmt.Sprintf(pos.wrap, fmt.Sprintf(sp.form, table))
				if !pos.check(text) {
					t.Errorf("%s: %q passed, want refused", pos.name, text)
				}
			}
			text := fmt.Sprintf(pos.wrap, fmt.Sprintf(sp.form, literalList))
			want := false
			if pos.name == "show body" && !sp.keyword {
				want = true
			}
			if got := pos.check(text); got != want {
				t.Errorf("%s: %q ungoverned=%v, want %v", pos.name, text, got, want)
			}
		}
	}
}
