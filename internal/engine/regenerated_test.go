package engine

import (
	"errors"
	"testing"
)

const differs = "engine: generate: the regenerated statement differs from the input: "

func TestCheckRegenerated(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want string // "" = regenerated faithfully; otherwise the exact error text
	}{
		// Regenerated faithfully, including every measured cosmetic respelling.
		{"plain select", "SELECT a, count() FROM db1.o WHERE b = 1 GROUP BY a ORDER BY a DESC LIMIT 10", ""},
		{"nulls last is the default", "SELECT a FROM db1.o ORDER BY a DESC NULLS LAST, b ASC NULLS LAST", ""},
		{"nulls first is kept", "SELECT a FROM db1.o ORDER BY a NULLS FIRST", ""},
		{"select all", "SELECT ALL a FROM db1.o", ""},
		{"top", "SELECT TOP 5 a FROM db1.o", ""},
		{"limit n, m", "SELECT a FROM db1.o LIMIT 5, 10", ""},
		{"limit by n, m", "SELECT a FROM db1.o LIMIT 1, 2 BY a", ""},
		{"comma join", "SELECT * FROM db1.o, db1.p", ""},
		{"implicit alias", "SELECT a x FROM db1.o t1", ""},
		{"div mod", "SELECT a DIV 2, a MOD 2, mod(a, 2) FROM db1.o", ""},
		{"regexp", "SELECT a FROM db1.o WHERE s REGEXP 'x'", ""},
		{"null-safe equality", "SELECT a <=> b FROM db1.o", ""},
		{"ternary", "SELECT a > 1 ? 'x' : 'y' FROM db1.o WHERE b = {p:UInt8}", ""},
		{"position in", "SELECT POSITION('a' IN s) FROM db1.o", ""},
		{"interval number", "SELECT now() - INTERVAL 1 DAY", ""},
		{"heredoc", "SELECT * FROM merge($tag$hg_safe$tag$, 'db1__t')", ""},
		{"settings then format", "SELECT a FROM db1.o FORMAT JSON SETTINGS max_threads = 1", ""},
		{"function aliases", "SELECT pow(a, 2), log(a), ceiling(a), substr(s, 1), lcase(s), ucase(s), DATE_TRUNC('day', t) FROM db1.o", ""},
		{"type aliases", "CREATE TABLE db1.n (a INT, b BIGINT, c TEXT, d VARCHAR(255), e DOUBLE, f BOOLEAN, g TIMESTAMP, h NUMERIC(10, 2)) ENGINE = Memory", ""},
		{"truncate table keyword", "TRUNCATE db1.o", ""},
		{"insert into table", "INSERT INTO TABLE db1.o (a, b) VALUES (1, 'a')", ""},
		{"insert format payload", "INSERT INTO db1.o FORMAT CSV 1,(2", ""},
		{"insert select format comment", "INSERT INTO db1.o SELECT * FROM db1.p FORMAT JSON -- c", ""},
		{"definer string", "CREATE DEFINER='live' VIEW db1.v AS SELECT 1", ""},
		{"driver table", "CREATE TABLE `db1`.`p_e` (`id` String, `n` UInt64 DEFAULT 0 COMMENT 'c' CODEC(Delta, ZSTD(1)), INDEX `i` id TYPE bloom_filter GRANULARITY 1, PROJECTION `p` (SELECT * ORDER BY `n`)) ENGINE = ReplacingMergeTree(`n`) PARTITION BY `n` ORDER BY (`id`) SETTINGS index_granularity=8192 COMMENT 'h'", ""},
		{"command keeps its text", "SHOW CREATE TABLE db1.o XYZ", ""},

		// Fix round 1: shapes the stricter rules must keep passing.
		{"clickhouse escapes kept", `SELECT 'it''s', 'a\'b', '\\', '\n', '\x41', $$a\_b$$, '''q''' FROM db1.o`, ""},
		{"quoted identifier escapes kept", "SELECT `a``b`, \"a\"\"b\", `a\\`b` FROM db1.o", ""},
		{"literal spellings kept", "SELECT x'41', X'4A', b'0101', 0x1F, 0b101, 1_000, 1e3, 1.50, 0x1Fp1, E'x' FROM db1.o", ""},
		{"unicode quotes are verbatim", `SELECT ‘a\_b’, “x\_y”, ‘it's’ FROM db1.o`, ""},
		{"leading-dot float", "SELECT .5 FROM db1.o", ""},
		{"not like isolated", "SELECT a FROM db1.o WHERE s NOT LIKE 'x%'", ""},
		{"not like beside and / or", "SELECT a FROM db1.o WHERE s NOT LIKE 'x%' AND b = 1 OR c NOT ILIKE {p:String}", ""},
		{"null-safe equality beside and / or", "SELECT a FROM db1.o WHERE a <=> b AND c OR t.d <=> 1", ""},
		{"div beside alias and order", "SELECT a DIV 2 AS x, t.a MOD {p:UInt8} FROM db1.o AS t ORDER BY a DIV 2 DESC", ""},
		{"ternary of lower operators", "SELECT a OR b ? c + 1 : d AND e, NOT a ? b IS NULL : c IN (1) FROM db1.o", ""},
		{"insert select column named format", "INSERT INTO db1.o SELECT format FROM db1.p", ""},
		{"insert select format tail is ignored data", "INSERT INTO db1.o SELECT a FROM db1.p FORMAT JSON SETTINGS max_threads = 1", ""},

		// Dropped or respelled with a different meaning.
		{"delete in partition", "DELETE FROM db1.o IN PARTITION '2024-01' WHERE a = 1",
			"engine: generate: the regenerated statement differs from the input: lost [2024-01 IN PARTITION], added nothing"},
		{"with ties before format", "SELECT a FROM db1.o ORDER BY a LIMIT 1 WITH TIES FORMAT JSON",
			"engine: generate: the regenerated statement differs from the input: lost [TIES WITH], added nothing"},
		{"with ties in a subquery", "SELECT * FROM (SELECT a FROM db1.o ORDER BY a LIMIT 1 WITH TIES) AS s",
			"engine: generate: the regenerated statement differs from the input: lost [TIES WITH], added nothing"},
		{"drop on cluster", "DROP TABLE db1.o ON CLUSTER c",
			"engine: generate: the regenerated statement differs from the input: lost [C CLUSTER ON], added nothing"},
		{"drop temporary", "DROP TEMPORARY TABLE n",
			"engine: generate: the regenerated statement differs from the input: lost [TEMPORARY], added nothing"},
		{"drop if empty", "DROP TABLE IF EMPTY db1.o",
			"engine: generate: the regenerated statement differs from the input: lost [EMPTY IF], added nothing"},
		{"limit by then limit", "SELECT a FROM db1.o LIMIT 2 BY a LIMIT 10",
			"engine: generate: the regenerated statement differs from the input: lost [2 LIMIT], added nothing"},
		{"ephemeral", "CREATE TABLE db1.n (a Int32, e Int32 EPHEMERAL) ENGINE = Memory",
			"engine: generate: the regenerated statement differs from the input: lost [EPHEMERAL], added nothing"},
		{"insert column transformer", "INSERT INTO db1.o (* EXCEPT (b)) VALUES (1)",
			"engine: generate: the regenerated statement differs from the input: lost [STAR B EXCEPT], added nothing"},
		{"truncate settings", "TRUNCATE TABLE db1.o SETTINGS max_threads = 1",
			"engine: generate: the regenerated statement differs from the input: lost [1 EQ MAX_THREADS SETTINGS], added nothing"},
		{"cast gains nullable", "SELECT a::String FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost nothing, added [NULLABLE]"},
		{"char length is not length", "SELECT CHAR_LENGTH(s) FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost [CHAR_LENGTH], added [LENGTH]"},
		{"group_concat separator", "SELECT group_concat(s, '-') FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost nothing, added [CONCAT]"},
		{"no such function", "SELECT startsWith(s, 'x') FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost [STARTSWITH], added [STARTS_WITH]"},
		{"live view becomes a view", "CREATE DEFINER=alice LIVE VIEW db1.v AS SELECT 1",
			"engine: generate: the regenerated statement differs from the input: lost [ALICE LIVE], added [ALICELIVE]"},

		// Fix round 1 (review C1): ClickHouse keeps the backslash of an
		// unknown escape; Polyglot drops it.
		{"like escape in delete", `DELETE FROM db1.o WHERE s LIKE 'x\_%'`, differs + `lost [x\_%], added [x_%]`},
		{"like escape in alter delete", `ALTER TABLE db1.o DELETE WHERE s LIKE 'x\_%'`, differs + `lost [x\_%], added [x_%]`},
		{"like percent escape", `SELECT a FROM db1.o WHERE s LIKE '100\%'`, differs + `lost [100\%], added [100%]`},
		{"ilike escape", `SELECT a FROM db1.o WHERE s ILIKE '%\_%'`, differs + `lost [%\_%], added [%_%]`},
		{"escaped quoted table", "SELECT * FROM db1.`a\\_b`", differs + `lost [A\_B], added [A_B]`},
		{"escaped double-quoted table", `SELECT * FROM db1."a\_b"`, differs + `lost [A\_B], added [A_B]`},
		{"escaped quoted column", "SELECT `a\\_b` FROM db1.o", differs + `lost [A\_B], added [A_B]`},
		{"escaped value", `INSERT INTO db1.o VALUES (1, 'a\_b')`, differs + `lost [a\_b], added [a_b]`},
		{"null escape value", `INSERT INTO db1.o VALUES ('\N')`, differs + `lost [], added [\N]`},
		{"escape z", `SELECT '\Z'`, differs + "lost [\\Z], added [\x1a]"},
		{"escape slash", `SELECT '\/'`, differs + `lost [/], added [\/]`},
		{"escape e", `SELECT '\e'`, differs + "lost [\x1b], added [\\e]"},
		{"escape bad hex", `SELECT '\xZZ'`, differs + "lost [\xef], added [\\xZZ]"},
		{"escape equals", `SELECT 'a\=b'`, differs + `lost [a=b], added [a\=b]`},
		{"escape backtick in a string", "SELECT 'a\\`b'", differs + "lost [a`b], added [a\\`b]"},
		{"escape hex runs past the quote", `SELECT '\x4', 'b'`, differs + `lost ['\x4'], added [\x4]`},
		{"triple double quote is an identifier", `SELECT """abc""" FROM db1.o`, differs + `lost ["ABC"], added [abc]`},

		// Fix round 1 (review I1 / I2): a respelling that regroups.
		{"null-safe equality under =", "SELECT a <=> b = c FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality under <", "SELECT a <=> b < c FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality under !=", "SELECT a <=> b != c FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality under like", "SELECT a <=> b LIKE c FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality under in", "SELECT a <=> b IN (1) FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality under between", "SELECT a <=> b BETWEEN 1 AND 2 FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality in where", "SELECT a FROM db1.o WHERE a <=> b = 0", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality after between and", "SELECT x BETWEEN 1 AND a <=> b FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"not before div", "SELECT NOT a DIV b FROM db1.o", differs + "lost [DIV], added [INTDIV]"},
		{"not before regexp", "SELECT NOT a REGEXP 'x' FROM db1.o", differs + "lost [REGEXP], added [MATCH]"},
		{"ternary over an unmeasured operator", "SELECT a::Int8 ? 1 : 2 FROM db1.o", differs + "lost [COLON PARAMETER], added [IF]"},
		{"ternary with an implicit alias", "SELECT a ? b : c x FROM db1.o", differs + "lost [COLON PARAMETER], added [IF]"},
		{"not like under =", "SELECT a NOT LIKE b = c FROM db1.o", differs + "lost [NOT LIKE], added [NOT]"},

		// Fix round 1 (review I3): a column named format is not a FORMAT clause.
		{"format column hides limit by", "INSERT INTO db1.o SELECT format FROM db1.p LIMIT 1 BY a LIMIT 2", differs + "lost [1 LIMIT], added nothing"},
		{"format column list hides limit by", "INSERT INTO db1.o (format) SELECT a FROM db1.p LIMIT 1 BY a LIMIT 2", differs + "lost [1 LIMIT], added nothing"},
		{"format column hides a cast", "INSERT INTO db1.o SELECT format, a::String FROM db1.p", differs + "lost nothing, added [NULLABLE]"},
		{"format column hides char_length", "INSERT INTO db1.o SELECT a FROM db1.p WHERE format = 1 AND CHAR_LENGTH(s) = 1", differs + "lost [CHAR_LENGTH], added [LENGTH]"},
		{"format alias hides from", "INSERT INTO db1.o SELECT format x FROM db1.p", differs + "lost [FROM DB1.P], added nothing"},
		{"format column hides startswith", "INSERT INTO db1.o SELECT a FROM db1.p WHERE format = 1 AND startsWith(s, 'x')", differs + "lost [STARTSWITH], added [STARTS_WITH]"},

		// Review M3: cosmetic respellings still refused (fail closed).
		{"mod of a parenthesised operand", "SELECT a MOD (2 + 1) FROM db1.o", differs + "lost [MOD], added [PERCENT]"},
		{"regexp of a parenthesised operand", "SELECT a REGEXP ('x' = 1) FROM db1.o", differs + "lost [REGEXP], added [MATCH]"},
		{"format values rows", "INSERT INTO db1.o FORMAT Values (1, 'x')", differs + "lost [FORMAT], added nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatalf("ParseOne: %v", err)
			}
			if err := CheckParsedInFull(e, tc.sql, ast); err != nil {
				t.Fatalf("CheckParsedInFull: %v (the input must pass the parse gate)", err)
			}
			err = CheckRegenerated(e, tc.sql, ast)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
			if !errors.Is(err, ErrNotRegeneratedFaithfully) {
				t.Fatalf("err = %v does not wrap ErrNotRegeneratedFaithfully", err)
			}
		})
	}
}

// regeneratedAs is the real engine with Generate replaced, so a test can
// present a regeneration the pinned Polyglot does not produce today.
type regeneratedAs struct {
	Engine
	gen string
}

func (r regeneratedAs) Generate(AST) (string, error) { return r.gen, nil }

// TestCheckRegeneratedLiteralValues pins that literals are compared by the
// value ClickHouse reads (review I4): the pinned Polyglot regenerates these
// literals verbatim, so the regeneration is supplied by the test.
func TestCheckRegeneratedLiteralValues(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name, sql, gen string
		want           string // "" = the same statement
	}{
		{"hex number", "SELECT 0x1F", "SELECT 0x20", differs + "lost [0x1F], added [0x20]"},
		{"hex number as decimal", "SELECT 0x1F", "SELECT 31", ""},
		{"binary number", "SELECT 0b101", "SELECT 0b110", differs + "lost [0b101], added [0b110]"},
		{"float is not an integer", "SELECT 1e3", "SELECT 1000", differs + "lost [1e3], added [1000]"},
		{"float spellings", "SELECT 1e3, .5", "SELECT 1000.0, 0.50", ""},
		{"float value", "SELECT 1.5", "SELECT 1.6", differs + "lost [1.5], added [1.6]"},
		{"hex string", "SELECT x'41'", "SELECT x'42'", differs + "lost [x'41'], added [x'42']"},
		{"hex string as string", "SELECT x'41'", "SELECT 'A'", ""},
		{"bit string", "SELECT b'0101'", "SELECT b'0110'", differs + "lost [b'0101'], added [b'0110']"},
		{"escape string case", "SELECT E'A'", "SELECT E'a'", differs + "lost [E'A'], added [E'a']"},
		{"escape string prefix case", "SELECT e'x'", "SELECT E'x'", ""},
		{"string escapes with the same value", `SELECT 'a\_b', '\x41'`, `SELECT 'a\\_b', 'A'`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatalf("ParseOne: %v", err)
			}
			err = CheckRegenerated(regeneratedAs{Engine: e, gen: tc.gen}, tc.sql, ast)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want || !errors.Is(err, ErrNotRegeneratedFaithfully) {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
}

// regenErrEngine fails the call a test names, so the error paths run without
// the FFI library.
type regenErrEngine struct {
	Engine        // nil: any other call panics
	genErr        error
	tokenizeErr   error
	tokenizeOK    int // Tokenize calls that succeed before tokenizeErr applies
	generateCalls int
	tokenizeCalls int
}

func (f *regenErrEngine) Generate(AST) (string, error) {
	f.generateCalls++
	if f.genErr != nil {
		return "", f.genErr
	}
	return "SELECT 1", nil
}

func (f *regenErrEngine) Tokenize(string) (AST, error) {
	f.tokenizeCalls++
	if f.tokenizeErr != nil && f.tokenizeCalls > f.tokenizeOK {
		return nil, f.tokenizeErr
	}
	return AST(`[]`), nil
}

// TestCheckRegeneratedErrors pins that every failure to run the comparison is
// an error (review M4 / M5): a caller refuses on any non-nil result, so no
// error path can pass a statement.
func TestCheckRegeneratedErrors(t *testing.T) {
	errGen := errors.New("fake generate error")
	errTok := errors.New("fake tokenize error")
	selectAST := AST(`{"select":{}}`)

	t.Run("node kind", func(t *testing.T) {
		for _, ast := range []AST{AST(`not json`), AST(`{}`), AST(`{"select":{},"insert":{}}`)} {
			f := &regenErrEngine{}
			if err := CheckRegenerated(f, "SELECT 1", ast); err == nil {
				t.Fatalf("CheckRegenerated(%s) = nil, want the NodeKind error", ast)
			}
			if f.generateCalls != 0 || f.tokenizeCalls != 0 {
				t.Fatalf("%s: generate %d / tokenize %d calls after a NodeKind error, want none", ast, f.generateCalls, f.tokenizeCalls)
			}
		}
	})
	t.Run("generator", func(t *testing.T) {
		f := &regenErrEngine{genErr: errGen}
		if err := CheckRegenerated(f, "SELECT 1", selectAST); !errors.Is(err, errGen) {
			t.Fatalf("err = %v, want the generator error", err)
		}
	})
	t.Run("tokenizer on the input", func(t *testing.T) {
		f := &regenErrEngine{tokenizeErr: errTok}
		if err := CheckRegenerated(f, "SELECT 1", selectAST); !errors.Is(err, errTok) {
			t.Fatalf("err = %v, want the tokenizer error", err)
		}
	})
	t.Run("tokenizer on the regeneration", func(t *testing.T) {
		f := &regenErrEngine{tokenizeErr: errTok, tokenizeOK: 1}
		if err := CheckRegenerated(f, "SELECT 1", selectAST); !errors.Is(err, errTok) {
			t.Fatalf("err = %v, want the tokenizer error", err)
		}
	})
	t.Run("two tokenizer calls on success", func(t *testing.T) {
		f := &regenErrEngine{}
		if err := CheckRegenerated(f, "SELECT 1", selectAST); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if f.generateCalls != 1 || f.tokenizeCalls != 2 {
			t.Fatalf("generate %d / tokenize %d calls, want 1 / 2", f.generateCalls, f.tokenizeCalls)
		}
	})
	t.Run("command node", func(t *testing.T) {
		f := &regenErrEngine{genErr: errGen, tokenizeErr: errTok}
		if err := CheckRegenerated(f, "SHOW x", AST(`{"command":{"this":"SHOW x"}}`)); err != nil {
			t.Fatalf("err = %v, want nil: a command node is regenerated verbatim", err)
		}
	})
}

// TestClickHouseUnquote pins the decoder to ClickHouse's own reading, measured
// on clickhouse-server 26.2 and 26.7.5.10 with hex() of each string literal
// and the system.columns name of each quoted identifier.
func TestClickHouseUnquote(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
	}{
		{`'\a\b\e\f\n\r\t\v\0'`, "\a\b\x1b\f\n\r\t\v\x00"},
		{`'\\\'\"\/\='`, "\\'\"/="},
		{"'\\`'", "`"},
		{"'a\\\tb\\\nc'", "a\tb\nc"},
		{`'a\Nb'`, "ab"},
		{`'\x41\x4a\xZZ\xaZ\xg1'`, "\x41\x4a\xef\x9f\xf1"},
		{`'a\_b\%c\Zd\ue\1f'`, `a\_b\%c\Zd\ue\1f`},
		{"'a\\\x7fb\\\xc3\xa9'", "a\\\x7fb\\\xc3\xa9"},
		{`'it''s'`, "it's"},
		{`'''abc'''`, "'abc'"},
		{"`a\\_b`", `a\_b`},
		{"`a``b`", "a`b"},
		{"`a\\`b`", "a`b"},
		{"`a\\'b`", "a'b"},
		{`"a\_b"`, `a\_b`},
		{`"a""b"`, `a"b`},
		{`"""abc"""`, `"abc"`},
		{`"a\x41b"`, "aAb"},
	} {
		if got, ok := clickhouseUnquote(tc.raw); !ok || got != tc.want {
			t.Errorf("clickhouseUnquote(%q) = %q, %v; want %q", tc.raw, got, ok, tc.want)
		}
	}
	// ClickHouse would not read these with the same extent.
	for _, raw := range []string{`'\x4'`, `'\x'`, `'\x'''`, `'a\'`, `'a'b'`, `'a`, `x`, `'a"`} {
		if got, ok := clickhouseUnquote(raw); ok {
			t.Errorf("clickhouseUnquote(%q) = %q, want a failure", raw, got)
		}
	}
}

func TestNumberKey(t *testing.T) {
	for _, tc := range []struct {
		raw, want string // want "" = not decoded
	}{
		{"31", "31"},
		{"0031", "31"},
		{"0x1F", "31"},
		{"0X1f", "31"},
		{"0b11111", "31"},
		{"1_000", "1000"},
		{"18446744073709551615", "18446744073709551615"},
		{"18446744073709551616", "F18446744073709551616"},
		{"1e3", "F1000"},
		{"1000.0", "F1000"},
		{".5", "F1/2"},
		{"0.50", "F1/2"},
		{"1.", "F1"},
		{"1.5e-3", "F3/2000"},
		{"0x1Fp1", "F62"},
		{"0x1.8p1", "F3"},
		{"1e4000", ""},
		{"0x1p99999", ""},
		{"0x", ""},
		{"0b12", ""},
		{"1a", ""},
		{"", ""},
	} {
		got, ok := numberKey(tc.raw)
		if tc.want == "" {
			if ok {
				t.Errorf("numberKey(%q) = %q, want no value", tc.raw, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("numberKey(%q) = %q, %v; want %q", tc.raw, got, ok, tc.want)
		}
	}
}

func TestLiteralSpelling(t *testing.T) {
	for _, tc := range []struct {
		raw, key string
	}{
		{`'a\_b'`, `S:a\_b`},
		{`$$a\_b$$`, `S:a\_b`},
		{`$t$x$y$t$`, `S:x$y`},
		{`x'41'`, "S:A"},
		{`X'414'`, "S:\x04\x14"},
		{`b'0101'`, "S:\x05"},
		{`B'0100000101000010'`, "S:AB"},
		{`E'a\nb'`, "S:E'a\nb"},
		{`e'a\nb'`, "S:E'a\nb"},
		{`N'abc'`, "S:N'abc"},
		{"`a\\_b`", `W:A\_B`},
		{`"int"`, "W:INT32"},
		{`'\x4'`, `U:'\x4'`},
		{`x'4G'`, `U:x'4G'`},
		{`b'012'`, `U:b'012'`},
		{`$a$x$b$`, `U:$a$x$b$`},
		{`‘a\_b’`, `S:a\_b`},
		{`“a\_b”`, `W:A\_B`},
		{`‘a’b’`, `U:‘a’b’`},
	} {
		if got := literalSpelling(tc.raw, "", true); got.key != tc.key {
			t.Errorf("literalSpelling(%q) = %q, want %q", tc.raw, got.key, tc.key)
		}
	}
	if got := literalSpelling(`'\x4'`, "", false); got.key != `V:'\x4'` {
		t.Errorf("an undecodable regeneration lexeme = %q, want the V: side", got.key)
	}
	if got := literalSpelling(`'01'`, "W:INTERVAL", true); got.key != "N:1" {
		t.Errorf("INTERVAL '01' = %q, want the number 1", got.key)
	}
}
