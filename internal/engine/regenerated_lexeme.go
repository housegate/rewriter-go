package engine

import (
	"math"
	"math/big"
	"regexp"
	"strings"
)

// This file reads a ClickHouse literal or quoted identifier the way
// ClickHouse does, for CheckRegenerated's comparison (literalSpelling,
// numberSpelling).

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

// numberSpelling spells the numeric literal token tk of src by value and
// shows its source. A literal directly followed by a letter, a digit or an _
// is undecodable: ClickHouse reads the longer lexeme, an identifier (0x1F_,
// 0x1Fg) or a syntax error, where Polyglot reads a number and an alias.
func numberSpelling(src string, tk rawToken, input bool) spelling {
	raw := src[tk.Span.Start:tk.Span.End]
	if tk.Span.End < len(src) && isWordByte(src[tk.Span.End]) {
		return undecodable(raw, input)
	}
	k, ok := numberKey(raw)
	if !ok {
		return undecodable(raw, input)
	}
	return spelling{"N:" + k, raw}
}

func isWordByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isNumberLike reports an IDENTIFIER token that Polyglot lexes from a
// malformed or binary number: it starts with a digit and holds only the
// characters of a number literal (1_e3, 1__0, 1.5a, 0b101). Such a token is
// spelled as a number, so it is compared by value or refused; a name such as
// 2024_events stays an identifier.
func isNumberLike(raw string) bool {
	if raw == "" || raw[0] < '0' || raw[0] > '9' {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; unhexDigit(c) > 0xF && !strings.ContainsRune("_.xXpP", rune(c)) {
			return false
		}
	}
	return true
}

var (
	maxUInt64        = new(big.Int).SetUint64(math.MaxUint64)
	decimalFloatForm = regexp.MustCompile(`^([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]{1,3})?$`)
	hexFloatForm     = regexp.MustCompile(`^0[xX]([0-9a-fA-F]+\.?[0-9a-fA-F]*|\.[0-9a-fA-F]+)[pP][+-]?[0-9]{1,4}$`)
)

// numberKey is a numeric literal's value as ClickHouse types it (measured
// with toTypeName on 26.2 and 26.7.5.10). A decimal, 0x hex or 0b binary
// integer up to 2^64-1 is an unsigned integer, so 0x1F, 0b11111 and 31 are
// the same UInt8 and share a key; an underscore between two digits is ignored
// (1_000 is 1000), and any other underscore is not decoded
// (underscoresBetweenDigits).
// Every other literal (1e3, .5, 1.50, 0x1Fp1, an integer above 2^64-1) is a
// Float64, keyed by its exact rational value behind an F, so 1e3 (Float64)
// and 1000 (UInt16) differ while .5 and 0.50 agree. Any other form is not
// decoded, nor is an exponent of more than three decimal or four binary
// digits: the bound keeps the rational small, and ClickHouse already rejects
// 1e400 as out of the Float64 range.
func numberKey(raw string) (string, bool) {
	if !underscoresBetweenDigits(raw) {
		return "", false
	}
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

// underscoresBetweenDigits reports whether every _ of a numeric literal sits
// between two digits of its radix, hex after 0x and binary after 0b. Measured
// on 26.2: 1_000, 0x1_F, 0x1e_3, 0b1_01, 1.5_0, 1e1_0, 1_000.5, 0_1 and
// 0x1p1_0 are numbers; 1e_3, 0x_1F, 1_e3, 1__0, 1_, 0x1F_ and 0b_1 are
// identifiers; 1._5 is a syntax error.
func underscoresBetweenDigits(raw string) bool {
	digit := func(c byte) bool { return c >= '0' && c <= '9' }
	if len(raw) > 1 && raw[0] == '0' {
		switch raw[1] {
		case 'x', 'X':
			digit = func(c byte) bool { return unhexDigit(c) <= 0xF }
		case 'b', 'B':
			digit = func(c byte) bool { return c == '0' || c == '1' }
		}
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] == '_' && (i == 0 || i == len(raw)-1 || !digit(raw[i-1]) || !digit(raw[i+1])) {
			return false
		}
	}
	return true
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
