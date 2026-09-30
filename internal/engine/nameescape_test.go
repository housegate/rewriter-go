package engine

import (
	"strings"
	"testing"
)

// TestDecodeIdentifierEscapes pins the residual ClickHouse decode applied to an
// AST function name (Polyglot-decoded, no span): \N and the backslash escapes
// Polyglot leaves, with ClickHouse's unhex for \x.
func TestDecodeIdentifierEscapes(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"in", "in", true},
		{`\Nin`, "in", true},
		{`i\Nn`, "in", true},
		{`in\N`, "in", true},
		{`\in`, `\in`, true},  // unknown escape keeps its backslash
		{`\\in`, `\in`, true}, // \\ is one backslash
		{`\NjoinGet`, "joinGet", true},
		{`\x69n`, "in", true},
		{`n\x7ZtIn`, "notIn", true}, // ClickHouse unhex: 7*16 + (-1) = 'o'
		{`\xZZ`, "\xef", true},      // (-1)*16 + (-1)
		{`\x6`, "", false},          // fewer than two bytes after \x
		{`ab\`, "", false},          // trailing lone backslash
	} {
		got, ok := decodeIdentifierEscapes(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("decodeIdentifierEscapes(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestClickHouseEscapeFidelity pins the escape table measured on ClickHouse
// 26.2 (DESCRIBE (SELECT 1 AS `…`)): \/ and \= drop the backslash, \e is ESC,
// \: and unknown letters keep it, \x takes any two bytes.
func TestClickHouseEscapeFidelity(t *testing.T) {
	for in, want := range map[string]string{
		`a\/b`: "a/b", `a\=b`: "a=b", `a\eb`: "a\x1bb", `a\:b`: `a\:b`, `a\qb`: `a\qb`,
		`a\'b`: "a'b", `a\"b`: `a"b`, "a\\`b": "a`b", `a\\b`: `a\b`, `a\tb`: "a\tb", `a\0b`: "a\x00b",
		`\x6Z`: "_", `\x7Z`: "o", `\xZ0`: "\xf0", `\x0Z`: "\xff", `\xg1`: "\xf1", `\x-1`: "\xf1",
	} {
		if got, ok := decodeIdentifierEscapes(in); !ok || got != want {
			t.Errorf("decodeIdentifierEscapes(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// TestCanonicalCallableInName: the IN family is matched case-sensitively on a
// name that is already decoded; nothing is decoded here.
func TestCanonicalCallableInName(t *testing.T) {
	for _, n := range []string{"in", "notIn", "globalIn", "globalNotIn", "nullIn", "notNullIn",
		"globalNullIn", "globalNotNullIn", "inIgnoreSet", "globalNotNullInIgnoreSet"} {
		if _, ok := canonicalCallableInName(n); !ok {
			t.Errorf("canonicalCallableInName(%q) = false", n)
		}
	}
	for _, n := range []string{"IN", "In", "NOTIN", "NotIn", "GLOBALIN", `\in`, `\Nin`, `\x69n`, "notin", "isNull"} {
		if _, ok := canonicalCallableInName(n); ok {
			t.Errorf("canonicalCallableInName(%q) = true, want unknown", n)
		}
	}
	// An AST function name is decoded once, by functionName.
	for n, want := range map[string]bool{`\Nin`: true, `i\Nn`: true, `n\x7ZtIn`: true, `\in`: false, "IN": false} {
		_, ok := canonicalCallableInName(functionName(map[string]any{"name": n}))
		if ok != want {
			t.Errorf("functionName(%q) IN-family = %v, want %v", n, ok, want)
		}
	}
}

// TestNameMatchersDoNotDecode: IsStringLookup and SQLBearingSetting compare the
// decoded name they are given (round 2, F3).
func TestNameMatchersDoNotDecode(t *testing.T) {
	for _, n := range []string{"joinGet", "dictGetUInt64", "hasColumnInTable", "JOINGET"} {
		if !IsStringLookup(n) {
			t.Errorf("IsStringLookup(%q) = false", n)
		}
	}
	for _, n := range []string{`\NjoinGet`, `\join`, "notALookup"} {
		if IsStringLookup(n) {
			t.Errorf("IsStringLookup(%q) = true", n)
		}
	}
	for _, n := range []string{"dialect", "polyglot_dialect", "additional_table_filters", "Dialect"} {
		if !SQLBearingSetting(n) {
			t.Errorf("SQLBearingSetting(%q) = false", n)
		}
	}
	for _, n := range []string{`\Ndialect`, "max_threads"} {
		if SQLBearingSetting(n) {
			t.Errorf("SQLBearingSetting(%q) = true", n)
		}
	}
}

// TestOpaqueQuotedNameDecode drives the opaque-text scanners: token text is
// decoded once, exactly as ClickHouse reads the forwarded text.
func TestOpaqueQuotedNameDecode(t *testing.T) {
	e := newTestEngine(t)
	for _, s := range []string{
		"DELETE WHERE `\\Nin`(a, `db2.x`)",
		"DELETE WHERE `i\\Nn`(a, `db2.x`)",
		"DELETE WHERE `in\\N`(a, `db2.x`)",
		"DELETE WHERE \"i\\Nn\"(a, `db2.x`)",
		"DELETE WHERE `not\\x49n`(a, `db2.x`)",
		"DELETE WHERE `glob\\x61lIn`(a, `db2.x`)",
		"DELETE WHERE `n\\x7ZtIn`(a, `db2.x`)",
	} {
		if !OpaqueTextIsUngoverned(e, s) {
			t.Errorf("OpaqueTextIsUngoverned(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"DELETE WHERE `\\in`(a, `db2.x`)",
		"DELETE WHERE \"\\in\"(a, `db2.x`)",
		"DELETE WHERE `\\\\in`(a, `db2.x`)",
		"DELETE WHERE `\\\\x69n`(a, `db2.x`)", // \x69n: an unknown function (F3)
		"DELETE WHERE `\\\\Nin`(a, `db2.x`)",  // \Nin: an unknown function
		"DELETE WHERE `IN`(a, `db2.x`)",
		"DELETE WHERE `NOTIN`(a, `db2.x`)",
	} {
		if OpaqueTextIsUngoverned(e, s) {
			t.Errorf("OpaqueTextIsUngoverned(%q) = true, want false", s)
		}
	}
	if !OpaqueInsertQueryIsUngoverned(e, "SETTINGS x=1 VALUES (`\\NjoinGet`('db2.x','v',1))") {
		t.Errorf("OpaqueInsertQueryIsUngoverned escaped joinGet = false, want true")
	}
}

// TestDecodeQuotedIdentifier decodes source spellings as ClickHouse's
// ParserIdentifier does and classifies what ClickHouse rejects.
func TestDecodeQuotedIdentifier(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		st        quotedDecode
	}{
		{"`ph\\Nys`", "phys", decodedExact},
		{`"ph\Nys"`, "phys", decodedExact},
		{"`hg_\\Nsafe`", "hg_safe", decodedExact},
		{"`hg\\x6Zsafe`", "hg_safe", decodedExact}, // non-hex \x: ClickHouse unhex
		{"`t\\N`", "t", decodedExact},
		{"`t\\\\N`", `t\N`, decodedExact},
		{"`\\x74`", "t", decodedExact},
		{"`a``b`", "a`b", decodedExact},
		{`"a""b"`, `a"b`, decodedExact},
		{"`a\\`b`", "a`b", decodedExact},
		{"“in”", "in", decodedExact},
		{"“a\\Nb”", `a\Nb`, decodedExact},
		{"`\\xff`", "\xff", decodedNotUTF8}, // ClickHouse accepts it
		{"`a\\xZZb`", "a\xefb", decodedNotUTF8},
		{"`\\N`", "", decodedRejected},    // empty identifier
		{"`ab\\x6`", "", decodedRejected}, // \x swallows the closing quote
		{"`abc", "", decodedRejected},
		{"abc", "", decodedRejected},
	} {
		got, st := decodeQuotedIdentifier(tc.raw)
		if st != tc.st || (st != decodedRejected && got != tc.want) {
			t.Errorf("decodeQuotedIdentifier(%q) = %q, %d; want %q, %d", tc.raw, got, st, tc.want, tc.st)
		}
	}
	for raw, want := range map[string]string{`'ph\Nys'`: "phys", `'a''b'`: "a'b", `'a\\b'`: `a\b`, `'x\x41'`: "xA"} {
		if got, st := decodeQuotedString(raw); st != decodedExact || got != want {
			t.Errorf("decodeQuotedString(%q) = %q, %d; want %q", raw, got, st, want)
		}
	}
	if _, st := decodeQuotedString(`'\xFF'`); st != decodedNotUTF8 {
		t.Errorf("decodeQuotedString('\\xFF') = %d, want decodedNotUTF8", st)
	}
}

// TestQuoteIdentifierSQLRoundTrips checks that a spliced name decodes back to
// itself, whatever bytes a decoded name may now hold.
func TestQuoteIdentifierSQLRoundTrips(t *testing.T) {
	for _, name := range []string{"db1.t", `t\N`, "a`b", `a\x41`, "tab\there", "esc\x1bape", "nul\x00", "é"} {
		quoted := quoteIdentifierSQL(name)
		if got, st := decodeQuotedIdentifier(quoted); st != decodedExact || got != name {
			t.Errorf("quoteIdentifierSQL(%q) = %s decodes to %q, %d", name, quoted, got, st)
		}
	}
}

// TestDecodeASTIdentifiersIngestion checks the ParseOne ingestion.
func TestDecodeASTIdentifiersIngestion(t *testing.T) {
	e := newTestEngine(t)
	ast, err := e.ParseOne("SELECT `_hg_\\Nrow_id` FROM `db\\N1`.`t\\N`")
	if err != nil {
		t.Fatal(err)
	}
	tables, err := CollectSelectTables(ast)
	if err != nil || len(tables) != 1 || tables[0].DB != "db1" || tables[0].Table != "t" {
		t.Fatalf("tables = %+v, %v", tables, err)
	}
	if !strings.Contains(string(ast), `"_hg_row_id"`) {
		t.Fatalf("column not decoded: %s", ast)
	}
	// A quote-lost node (Polyglot marks an EXCEPT list quoted: false) is
	// decoded from its span too, and re-marked quoted (round 2, F4).
	ex, err := e.ParseOne("SELECT * EXCEPT (`_hg_\\Nrow_id`) FROM db1.t")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ex), `"_hg_row_id"`) || strings.Contains(string(ex), `\\N`) {
		t.Fatalf("EXCEPT list not decoded: %s", ex)
	}
	if _, err := e.ParseOne("SELECT * FROM `\\N`.x"); err == nil {
		t.Fatal("empty decoded identifier parsed")
	}
	// ClickHouse accepts a non-UTF-8 name: not refused, Polyglot's name kept.
	if _, err := e.ParseOne("SELECT * FROM db1.`\\xFF`"); err != nil {
		t.Fatalf("non-UTF-8 identifier refused: %v", err)
	}
	// No backslash and not a command: byte-identical to Polyglot's AST.
	plain, _ := e.ParseOne("SELECT * FROM db1.t")
	again, _ := decodeASTIdentifiers(e, "SELECT * FROM db1.t", plain)
	if string(plain) != string(again) {
		t.Fatal("fast path changed the AST")
	}
	// A command node carries the original statement text (round 2, F1).
	cmd, err := e.ParseOne("/* c */ RENAME TABLE db1.`\"` TO db1.y ;")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cmd), "RENAME TABLE db1.`\\\"` TO db1.y\"") {
		t.Fatalf("command text not the original: %s", cmd)
	}
	// Tokens carry the decoded text too; strings never fail for their bytes.
	toks, err := tokenizeRaw(e, "DELETE WHERE `ph\\Nys`.x = 'a\\Nb'")
	if err != nil {
		t.Fatal(err)
	}
	if toks[2].Text != "phys" || toks[len(toks)-1].Text != "ab" {
		t.Fatalf("tokens = %+v", toks)
	}
	for _, s := range []string{"SELECT '\\xFF'", "SELECT 'a\\xZZb'", "SELECT 'a\\x6'"} {
		if _, err := tokenizeRaw(e, s); err != nil {
			t.Errorf("tokenizeRaw(%q): %v", s, err)
		}
	}
}

// TestSameASTIgnoresKeyOrder keeps the whole-statement parse gate sound after
// ingestion re-encodes an AST with sorted keys.
func TestSameASTIgnoresKeyOrder(t *testing.T) {
	if !sameAST(AST(`{"a":1,"b":{"c":"x"}}`), AST(`{"b":{"c":"x"},"a":1}`)) {
		t.Fatal("reordered keys compared unequal")
	}
	if sameAST(AST(`{"a":1}`), AST(`{"a":2}`)) {
		t.Fatal("different values compared equal")
	}
}
