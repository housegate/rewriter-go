package engine

import "strings"

// decodeIdentifierEscapes finishes the ClickHouse quoted-identifier decode that
// Polyglot's tokenizer leaves incomplete, so every function / table / setting
// name matcher compares the name ClickHouse actually resolves.
//
// Polyglot already strips the surrounding quotes and resolves the quote-level
// escapes of a QUOTED_IDENTIFIER (backtick, double-quote and the unicode “…”
// form): a doubled quote and a `\xHH` byte are gone from its .Text / AST .name.
// It does NOT resolve the remaining backslash escapes ClickHouse's
// readBackQuotedStringWithSQLStyle / readDoubleQuotedStringWithSQLStyle apply,
// so a name such as `\Nin`, `i\Nn` or `in\N` reaches a matcher undecoded and,
// compared literally, hides the real function ClickHouse runs
// (measured on ClickHouse 26.2: ``\Nin``, ``i\Nn`` and ``in\N`` all resolve to
// the IN operator, which reads its second operand as a table). This decoder
// applies ClickHouse's identifier escaping to that residual text:
//
//   - `\xHH` (exactly two hex digits) -> that byte;
//   - `\N` -> nothing (the NULL escape);
//   - the C-style control escapes \a \b \f \n \r \t \v \0 -> their byte;
//   - `\\`, `` \` ``, `\'`, `\"` -> the bare quote / backslash;
//   - every other escape keeps its backslash (ClickHouse does not drop it, so
//     ``\in`` stays ``\in`` and remains an unknown function, no read);
//   - a `\x` without two hex digits is one such unknown escape.
//
// ok is false only when the text cannot be decoded (a trailing lone backslash);
// the caller refuses the statement. Valid Polyglot tokens never produce that,
// because Polyglot fails the tokenize for a malformed quoted identifier and
// every opaque-text scanner already fails closed on a tokenize error.
//
// The decode does not re-wrap or re-quote and is idempotent for a name with no
// backslash (the common case takes the fast path below), so it is safe to call
// on every name a matcher inspects.
func decodeIdentifierEscapes(s string) (string, bool) {
	if !strings.Contains(s, "\\") {
		return s, true
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			return "", false // trailing lone backslash: cannot decode
		}
		switch n := s[i+1]; n {
		case 'x', 'X':
			if i+3 < len(s) && isHexDigit(s[i+2]) && isHexDigit(s[i+3]) {
				b.WriteByte(hexNibble(s[i+2])<<4 | hexNibble(s[i+3]))
				i += 4
				continue
			}
			// Not two hex digits: an unknown escape ClickHouse keeps verbatim.
			b.WriteByte('\\')
			b.WriteByte(n)
			i += 2
		case 'N':
			i += 2 // the NULL escape decodes to nothing
		case 'a':
			b.WriteByte('\a')
			i += 2
		case 'b':
			b.WriteByte('\b')
			i += 2
		case 'f':
			b.WriteByte('\f')
			i += 2
		case 'n':
			b.WriteByte('\n')
			i += 2
		case 'r':
			b.WriteByte('\r')
			i += 2
		case 't':
			b.WriteByte('\t')
			i += 2
		case 'v':
			b.WriteByte('\v')
			i += 2
		case '0':
			b.WriteByte(0)
			i += 2
		case '\\', '`', '\'', '"':
			b.WriteByte(n)
			i += 2
		default:
			// Every other escape keeps its backslash: ClickHouse reads `\in` as
			// the two-character name \in, an unknown function that reads nothing.
			b.WriteByte('\\')
			b.WriteByte(n)
			i += 2
		}
	}
	return b.String(), true
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
