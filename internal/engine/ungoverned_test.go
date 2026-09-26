package engine

import (
	"fmt"
	"strings"
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
		"DELETE WHERE `in`(a, (1, 2))":                                              true, // residual round 4 accepted cost
		"DELETE WHERE `in`(42, (1, 2))":                                             false,
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

// TestOpaqueInRuleCoversEverySpelling pins the operand-region IN rule of
// every opaque scanner (spec 2026-09-26, residual round 4) over four axes:
// spelling (the IN keyword alone and after NOT / GLOBAL / GLOBAL NOT, the
// keyword-lexed callable `in(` in several spacings, and every IN-family
// function name bare / backtick-quoted / double-quoted in several cases) ×
// left operand or prefix (a VAR, keyword-lexed names, a CASE … END, NOT,
// GLOBAL, a unary sign, nothing) × operand form × position. The token before
// an occurrence never matters: every non-literal operand is refused in every
// combination, and every literal operand passes in every combination — except
// in a SHOW body, where a named function call is refused by the SHOW rule.
func TestOpaqueInRuleCoversEverySpelling(t *testing.T) {
	e := newTestEngine(t)
	type spelling struct {
		form  string // %s = operand
		named bool   // a named call (VAR / quoted name followed by "(")
	}
	spellings := []spelling{
		{"IN %s", false}, {"NOT IN %s", false}, {"GLOBAL IN %s", false}, {"GLOBAL NOT IN %s", false},
		{"not in %s", false}, {"global not in %s", false},
		{"in(42, %s)", false}, {"IN(42, %s)", false}, {"In /* c */ (42, %s)", false}, {"in  (42, %s)", false},
	}
	for _, name := range []string{"in", "notIn", "globalIn", "globalNotIn", "nullIn", "notNullIn",
		"globalNullIn", "globalNotNullIn", "inIgnoreSet", "notInIgnoreSet", "NOTIN", "GlobalNotIn", "NULLIN"} {
		if name != "in" { // bare in( lexes as the IN keyword: the keyword-callable rows above
			spellings = append(spellings, spelling{name + "(42, %s)", true}, spelling{name + " /* c */ (42, %s)", true})
		}
		spellings = append(spellings, spelling{"`" + name + "`(42, %s)", true}, spelling{`"` + name + `"(42, %s)`, true})
	}
	prefixes := []string{"a ", "date ", "key ", "timestamp ", "first ", "table ", "interval ", "final ", "all ",
		"CASE WHEN a THEN 1 END ", "NOT ", "not ", "GLOBAL ", "- ", "(", ""}
	refused := []string{"p", "`db2.x`", `"db2.x"`, "phys.x", `phys."db2.x"`, "hg_safe.db1__t", "{p:Identifier}",
		"((db1.p))", "(`db2.x`)", "(SELECT 1)", "(1, b)", "(b, 1)", "(42, (SELECT 1))", "[1, b]", "tuple(1, b)",
		"`partition`", "(1, 2 + b)", "b"}
	literals := []string{"(1, 2)", "(-1, +2)", "('x', 'y')", "(NULL, TRUE, false)", "((1, 2), (3, 4))",
		"[1, 2]", "([1, 2], [3])", "1", "'x'", "NULL", "-1"}
	type position struct {
		name string
		wrap string // %s = expression
		scan func(string) bool
	}
	alter := func(s string) bool { return OpaqueTextIsUngoverned(e, s) }
	insert := func(s string) bool { return OpaqueInsertQueryIsUngoverned(e, s) }
	show := func(s string) bool {
		info, err := ParseDBLevel(e, s)
		if err != nil {
			return true
		}
		return ShowBodyIsUngoverned(e, info, s)
	}
	positions := []position{
		{"alter update assignment", "ALTER TABLE db1.o UPDATE b = %s WHERE 1", alter},
		{"alter update where", "ALTER TABLE db1.o UPDATE b = 1 WHERE %s", alter},
		{"alter delete where", "ALTER TABLE db1.o DELETE WHERE %s", alter},
		{"multi-command tail", "ALTER TABLE db1.o DELETE WHERE 1, UPDATE b = 1, c = 2 WHERE %s", alter},
		{"modify ttl", "ALTER TABLE db1.o MODIFY TTL d + INTERVAL 1 DAY DELETE WHERE %s", alter},
		{"modify column default", "ALTER TABLE db1.o MODIFY COLUMN b UInt8 DEFAULT %s", alter},
		{"modify column materialized", "ALTER TABLE db1.o MODIFY COLUMN b UInt8 MATERIALIZED %s", alter},
		{"add column default", "ALTER TABLE db1.o ADD COLUMN c UInt8 DEFAULT %s", alter},
		{"add projection body", "ADD PROJECTION p(SELECT a WHERE %s)", alter},
		{"opaque insert select", "SETTINGS max_threads = 1 SELECT %s", insert},
		{"opaque insert values", "SETTINGS max_threads = 1 VALUES (%s)", insert},
		{"opaque insert format values", "SETTINGS max_threads = 1 FORMAT Values (%s)", insert},
		{"show columns where", "SHOW COLUMNS FROM db1.o WHERE %s", show},
		{"show dictionaries limit", "SHOW DICTIONARIES FROM default LIMIT %s", show},
	}
	var refusedChecked, literalChecked int
	for _, pos := range positions {
		for _, sp := range spellings {
			for _, prefix := range prefixes {
				closeParen := ""
				if prefix == "(" {
					closeParen = ")"
				}
				for _, operand := range refused {
					text := fmt.Sprintf(pos.wrap, prefix+fmt.Sprintf(sp.form, operand)+closeParen)
					refusedChecked++
					if !pos.scan(text) {
						t.Errorf("%s: %q passed, want refused", pos.name, text)
					}
				}
				for _, operand := range literals {
					text := fmt.Sprintf(pos.wrap, prefix+fmt.Sprintf(sp.form, operand)+closeParen)
					want := strings.HasPrefix(pos.name, "show") && sp.named
					literalChecked++
					if got := pos.scan(text); got != want {
						t.Errorf("%s: %q ungoverned=%v, want %v", pos.name, text, got, want)
					}
				}
			}
		}
	}
	t.Logf("%d positions x %d spellings x %d prefixes: %d refused-operand and %d literal-operand texts",
		len(positions), len(spellings), len(prefixes), refusedChecked, literalChecked)
}

// TestOpaqueInRegionEdges pins the region edges. An unquoted PARTITION after IN
// is ClickHouse's IN PARTITION clause, not an IN occurrence (measured on 26.8:
// `a IN partition` is a syntax error in every expression position, while the
// quoted name reads the table). A tuple( / array( literal constructor directly
// after IN contributes its argument group; as a callable's argument it is a
// named token of the region and is refused. A missing or unterminated region
// fails closed.
func TestOpaqueInRegionEdges(t *testing.T) {
	e := newTestEngine(t)
	for text, want := range map[string]bool{
		"ALTER TABLE db1.o UPDATE b = 1 IN PARTITION 5 WHERE 1":          false,
		"ALTER TABLE db1.o DELETE IN PARTITION ID 'x' WHERE 1":           false,
		"ALTER TABLE db1.o CLEAR COLUMN b IN PARTITION tuple()":          false,
		"ALTER TABLE db1.o MATERIALIZE INDEX i IN PARTITION 5":           false,
		"ALTER TABLE db1.o DELETE WHERE a IN `partition`":                true,
		"ALTER TABLE db1.o DELETE IN PARTITION 5 WHERE a IN (`db2.x`)":   true,
		"ALTER TABLE db1.o UPDATE b = 1 IN PARTITION (SELECT 1) WHERE 1": true,
		"ALTER TABLE db1.o DELETE WHERE a IN":                            true,
		"ALTER TABLE db1.o DELETE WHERE a IN (1, 2":                      true,
		"ALTER TABLE db1.o DELETE WHERE a IN [1, 2":                      true,
		"ALTER TABLE db1.o DELETE WHERE a IN tuple(1, 2)":                false,
		"ALTER TABLE db1.o DELETE WHERE a NOT IN array(1, -2)":           false,
		"ALTER TABLE db1.o DELETE WHERE a IN tuple(1, b)":                true,
		"ALTER TABLE db1.o DELETE WHERE a IN tuple(1, (SELECT 1))":       true,
		"ALTER TABLE db1.o DELETE WHERE a IN tuple":                      true,
		"ALTER TABLE db1.o DELETE WHERE in(42, tuple(1, 2))":             true,
		"ALTER TABLE db1.o DELETE WHERE notIn(42, [1, 2])":               false,
		"ALTER TABLE db1.o DELETE WHERE notIn":                           true,
	} {
		if got := OpaqueTextIsUngoverned(e, text); got != want {
			t.Errorf("OpaqueTextIsUngoverned(%q) = %v, want %v", text, got, want)
		}
	}
}
