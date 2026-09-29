package engine

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strings"
)

// ErrNotRegeneratedFaithfully marks a statement whose regenerated SQL does
// not spell the input. The pinned Polyglot consumes every token of such a
// statement, so the whole-statement parse gate passes it, but it parses some
// clauses into nothing (DELETE … IN PARTITION, DROP … ON CLUSTER, DROP
// TEMPORARY, WITH TIES before FORMAT / SETTINGS or inside a subquery, a
// column's EPHEMERAL) and prints others differently (LIMIT n BY … LIMIT m as
// LIMIT m BY …, a :: cast wrapped in Nullable, CHAR_LENGTH as LENGTH, 'x\_%'
// as 'x_%', a <=> b = c as a IS NOT DISTINCT FROM b = c). The SQL generated
// from that AST means something other than the input.
var ErrNotRegeneratedFaithfully = errors.New("engine: generate: the regenerated statement differs from the input")

// CheckRegenerated returns nil when Generate(ast), which e.ParseOne produced
// for sql, spells the same statement as sql, and an error wrapping
// ErrNotRegeneratedFaithfully otherwise. An AST whose kind cannot be read and
// a tokenizer or generator error are returned as is: every non-nil result
// means the statement is not known to be regenerated faithfully.
//
// Both texts are tokenized by the engine and reduced to a multiset of
// spellings (fidelitySpellings): commas, parentheses, dots and semicolons are
// dropped, words are upper-cased, the ClickHouse aliases in spellingClass are
// folded, and the measured cosmetic respellings of the input are applied.
// Literals and quoted identifiers are compared by the value ClickHouse reads
// from their source text (literalSpelling, numberKey), never by the value
// Polyglot decoded. The multisets must be equal, apart from the neutral
// additions spellingDiff allows. Every rewrite a caller applies afterwards
// changes only names, so a faithful identity regeneration is what makes the
// rewritten SQL faithful.
//
// A command or raw node carries its text and is regenerated verbatim, so it
// always passes; the handlers that re-render a command from parsed fields
// check their own coverage. An INSERT statement whose AST carries a FORMAT
// data clause (insertHasFormatClause, the gate GenerateInsert splices on) is
// compared only up to the name after its last FORMAT keyword: what follows is
// data that GenerateInsert splices back verbatim (the parse gate's payload
// rule). A column or alias named format is not such a clause. An INSERT …
// SELECT … FORMAT <name> is compared in full, apart from the text after the
// name, which ClickHouse ignores (withoutIgnoredData).
func CheckRegenerated(e Engine, sql string, ast AST) error {
	kind, err := NodeKind(ast)
	if err != nil {
		return err
	}
	if kind == NodeCommand || kind == NodeRaw {
		return nil
	}
	gen, err := e.Generate(ast)
	if err != nil {
		return err
	}
	in, err := tokenizeRaw(e, sql)
	if err != nil {
		return err
	}
	out, err := tokenizeRaw(e, gen)
	if err != nil {
		return err
	}
	if kind == NodeInsert {
		switch {
		case insertHasFormatClause(ast):
			in, out = throughFormatName(in), throughFormatName(out)
		case insertSelectHasFormat(ast):
			in = withoutIgnoredData(in)
		}
	}
	lost, added := spellingDiff(fidelitySpellings(sql, in, true), fidelitySpellings(gen, out, false))
	if len(lost) == 0 && len(added) == 0 {
		return nil
	}
	return fmt.Errorf("%w: lost %s, added %s", ErrNotRegeneratedFaithfully, spellingList(lost), spellingList(added))
}

// throughFormatName keeps the tokens up to and including the name after the
// last FORMAT keyword.
func throughFormatName(toks []rawToken) []rawToken {
	for i := len(toks) - 1; i >= 0; i-- {
		if toks[i].TokenType == "FORMAT" && i+1 < len(toks) {
			return toks[:i+2]
		}
	}
	return toks
}

// insertSelectHasFormat reports whether an INSERT's source is a SELECT whose
// AST carries its own FORMAT clause (INSERT INTO t SELECT … FORMAT JSON).
func insertSelectHasFormat(ast AST) bool {
	_, body, _, err := bodyOf(ast)
	if err != nil || body == nil {
		return false
	}
	q, _ := body["query"].(map[string]any)
	sel, _ := q["select"].(map[string]any)
	return sel["format"] != nil
}

// withoutIgnoredData drops the tokenizer's inline-data token (a zero-width
// token at EOF right after FORMAT <name>) from an INSERT … SELECT … FORMAT
// <name> <text>. ClickHouse reads that text as INSERT data and ignores it
// for an INSERT … SELECT (measured on 26.2: formatQuery drops it, and
// INSERT INTO o SELECT 3 FORMAT TSV 7 inserts only 3); Generate drops it too.
// No other token is dropped: in INSERT … SELECT format x FROM p the select has
// no FORMAT clause, so the FROM p the tokenizer took for data is compared, and
// its loss refused.
func withoutIgnoredData(toks []rawToken) []rawToken {
	if n := len(toks); n >= 3 && toks[n-1].Span.Start == toks[n-1].Span.End && toks[n-3].TokenType == "FORMAT" {
		return toks[:n-1]
	}
	return toks
}

// spellingClass folds a ClickHouse alias the generator respells into the name
// it prints. Each pair resolves to the same data type or function in
// ClickHouse's system.data_type_families / system.functions alias_to
// (measured on 26.2 and 26.7.5.10). A respelling that is not an alias stays a
// difference: CHAR_LENGTH → LENGTH (characters → bytes), instr → POSITION
// (case-insensitive → case-sensitive), toStartOfDay → dateTrunc('DAY', …)
// (DateTime64 / Date32 results differ), startsWith → STARTS_WITH and
// toTypeName → TYPEOF (no such functions).
var spellingClass = map[string]string{
	"BOOLEAN":    "BOOL",
	"INT":        "INT32",
	"INTEGER":    "INT32",
	"TINYINT":    "INT8",
	"SMALLINT":   "INT16",
	"BIGINT":     "INT64",
	"FLOAT":      "FLOAT32",
	"REAL":       "FLOAT32",
	"DOUBLE":     "FLOAT64",
	"TEXT":       "STRING",
	"CHAR":       "STRING",
	"VARCHAR":    "STRING",
	"TIMESTAMP":  "DATETIME",
	"NUMERIC":    "DECIMAL",
	"POW":        "POWER",
	"LN":         "LOG",
	"CEILING":    "CEIL",
	"SUBSTR":     "SUBSTRING",
	"LCASE":      "LOWER",
	"UCASE":      "UPPER",
	"DATE_TRUNC": "DATETRUNC",
}

// spelling is one token as the check compares it: key is what is counted,
// show is how an error names it.
type spelling struct {
	key, show string
}

func word(w string) spelling { return spelling{"W:" + w, w} }

// negatedOperators are the operators a NOT directly before them negates
// (x NOT LIKE y, x NOT IN y, …). Such a NOT is spelled together with its
// operator, so a regeneration that moves it (x NOT LIKE y printed as
// NOT x LIKE y) is a difference unless the input respelling below applies.
var negatedOperators = map[string]bool{"W:LIKE": true, "W:ILIKE": true, "W:IN": true, "W:BETWEEN": true, "W:REGEXP": true}

// fidelitySpellings reduces a token stream of src to the spellings the check
// compares. With input set it also applies the measured cosmetic respellings
// of the input, each one ClickHouse's own formatter or execution shows to be
// the same statement: NULLS LAST, ClickHouse's default in both sort
// directions, is dropped; SELECT ALL is SELECT; TOP n is LIMIT n; LIMIT n, m
// is LIMIT m OFFSET n.
//
// The remaining respellings change an operator's precedence class, so the
// regeneration can regroup its operands: Polyglot prints a <=> b = c as
// a IS NOT DISTINCT FROM b = c, NOT a DIV b as intDiv(NOT a, b) and
// a NOT LIKE b = c as NOT a LIKE b = c, each of which ClickHouse reads
// differently. Each is cosmetic only where nothing can regroup it: x DIV y is
// intDiv(x, y), x MOD y is x % y, x REGEXP y is match(x, y), a <=> b is
// a IS NOT DISTINCT FROM b and x NOT LIKE / NOT ILIKE y is NOT x LIKE /
// ILIKE y (measured equal by execution, NULLs included) only when both
// operands are single operands and the application is bordered on both sides
// (isolated); POSITION(x IN y) is POSITION(y, x) only when x and y are single
// operands; a ? b : c is if(a, b, c) only when the whole ternary is bordered
// and made of measured operators (ternaryColon). Otherwise the input keeps
// its own spelling, which the regeneration does not contain, and the
// statement is refused.
func fidelitySpellings(src string, toks []rawToken, input bool) []spelling {
	var out []spelling
	last := func() string {
		if len(out) == 0 {
			return ""
		}
		return out[len(out)-1].key
	}
	spell := func(i int, prev string) spelling {
		return spellingOf(src, toks[i], prev, input)
	}
	skip := map[int]bool{} // the IN of POSITION(x IN y) and the : of a ternary
	for i := 0; i < len(toks); i++ {
		if skip[i] {
			continue
		}
		tk := toks[i]
		sp := spell(i, last())
		switch {
		case sp.key == "" || sp.key == "W:TABLE":
			continue // punctuation; TRUNCATE t / INSERT INTO TABLE t: TABLE is optional
		case last() == "W:STRING" && tk.TokenType == "NUMBER" && i > 0 && toks[i-1].TokenType == "L_PAREN":
			continue // VARCHAR(n) / CHAR(n): ClickHouse ignores the length
		case tk.TokenType == "STRING" && strings.HasPrefix(sp.key, "S:") &&
			len(out) >= 2 && out[len(out)-2].key == "W:DEFINER" && last() == "O:EQ":
			sp = word(strings.ToUpper(sp.key[2:])) // DEFINER = 'u' names the same user as DEFINER = u
		case sp.key == "W:NOT" && i+1 < len(toks):
			if next := spell(i+1, sp.key); negatedOperators[next.key] {
				sp = word("NOT " + next.key[2:])
			}
		}
		if input {
			callNext := i+1 < len(toks) && toks[i+1].TokenType == "L_PAREN"
			switch {
			case sp.key == "W:NULLS" && i+1 < len(toks) && spell(i+1, sp.key).key == "W:LAST":
				i++
				continue
			case sp.key == "W:ALL" && last() == "W:SELECT":
				continue
			case sp.key == "W:TOP":
				sp = word("LIMIT")
			case sp.key == "W:DIV" && isolated(toks, i, i):
				sp = word("INTDIV")
			case sp.key == "W:MOD" && !callNext && isolated(toks, i, i):
				sp = spelling{"O:PERCENT:%", "PERCENT"}
			case sp.key == "W:REGEXP" && !callNext && isolated(toks, i, i):
				sp = word("MATCH")
			case tk.TokenType == "NULLSAFE_EQ" && isolated(toks, i, i):
				out = append(out, word("IS"), word("NOT"), word("DISTINCT"))
				sp = word("FROM")
			case (sp.key == "W:NOT LIKE" || sp.key == "W:NOT ILIKE") && isolated(toks, i, i+1):
				sp = word("NOT")
			case tk.TokenType == "PARAMETER" && tk.Text == "?":
				if c := ternaryColon(toks, i); c >= 0 {
					sp = word("IF")
					skip[c] = true
				}
			case sp.key == "W:POSITION" && callNext:
				if in := operandAfter(toks, i+2); in > 0 && in < len(toks) && toks[in].TokenType == "IN" {
					if end := operandAfter(toks, in+1); end > 0 && end < len(toks) && toks[end].TokenType == "R_PAREN" {
						skip[in] = true
					}
				}
			case sp.key == "W:LIMIT" && i+3 < len(toks) && toks[i+2].TokenType == "COMMA":
				out = append(out, sp, spell(i+1, sp.key), spell(i+3, ""), word("OFFSET"))
				i += 3
				continue
			}
		}
		out = append(out, sp)
	}
	return out
}

// spellingOf is one token's spelling: none for punctuation, N: for a number
// (by value, numberKey), S: for a string (by the value ClickHouse reads,
// literalSpelling; INTERVAL '1' DAY's '1' counts as the number the input
// wrote), W: for a word or identifier (a quoted identifier by the name
// ClickHouse reads), O: for any other token with its text (= / == and
// <> / != are the same ClickHouse operator, so EQ and NEQ drop it). A literal
// ClickHouse would not read as one lexeme of the same extent is spelled by
// side (U: in the input, V: in the regeneration), so it never matches.
func spellingOf(src string, tk rawToken, prev string, input bool) spelling {
	raw := src[tk.Span.Start:tk.Span.End]
	switch tk.TokenType {
	case "COMMA", "L_PAREN", "R_PAREN", "DOT", "SEMICOLON":
		return spelling{}
	case "NUMBER", "HEX_NUMBER":
		return numberSpelling(raw, input)
	case "IDENTIFIER":
		if isBinaryLiteral(raw) {
			return numberSpelling(raw, input)
		}
		return spelling{"O:IDENTIFIER:" + raw, raw}
	case "EQ", "NEQ":
		return spelling{"O:" + tk.TokenType, tk.TokenType}
	}
	if isQuotedLexeme(tk.TokenType) {
		return literalSpelling(raw, prev, input)
	}
	if tk.Text == "" {
		return spelling{}
	}
	if tk.TokenType == "VAR" || isWordStart(tk.Text) {
		return word(foldClass(strings.ToUpper(tk.Text)))
	}
	return spelling{"O:" + tk.TokenType + ":" + tk.Text, tk.TokenType}
}

func foldClass(w string) string {
	if c, ok := spellingClass[w]; ok {
		return c
	}
	return w
}

func undecodable(raw string, input bool) spelling {
	if input {
		return spelling{"U:" + raw, raw}
	}
	return spelling{"V:" + raw, raw}
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func isWordStart(s string) bool {
	c := s[0]
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isQuotedLexeme reports whether a token type is a quoted literal or a quoted
// identifier, whose value literalSpelling reads from the source.
func isQuotedLexeme(tokenType string) bool {
	return tokenType == "STRING" || tokenType == "QUOTED_IDENTIFIER" || strings.HasSuffix(tokenType, "_STRING")
}

// literalSpelling spells a quoted lexeme by the value ClickHouse reads from
// its source text raw. Polyglot's own decoded text is not used: Polyglot
// decodes backslash escapes with other rules (\_ as _, \% as %, \Z as 0x1A,
// \N as \N) and re-escapes the decoded value, so 'x\_%' regenerates as 'x_%',
// a different LIKE pattern, and db1.`a\_b` as db1."a_b", a different table.
//
//   - '…' is a string; "…" and `…` are identifiers. All three decode alike
//     (clickhouseUnquote), so """abc""" is the identifier "abc", not a string.
//   - $tag$…$tag$ is a heredoc string and ‘…’ a string, “…” an identifier,
//     each with its body verbatim (unicodeQuoted).
//   - x'…' / X'…' and b'…' / B'…' are strings of the bytes they spell: hex
//     digits, an odd count padded on the left (x'414' is 0x04 0x14), and
//     bits, padded on the left to whole bytes (b'101' is 0x05).
//   - Any other prefix (N'…', E'…', U&'…', r'…') is kept, upper-cased, in the
//     spelling, with the body decoded like '…'. ClickHouse 26.2 and 26.7.5.10
//     reject these prefixes as syntax errors; the check only keeps their
//     values apart.
func literalSpelling(raw, prev string, input bool) spelling {
	if raw == "" {
		return undecodable(raw, input)
	}
	if v, ok := unicodeQuoted(raw, "‘", "’"); ok {
		return spelling{"S:" + v, v}
	}
	if v, ok := unicodeQuoted(raw, "“", "”"); ok {
		return word(foldClass(strings.ToUpper(v)))
	}
	switch raw[0] {
	case '\'':
		v, ok := clickhouseUnquote(raw)
		if !ok {
			return undecodable(raw, input)
		}
		if prev == "W:INTERVAL" && isDigits(v) {
			if k, ok := numberKey(v); ok {
				return spelling{"N:" + k, v}
			}
		}
		return spelling{"S:" + v, v}
	case '"', '`':
		v, ok := clickhouseUnquote(raw)
		if !ok {
			return undecodable(raw, input)
		}
		return word(foldClass(strings.ToUpper(v)))
	case '$':
		v, ok := heredocBody(raw)
		if !ok {
			return undecodable(raw, input)
		}
		return spelling{"S:" + v, v}
	}
	q := strings.IndexByte(raw, '\'')
	if q <= 0 {
		return undecodable(raw, input)
	}
	prefix, body := raw[:q], raw[q:]
	var v string
	var ok bool
	switch prefix {
	case "x", "X":
		v, ok = hexStringBytes(body)
	case "b", "B":
		v, ok = bitStringBytes(body)
	default:
		v, ok = clickhouseUnquote(body)
		v = strings.ToUpper(prefix) + "'" + v
	}
	if !ok {
		return undecodable(raw, input)
	}
	return spelling{"S:" + v, raw}
}

// clickhouseUnquote decodes a ClickHouse quoted lexeme, a string '…' or an
// identifier "…" / `…`, whose first and last bytes are the quote. The rules
// were measured on clickhouse-server 26.2 and 26.7.5.10 (hex() of '\c' for
// every c in 0x20–0x7E, and the system.columns names of `a\cb` and "a\cb")
// and are the same for the three quotes:
//
//	source              value
//	\a \b \e \f \n      0x07 0x08 0x1B 0x0C 0x0A
//	\r \t \v \0         0x0D 0x09 0x0B 0x00
//	\\ \' \" \` \/ \=   the character alone
//	\ + a byte ≤ 0x1F   that byte alone (a raw tab or newline after \)
//	\N                  nothing
//	\xHH                the byte 0xHH; the two bytes are read raw and a
//	                    non-hex digit counts as 0xFF (\xZZ is 0xEF)
//	\ + any other byte  the backslash and the byte: \_ \% \Z \u \1, 0x7F
//	                    and a UTF-8 lead byte all keep the backslash
//	the quote, doubled  the quote alone ('it''s', `a``b`, "a""b")
//
// It fails when ClickHouse would not read the lexeme with the same extent: a
// lone quote inside, a trailing backslash, or a \x whose two bytes are not
// both inside the body or include a quote or a backslash (ClickHouse would
// read past the closing quote).
func clickhouseUnquote(raw string) (string, bool) {
	if len(raw) < 2 || raw[len(raw)-1] != raw[0] {
		return "", false
	}
	q, body := raw[0], raw[1:len(raw)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == q:
			if i+1 >= len(body) || body[i+1] != q {
				return "", false
			}
			b.WriteByte(q)
			i++
		case c == '\\':
			if i+1 >= len(body) {
				return "", false
			}
			i++
			switch n := body[i]; {
			case n == 'x':
				if i+2 >= len(body) {
					return "", false
				}
				h, l := body[i+1], body[i+2]
				if h == q || l == q || h == '\\' || l == '\\' {
					return "", false
				}
				b.WriteByte(unhexDigit(h)<<4 + unhexDigit(l))
				i += 2
			case n == 'N':
			case strings.IndexByte("abefnrtv0", n) >= 0:
				b.WriteByte("\a\b\x1b\f\n\r\t\v\x00"[strings.IndexByte("abefnrtv0", n)])
			case strings.IndexByte("\\'\"`/=", n) >= 0 || n <= 0x1F:
				b.WriteByte(n)
			default:
				b.WriteByte('\\')
				b.WriteByte(n)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// unhexDigit is ClickHouse's unhex of one byte: 0xFF for a non-hex digit.
// Shifted into the high nibble, only its low four bits survive in the byte,
// as in ClickHouse's unhex2.
func unhexDigit(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0xFF
}

// unicodeQuoted returns the body of a lexeme quoted with the Unicode quotes
// opening … closing. ClickHouse reads ‘…’ as a string and “…” as an identifier,
// with the body verbatim: no escape, no doubled quote, and the first closing
// quote ends it (measured on 26.2 and 26.7.5.10: hex(‘a\_b\x41\N\\’) is
// 615C5F625C7834315C4E5C5C, and “e`f\x41\N” names the column e`f\x41\N).
func unicodeQuoted(raw, opening, closing string) (string, bool) {
	if !strings.HasPrefix(raw, opening) || !strings.HasSuffix(raw, closing) || len(raw) < len(opening)+len(closing) {
		return "", false
	}
	body := raw[len(opening) : len(raw)-len(closing)]
	if strings.Contains(body, closing) {
		return "", false
	}
	return body, true
}

// heredocBody returns the body of $tag$body$tag$.
func heredocBody(raw string) (string, bool) {
	if len(raw) < 2 || raw[0] != '$' {
		return "", false
	}
	j := strings.IndexByte(raw[1:], '$')
	if j < 0 {
		return "", false
	}
	tag := raw[:j+2]
	if len(raw) < 2*len(tag) || !strings.HasSuffix(raw, tag) {
		return "", false
	}
	return raw[len(tag) : len(raw)-len(tag)], true
}

func quotedBody(body string) (string, bool) {
	if len(body) < 2 || body[0] != '\'' || body[len(body)-1] != '\'' {
		return "", false
	}
	return body[1 : len(body)-1], true
}

func hexStringBytes(body string) (string, bool) {
	h, ok := quotedBody(body)
	if !ok {
		return "", false
	}
	if len(h)%2 == 1 {
		h = "0" + h
	}
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		hi, lo := unhexDigit(h[i]), unhexDigit(h[i+1])
		if hi > 0xF || lo > 0xF {
			return "", false
		}
		b.WriteByte(hi<<4 | lo)
	}
	return b.String(), true
}

func bitStringBytes(body string) (string, bool) {
	bits, ok := quotedBody(body)
	if !ok {
		return "", false
	}
	if r := len(bits) % 8; r != 0 {
		bits = strings.Repeat("0", 8-r) + bits
	}
	var b strings.Builder
	for i := 0; i < len(bits); i += 8 {
		var v byte
		for _, c := range []byte(bits[i : i+8]) {
			if c != '0' && c != '1' {
				return "", false
			}
			v = v<<1 | (c - '0')
		}
		b.WriteByte(v)
	}
	return b.String(), true
}

// numberSpelling spells a numeric literal by value and shows its source.
func numberSpelling(raw string, input bool) spelling {
	k, ok := numberKey(raw)
	if !ok {
		return undecodable(raw, input)
	}
	return spelling{"N:" + k, raw}
}

var (
	maxUInt64        = new(big.Int).SetUint64(math.MaxUint64)
	decimalFloatForm = regexp.MustCompile(`^([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]{1,3})?$`)
	hexFloatForm     = regexp.MustCompile(`^0[xX]([0-9a-fA-F]+\.?[0-9a-fA-F]*|\.[0-9a-fA-F]+)[pP][+-]?[0-9]{1,4}$`)
)

// numberKey is a numeric literal's value as ClickHouse types it (measured
// with toTypeName on 26.2 and 26.7.5.10). A decimal, 0x hex or 0b binary
// integer up to 2^64-1 is an unsigned integer, so 0x1F, 0b11111 and 31 are
// the same UInt8 and share a key; underscores are ignored (1_000 is 1000).
// Every other literal (1e3, .5, 1.50, 0x1Fp1, an integer above 2^64-1) is a
// Float64, keyed by its exact rational value behind an F, so 1e3 (Float64)
// and 1000 (UInt16) differ while .5 and 0.50 agree. Any other form is not
// decoded, nor is an exponent of more than three decimal or four binary
// digits: the bound keeps the rational small, and ClickHouse already rejects
// 1e400 as out of the Float64 range.
func numberKey(raw string) (string, bool) {
	s := strings.ReplaceAll(raw, "_", "")
	var n big.Int
	switch {
	case isDigits(s):
		n.SetString(s, 10)
	case len(s) > 2 && (s[:2] == "0x" || s[:2] == "0X") && isHexDigits(s[2:]):
		n.SetString(s[2:], 16)
	case isBinaryLiteral(s):
		n.SetString(s[2:], 2)
	case decimalFloatForm.MatchString(s):
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			return "", false
		}
		return "F" + r.RatString(), true
	case hexFloatForm.MatchString(s):
		f, _, err := big.ParseFloat(s, 0, uint(4*len(s)+64), big.ToNearestEven)
		if err != nil || f.IsInf() {
			return "", false
		}
		r, _ := f.Rat(nil)
		return "F" + r.RatString(), true
	default:
		return "", false
	}
	if n.Cmp(maxUInt64) > 0 {
		return "F" + new(big.Rat).SetInt(&n).RatString(), true
	}
	return n.String(), true
}

func isHexDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if unhexDigit(s[i]) > 0xF {
			return false
		}
	}
	return s != ""
}

// isBinaryLiteral reports a 0b binary integer, which Polyglot lexes as an
// IDENTIFIER token.
func isBinaryLiteral(s string) bool {
	s = strings.ReplaceAll(s, "_", "")
	if len(s) < 3 || s[:2] != "0b" {
		return false
	}
	for _, c := range s[2:] {
		if c != '0' && c != '1' {
			return false
		}
	}
	return true
}

// An operand the precedence rules accept is a single token (a name, a
// literal, NULL / TRUE / FALSE), a dotted name (t.a) or a {name:Type}
// parameter: both ClickHouse and Polyglot bind each of them tighter than any
// operator.
func isAtomicToken(tk rawToken) bool {
	switch tk.TokenType {
	case "VAR", "QUOTED_IDENTIFIER", "NUMBER", "HEX_NUMBER", "NULL", "TRUE", "FALSE":
		return true
	case "IDENTIFIER":
		return isBinaryLiteral(tk.Text)
	}
	return isQuotedLexeme(tk.TokenType)
}

// operandBefore returns the index where the operand ending at toks[j] starts,
// or -1 when that operand is not one the precedence rules accept.
func operandBefore(toks []rawToken, j int) int {
	if j < 0 {
		return -1
	}
	switch {
	case toks[j].TokenType == "R_BRACE":
		for k := j - 1; k >= 0; k-- {
			switch t := toks[k].TokenType; {
			case t == "L_BRACE":
				return k
			case isOpenBracket(t) || isCloseBracket(t):
				return -1
			}
		}
		return -1
	case isNameTok(toks[j].TokenType):
		for j >= 2 && toks[j-1].TokenType == "DOT" && isNameTok(toks[j-2].TokenType) {
			j -= 2
		}
		return j
	case isAtomicToken(toks[j]):
		return j
	}
	return -1
}

// operandAfter returns the index just past the operand starting at toks[k],
// or -1 when that operand is not one the precedence rules accept.
func operandAfter(toks []rawToken, k int) int {
	if k >= len(toks) {
		return -1
	}
	switch {
	case toks[k].TokenType == "L_BRACE":
		for j := k + 1; j < len(toks); j++ {
			switch t := toks[j].TokenType; {
			case t == "R_BRACE":
				return j + 1
			case isOpenBracket(t) || isCloseBracket(t):
				return -1
			}
		}
		return -1
	case isNameTok(toks[k].TokenType):
		for k+2 < len(toks) && toks[k+1].TokenType == "DOT" && isNameTok(toks[k+2].TokenType) {
			k += 2
		}
		return k + 1
	case isAtomicToken(toks[k]):
		return k + 1
	}
	return -1
}

func isOpenBracket(t string) bool  { return t == "L_PAREN" || t == "L_BRACKET" || t == "L_BRACE" }
func isCloseBracket(t string) bool { return t == "R_PAREN" || t == "R_BRACKET" || t == "R_BRACE" }

// opensExpression and closesExpression are the tokens that may border an
// isolated operator application: clause keywords, commas, brackets, and
// AND / OR, which bind looser than every operator the rules respell, in both
// spellings (measured with formatQuery on 26.2 and 26.7.5.10: a <=> b AND c,
// c OR a <=> b OR d, a NOT LIKE b AND c, c AND a NOT LIKE b, a <=> b AS x,
// WHEN / THEN / ELSE, WHERE and ORDER BY … DESC borders). The AND of
// x BETWEEN y AND z is not a border: x BETWEEN 1 AND a <=> b respelled is
// (x BETWEEN 1 AND a) <=> b.
var (
	opensExpression = map[string]bool{
		"L_PAREN": true, "COMMA": true, "SELECT": true, "DISTINCT": true, "WHERE": true, "PREWHERE": true,
		"HAVING": true, "WHEN": true, "THEN": true, "ELSE": true, "BY": true, "AND": true, "OR": true,
	}
	closesExpression = map[string]bool{
		"R_PAREN": true, "COMMA": true, "SEMICOLON": true, "FROM": true, "WHERE": true, "PREWHERE": true,
		"GROUP": true, "ORDER": true, "HAVING": true, "LIMIT": true, "SETTINGS": true, "FORMAT": true,
		"AS": true, "WHEN": true, "THEN": true, "ELSE": true, "END": true, "AND": true, "OR": true,
		"ASC": true, "DESC": true, "UNION": true,
	}
)

// isolated reports whether the operator toks[first..last] applies to two
// accepted operands and is bordered on both sides, so no neighbouring
// operator can regroup it in either spelling.
func isolated(toks []rawToken, first, last int) bool {
	start, end := operandBefore(toks, first-1), operandAfter(toks, last+1)
	if start < 0 || end < 0 {
		return false
	}
	if end < len(toks) && !closesExpression[toks[end].TokenType] {
		return false
	}
	if start == 0 {
		return true
	}
	b := toks[start-1].TokenType
	return opensExpression[b] && !(b == "AND" && isBetweenAnd(toks, start-1))
}

// isBetweenAnd reports whether the AND at toks[j] is the one of a BETWEEN.
func isBetweenAnd(toks []rawToken, j int) bool {
	depth := 0
	for k := j - 1; k >= 0; k-- {
		switch t := toks[k].TokenType; {
		case isCloseBracket(t):
			depth++
		case isOpenBracket(t):
			if depth == 0 {
				return false
			}
			depth--
		case depth > 0:
		case t == "BETWEEN":
			return true
		case opensExpression[t]:
			return false
		}
	}
	return false
}

// The ternary a ? b : c binds looser than every other operator except the
// lambda arrow, in ClickHouse and in Polyglot alike. Measured with
// formatQuery on 26.2 and 26.7.5.10 against Polyglot's IF(…): each operator
// in ternaryOperators on either side of ? and :, NOT, unary minus, t.a, a[1],
// f(b), (c) and {p:T} operands, and each border in ternaryOpens /
// ternaryCloses. AND / OR are not borders here: they bind tighter than the
// ternary.
var (
	ternaryOperators = map[string]bool{
		"EQ": true, "NEQ": true, "LT": true, "GT": true, "LTE": true, "GTE": true, "PLUS": true, "DASH": true,
		"STAR": true, "SLASH": true, "PERCENT": true, "D_PIPE": true, "AND": true, "OR": true, "NOT": true,
		"LIKE": true, "I_LIKE": true, "IN": true, "BETWEEN": true, "IS": true, "DOT": true,
	}
	ternaryOpens = map[string]bool{
		"COMMA": true, "SELECT": true, "DISTINCT": true, "WHERE": true, "PREWHERE": true, "HAVING": true,
		"WHEN": true, "THEN": true, "ELSE": true, "BY": true, "ARROW": true,
	}
	ternaryCloses = map[string]bool{
		"COMMA": true, "SEMICOLON": true, "FROM": true, "WHERE": true, "PREWHERE": true, "GROUP": true,
		"ORDER": true, "HAVING": true, "LIMIT": true, "SETTINGS": true, "FORMAT": true, "AS": true,
		"WHEN": true, "THEN": true, "ELSE": true, "END": true, "ASC": true, "DESC": true, "UNION": true,
	}
)

// ternaryColon returns the index of the : of the ternary whose ? is toks[q]
// when Polyglot's IF(a, b, c) regroups nothing, and -1 otherwise. The ternary
// must run from a border in ternaryOpens (or an opening bracket, or the start)
// to one in ternaryCloses (or a closing bracket, or the end). At its own
// bracket depth it may hold only operands and ternaryOperators, one ? and one
// :, each branch non-empty and ending in an operand, and no operand directly
// after another (an implicit alias, INTERVAL 1 DAY, x DIV y).
func ternaryColon(toks []rawToken, q int) int {
	start := 0 // the first token of the ternary
	for k, depth := q-1, 0; k >= 0; k-- {
		t := toks[k].TokenType
		if isCloseBracket(t) {
			depth++
		} else if isOpenBracket(t) {
			if depth == 0 {
				start = k + 1
				break
			}
			depth--
		} else if depth == 0 && ternaryOpens[t] {
			start = k + 1
			break
		}
	}
	end := len(toks) // one past the last token of the ternary
	for k, depth := q+1, 0; k < len(toks); k++ {
		t := toks[k].TokenType
		if isOpenBracket(t) {
			depth++
		} else if isCloseBracket(t) {
			if depth == 0 {
				end = k
				break
			}
			depth--
		} else if depth == 0 && ternaryCloses[t] {
			end = k
			break
		}
	}
	colon, afterOperand := -1, false
	for k, depth := start, 0; k < end; k++ {
		tk := toks[k]
		t := tk.TokenType
		switch {
		case isOpenBracket(t):
			if depth == 0 && afterOperand && t != "L_BRACKET" && !(t == "L_PAREN" && isNameTok(toks[k-1].TokenType)) {
				return -1 // (a)(b), 1(b), a {p:T}: not a call or a subscript
			}
			depth++
		case isCloseBracket(t):
			depth--
			if depth == 0 {
				afterOperand = true
			}
		case depth > 0:
		case k == q || (t == "COLON" && colon < 0 && k > q):
			if !afterOperand {
				return -1 // an empty or unfinished branch
			}
			if k != q {
				colon = k
			}
			afterOperand = false
		case isAtomicToken(tk):
			if afterOperand {
				return -1
			}
			afterOperand = true
		case ternaryOperators[t]:
			afterOperand = false
		default:
			return -1
		}
	}
	if colon < 0 || !afterOperand {
		return -1
	}
	return colon
}

// spellingDiff returns the spellings of in missing from out and those of out
// missing from in, both sorted by key, as their show text. The generator may
// add an alias's AS and the = of ENGINE = / SETTINGS k = v, and prints a
// comma join as CROSS JOIN, so those additions are neutral.
func spellingDiff(in, out []spelling) (lost, added []string) {
	count := map[string]int{}
	show := map[string]string{}
	for _, s := range in {
		count[s.key]++
		if _, ok := show[s.key]; !ok {
			show[s.key] = s.show
		}
	}
	for _, s := range out {
		if count[s.key]--; count[s.key] < 0 {
			show[s.key] = s.show
		}
	}
	if c, j := count["W:CROSS"], count["W:JOIN"]; c < 0 && j < 0 {
		n := min(-c, -j)
		count["W:CROSS"] += n
		count["W:JOIN"] += n
	}
	var lostKeys, addedKeys []string
	for s, n := range count {
		if n < 0 && (s == "W:AS" || s == "O:EQ") {
			continue
		}
		for ; n > 0; n-- {
			lostKeys = append(lostKeys, s)
		}
		for ; n < 0; n++ {
			addedKeys = append(addedKeys, s)
		}
	}
	sort.Strings(lostKeys)
	sort.Strings(addedKeys)
	for _, k := range lostKeys {
		lost = append(lost, show[k])
	}
	for _, k := range addedKeys {
		added = append(added, show[k])
	}
	return lost, added
}

// spellingList renders the shown spellings for an error message.
func spellingList(s []string) string {
	if len(s) == 0 {
		return "nothing"
	}
	return "[" + strings.Join(s, " ") + "]"
}
