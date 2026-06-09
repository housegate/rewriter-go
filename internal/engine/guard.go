package engine

// maxParseDepth bounds bracket nesting handed to the polyglot parser. The Rust
// recursive-descent parser overflows its stack (SIGSEGV during the FFI call,
// which Go cannot recover) on deeply-nested input — empirically around depth ~180
// on darwin/arm64. We cap well below that: legitimate SQL essentially never nests
// expression brackets past ~40, and an over-cap statement fails open (the parser
// guard returns an error → SyntaxError → the caller forwards the original SQL to
// ClickHouse, whose own parser handles up to max_parser_depth=1000). Lower this if
// a platform with a smaller stack is found to crash below it.
const maxParseDepth = 100

// exceedsNestingDepth reports whether sql nests () or [] brackets deeper than max,
// counting ONLY brackets OUTSIDE string/identifier literals (so parens inside a
// quoted string don't trip the guard and cause a spurious fail-open). O(len(sql)),
// no FFI — safe to run before every parse. Quote runs ('...', "...", `...`) are
// skipped, honoring ClickHouse's doubled-quote (”) and backslash (\') escapes; an
// unterminated quote consumes to EOF (such SQL fails to parse anyway). The scan is
// conservative-by-construction: any miscount of literal content can only ADD to the
// depth (a string treated as code), never hide real structural nesting.
func exceedsNestingDepth(sql string, max int) bool {
	depth := 0
	for i := 0; i < len(sql); {
		c := sql[i]
		switch c {
		case '\'', '"', '`':
			q := c
			i++
			for i < len(sql) {
				if sql[i] == '\\' && i+1 < len(sql) {
					i += 2
					continue
				}
				if sql[i] == q {
					if i+1 < len(sql) && sql[i+1] == q { // doubled-quote escape
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case '(', '[':
			depth++
			if depth > max {
				return true
			}
			i++
		case ')', ']':
			if depth > 0 {
				depth--
			}
			i++
		default:
			i++
		}
	}
	return false
}
