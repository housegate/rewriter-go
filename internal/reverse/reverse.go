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
	"sort"
	"strconv"
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

// buildOffsetMap returns a byte-position map from rewritten to original via a
// forward Myers O(ND) diff: result[i] (i in [0,len(rewritten)]) is the byte offset
// in original corresponding to offset i in rewritten; result[N]=len(original).
// Positions inside a deleted span collapse to the surrounding equal-range boundary
// — best-effort but stable enough that "position N" inside a rewritten identifier
// remaps to the start of the user's original identifier. Faithful port of the C++
// buildOffsetMap (rewriter-server.cc:144-229). Runtime O((N+M)*D); D is tiny for
// SQL identifier swaps.
func buildOffsetMap(rewritten, original string) []int {
	N, M := len(rewritten), len(original)
	posMap := make([]int, N+1)
	posMap[N] = M
	if N == 0 {
		return posMap
	}
	if M == 0 {
		for i := 0; i < N; i++ {
			posMap[i] = 0
		}
		return posMap
	}

	MAX := N + M
	kIdx := func(k int) int { return k + MAX }
	V := make([]int, 2*MAX+1)
	var trace [][]int

	foundD := -1
	for d := 0; d <= MAX; d++ {
		snap := make([]int, len(V))
		copy(snap, V)
		trace = append(trace, snap)
		done := false
		for k := -d; k <= d && !done; k += 2 {
			var x int
			down := (k == -d) || (k != d && V[kIdx(k-1)] < V[kIdx(k+1)])
			if down {
				x = V[kIdx(k+1)]
			} else {
				x = V[kIdx(k-1)] + 1
			}
			y := x - k
			for x < N && y < M && rewritten[x] == original[y] {
				x++
				y++
			}
			V[kIdx(k)] = x
			if x >= N && y >= M {
				foundD = d
				done = true
			}
		}
		if done {
			break
		}
	}

	if foundD < 0 {
		for i := 0; i < N; i++ {
			posMap[i] = M
		}
		return posMap
	}

	x, y := N, M
	for d := foundD; d > 0; d-- {
		Vp := trace[d]
		k := x - y
		var down bool
		switch {
		case k == -d:
			down = true
		case k == d:
			down = false
		default:
			down = Vp[kIdx(k-1)] < Vp[kIdx(k+1)]
		}
		prevK := k - 1
		if down {
			prevK = k + 1
		}
		prevX := Vp[kIdx(prevK)]
		prevY := prevX - prevK
		slideStartX := prevX + 1
		slideStartY := prevY
		if down {
			slideStartX = prevX
			slideStartY = prevY + 1
		}
		for x > slideStartX && y > slideStartY {
			x--
			y--
			if x < N {
				posMap[x] = y
			}
		}
		if down {
			y = prevY
		} else {
			if prevX < N {
				posMap[prevX] = prevY
			}
			x = prevX
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		if x < N {
			posMap[x] = y
		}
	}
	for x > 0 {
		x--
		posMap[x] = 0
	}
	return posMap
}

var positionRe = regexp.MustCompile(`([Pp]osition\s+)(\d+)`)

// remapErrorPositions rewrites "position N" byte references (1-based) in err from
// rewritten coordinates to original coordinates via posMap. An N==0 or N past the
// rewritten length is left untouched (probably not a SQL position). Mirrors the
// C++ remapErrorPositions (rewriter-server.cc:244-273).
func remapErrorPositions(err string, posMap []int) string {
	if len(posMap) <= 1 {
		return err
	}
	n := len(posMap) - 1 // rewritten SQL byte length
	return positionRe.ReplaceAllStringFunc(err, func(m string) string {
		sub := positionRe.FindStringSubmatch(m)
		pos1, e := strconv.Atoi(sub[2])
		if e != nil || pos1 == 0 || pos1 > n {
			return m
		}
		return sub[1] + strconv.Itoa(posMap[pos1-1]+1)
	})
}

// Invert maps physical names in a ClickHouse error message back to the logical
// names the client used, using the forward maps from the most recent successful
// Rewrite. Stages mirror doRewriteErrorMessage (rewriter-server.cc:461-513):
//  1. when the rewritten SQL differs from the original: remap "position N" byte
//     refs, then swap the whole rewritten-SQL block back to the original;
//  2. per-table substitution (rewritten qualified name -> origin);
//  3. per-database substitution (physical -> logical).
//
// Empty error returns unchanged. Map entries whose origin==rewritten or whose
// rewritten value is empty are skipped. Caller (native.go) invokes this only when
// the stashed rewrite was Success.
// maxRemapSQL bounds the position-remap stage. buildOffsetMap is a Myers diff —
// O((N+M)*D) time and memory — so for very large SQL (well past any real query)
// the byte-position remap is skipped. The native rewriter runs in-process with no
// gRPC message-size limit shielding it (unlike the C++ service), so this cap is
// the "caller is responsible for bounding input size" guard the C++ buildOffsetMap
// documents. The cheap parts — the whole-SQL block swap and the boundary-delimited
// per-table/per-database substitutions — still run, so name inversion is unaffected;
// only the best-effort "position N" number is left pointing at rewritten coordinates.
const maxRemapSQL = 256 * 1024

func Invert(message, originalSQL, rewrittenSQL string, tableRewrites, databaseRewrites map[string]string) string {
	if message == "" {
		return message
	}
	err := message
	if rewrittenSQL != "" && rewrittenSQL != originalSQL {
		if len(rewrittenSQL) <= maxRemapSQL && len(originalSQL) <= maxRemapSQL {
			posMap := buildOffsetMap(rewrittenSQL, originalSQL)
			err = remapErrorPositions(err, posMap)
		}
		err = flexibleSQLReplace(err, rewrittenSQL, originalSQL)
	}
	for _, origin := range sortedKeys(tableRewrites) {
		rewritten := tableRewrites[origin]
		if origin == rewritten || rewritten == "" {
			continue
		}
		err = substituteQualified(err, rewritten, origin)
	}
	for _, origin := range sortedKeys(databaseRewrites) {
		physical := databaseRewrites[origin]
		if origin == physical || physical == "" {
			continue
		}
		err = substituteIdent(err, physical, origin)
	}
	return err
}

// sortedKeys returns m's keys in sorted order. The C++ iterates the protobuf maps
// in unspecified order; substitutions are boundary-delimited and non-overlapping,
// so order doesn't change the result — sorting only makes Go output deterministic.
func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
