package engine

import (
	"strings"
	"unicode/utf8"
)

// ClickHouse identifier and string-literal escape decoding.
//
// ClickHouse's ParserIdentifier reads a quoted identifier with
// readBackQuotedStringWithSQLStyle / readDoubleQuotedStringWithSQLStyle, and a
// string literal with readQuotedStringWithSQLStyle. All three share one escape
// rule (parseComplexEscapeSequence), measured on ClickHouse 26.2:
//
//   - `\xHH` -> that byte;
//   - `\N` -> nothing (the NULL escape);
//   - `\a \b \e \f \n \r \t \v \0` -> their control byte (`\e` is ESC 0x1b);
//   - `\\ \' \" \` \/ \=` -> the bare character;
//   - a backslash before a raw control byte (<= 0x1f) -> that byte;
//   - every other escape keeps its backslash (`\in` stays `\in`, `\:` stays
//     `\:`), so it names an unknown function, not `in`.
//
// A doubled quote character inside its own quotes is one quote character. The
// English-style “…” identifier form takes no escapes at all.
//
// Polyglot resolves only part of this (quotes, doubled quotes, `\xHH`, some
// control escapes) and keeps `\N`, `\e`, `\/`, `\=` and unknown escapes
// verbatim in its AST names and token text. The rewriter therefore decodes
// every quoted identifier itself, from its source spelling, when it reads the
// Polyglot AST (ParseOne) or tokens (tokenizeRaw): every downstream comparison
// sees the name ClickHouse resolves, and the generator (which escapes a
// backslash in a quoted name) emits that same name.

// clickHouseEscape decodes the escape whose backslash is at s[i] and returns
// the bytes it denotes and the index just past it. ok is false when the escape
// cannot be decoded: a trailing backslash, or `\x` not followed by two hex
// digits (ClickHouse reads the next two bytes blindly, so a non-hex pair is
// either a garbage byte or swallows the closing quote; both are refused).
func clickHouseEscape(s string, i int) (out string, next int, ok bool) {
	if i+1 >= len(s) {
		return "", i, false
	}
	c := s[i+1]
	switch c {
	case 'x', 'X':
		if i+3 < len(s) && isHexDigit(s[i+2]) && isHexDigit(s[i+3]) {
			return string([]byte{hexNibble(s[i+2])<<4 | hexNibble(s[i+3])}), i + 4, true
		}
		return "", i, false
	case 'N':
		return "", i + 2, true
	case 'a':
		return "\a", i + 2, true
	case 'b':
		return "\b", i + 2, true
	case 'e':
		return "\x1b", i + 2, true
	case 'f':
		return "\f", i + 2, true
	case 'n':
		return "\n", i + 2, true
	case 'r':
		return "\r", i + 2, true
	case 't':
		return "\t", i + 2, true
	case 'v':
		return "\v", i + 2, true
	case '0':
		return "\x00", i + 2, true
	case '\\', '\'', '"', '`', '/', '=':
		return string([]byte{c}), i + 2, true
	}
	if c <= 0x1f {
		return string([]byte{c}), i + 2, true
	}
	return string([]byte{'\\', c}), i + 2, true
}

// decodeIdentifierEscapes applies the ClickHouse escape rule to a name that has
// no surrounding quotes: a Polyglot function name (the AST carries no source
// span for it, so it cannot be decoded from source) or a defensive re-check of
// an already-decoded name. It is idempotent for a name with no backslash. A
// `\x` without two hex digits is kept verbatim here (the name came from a
// token Polyglot already accepted). ok is false for a trailing lone backslash.
//
// Because Polyglot already collapsed a source `\\` to one backslash, applying
// this to a Polyglot name can decode one level too far (`\\Nin` -> `in`): the
// caller then refuses a name ClickHouse would treat as unknown, which is the
// safe direction, and on the Raw ALTER path it is exactly what ClickHouse reads
// from the regenerated text.
func decodeIdentifierEscapes(s string) (string, bool) {
	if !strings.Contains(s, "\\") {
		return s, true
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 >= len(s) {
			return "", false
		}
		if n := s[i+1]; (n == 'x' || n == 'X') && !(i+3 < len(s) && isHexDigit(s[i+2]) && isHexDigit(s[i+3])) {
			b.WriteByte('\\')
			b.WriteByte(n)
			i += 2
			continue
		}
		out, next, ok := clickHouseEscape(s, i)
		if !ok {
			return "", false
		}
		b.WriteString(out)
		i = next
	}
	return b.String(), true
}

// Unicode English-style quotes: “ (U+201C) opens, ” (U+201D) closes.
const (
	leftDoubleQuote  = "“"
	rightDoubleQuote = "”"
)

// decodeQuotedIdentifier decodes a quoted identifier from its exact source
// spelling (backtick, double-quote or “…”), exactly as ClickHouse's
// ParserIdentifier does. ok is false when the spelling is not a well-formed
// quoted identifier, when an escape cannot be decoded, when the name is empty
// (ClickHouse rejects an empty identifier) or when the decoded bytes are not
// valid UTF-8 (the AST and the generator carry names as UTF-8 text, so such a
// name could not be emitted as the name ClickHouse would read).
func decodeQuotedIdentifier(raw string) (string, bool) {
	if strings.HasPrefix(raw, leftDoubleQuote) {
		if len(raw) < len(leftDoubleQuote)+len(rightDoubleQuote) || !strings.HasSuffix(raw, rightDoubleQuote) {
			return "", false
		}
		name := raw[len(leftDoubleQuote) : len(raw)-len(rightDoubleQuote)]
		return name, name != "" && utf8.ValidString(name)
	}
	if len(raw) < 2 || (raw[0] != '`' && raw[0] != '"') {
		return "", false
	}
	name, ok := decodeQuotedBody(raw, raw[0])
	return name, ok && name != ""
}

// decodeQuotedString decodes a single-quoted string literal from its exact
// source spelling, as ClickHouse's readQuotedStringWithSQLStyle does. ok is
// false for anything that is not a well-formed single-quoted literal (a
// heredoc, for example) or whose decoded bytes are not valid UTF-8.
func decodeQuotedString(raw string) (string, bool) {
	if len(raw) < 2 || raw[0] != '\'' {
		return "", false
	}
	return decodeQuotedBody(raw, '\'')
}

// decodeQuotedBody decodes raw = q … q with SQL-style doubled quotes and the
// ClickHouse escape rule. The closing quote must be the last byte.
func decodeQuotedBody(raw string, q byte) (string, bool) {
	var b strings.Builder
	b.Grow(len(raw))
	for i := 1; i < len(raw); {
		switch c := raw[i]; {
		case c == q:
			if i+1 < len(raw) && raw[i+1] == q {
				b.WriteByte(q)
				i += 2
				continue
			}
			if i != len(raw)-1 {
				return "", false // text after the closing quote
			}
			out := b.String()
			return out, utf8.ValidString(out)
		case c == '\\':
			out, next, ok := clickHouseEscape(raw, i)
			if !ok || next > len(raw)-1 {
				return "", false // bad escape, or it consumed the closing quote
			}
			b.WriteString(out)
			i = next
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", false // unterminated
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// quoteIdentifierSQL renders name as a backtick-quoted ClickHouse identifier
// that ClickHouse decodes back to exactly name: a backtick is doubled, a
// backslash is escaped, and a control byte is written as \xHH, so a decoded
// name (which may now hold any of them) is spliced as the same object the
// checks judged rather than re-read through its escapes.
func quoteIdentifierSQL(name string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(name) + 2)
	b.WriteByte('`')
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c == '`':
			b.WriteString("``")
		case c == '\\':
			b.WriteString(`\\`)
		case c < 0x20 || c == 0x7f:
			b.WriteString(`\x`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('`')
	return b.String()
}
