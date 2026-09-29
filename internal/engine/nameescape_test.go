package engine

import "testing"

// TestDecodeIdentifierEscapes pins the residual ClickHouse identifier decode
// this package applies on top of Polyglot's tokenizer output. Inputs are
// Polyglot-decoded names (quotes stripped, \xHH / doubled quotes / “…” already
// resolved by Polyglot), so the cases exercise the escapes Polyglot leaves:
// \N and the backslash escapes.
func TestDecodeIdentifierEscapes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"in", "in", true},
		{`\Nin`, "in", true},        // \N decodes to nothing
		{`i\Nn`, "in", true},
		{`in\N`, "in", true},
		{`\N\Nin`, "in", true},
		{`\in`, `\in`, true},        // unknown escape keeps its backslash
		{`\\in`, `\in`, true},       // Polyglot already collapsed the source \\ to one \; \i stays unknown
		{`joinGet`, "joinGet", true},
		{`\NjoinGet`, "joinGet", true},
		{`joinGe\Nt`, "joinGet", true},
		{`\x69n`, "in", true},       // defensive: a \xHH left in the text still decodes
		{`\xZZ`, `\xZZ`, true},      // not two hex digits: unknown escape, kept
		{`\x6`, `\x6`, true},        // truncated \x: kept, not an error
		{`ab\`, "", false},          // trailing lone backslash: cannot decode
	} {
		got, ok := decodeIdentifierEscapes(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("decodeIdentifierEscapes(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestCanonicalCallableInName pins the IN-family recognition: every
// escape-spelling that ClickHouse 26.2 resolves to an IN-family function is
// recognised, and every spelling ClickHouse treats as an unknown function
// (wrong case, or a name that decodes to something else) is not.
func TestCanonicalCallableInName(t *testing.T) {
	recognised := []string{
		"in", "notIn", "globalIn", "globalNotIn", "nullIn", "notNullIn",
		"globalNullIn", "globalNotNullIn", "inIgnoreSet", "globalNotNullInIgnoreSet",
		`\Nin`, `i\Nn`, `in\N`, // \N spellings resolve to in
		`not\x49n`,   // -> notIn
		`glob\x61lIn`, // -> globalIn
	}
	for _, n := range recognised {
		if _, ok := canonicalCallableInName(n); !ok {
			t.Errorf("canonicalCallableInName(%q) = false, want recognised", n)
		}
	}
	// Case-sensitive: ClickHouse runs `in` but not `IN` / `In` / `NOTIN`.
	unknown := []string{"IN", "In", "NOTIN", "NotIn", "GLOBALIN", `\in`, `\\in`, `i\n`, "notin", "isNull"}
	for _, n := range unknown {
		if _, ok := canonicalCallableInName(n); ok {
			t.Errorf("canonicalCallableInName(%q) = true, want unknown", n)
		}
	}
}

// TestIsStringLookupDecodesEscapes pins that an escaped lookup name is still
// recognised (so its target is governed / refused), while an unknown spelling
// is not misread as a lookup.
func TestIsStringLookupDecodesEscapes(t *testing.T) {
	for _, n := range []string{"joinGet", `\NjoinGet`, `joinGe\Nt`, "dictGetUInt64", `\NdictGetString`, "hasColumnInTable"} {
		if !IsStringLookup(n) {
			t.Errorf("IsStringLookup(%q) = false, want true", n)
		}
	}
	for _, n := range []string{`\join`, "notALookup", `\NnotALookup`} {
		if IsStringLookup(n) {
			t.Errorf("IsStringLookup(%q) = true, want false", n)
		}
	}
}

// TestSQLBearingSettingDecodesEscapes pins that an escaped dialect / SQL-bearing
// setting name is still refused.
func TestSQLBearingSettingDecodesEscapes(t *testing.T) {
	for _, n := range []string{"dialect", `\Ndialect`, "polyglot_dialect", `additional_table_filters`, `\Nadditional_result_filter`} {
		if !SQLBearingSetting(n) {
			t.Errorf("SQLBearingSetting(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"max_threads", `\Nmax_threads`, "log_comment"} {
		if SQLBearingSetting(n) {
			t.Errorf("SQLBearingSetting(%q) = true, want false", n)
		}
	}
}

// TestOpaqueQuotedNameDecode drives the opaque-text scanners with the escape
// spellings the ALTER / INSERT paths forward verbatim: an escaped IN-family or
// lookup name whose operand reads another table must be refused, exactly like
// its plain spelling; a wrong-case or unknown spelling must not be over-refused
// by the IN rule (its own operand is a bare identifier that reads nothing).
func TestOpaqueQuotedNameDecode(t *testing.T) {
	e := newTestEngine(t)
	// ungoverned=true: the escaped name is an IN that reads a table. (String
	// lookups in an ALTER action are refused by the T6 lookup scan through
	// lookupCallsInRawTokens / IsStringLookup, not OpaqueTextIsUngoverned; see
	// TestIsStringLookupDecodesEscapes and TestTableRef_OpaqueNameDecode.)
	refuse := []string{
		"DELETE WHERE `\\Nin`(a, `db2.x`)",
		"DELETE WHERE `i\\Nn`(a, `db2.x`)",
		"DELETE WHERE `in\\N`(a, `db2.x`)",
		"DELETE WHERE \"i\\Nn\"(a, `db2.x`)",
		"DELETE WHERE `not\\x49n`(a, `db2.x`)",
		"DELETE WHERE `glob\\x61lIn`(a, `db2.x`)",
	}
	for _, s := range refuse {
		if !OpaqueTextIsUngoverned(e, s) {
			t.Errorf("OpaqueTextIsUngoverned(%q) = false, want true", s)
		}
	}
	// ungoverned=false: ClickHouse reads nothing from these (unknown function).
	pass := []string{
		"DELETE WHERE `\\in`(a, `db2.x`)",
		"DELETE WHERE \"\\in\"(a, `db2.x`)",
		"DELETE WHERE `\\\\in`(a, `db2.x`)",
		"DELETE WHERE `IN`(a, `db2.x`)",
		"DELETE WHERE `NOTIN`(a, `db2.x`)",
	}
	for _, s := range pass {
		if OpaqueTextIsUngoverned(e, s) {
			t.Errorf("OpaqueTextIsUngoverned(%q) = true, want false", s)
		}
	}
	// The same for an opaque INSERT query text.
	if !OpaqueInsertQueryIsUngoverned(e, "SETTINGS x=1 VALUES (`\\NjoinGet`('db2.x','v',1))") {
		t.Errorf("OpaqueInsertQueryIsUngoverned escaped joinGet = false, want true")
	}
}
