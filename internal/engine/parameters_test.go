package engine

import "testing"

func TestTablePositionParameter(t *testing.T) {
	e := newTestEngine(t)
	for sql, want := range map[string]bool{
		"SELECT * FROM {p:Identifier}":                                  true,
		"SELECT * FROM db1.{p:Identifier}":                              true,
		"SELECT * FROM {d:Identifier}.t":                                true,
		"SELECT * FROM db1.o WHERE a IN {p:Identifier}":                 true,
		"SELECT * FROM db1.o WHERE in(a, db1.{p:Identifier})":           true,
		"INSERT INTO db1.{p:Identifier} VALUES (1)":                     true,
		"CREATE MATERIALIZED VIEW db1.mv TO {p:Identifier} AS SELECT 1": true,
		"SELECT {c:Identifier} FROM db1.o":                              false,
		"SELECT * FROM db1.o WHERE a = {v:UInt64}":                      false,
		"SELECT * FROM db1.o WHERE a IN (1, 2)":                         false,

		// Controller review (task-3 fix round), ruling 2: ALTER ... UPDATE only
		// scans the target (between ALTER TABLE and UPDATE) -- a parameter in
		// the assignment/predicate tail is a column/value position and stays
		// allowed, while one in the target is refused exactly like every other
		// table position.
		"ALTER TABLE db1.o UPDATE a = {c:Identifier} WHERE 1": false,
		"ALTER TABLE db1.{p:Identifier} UPDATE a = 1 WHERE 1": true,

		// Controller review, ruling 3: CREATE/DROP DATABASE's own target is a
		// database position too (previously unmodelled -- neither the read walk
		// nor InspectWrite's writeSlots visits a create_database/drop_database
		// node's "name" field at all, so this returned false before the fix).
		"CREATE DATABASE {d:Identifier}": true,
		"DROP DATABASE {d:Identifier}":   true,
		"CREATE DATABASE db1":            false,
		"DROP DATABASE db1":              false,

		// Controller re-review (task-3 fix round 2): the SHOW COLUMNS/INDEX
		// family's FIRST FROM/IN clause names a TABLE, tracked by ParseDBLevel
		// in HasTableClause/ShowTable/ShowTableResolved rather than
		// HasDBClause/DB/DBResolved (dblevel.go's parseShowTableThenDatabase).
		// dbLevelHoldsParameter's DBShow branch only checked the latter, so a
		// parameter in the table clause was never reported -- fixed by also
		// checking HasTableClause && !ShowTableResolved.
		"SHOW COLUMNS FROM {p:Identifier}":        true,
		"SHOW COLUMNS FROM db1.{p:Identifier}":    true,
		"SHOW INDEX FROM {p:Identifier}":          true,
		"SHOW INDEX FROM db1.{p:Identifier}":      true,
		"SHOW COLUMNS FROM t FROM {d:Identifier}": true, // database clause, already covered pre-fix
		"SHOW COLUMNS FROM db1.o":                 false,
		"SHOW COLUMNS FROM o FROM db1":            false,
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("%s: parse: %v", sql, err)
		}
		got, err := TablePositionParameter(e, ast, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if got != want {
			t.Errorf("TablePositionParameter(%s) = %v, want %v", sql, got, want)
		}
	}
}

func TestIdentifierParameterInText(t *testing.T) {
	e := newTestEngine(t)
	for sql, want := range map[string]bool{
		"EXISTS TABLE db1.{p:Identifier}":        true,
		"RENAME TABLE {d:Identifier}.t TO db1.z": true,
		// identifierParameterEnd (lexical.go) works on TOKENS, not source
		// bytes, so it tolerates the same whitespace ClickHouse's own tokenizer
		// tolerates inside the braces -- "{d : Identifier}" lexes to the exact
		// same 5 tokens (L_BRACE, VAR, COLON, VAR, R_BRACE) as the unspaced
		// spelling (measured). Controller review ruling 1 requires the
		// tokenizer-based scan, which makes this MORE conservative than the
		// prior hand-rolled string scan (which required byte-exact adjacency);
		// that is an intentional, documented widening, not a regression: T2's
		// job is "never rewrite past an Identifier parameter here", and
		// over-refusing an edge spelling is safe where under-refusing is not.
		"USE {d : Identifier}":    true,
		"EXISTS TABLE db1.t":      false,
		"SYSTEM RELOAD CONFIG":    false,
		"SELECT '{p:Identifier}'": false, // inside a string literal: one STRING
		// token, no separate L_BRACE -- confirmed via e.Tokenize.

		// Controller review, Critical finding: the prior hand-rolled scanner
		// did not recognize "#", "#!" or "//" line comments at all, so it fell
		// through to its generic quote-handling case on the "'" inside "it's"
		// and treated the rest of the (real) comment text as an unterminated
		// string, silently swallowing the genuine parameter that followed. The
		// tokenizer-based scan recognizes all five ClickHouse comment openers
		// as comments regardless of what they contain, so each of these must
		// still find the parameter that follows.
		"EXISTS TABLE db1.o -- it's\n db1.{p:Identifier}":  true,
		"EXISTS TABLE db1.o # it's\n db1.{p:Identifier}":   true,
		"EXISTS TABLE db1.o #! it's\n db1.{p:Identifier}":  true,
		"EXISTS TABLE db1.o // it's\n db1.{p:Identifier}":  true,
		"EXISTS TABLE db1.o /* it's */ db1.{p:Identifier}": true,

		// Controller review, Critical finding: an unterminated string/comment
		// must fail closed (return true) rather than silently reporting no
		// parameter -- measured, e.Tokenize does not itself error on either
		// (a single-quote string with no closing quote absorbs everything to
		// EOF; an unterminated /* comment just stops the token stream early),
		// so this is exactly what the sentinel-coverage check in
		// IdentifierParameterInText exists to catch.
		"EXISTS TABLE db1.'unterminated":                     true,
		"EXISTS TABLE db1./* unterminated db1.t":             true,
		"EXISTS TABLE db1.t -- a normal trailing comment":    false,
		"EXISTS TABLE db1.t /* a normal trailing comment */": false,
	} {
		if got := IdentifierParameterInText(e, sql); got != want {
			t.Errorf("IdentifierParameterInText(%s) = %v, want %v", sql, got, want)
		}
	}
}
