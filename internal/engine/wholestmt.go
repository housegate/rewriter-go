package engine

import (
	"bytes"
	"errors"
	"fmt"
)

// ErrNotParsedInFull marks a statement Polyglot accepted without consuming
// all of it. The pinned Polyglot's ClickHouse dialect skips every token after
// the point where its grammar stops (parser.rs `parse`, "ClickHouse fallback:
// consume unconsumed tokens until semicolon/EOF"), discards a statement-level
// SETTINGS / FORMAT tail it does not model, and closes any bracket the input
// left open at end of input. The AST, and every SQL generated from it, then
// means something other than the input.
var ErrNotParsedInFull = errors.New("engine: parse: statement was not parsed in full")

// CheckParsedInFull returns nil when ast, which e.ParseOne produced for sql,
// accounts for the whole statement, and an error wrapping ErrNotParsedInFull
// otherwise. A tokenizer error is returned as is.
//
// Polyglot exposes no consumed position, so the check measures it:
//
//  1. Brackets. Every '(' '[' '{' token is closed by its own kind, in order,
//     before the end of the statement.
//  2. The final token. Cut sql just before its last token that is neither a
//     bracket nor a semicolon. If the cut text parses to a byte-identical AST,
//     that token did not contribute to the AST: the parser stopped at or
//     before it, or discarded it. The cut keeps every earlier token at its
//     offset, so the spans inside the two ASTs compare byte for byte.
//     Brackets are skipped because Polyglot closes an open bracket at end of
//     input, so removing a trailing ')' alone never changes the AST.
//     Zero-width tokens are skipped too: the tokenizer emits one at end of
//     input for the text after INSERT … FORMAT <name>, and cutting before it
//     would cut nothing.
//
// An INSERT … FORMAT <name> statement is checked only up to the format name:
// the rest is the data payload, which Polyglot does not model and
// GenerateInsert splices back verbatim.
func CheckParsedInFull(e Engine, sql string, ast AST) error {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return err
	}
	if insertHasFormatClause(ast) {
		for i := len(toks) - 1; i >= 0; i-- {
			if toks[i].TokenType == "FORMAT" && i+1 < len(toks) {
				toks = toks[:i+2]
				break
			}
		}
	}
	for len(toks) > 0 && toks[len(toks)-1].TokenType == "SEMICOLON" {
		toks = toks[:len(toks)-1]
	}
	if err := checkBrackets(sql, toks); err != nil {
		return err
	}
	last := -1
	for i := len(toks) - 1; i >= 0; i-- {
		if !isBracketToken(toks[i].TokenType) && toks[i].Span.End > toks[i].Span.Start {
			last = i
			break
		}
	}
	if last <= 0 {
		return nil // no earlier token to cut back to
	}
	cut, perr := e.ParseOne(sql[:toks[last].Span.Start])
	if perr != nil || !bytes.Equal(cut, ast) {
		return nil
	}
	stop := firstIgnoredToken(e, sql, ast, toks[:last+1])
	return fmt.Errorf("%w: the parser stopped before %q", ErrNotParsedInFull, excerpt(sql[toks[stop].Span.Start:]))
}

// firstIgnoredToken finds, by binary search over the cut points of toks, the
// first token whose removal (with everything after it) leaves ast unchanged,
// then skips closing brackets, which Polyglot supplies itself at end of input.
// toks[len(toks)-1] is known to be ignored. The result names the stop point
// in the message only; it never decides whether the statement is refused.
func firstIgnoredToken(e Engine, sql string, ast AST, toks []rawToken) int {
	lo, hi := 1, len(toks)-1
	for lo < hi {
		mid := (lo + hi) / 2
		cut, err := e.ParseOne(sql[:toks[mid].Span.Start])
		if err == nil && bytes.Equal(cut, ast) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	for hi < len(toks)-1 && isClosingBracket(toks[hi].TokenType) {
		hi++
	}
	return hi
}

func checkBrackets(sql string, toks []rawToken) error {
	var open []rawToken
	for _, tk := range toks {
		switch tk.TokenType {
		case "L_PAREN", "L_BRACKET", "L_BRACE":
			open = append(open, tk)
		case "R_PAREN", "R_BRACKET", "R_BRACE":
			if len(open) == 0 || closerOf(open[len(open)-1].TokenType) != tk.TokenType {
				return fmt.Errorf("%w: unmatched %q before %q", ErrNotParsedInFull, tk.Text, excerpt(sql[tk.Span.Start:]))
			}
			open = open[:len(open)-1]
		}
	}
	if len(open) > 0 {
		tk := open[len(open)-1]
		return fmt.Errorf("%w: %q is never closed in %q", ErrNotParsedInFull, tk.Text, excerpt(sql[tk.Span.Start:]))
	}
	return nil
}

func closerOf(open string) string {
	switch open {
	case "L_PAREN":
		return "R_PAREN"
	case "L_BRACKET":
		return "R_BRACKET"
	}
	return "R_BRACE"
}

func isClosingBracket(t string) bool { return t == "R_PAREN" || t == "R_BRACKET" || t == "R_BRACE" }

func isBracketToken(t string) bool {
	return isClosingBracket(t) || t == "L_PAREN" || t == "L_BRACKET" || t == "L_BRACE"
}

// excerpt is the first 40 characters of s, for messages.
func excerpt(s string) string {
	const max = 40
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
