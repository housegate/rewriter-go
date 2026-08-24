package harness

import "testing"

func TestNormalizeSIIdentifierQuotes_SupportedCanonicalOutput(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no quotes", "SELECT 1", "SELECT 1"},
		{"backtick identifier", "SELECT a FROM `db1.t`", `SELECT a FROM "db1.t"`},
		{"existing ANSI identifier", `SELECT a FROM "db1.t"`, `SELECT a FROM "db1.t"`},
		{
			"qualified and aliased",
			"SELECT * FROM phys.`db1.t` AS `db1.t`",
			`SELECT * FROM phys."db1.t" AS "db1.t"`,
		},
		{"backtick inside literal", "SELECT '`raw`' FROM `t`", "SELECT '`raw`' FROM \"t\""},
		{"double quote inside literal", `SELECT '"' FROM "t"`, `SELECT '"' FROM "t"`},
		{
			"comment spelling inside literal is ordinary content",
			"SELECT '-- /* # // `raw`' FROM `t`",
			"SELECT '-- /* # // `raw`' FROM \"t\"",
		},
		{
			"FORMAT spelling inside literal is ordinary content",
			"SELECT 'INSERT FORMAT `raw`' FROM `t`",
			"SELECT 'INSERT FORMAT `raw`' FROM \"t\"",
		},
		{
			"FORMAT outside INSERT remains supported",
			"SELECT FORMAT FROM `t`",
			"SELECT FORMAT FROM \"t\"",
		},
		{
			"INSERT VALUES has no raw FORMAT tail",
			"INSERT INTO `t` VALUES ('`raw`')",
			"INSERT INTO \"t\" VALUES ('`raw`')",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeSIIdentifierQuotes(c.in); got != c.want {
				t.Errorf("NormalizeSIIdentifierQuotes(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeSIIdentifierQuotes_DoubledBacktickEscapeMatchesPolyglot(t *testing.T) {
	in := "SELECT * FROM `a``b`"
	want := "SELECT * FROM \"a`b\""
	if got := NormalizeSIIdentifierQuotes(in); got != want {
		t.Errorf("NormalizeSIIdentifierQuotes(%q) = %q, want %q", in, got, want)
	}
}

func TestNormalizeSIIdentifierQuotes_BackslashBacktickEscapeMatchesPolyglot(t *testing.T) {
	in := "SELECT * FROM `a\\`b`"
	want := "SELECT * FROM \"a`b\""
	if got := NormalizeSIIdentifierQuotes(in); got != want {
		t.Errorf("NormalizeSIIdentifierQuotes(%q) = %q, want %q", in, got, want)
	}
}

func TestNormalizeSIIdentifierQuotes_ReencodesANSIIdentifierDelimiter(t *testing.T) {
	in := "SELECT * FROM `a\"b`"
	want := `SELECT * FROM "a""b"`
	if got := NormalizeSIIdentifierQuotes(in); got != want {
		t.Errorf("NormalizeSIIdentifierQuotes(%q) = %q, want %q", in, got, want)
	}
}

func TestNormalizeSIIdentifierQuotes_SupportedLiteralAndANSIEscapes(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"backslash escaped quote in literal",
			"SELECT 'a\\'`raw`' FROM `t`",
			"SELECT 'a\\'`raw`' FROM \"t\"",
		},
		{
			"doubled quote in literal",
			"SELECT 'a''`raw`' FROM `t`",
			"SELECT 'a''`raw`' FROM \"t\"",
		},
		{
			"doubled ANSI quote",
			"SELECT \"a\"\"`raw`\" FROM `t`",
			"SELECT \"a\"\"`raw`\" FROM \"t\"",
		},
		{
			"ANSI backslash does not escape quote",
			"SELECT \"a\\\" FROM `t`",
			"SELECT \"a\\\" FROM \"t\"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeSIIdentifierQuotes(c.in); got != c.want {
				t.Errorf("NormalizeSIIdentifierQuotes(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeSIIdentifierQuotes_UnsupportedCommentsReturnOriginal(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"dash line comment", "SELECT `t` -- keep `raw`\n"},
		{"hash line comment", "SELECT `t` # keep `raw`\n"},
		{"slash line comment", "SELECT `t` // keep `raw`\n"},
		{"block comment", "SELECT `t` /* keep `raw` */"},
		{"hint comment", "SELECT `t` /*+ keep `raw` */"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeSIIdentifierQuotes(c.sql); got != c.sql {
				t.Errorf("NormalizeSIIdentifierQuotes(%q) = %q, want original", c.sql, got)
			}
		})
	}
}

func TestNormalizeSIIdentifierQuotes_UnsupportedDollarQuotesReturnOriginal(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"bare dollar quote", "SELECT `t`, $$`raw`$$"},
		{"tagged dollar quote", "SELECT `t`, $tag$`raw`$tag$"},
		{"unquoted dollar token", "SELECT `t`, $name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeSIIdentifierQuotes(c.sql); got != c.sql {
				t.Errorf("NormalizeSIIdentifierQuotes(%q) = %q, want original", c.sql, got)
			}
		})
	}
}

func TestNormalizeSIIdentifierQuotes_UnsupportedQuoteFormsReturnOriginal(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"triple double quote", "SELECT `t`, \"\"\"`raw`\"\"\""},
		{"triple single quote", "SELECT `t`, '''`raw`'''"},
		{"smart single quote", "SELECT `t`, ‘`raw`’"},
		{"smart double quote", "SELECT `t`, “`raw`”"},
		{"escape-prefixed literal", "SELECT `t`, E'`raw`'"},
		{"national-prefixed literal", "SELECT `t`, N'`raw`'"},
		{"unicode-prefixed literal", "SELECT `t`, U&'`raw`'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeSIIdentifierQuotes(c.sql); got != c.sql {
				t.Errorf("NormalizeSIIdentifierQuotes(%q) = %q, want original", c.sql, got)
			}
		})
	}
}

func TestNormalizeSIIdentifierQuotes_InsertFormatReturnsOriginal(t *testing.T) {
	sql := "INSERT INTO `db.t` FORMAT CSV\n1,`raw`\n\nSELECT `still_payload`"
	if got := NormalizeSIIdentifierQuotes(sql); got != sql {
		t.Errorf("NormalizeSIIdentifierQuotes(%q) = %q, want original", sql, got)
	}
}

func TestNormalizeSIIdentifierQuotes_MalformedDelimitersReturnOriginal(t *testing.T) {
	cases := []struct{ name, sql string }{
		{"unterminated backtick", "SELECT `a"},
		{"unterminated backtick rolls back earlier rewrite", "SELECT `ok`, `bad"},
		{"escaped backtick without close", "SELECT `ok`, `a\\`b"},
		{"unterminated ANSI identifier rolls back earlier rewrite", "SELECT `ok`, \"bad"},
		{"unterminated literal rolls back earlier rewrite", "SELECT `ok`, 'bad"},
		{"dangling literal escape rolls back earlier rewrite", "SELECT `ok`, 'bad\\"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeSIIdentifierQuotes(c.sql); got != c.sql {
				t.Errorf("NormalizeSIIdentifierQuotes(%q) = %q, want original", c.sql, got)
			}
		})
	}
}

// The comparison guard may deliberately leave a harmless identifier-style
// difference visible when it encounters unsupported syntax. It must never
// erase a semantic difference inside a literal and produce a false equality.
func TestNormalizeSIIdentifierQuotes_LiteralQuotingBugIsNotHidden(t *testing.T) {
	correct := "SELECT * FROM t WHERE s = '`raw`'"
	buggy := `SELECT * FROM t WHERE s = '"raw"'`
	if NormalizeSIIdentifierQuotes(correct) == NormalizeSIIdentifierQuotes(buggy) {
		t.Fatal("normalization must not equate a backtick and a double quote inside a string literal")
	}
}
