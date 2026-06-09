// Package reverse ports the C++ doRewriteErrorMessage inversion: it substitutes
// physical (rewritten) table/database names in a ClickHouse error message back to
// the logical names the client used, using the forward rewrite maps captured
// during the most recent successful Rewrite. Pure stdlib — no engine, no polyglot.
//
// Go's regexp (RE2) has no lookahead, so the boundary-aware identifier matches
// the C++ builds with `(^|[^\w])…(?=[^\w]|$)` are reproduced by a boundary-checked
// scan (FindAllStringIndex + isWordByte on the surrounding bytes), which keeps the
// boundaries NON-consuming — two adjacent same-target hits separated by one
// boundary char both fire, matching the C++ lookahead.
package reverse

import (
	"regexp"
	"strings"
)

// isWordByte reports whether b is an ASCII identifier byte ([A-Za-z0-9_]) — the
// complement of the C++ boundary class [^A-Za-z0-9_].
func isWordByte(b byte) bool {
	return b == '_' ||
		(b >= '0' && b <= '9') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= 'a' && b <= 'z')
}

// substituteBoundary replaces every boundary-delimited match of core in s with
// repl. A match qualifies only when the byte before it is start-of-string or a
// non-word byte AND the byte after it is end-of-string or a non-word byte. The
// boundary bytes are NOT consumed (FindAllStringIndex yields non-overlapping
// leftmost matches; boundary failures are skipped, not replaced).
func substituteBoundary(s string, core *regexp.Regexp, repl string) string {
	locs := core.FindAllStringIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		st, en := loc[0], loc[1]
		beforeOK := st == 0 || !isWordByte(s[st-1])
		afterOK := en == len(s) || !isWordByte(s[en])
		if !beforeOK || !afterOK {
			continue
		}
		b.WriteString(s[last:st])
		b.WriteString(repl)
		last = en
	}
	b.WriteString(s[last:])
	return b.String()
}

// identCore compiles the case-insensitive core for makeIdentRegex: the identifier
// optionally wrapped in backticks.
func identCore(ident string) *regexp.Regexp {
	return regexp.MustCompile("(?i)`?" + regexp.QuoteMeta(ident) + "`?")
}

// qualifiedCore compiles the case-insensitive core for makeQualifiedIdentRegex.
// Splits on the FIRST dot (how table_rewrites encodes "<db>.<table>", with the
// dynamic prefix packed into the table half). If the table half itself contains a
// dot, ClickHouse's WhenNecessary formatter wraps it in backticks, so require them
// (avoids greedy-matching across an unrelated ".foo" suffix); otherwise optional.
func qualifiedCore(qualified string) *regexp.Regexp {
	dot := strings.IndexByte(qualified, '.')
	if dot < 0 {
		return identCore(qualified)
	}
	db, table := qualified[:dot], qualified[dot+1:]
	tablePat := "`?" + regexp.QuoteMeta(table) + "`?"
	if strings.Contains(table, ".") {
		tablePat = "`" + regexp.QuoteMeta(table) + "`"
	}
	return regexp.MustCompile("(?i)`?" + regexp.QuoteMeta(db) + "`?\\." + tablePat)
}

// substituteIdent replaces boundary-delimited occurrences of target (optionally
// backticked) in s with repl. Mirrors makeIdentRegex + its $1-preserving replace.
func substituteIdent(s, target, repl string) string {
	return substituteBoundary(s, identCore(target), repl)
}

// substituteQualified replaces boundary-delimited occurrences of the qualified
// "db.table" name in s with repl. Mirrors makeQualifiedIdentRegex.
func substituteQualified(s, qualified, repl string) string {
	return substituteBoundary(s, qualifiedCore(qualified), repl)
}

// flexibleSQLReplace swaps an occurrence of rewrittenSQL in s for originalSQL:
// a verbatim find replaces the FIRST occurrence; if absent, a whitespace-flexible,
// case-insensitive regex (runs of whitespace in rewrittenSQL match \s+) replaces
// ALL occurrences. Mirrors the C++ whole-SQL replacement.
func flexibleSQLReplace(s, rewrittenSQL, originalSQL string) string {
	if rewrittenSQL == "" {
		return s
	}
	if i := strings.Index(s, rewrittenSQL); i >= 0 {
		return s[:i] + originalSQL + s[i+len(rewrittenSQL):]
	}
	return flexibleSQLRegex(rewrittenSQL).ReplaceAllLiteralString(s, originalSQL)
}

// flexibleSQLRegex builds the whitespace-insensitive, case-insensitive regex that
// matches rewrittenSQL with any run of whitespace collapsed to \s+.
func flexibleSQLRegex(sql string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("(?i)")
	inWS := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch c {
		case ' ', '\t', '\n', '\r', '\f', '\v':
			if !inWS {
				b.WriteString(`\s+`)
				inWS = true
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			inWS = false
		}
	}
	return regexp.MustCompile(b.String())
}
