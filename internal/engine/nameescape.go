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
// the bytes it denotes and the index just past it. ok is false only when
// ClickHouse itself cannot read the escape: a trailing backslash, or a `\x`
// with fewer than two bytes after it. `\x` always consumes the next two bytes,
// valid hex or not (clickHouseUnhex2), exactly as ClickHouse does; a caller
// reading a quoted body must still check that those bytes did not include the
// closing quote.
func clickHouseEscape(s string, i int) (out string, next int, ok bool) {
	if i+1 >= len(s) {
		return "", i, false
	}
	c := s[i+1]
	switch c {
	case 'x', 'X':
		if i+3 >= len(s) {
			return "", i, false
		}
		return string([]byte{clickHouseUnhex2(s[i+2], s[i+3])}), i + 4, true
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

// clickHouseUnhex2 is ClickHouse's unhex2: each byte maps to its hex digit
// value, or to -1 when it is not a hex digit, and the pair combines as
// d0*16 + d1 modulo 256. Measured on ClickHouse 26.2: `\x6Z` is '_', `\x7Z`
// is 'o' (so `n\x7ZtIn` is notIn), `\xZ0` is 0xF0, `\xZZ` is 0xEF.
func clickHouseUnhex2(a, b byte) byte {
	digit := func(c byte) int {
		switch {
		case c >= '0' && c <= '9':
			return int(c - '0')
		case c >= 'a' && c <= 'f':
			return int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			return int(c-'A') + 10
		}
		return -1
	}
	return byte((digit(a)*16 + digit(b)) & 0xff)
}

// decodeIdentifierEscapes applies the ClickHouse escape rule to a function
// name read from the Polyglot AST. A function node carries no source span, so
// its source spelling cannot be decoded exactly; Polyglot has already resolved
// part of it (quotes, doubled quotes, most `\xHH`) and keeps the rest (`\N`,
// `\e`, `\/`, `\=`, non-hex `\x`, unknown escapes) verbatim. Finishing that
// decode can read one level too far where Polyglot collapsed a source `\\`
// first (`\\Nin` -> `in`, `\\x69n` -> `in`): the caller then treats an
// unknown function as the IN family, which only governs or refuses more (the
// accepted Low). Token text and AST identifier names are decoded exactly from
// source and must never be passed through this again. ok is false for a
// trailing lone backslash.
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
	leftDoubleQuote  = "\u201c"
	rightDoubleQuote = "\u201d"
)

// quotedDecode classifies decoding a quoted spelling as ClickHouse does.
type quotedDecode uint8

const (
	// decodedExact: the value ClickHouse reads, valid UTF-8.
	decodedExact quotedDecode = iota
	// decodedNotUTF8: ClickHouse reads these bytes, but they are not valid
	// UTF-8, so they cannot travel through the AST or a protobuf string. The
	// caller keeps Polyglot's own (valid UTF-8) value, as before round 1. Such a
	// name holds a non-ASCII byte in ClickHouse's reading, so it can never be
	// one of the ASCII protected, reserved or Active names a check looks for.
	decodedNotUTF8
	// decodedRejected: ClickHouse itself rejects the spelling (unterminated, a
	// `\x` that swallows the closing quote, a trailing backslash, or an empty
	// identifier).
	decodedRejected
)

// decodeQuotedIdentifier decodes a quoted identifier from its exact source
// spelling (backtick, double-quote or “…”), exactly as ClickHouse's
// ParserIdentifier does.
func decodeQuotedIdentifier(raw string) (string, quotedDecode) {
	if strings.HasPrefix(raw, leftDoubleQuote) {
		if len(raw) < len(leftDoubleQuote)+len(rightDoubleQuote) || !strings.HasSuffix(raw, rightDoubleQuote) {
			return "", decodedRejected
		}
		name := raw[len(leftDoubleQuote) : len(raw)-len(rightDoubleQuote)]
		if name == "" {
			return "", decodedRejected
		}
		return name, decodedExact
	}
	if len(raw) < 2 || (raw[0] != '`' && raw[0] != '"') {
		return "", decodedRejected
	}
	name, st := decodeQuotedBody(raw, raw[0])
	if st == decodedExact && name == "" {
		return "", decodedRejected
	}
	return name, st
}

// decodeQuotedString decodes a single-quoted string literal from its exact
// source spelling, as ClickHouse's readQuotedStringWithSQLStyle does. A literal
// is never refused for its bytes: the caller keeps Polyglot's value for
// anything but decodedExact.
func decodeQuotedString(raw string) (string, quotedDecode) {
	if len(raw) < 2 || raw[0] != '\'' {
		return "", decodedRejected
	}
	return decodeQuotedBody(raw, '\'')
}

// decodeQuotedBody decodes raw = q … q with SQL-style doubled quotes and the
// ClickHouse escape rule. The closing quote must be the last byte.
func decodeQuotedBody(raw string, q byte) (string, quotedDecode) {
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
				return "", decodedRejected // text after the closing quote
			}
			out := b.String()
			if !utf8.ValidString(out) {
				return out, decodedNotUTF8
			}
			return out, decodedExact
		case c == '\\':
			out, next, ok := clickHouseEscape(raw, i)
			if !ok || next > len(raw)-1 {
				return "", decodedRejected // bad escape, or it consumed the closing quote
			}
			b.WriteString(out)
			i = next
		default:
			b.WriteByte(c)
			i++
		}
	}
	return "", decodedRejected // unterminated
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
