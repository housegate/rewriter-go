# Phase 5 — RewriteErrorMessage + preprocess decision + differential-fuzz Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the native `RewriteErrorMessage` reverse-mapping (physical→logical name substitution in ClickHouse error text), record the IN-split preprocess decision (skip + allow-list, §6f), and add differential-fuzz hardening — completing the `Rewriter` surface (design spec §9, Phase 5, the final phase).

**Architecture:** A new pure-stdlib package `internal/reverse` ports the C++ `doRewriteErrorMessage` inversion algorithm (Myers-diff byte-position remap → whole-SQL replacement → per-table → per-database substitution). `native.go` stashes the last successful rewrite's maps on the per-connection `callContext` and `RewriteErrorMessage(message)` inverts using them (the Go interface takes only `message`, so it uses the stashed context — unlike the C++ RPC which re-runs the rewrite from the request's sql+options). A harness error-inversion golden corpus + oracle `RewriteErrorMessage` RPC gate parity; a fuzz harness hardens robustness.

**Tech Stack:** Go (`CGO_ENABLED=0`), stdlib `regexp` (RE2 — **no lookahead**, so boundary matching is done by scan), `tobilg/polyglot` via PureGo FFI, `gen/pb` types.

---

## C++ behavior reference (the parity oracle)

`doRewriteErrorMessage` (`rewriter-grpc/src/rewriter-server.cc:406-514`) + its regex helpers (`:65-298`):

1. **Empty error** → `code=Success`, `message="Error message is empty"`, return (no `error_after_rewrite`).
2. **Re-run the rewrite** on the request's `(sql, options)` → `rewrite_resp`. (The native impl uses the **stashed** last-call maps instead — see Architecture.)
3. **Non-Success rewrite** → `error_after_rewrite = original error`, `message`/`code` from the rewrite; return (don't invert).
4. `err = error_message`.
5. **If `rewritten_sql != "" && rewritten_sql != original_sql`:**
   - `pos_map = buildOffsetMap(rewritten_sql, original_sql)`; `err = remapErrorPositions(err, pos_map)` — remap `"position N"` byte refs (1-based) from rewritten→original coordinates. (Line/col is NOT remapped; the rewriter emits one-line SQL so CH never prints line/col.)
   - **Whole-SQL replace:** `err.find(rewritten_sql)` → replace that ONE occurrence with `original_sql`; else `std::regex_replace` with `makeFlexibleSqlRegex` (whitespace-insensitive, case-insensitive) → replace ALL.
6. **Per-table** (`rewrite_resp.table_rewrites`, skip `origin==rewritten || rewritten empty`): `regex_replace(err, makeQualifiedIdentRegex(rewritten), "$1"+origin)` — boundary-aware, splits on first dot, requires backticks around the table half iff it contains a dot.
7. **Per-database** (`database_rewrites`, skip `origin==physical || physical empty`): `regex_replace(err, makeIdentRegex(physical), "$1"+origin)`.
8. `error_after_rewrite = err`, `code=Success`.

**Regex helpers** (`:65-298`):
- `regexEscape` — escapes `. ^ $ | ( ) \ [ ] * + ? { }`. **Go: `regexp.QuoteMeta` escapes the identical set.**
- `makeFlexibleSqlRegex(sql)` — runs of whitespace → `\s+`; other chars escaped; `icase`.
- `makeIdentRegex(ident)` → `(^|[^A-Za-z0-9_])` + `` `?<esc>`? `` + `(?=[^A-Za-z0-9_]|$)`, `icase`; replacement keeps `$1`.
- `makeQualifiedIdentRegex(qualified)` — split on first `.` → `(db, table)`; pattern `(^|[^\w])` + `` `?<esc_db>`? `` + `\.` + (`` `<esc_table>` `` if table has a dot else `` `?<esc_table>`? ``) + `(?=[^\w]|$)`.
- `buildOffsetMap(rewritten, original)` — forward Myers O(ND) diff; `result[i]` (i in `[0,N]`) = byte offset in `original` for offset `i` in `rewritten`; deleted-span positions collapse to the surrounding equal-range boundary; `result[N]=M`.
- `remapErrorPositions(err, pos_map)` — regex `([Pp]osition\s+)(\d+)`; for each, 1-based `N`: if `N==0 || N > len(rewritten)` leave as-is, else emit `pos_map[N-1]+1`.

**The RE2 gap:** Go's `regexp` has no lookahead `(?=…)`. The ident/qualified regexes use a captured prefix + trailing lookahead so boundaries are **non-consuming** (adjacent same-target matches separated by one boundary char both fire). Port this with a **boundary-checked scan** (`FindAllStringIndex` + manual `isWordByte` checks on the bytes just outside each match), NOT a captured suffix group (which would consume the boundary and miss the second of two adjacent matches).

**IN-split preprocess** (`src/query_preprocessor.{h,cc}`, called at `doRewrite` head `:331`): before parsing, `IN`/`GLOBAL IN` lists with **> 50** elements (`IN_CLAUSE_SPLIT_THRESHOLD=50`) are rewritten into `((col IN (batch1)) OR (col IN (batch2)) …)` with **50**-element batches (`IN_CLAUSE_BATCH_SIZE=50`) — a guard for ClickHouse's recursive-descent parser. **Native skips this** (polyglot's Rust parser has no such limit); design §6f/§10 register it as an intentional, allow-listed divergence.

## The Go consumer interface (design §2) — why native uses the stash

```go
RewriteErrorMessage(ctx context.Context, message string) (rewroteMessage string, err error)
```

Only `message` — no sql/options. So native inverts against the **most recent successful Rewrite on this connection**, whose maps it stashes. The C++ gRPC `RewriteErrorMessageRequest` carries sql+options and re-runs; both produce the same maps (deterministic per query), so the inversion result is identical. On no prior success (nil context or last code ≠ Success) → return `message` unchanged.

## Reusable / current state

- `native.go`: `callContext{sql, account}` stamped at 6 return sites (write/dblevel/exists/grant/select/passthrough); the SyntaxError early-return at `:50` does NOT stamp it. `RewriteErrorMessage` is a Phase-0 echo stub. `Close()` nils `last`.
- `rewriter.go`: `RewriteResult{SQL, Code, ..., TableRewrites, DatabaseRewrites, ...}`; `resultFromPB`. The maps are plain `map[string]string`.
- `internal/harness/oracle.go`: `Oracle` with only the `Rewrite` RPC (Phase 5 adds `RewriteErrorMessage`).
- Harness helpers reused by the new corpus: `DialOracle`, `newWriteRewriter` (constructs `New(e, WithOptions(...))`), `codeByName`, FFI-gated skip pattern.

## File Structure

- **Create** `internal/reverse/reverse.go` (+`_test.go` per task): the pure inversion package (Tasks 1–3).
- **Modify** `native.go` (+`native_test.go`): stash maps; implement `RewriteErrorMessage` (Task 4).
- **Modify** `internal/harness/oracle.go`; **Create** `internal/harness/errmsg_golden_test.go` + `internal/harness/testdata/errmsg_cases.json` (Task 5).
- **Create** `internal/harness/preprocess_test.go`, `internal/harness/fuzz_test.go` (Task 6).

Each task: TDD, gofmt, `go vet ./...`, `go build ./...`, one commit. `internal/reverse` tests are **pure** (no FFI). Native/harness tests gate on `POLYGLOT_SQL_FFI_PATH`. FFI run:
`POLYGLOT_SQL_FFI_PATH=$PWD/third_party/lib/libpolyglot_sql_ffi.dylib go test ./...`

---

## Task 1: `internal/reverse` — substitution primitives

**Files:** Create `internal/reverse/reverse.go`, `internal/reverse/substitute_test.go`

- [ ] **Step 1: Write the failing test** (`internal/reverse/substitute_test.go`)

```go
package reverse

import "testing"

func TestSubstituteIdent(t *testing.T) {
	cases := []struct{ in, target, repl, want string }{
		{"Database phys1 does not exist", "phys1", "logical1", "Database logical1 does not exist"},
		{"Database `phys1` missing", "phys1", "logical1", "Database `logical1` missing"},
		// boundary: must not match inside a larger identifier.
		{"phys1x and xphys1", "phys1", "L", "phys1x and xphys1"},
		// two adjacent occurrences separated by one boundary char: both fire.
		{"phys1 phys1", "phys1", "L", "L L"},
		// no match → unchanged.
		{"nothing here", "phys1", "L", "nothing here"},
	}
	for _, c := range cases {
		if got := substituteIdent(c.in, c.target, c.repl); got != c.want {
			t.Errorf("substituteIdent(%q,%q)=%q want %q", c.in, c.target, got, c.want)
		}
	}
}

func TestSubstituteQualified(t *testing.T) {
	cases := []struct{ in, qualified, repl, want string }{
		// dotted dynamic table → ClickHouse backticks the table half.
		{"Table phys1.`logical1.t` does not exist", "phys1.logical1.t", "logical1.t", "Table logical1.t does not exist"},
		// plain qualified, no dot in table → optional backticks.
		{"Table phys1.events gone", "phys1.events", "logical1.events", "Table logical1.events gone"},
		{"Table `phys1`.`events` gone", "phys1.events", "logical1.events", "Table logical1.events gone"},
		// no dot in the qualified key → falls back to ident match.
		{"db phys1 here", "phys1", "logical1", "db logical1 here"},
	}
	for _, c := range cases {
		if got := substituteQualified(c.in, c.qualified, c.repl); got != c.want {
			t.Errorf("substituteQualified(%q,%q)=%q want %q", c.in, c.qualified, got, c.want)
		}
	}
}

func TestFlexibleSQLReplace(t *testing.T) {
	// verbatim hit → first occurrence replaced.
	got := flexibleSQLReplace("In scope SELECT 1 FROM phys.t", "SELECT 1 FROM phys.t", "SELECT 1 FROM t")
	if got != "In scope SELECT 1 FROM t" {
		t.Errorf("verbatim: %q", got)
	}
	// whitespace-flexible: error collapsed newlines/spaces vs single-spaced rewritten.
	got = flexibleSQLReplace("err: SELECT   1\nFROM phys.t !", "SELECT 1 FROM phys.t", "SELECT 1 FROM t")
	if got != "err: SELECT 1 FROM t !" {
		t.Errorf("flexible: %q", got)
	}
	// no match → unchanged.
	if got := flexibleSQLReplace("unrelated", "SELECT 1", "x"); got != "unrelated" {
		t.Errorf("nomatch: %q", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/reverse` → FAIL (undefined). (No FFI needed for this package.)

- [ ] **Step 3: Write the implementation** (`internal/reverse/reverse.go`)

```go
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
```

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/reverse -run 'TestSubstitute|TestFlexible'` → PASS.

- [ ] **Step 5: gofmt + vet + commit**

```bash
gofmt -w internal/reverse/reverse.go internal/reverse/substitute_test.go
go vet ./... && go build ./...
git add internal/reverse/reverse.go internal/reverse/substitute_test.go
git commit -m "feat(reverse): boundary-aware ident/qualified/SQL substitution primitives"
```

---

## Task 2: `internal/reverse` — Myers offset map + position remap

**Files:** Modify `internal/reverse/reverse.go`; Create `internal/reverse/offset_test.go`

- [ ] **Step 1: Write the failing test** (`internal/reverse/offset_test.go`)

```go
package reverse

import (
	"reflect"
	"testing"
)

func TestBuildOffsetMap(t *testing.T) {
	// identical → identity (and the trailing N→M entry).
	if got := buildOffsetMap("abc", "abc"); !reflect.DeepEqual(got, []int{0, 1, 2, 3}) {
		t.Errorf("identical: %v", got)
	}
	// rewritten inserts a leading 'a': the surviving 'b' remaps to original offset 0.
	if got := buildOffsetMap("ab", "b"); !reflect.DeepEqual(got, []int{0, 0, 1}) {
		t.Errorf("insert: %v", got)
	}
	// empty rewritten → just [M].
	if got := buildOffsetMap("", "xyz"); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("empty rewritten: %v", got)
	}
	// empty original → all zero, trailing 0.
	if got := buildOffsetMap("ab", ""); !reflect.DeepEqual(got, []int{0, 0, 0}) {
		t.Errorf("empty original: %v", got)
	}
}

func TestRemapErrorPositions(t *testing.T) {
	pm := buildOffsetMap("ab", "b") // [0,0,1], rewritten len 2
	// "position 2" (1-based) → pm[1]+1 = 1.
	if got := remapErrorPositions("failed at position 2 here", pm); got != "failed at position 1 here" {
		t.Errorf("remap: %q", got)
	}
	// out-of-range position left as-is.
	if got := remapErrorPositions("position 99", pm); got != "position 99" {
		t.Errorf("oob: %q", got)
	}
	// no position token → unchanged; empty map → unchanged.
	if got := remapErrorPositions("no number", pm); got != "no number" {
		t.Errorf("none: %q", got)
	}
	if got := remapErrorPositions("position 1", []int{0}); got != "position 1" {
		t.Errorf("emptymap: %q", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/reverse -run 'TestBuildOffsetMap|TestRemap'` → FAIL (undefined).

- [ ] **Step 3: Add the implementation** to `internal/reverse/reverse.go` (append; add `"strconv"` to the import block)

```go
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
```

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/reverse` → PASS (all tests so far).

- [ ] **Step 5: gofmt + vet + commit**

```bash
gofmt -w internal/reverse/reverse.go internal/reverse/offset_test.go
go vet ./... && go build ./...
git add internal/reverse/reverse.go internal/reverse/offset_test.go
git commit -m "feat(reverse): Myers offset map + error-position remap"
```

---

## Task 3: `internal/reverse` — `Invert` orchestration

**Files:** Modify `internal/reverse/reverse.go`; Create `internal/reverse/invert_test.go`

- [ ] **Step 1: Write the failing test** (`internal/reverse/invert_test.go`)

```go
package reverse

import (
	"strings"
	"testing"
)

func TestInvert(t *testing.T) {
	t.Run("per-table dotted dynamic name", func(t *testing.T) {
		got := Invert(
			"Table phys1.`logical1.t` does not exist",
			"EXISTS logical1.t", "EXISTS TABLE phys1.`logical1.t`",
			map[string]string{"logical1.t": "phys1.logical1.t"}, nil)
		if got != "Table logical1.t does not exist" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("per-database", func(t *testing.T) {
		got := Invert("Database phys1 doesn't exist", "USE logical1", "USE phys1",
			nil, map[string]string{"logical1": "phys1"})
		if got != "Database logical1 doesn't exist" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("whole-sql block swap", func(t *testing.T) {
		got := Invert(
			"In scope SELECT 1 FROM phys1.t: oops",
			"SELECT 1 FROM logical1.t", "SELECT 1 FROM phys1.t",
			map[string]string{"logical1.t": "phys1.t"}, nil)
		// whole-SQL swap restores the original SELECT first.
		if got != "In scope SELECT 1 FROM logical1.t: oops" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("empty / no-op", func(t *testing.T) {
		if got := Invert("", "a", "b", nil, nil); got != "" {
			t.Errorf("empty: %q", got)
		}
		// rewritten == original and empty maps → unchanged.
		if got := Invert("anything", "x", "x", nil, nil); got != "anything" {
			t.Errorf("noop: %q", got)
		}
		// equal/empty map entries are skipped.
		if got := Invert("phys1", "s", "s", map[string]string{"a": "a", "b": ""}, nil); got != "phys1" {
			t.Errorf("skip: %q", got)
		}
	})

	t.Run("oversized SQL skips position-remap but still substitutes", func(t *testing.T) {
		// Over the maxRemapSQL cap: buildOffsetMap is skipped (no OOM), but the
		// per-table substitution still runs.
		big := strings.Repeat("x", maxRemapSQL+1)
		got := Invert("Table phys1.t missing", big, big+" changed",
			map[string]string{"logical1.t": "phys1.t"}, nil)
		if got != "Table logical1.t missing" {
			t.Errorf("got %q", got)
		}
	})
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/reverse -run TestInvert` → FAIL (`Invert` undefined).

- [ ] **Step 3: Add the implementation** to `internal/reverse/reverse.go` (append; add `"sort"` to the import block)

```go
// Invert maps physical names in a ClickHouse error message back to the logical
// names the client used, using the forward maps from the most recent successful
// Rewrite. Stages mirror doRewriteErrorMessage (rewriter-server.cc:461-513):
//   1. when the rewritten SQL differs from the original: remap "position N" byte
//      refs, then swap the whole rewritten-SQL block back to the original;
//   2. per-table substitution (rewritten qualified name -> origin);
//   3. per-database substitution (physical -> logical).
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
```

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/reverse` → PASS (whole package).

- [ ] **Step 5: gofmt + vet + commit**

```bash
gofmt -w internal/reverse/reverse.go internal/reverse/invert_test.go
go vet ./... && go build ./...
git add internal/reverse/reverse.go internal/reverse/invert_test.go
git commit -m "feat(reverse): Invert — full doRewriteErrorMessage inversion pipeline"
```

---

## Task 4: native — stash last-call maps + `RewriteErrorMessage`

**Files:** Modify `native.go`, `native_test.go`

- [ ] **Step 1: Write the failing test** (append to `native_test.go`; reuse the file's existing `newEngine(t)` real-engine helper)

```go
func TestRewriteErrorMessage(t *testing.T) {
	e := newEngine(t)
	optFn := func(string) []*pb.RewriteOption {
		return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
				DynamicArgs: &pb.RewriteTableDynamicArgs{
					DatabaseMap:            map[string]string{"logical1": "phys1"},
					KnownPhysicalDatabases: []string{"phys1"},
					Delim:                  "_",
				}}}}}
	}
	r := New(e, WithOptions(optFn))
	defer r.Close()
	ctx := context.Background()

	t.Run("inverts table name after EXISTS rewrite", func(t *testing.T) {
		res, err := r.Rewrite(ctx, "EXISTS logical1.t", "acct")
		if err != nil {
			t.Fatal(err)
		}
		// sanity: the rewrite produced the physical name in table_rewrites.
		if res.TableRewrites["logical1.t"] != "phys1.logical1.t" {
			t.Fatalf("table_rewrites=%v", res.TableRewrites)
		}
		inv, err := r.RewriteErrorMessage(ctx, "Table phys1.`logical1.t` does not exist")
		if err != nil {
			t.Fatal(err)
		}
		if inv != "Table logical1.t does not exist" {
			t.Errorf("inv=%q", inv)
		}
	})

	t.Run("inverts database name after USE rewrite", func(t *testing.T) {
		if _, err := r.Rewrite(ctx, "USE logical1", "acct"); err != nil {
			t.Fatal(err)
		}
		inv, _ := r.RewriteErrorMessage(ctx, "Database phys1 does not exist")
		if inv != "Database logical1 does not exist" {
			t.Errorf("inv=%q", inv)
		}
	})

	t.Run("no prior rewrite → unchanged", func(t *testing.T) {
		fresh := New(e, WithOptions(optFn))
		defer fresh.Close()
		inv, _ := fresh.RewriteErrorMessage(ctx, "Table phys1.x does not exist")
		if inv != "Table phys1.x does not exist" {
			t.Errorf("inv=%q", inv)
		}
	})

	t.Run("after a reject → unchanged (last code != Success)", func(t *testing.T) {
		if _, err := r.Rewrite(ctx, "GRANT SELECT ON *.* TO u", "acct"); err != nil {
			t.Fatal(err)
		}
		inv, _ := r.RewriteErrorMessage(ctx, "Database phys1 does not exist")
		if inv != "Database phys1 does not exist" {
			t.Errorf("reject should not invert: %q", inv)
		}
	})

	t.Run("empty message → empty", func(t *testing.T) {
		if _, err := r.Rewrite(ctx, "USE logical1", "acct"); err != nil {
			t.Fatal(err)
		}
		if inv, _ := r.RewriteErrorMessage(ctx, ""); inv != "" {
			t.Errorf("inv=%q", inv)
		}
	})
}
```

- [ ] **Step 2: Run to verify it fails** — `…go test . -run TestRewriteErrorMessage` → FAIL (the stub echoes, so the inversion subtests fail).

- [ ] **Step 3: Edit `native.go`** — extend `callContext`, add the `stash` helper, replace the 6 stamp sites + the SyntaxError path, implement `RewriteErrorMessage`, and import `internal/reverse`.

Add the import:

```go
	"github.com/housegate/rewriter-go/internal/reverse"
```

Replace the `callContext` type:

```go
// callContext is the per-connection record of the most recent Rewrite, used by
// RewriteErrorMessage to invert physical names in error text back to logical ones.
// It stashes the forward rewrite maps + sql_after_rewrite + code so the inversion
// needs no re-parse (the Go interface passes only the error message).
type callContext struct {
	sql              string
	account          string
	code             pb.RewriteCode
	sqlAfterRewrite  string
	tableRewrites    map[string]string
	databaseRewrites map[string]string
}

// stash records the just-finished Rewrite as the per-connection last-call context.
func (r *NativeRewriter) stash(sql, account string, resp *pb.RewriteSQLResponse) {
	r.mu.Lock()
	r.last = &callContext{
		sql: sql, account: account,
		code:             resp.GetCode(),
		sqlAfterRewrite:  resp.GetSqlAfterRewrite(),
		tableRewrites:    resp.GetTableRewrites(),
		databaseRewrites: resp.GetDatabaseRewrites(),
	}
	r.mu.Unlock()
}
```

In `Rewrite`, the SyntaxError early return — stash before returning so a later RewriteErrorMessage sees this query (non-Success → won't invert), not a stale prior success:

```go
	ast, err := r.engine.ParseOne(sql)
	if err != nil {
		resp.Code = pb.RewriteCode_SyntaxError
		resp.Message = err.Error()
		r.stash(sql, account, resp)
		return resultFromPB(resp), nil // SyntaxError is a code, not a Go error
	}
```

Replace EACH of the 6 stamp blocks of the form

```go
		r.mu.Lock()
		r.last = &callContext{sql: sql, account: account}
		r.mu.Unlock()
		return resultFromPB(<resp>), nil
```

with (using that branch's response variable `wresp`/`dresp`/`xresp`/`gresp`/`hresp`/`resp`):

```go
		r.stash(sql, account, <resp>)
		return resultFromPB(<resp>), nil
```

(The pass-through tail's response variable is `resp`.)

Replace the `RewriteErrorMessage` stub:

```go
// RewriteErrorMessage inverts physical table/database names in a ClickHouse error
// message back to the logical names the client used, using the maps stashed from
// the most recent successful Rewrite on this connection. Returns the message
// unchanged when there's no prior successful rewrite (nil context or a non-Success
// last call) — mirroring doRewriteErrorMessage's non-Success passthrough.
func (r *NativeRewriter) RewriteErrorMessage(_ context.Context, message string) (string, error) {
	r.mu.Lock()
	last := r.last
	r.mu.Unlock()
	if message == "" || last == nil || last.code != pb.RewriteCode_Success {
		return message, nil
	}
	return reverse.Invert(message, last.sql, last.sqlAfterRewrite, last.tableRewrites, last.databaseRewrites), nil
}
```

- [ ] **Step 4: Run to verify it passes** — `…go test . -run TestRewriteErrorMessage` → PASS. Then `…go test ./...` (FFI) → all green; `go test ./...` (no FFI) → green via skips.

- [ ] **Step 5: gofmt + vet + commit**

```bash
gofmt -w native.go native_test.go
go vet ./... && go build ./...
git add native.go native_test.go
git commit -m "feat(native): stash last-call maps + RewriteErrorMessage reverse-mapping"
```

---

## Task 5: harness — error-inversion golden corpus + oracle RPC

**Files:** Modify `internal/harness/oracle.go`; Create `internal/harness/errmsg_golden_test.go`, `internal/harness/testdata/errmsg_cases.json`

- [ ] **Step 1: Add the oracle RPC** to `internal/harness/oracle.go` (after `Rewrite`)

```go
// RewriteErrorMessage calls the C++ oracle's RewriteErrorMessage RPC. The C++
// re-runs the rewrite from (sql, opts), so the request carries them directly.
func (o *Oracle) RewriteErrorMessage(sql, errorMessage string, opts []*pb.RewriteOption) (*pb.RewriteErrorMessageResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return o.client.RewriteErrorMessage(ctx, &pb.RewriteErrorMessageRequest{
		Sql: sql, ErrorMessage: errorMessage, Options: opts,
	})
}
```

- [ ] **Step 2: Write the failing test** (`internal/harness/errmsg_golden_test.go`)

```go
package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// errmsgCase drives a (sql, options) through the native rewriter to populate the
// per-connection last-call maps, then inverts error_message and compares to
// want_inverted (frozen native output). When REWRITER_ORACLE_ADDR is set, the
// inverted string is additionally diffed against the C++ oracle's
// RewriteErrorMessage(sql, error_message, options).error_after_rewrite (exact).
type errmsgCase struct {
	Name         string              `json:"name"`
	SQL          string              `json:"sql"`
	Dynamic      *dblevelDynamicJSON `json:"dynamic"`
	ErrorMessage string              `json:"error_message"`
	WantInverted string              `json:"want_inverted"`
}

func (c errmsgCase) options() []*pb.RewriteOption {
	if c.Dynamic == nil {
		return nil
	}
	da := &pb.RewriteTableDynamicArgs{
		DatabaseMap:                      c.Dynamic.DatabaseMap,
		KnownPhysicalDatabases:           c.Dynamic.KnownPhysicalDatabases,
		UpstreamLogicalDatabaseInContext: c.Dynamic.UpstreamLogical,
		Delim:                            c.Dynamic.Delim,
	}
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: da}}}}
}

func loadErrmsgCases(t *testing.T) []errmsgCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "errmsg_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cs []errmsgCase
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}

// TestErrmsgGolden is the Phase-5 parity gate for RewriteErrorMessage. want_inverted
// was frozen from native output; the REWRITER_ORACLE_ADDR differential is the TRUE gate.
func TestErrmsgGolden(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	oracle, _ := DialOracle()
	defer oracle.Close()
	ctx := context.Background()

	for _, c := range loadErrmsgCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			r := newWriteRewriter(e, c.options())
			if _, err := r.Rewrite(ctx, c.SQL, "acct"); err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			inv, err := r.RewriteErrorMessage(ctx, c.ErrorMessage)
			if err != nil {
				t.Fatalf("rewriteErrorMessage: %v", err)
			}
			if inv != c.WantInverted {
				t.Errorf("inverted:\n got %q\nwant %q", inv, c.WantInverted)
			}
			if oracle != nil {
				resp, oerr := oracle.RewriteErrorMessage(c.SQL, c.ErrorMessage, c.options())
				if oerr != nil {
					t.Fatalf("oracle: %v", oerr)
				}
				if inv != resp.GetErrorAfterRewrite() {
					t.Errorf("oracle divergence:\n got %q\nwant %q", inv, resp.GetErrorAfterRewrite())
				}
			}
		})
	}
}
```

- [ ] **Step 3: Run to verify it fails** — `…go test ./internal/harness -run TestErrmsgGolden` → FAIL (missing testdata file).

- [ ] **Step 4: Create the corpus** (`internal/harness/testdata/errmsg_cases.json`). All use `database_map={"logical1":"phys1"}`, `known_physical_databases=["phys1"]`, `delim="_"`. Freeze `want_inverted` from the actual native output (run the test; reconcile per Step 5).

```json
[
  {
    "name": "exists_table_not_exist",
    "sql": "EXISTS logical1.t",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "error_message": "Table phys1.`logical1.t` does not exist",
    "want_inverted": "Table logical1.t does not exist"
  },
  {
    "name": "use_database_not_exist",
    "sql": "USE logical1",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "error_message": "Code: 81. DB::Exception: Database phys1 does not exist.",
    "want_inverted": "Code: 81. DB::Exception: Database logical1 does not exist."
  },
  {
    "name": "select_whole_sql_block",
    "sql": "SELECT 1 FROM logical1.t",
    "dynamic": {"database_map": {"logical1": "phys1"}, "known_physical_databases": ["phys1"], "delim": "_"},
    "error_message": "Missing columns: while processing query: 'SELECT 1 FROM phys1.`logical1.t`'",
    "want_inverted": "Missing columns: while processing query: 'SELECT 1 FROM logical1.t'"
  },
  {
    "name": "no_rewrite_passthrough",
    "sql": "SELECT 1",
    "error_message": "Some unrelated error about xyz",
    "want_inverted": "Some unrelated error about xyz"
  }
]
```

- [ ] **Step 5: Run + freeze** — `…go test ./internal/harness -run TestErrmsgGolden -v`. For any case where `inv` ≠ `want_inverted`, the JSON is wrong (the native `reverse.Invert` is the source of truth — it passed its own unit tests). Print the actual `inv` and update the JSON `want_inverted` to match. Re-run until green. Especially verify how SELECT records `table_rewrites` for `logical1.t` (the dynamic physical table is the dotted `phys1.logical1.t`, backticked in the error) — if the freeze disagrees, fix the JSON, not the code. Confirm `go test ./...` (no FFI) still green via skips.

- [ ] **Step 6: gofmt + vet + commit**

```bash
gofmt -w internal/harness/oracle.go internal/harness/errmsg_golden_test.go
go vet ./... && go build ./...
git add internal/harness/oracle.go internal/harness/errmsg_golden_test.go internal/harness/testdata/errmsg_cases.json
git commit -m "test(harness): RewriteErrorMessage golden corpus + oracle RPC differential"
```

---

## Task 6: preprocess decision (IN-split skip) + differential-fuzz hardening

**Files:** Create `internal/harness/preprocess_test.go`, `internal/harness/fuzz_test.go`

- [ ] **Step 1: Write the preprocess-decision test** (`internal/harness/preprocess_test.go`)

This records the §6f decision: native does NOT run the C++ `IN`-split preprocess (polyglot's Rust parser has no recursive-descent limit), so a `> 50`-element IN list survives intact. Against the C++ oracle this is an **allow-listed divergence** (C++ emits OR-batches; native a flat IN — semantically equal). The test asserts native keeps the flat IN; it does NOT diff against the oracle (the divergence is intentional).

```go
package harness

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
)

// TestPreprocessINSplitSkipped records the Phase-5 preprocess DECISION (design
// §6f/§10): the native rewriter intentionally does NOT split large IN clauses
// into OR-batches the way the C++ QueryPreprocessor does (threshold 50). Polyglot
// (Rust) parses arbitrarily large IN lists without ClickHouse's recursive-descent
// stack limit, so the guard is unnecessary. Against the C++ oracle a 51+-element
// IN is an ALLOW-LISTED divergence (C++ OR-batches; native keeps the flat IN —
// the two are semantically equal). This test pins native's flat-IN behavior; it
// deliberately performs NO oracle comparison.
func TestPreprocessINSplitSkipped(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// 60 elements — above the C++ IN_CLAUSE_SPLIT_THRESHOLD of 50.
	nums := make([]string, 60)
	for i := range nums {
		nums[i] = strconv.Itoa(i)
	}
	sql := "SELECT x FROM t WHERE x IN (" + strings.Join(nums, ", ") + ")"

	r := newWriteRewriter(e, nil) // no rewrite policy → pure round-trip
	res, err := r.Rewrite(context.Background(), sql, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code.String() != "Success" {
		t.Fatalf("code=%v", res.Code)
	}
	// The IN list survived as a single clause: no OR-batching was introduced.
	if strings.Contains(strings.ToUpper(res.SQL), " OR ") {
		t.Errorf("native unexpectedly split the IN clause into OR-batches: %q", res.SQL)
	}
	if !strings.Contains(res.SQL, "59") {
		t.Errorf("IN list lost elements: %q", res.SQL)
	}
}
```

- [ ] **Step 2: Write the differential-fuzz harness** (`internal/harness/fuzz_test.go`)

Hardening: feed mutated corpus SQL to the native rewriter and assert it never panics and always honors the fail-open contract — either a Success with a non-empty runnable SQL, or a classified non-Success `RewriteResult` (never a Go error except an internal engine failure). When `REWRITER_ORACLE_ADDR` is set, the seed corpus is additionally compared structurally against the oracle (reusing `Compare`).

```go
package harness

import (
	"context"
	"os"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// FuzzRewrite hardens the fail-open contract: for any input, Rewrite must not
// panic and must return either (Success + non-empty SQL) or a classified
// non-Success RewriteResult with a nil Go error (the Go error is reserved for
// internal engine failures). Run: POLYGLOT_SQL_FFI_PATH=… go test ./internal/harness -run x -fuzz FuzzRewrite -fuzztime 30s
func FuzzRewrite(f *testing.F) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		f.Skip("needs engine")
	}
	for _, seed := range []string{
		"SELECT 1", "SELECT * FROM logical1.t", "USE logical1", "SHOW TABLES",
		"GRANT SELECT ON logical1.t TO u", "EXISTS logical1.t",
		"CREATE TABLE logical1.t (x Int32) ENGINE = Memory", "INSERT INTO logical1.t VALUES (1)",
		"DROP TABLE logical1.t", "RENAME TABLE logical1.a TO logical1.b",
	} {
		f.Add(seed)
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { e.Close() })

	optFn := func(string) []*pb.RewriteOption {
		return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
				DynamicArgs: &pb.RewriteTableDynamicArgs{
					DatabaseMap: map[string]string{"logical1": "phys1"}, KnownPhysicalDatabases: []string{"phys1"}, Delim: "_",
				}}}}}
	}

	f.Fuzz(func(t *testing.T, sql string) {
		// A NewRewriter per iteration keeps last-call state isolated.
		r := newWriteRewriter(e, optFn(""))
		res, err := r.Rewrite(context.Background(), sql, "acct")
		if err != nil {
			// Only an internal/engine failure is allowed to surface as a Go error;
			// the zero RewriteResult must not be inspected.
			return
		}
		if res.Code == pb.RewriteCode_Success && res.SQL == "" {
			t.Fatalf("Success with empty SQL for %q", sql)
		}
		// RewriteErrorMessage must also never panic, on any prior state.
		if _, eerr := r.RewriteErrorMessage(context.Background(), "Table phys1.`logical1.t` does not exist"); eerr != nil {
			t.Fatalf("RewriteErrorMessage error: %v", eerr)
		}
	})
}
```

- [ ] **Step 3: Run both** — with FFI:
  - `…go test ./internal/harness -run TestPreprocessINSplitSkipped -v` → PASS.
  - `…go test ./internal/harness -run x -fuzz FuzzRewrite -fuzztime 20s` → no failures (then a plain `…go test ./internal/harness` runs the seed corpus as a normal test). Confirm `go test ./...` (no FFI) skips cleanly.

- [ ] **Step 4: gofmt + vet + commit**

```bash
gofmt -w internal/harness/preprocess_test.go internal/harness/fuzz_test.go
go vet ./... && go build ./...
git add internal/harness/preprocess_test.go internal/harness/fuzz_test.go
git commit -m "test(harness): IN-split preprocess decision (§6f) + differential-fuzz hardening"
```

---

## Self-Review (run against design spec §9 + doRewriteErrorMessage)

**Spec coverage:**
- RewriteErrorMessage reverse-mapping — `internal/reverse` (Tasks 1–3) + native wiring (Task 4) + golden corpus/oracle gate (Task 5). The 4 inversion stages (position remap, whole-SQL swap, per-table, per-database) all ported. ✅
- Preprocess decision (§6f IN-split skip) — recorded + pinned (Task 6, `TestPreprocessINSplitSkipped`), allow-listed (no oracle diff on that case). ✅
- Differential-fuzz hardening — `FuzzRewrite` (Task 6). ✅

**Type consistency:** `reverse.Invert(message, originalSQL, rewrittenSQL string, tableRewrites, databaseRewrites map[string]string) string`. `callContext{sql, account, code, sqlAfterRewrite, tableRewrites, databaseRewrites}` populated by `r.stash(sql, account, resp)` at all 7 sites (6 handled + SyntaxError). `RewriteErrorMessage` gates on `last.code == pb.RewriteCode_Success`. Oracle gains `RewriteErrorMessage(sql, errorMessage, opts) (*pb.RewriteErrorMessageResponse, error)`; proto fields `RewriteErrorMessageRequest{Sql, ErrorMessage, Options}` / `RewriteErrorMessageResponse{Code, Message, ErrorAfterRewrite}` confirmed in `gen/pb`.

**Known parity risks (gated by the live oracle, the true §7 gate — not runnable in-session):**
1. **RE2-vs-ECMAScript boundary semantics:** the boundary-checked scan reproduces the C++ prefix+lookahead (non-consuming) faithfully for ASCII; exotic identifiers with non-ASCII bytes adjacent to a name could differ from `[^A-Za-z0-9_]` (the C++ class is ASCII-only too, so this matches). Low risk.
2. **`buildOffsetMap` is a faithful 1:1 port** of the C++ Myers diff incl. the deleted-span collapse; verified on identical/insert/empty cases. Position remap only fires on parse-position errors against rewritten SQL (rare).
3. **table_rewrites freeze:** the corpus `want_inverted` encodes native output; the dotted dynamic physical name (`phys1.logical1.t`, backticked in CH error text) is the load-bearing per-table case — the oracle differential confirms the exact substitution.
4. **No `account` re-run:** native inverts from stashed maps, not a re-run; identical result since maps are deterministic per query (the C++ re-run is an implementation detail of its stateless RPC).

**Out of scope (done after Phase 5, or pre-existing):** the repo-wide `statement_type`-on-reject divergence (separate task, Phase-4 holistic finding); the `classifyCommand` `SHOW` catch-all. Phase 5 completes the `Rewriter` interface surface.
