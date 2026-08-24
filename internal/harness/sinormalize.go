package harness

import "strings"

// NormalizeSIIdentifierQuotes canonicalizes matched ClickHouse backtick
// identifiers to ANSI double-quoted identifiers for the shared Go/C++ corpus.
// It supports only the deliberately narrow SQL subset emitted by the two
// generators: ordinary text, single-quoted literals, matched backtick
// identifiers, and matched ANSI double-quoted identifiers.
//
// This is a comparison guard, not a SQL lexer. If normal-state input contains a
// construct whose boundaries require dialect-aware tokenization -- comments,
// dollar/triple/smart quotes, prefixed literals, or INSERT ... FORMAT raw data --
// the function returns the complete original SQL unchanged. It does the same
// for every unmatched supported delimiter. That fail-closed choice may leave a
// harmless identifier-style difference visible (false inequality), but it can
// never normalize bytes inside an uncertain literal or payload and hide a real
// semantic difference (false equality).
//
// The C++ mirror in rewriter-grpc/tests/si_normalize.h must keep this exact
// supported domain and rollback behavior.
func NormalizeSIIdentifierQuotes(sql string) string {
	var out strings.Builder
	out.Grow(len(sql))

	sawInsert := false
	for i := 0; i < len(sql); {
		switch sql[i] {
		case '\'':
			if strings.HasPrefix(sql[i:], "'''") || siHasLiteralPrefix(sql, i) {
				return sql
			}
			end, ok := siAppendSingleQuoted(&out, sql, i)
			if !ok {
				return sql
			}
			i = end
		case '"':
			if strings.HasPrefix(sql[i:], `"""`) || siHasLiteralPrefix(sql, i) {
				return sql
			}
			end, ok := siAppendANSIIdentifier(&out, sql, i)
			if !ok {
				return sql
			}
			i = end
		case '`':
			end, ok := siAppendBacktickIdentifier(&out, sql, i)
			if !ok {
				return sql
			}
			i = end
		case '-':
			if i+1 < len(sql) && sql[i+1] == '-' {
				return sql
			}
			out.WriteByte(sql[i])
			i++
		case '#', '$':
			return sql
		case '/':
			if i+1 < len(sql) && (sql[i+1] == '/' || sql[i+1] == '*') {
				return sql
			}
			out.WriteByte(sql[i])
			i++
		default:
			if siUnsupportedUnicodeQuoteAt(sql, i) {
				return sql
			}
			if siASCIIWordStart(sql[i]) {
				end := i + 1
				for end < len(sql) && siASCIIWordByte(sql[end]) {
					end++
				}
				word := sql[i:end]
				if strings.EqualFold(word, "INSERT") {
					sawInsert = true
				} else if sawInsert && strings.EqualFold(word, "FORMAT") {
					return sql
				}
				out.WriteString(word)
				i = end
				continue
			}
			out.WriteByte(sql[i])
			i++
		}
	}
	return out.String()
}

func siASCIIWordStart(c byte) bool {
	return c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func siASCIIWordByte(c byte) bool {
	return siASCIIWordStart(c) || c >= '0' && c <= '9'
}

func siHasLiteralPrefix(sql string, quote int) bool {
	if quote > 0 && siASCIIWordByte(sql[quote-1]) {
		return true
	}
	return quote >= 2 && sql[quote-1] == '&' && (sql[quote-2] == 'U' || sql[quote-2] == 'u')
}

func siUnsupportedUnicodeQuoteAt(sql string, start int) bool {
	return strings.HasPrefix(sql[start:], "\u2018") ||
		strings.HasPrefix(sql[start:], "\u2019") ||
		strings.HasPrefix(sql[start:], "\u201c") ||
		strings.HasPrefix(sql[start:], "\u201d")
}

func siAppendSingleQuoted(out *strings.Builder, sql string, start int) (int, bool) {
	out.WriteByte('\'')
	for i := start + 1; i < len(sql); i++ {
		out.WriteByte(sql[i])
		switch sql[i] {
		case '\\':
			if i+1 >= len(sql) {
				return 0, false
			}
			i++
			out.WriteByte(sql[i])
		case '\'':
			if i+1 < len(sql) && sql[i+1] == '\'' {
				i++
				out.WriteByte(sql[i])
			} else {
				return i + 1, true
			}
		}
	}
	return 0, false
}

func siAppendANSIIdentifier(out *strings.Builder, sql string, start int) (int, bool) {
	out.WriteByte('"')
	for i := start + 1; i < len(sql); i++ {
		out.WriteByte(sql[i])
		if sql[i] != '"' {
			continue
		}
		if i+1 < len(sql) && sql[i+1] == '"' {
			i++
			out.WriteByte(sql[i])
			continue
		}
		return i + 1, true
	}
	return 0, false
}

func siAppendBacktickIdentifier(out *strings.Builder, sql string, start int) (int, bool) {
	out.WriteByte('"')
	for i := start + 1; i < len(sql); i++ {
		switch sql[i] {
		case '\\':
			if i+1 < len(sql) && sql[i+1] == '`' {
				i++
				out.WriteByte('`')
			} else {
				out.WriteByte(sql[i])
			}
		case '`':
			if i+1 < len(sql) && sql[i+1] == '`' {
				i++
				out.WriteByte('`')
			} else {
				out.WriteByte('"')
				return i + 1, true
			}
		case '"':
			out.WriteString(`""`)
		default:
			out.WriteByte(sql[i])
		}
	}
	return 0, false
}
