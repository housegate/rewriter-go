package engine

import (
	"encoding/json"
	"fmt"
)

// IdentifierParameterMessage is the cross-engine rejection text (spec T2).
const IdentifierParameterMessage = "query parameters are not supported in a database or table position"

// nameOf reads a function/call node's "name" field.
func nameOf(function map[string]any) string {
	name, _ := function["name"].(string)
	return name
}

// inOperandHoldsParameter reports whether an IN operand identifier (column or
// dot node) has an Identifier parameter in its table or database part.
func inOperandHoldsParameter(arg map[string]any) bool {
	if col, ok := arg["column"].(map[string]any); ok {
		return unresolvedIdentifierNode(col["name"]) || unresolvedIdentifierNode(col["table"])
	}
	if dot, ok := arg["dot"].(map[string]any); ok {
		if unresolvedIdentifierNode(dot["field"]) {
			return true
		}
		if inner, ok := dot["this"].(map[string]any); ok {
			return inOperandHoldsParameter(inner)
		}
	}
	return unresolvedIdentifierNode(arg)
}

// TablePositionParameter reports whether any database or table position of
// the statement holds an Identifier query parameter: FROM / JOIN / subquery /
// CTE / UNION table nodes, IN and callable-IN operands, every write slot, the
// MV TO target, CREATE/DROP DATABASE's own target, and — for opaque command
// nodes — the position-scoped scans in commandTextParameterHit. sql is the
// original, unmodified source text that produced ast (never a
// CommandSQL(ast)/reprinted form — see commandTextParameterHit's doc comment
// for why that distinction matters), used only for the command-node case.
func TablePositionParameter(e Engine, ast AST, sql string) (bool, error) {
	kind, err := NodeKind(ast)
	if err != nil {
		return false, err
	}
	switch kind {
	case NodeCommand:
		return commandTextParameterHit(e, ast, sql)
	case NodeCreateDB, NodeDropDB:
		db, _, _, err := DatabaseTarget(ast)
		if err != nil {
			return false, err
		}
		return db == "" || looksLikeUnresolvedIdentifierName(db), nil
	}
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return false, fmt.Errorf("engine: decode statement: %w", err)
	}
	found := false
	if err := walkStatementObjects(root, readSourceScope{}, readSourceVisitor{
		parameter: func(map[string]any) { found = true },
	}); err != nil {
		return false, err
	}
	if found {
		return true, nil
	}
	// An opaque Raw ALTER action (DELETE WHERE …, MODIFY TTL …, MODIFY QUERY
	// …) is text the walker cannot see: any Identifier parameter in it is
	// refused (spec 2026-09-26 R2).
	texts, err := OpaqueAlterTexts(ast)
	if err != nil {
		return false, err
	}
	for _, text := range texts {
		if IdentifierParameterInText(e, text) {
			return true, nil
		}
	}
	info, err := InspectWrite(ast)
	if err != nil {
		return false, err
	}
	return info.ParameterTarget, nil
}

// commandTextParameterHit applies T2 to an opaque `command` node (spec
// 2026-09-26 R8): the whole text is scanned, whatever the command class, so an
// Identifier parameter anywhere in it — a target, an ALTER … UPDATE tail, an
// EXPLAIN body — is refused with the T2 message before the unmodelled-class
// and protected-database checks run. sql must be the caller's original source
// text, never CommandSQL(ast): measured on rewriter-go v0.13.0, a command's
// reprint pads a space inside an embedded Identifier-typed brace parameter
// ("db1.{p:Identifier}" round-trips as "db1.{ p:Identifier }"); the
// token-based scan tolerates that, but the original text is the one
// ClickHouse executes.
func commandTextParameterHit(e Engine, _ AST, sql string) (bool, error) {
	return IdentifierParameterInText(e, sql), nil
}

// commandScanSentinel is appended (on its own line) to the text IdentifierParameterInText
// tokenizes, purely to prove the tokenizer consumed the whole input — see its
// doc comment.
const commandScanSentinel = "_rewriter_go_command_scan_sentinel"

// IdentifierParameterInText tokenizes sql via the engine's own lexer and scans
// for an Identifier query parameter (`{name:Identifier}`) at every L_BRACE
// using identifierParameterEnd (lexical.go) — the same token sequence the real
// parser recognizes, so comments/strings/heredocs/whitespace are handled
// exactly as ClickHouse's own grammar handles them, not by a hand-rolled scan
// (spec 2026-09-26 T2; a prior hand-rolled version was bypassable — see git
// history — because it did not recognize `#`/`#!`/`//` line comments at all
// and so treated the "'" inside "EXISTS TABLE // it's\n db1.{p:Identifier}"
// as an opening quote, hiding the real parameter behind an unterminated
// "string").
//
// A tokenizer error is a hit (fail closed): the caller cannot trust
// "no parameter found" for text it could not even lex. Measured on
// rewriter-go v0.13.0, an unterminated string or block comment does NOT make
// e.Tokenize error — a single-quote string with no closing quote silently
// absorbs everything after it to EOF, and an unterminated /* comment silently
// stops the token stream partway through with nothing recognized afterward —
// so both are also a hit, detected by requiring commandScanSentinel, appended
// on its own new line, to survive as the tokenizer's own final token: an
// absorbed or truncated scan cannot produce that token. A line comment
// (--, #, #!, //) still terminates at that newline, so an ordinary
// well-formed trailing comment does not trip this. A malformed heredoc opener
// with no matching closer (`$tag$...` never closed) needs no special case
// here: measured, it lexes as one ordinary VAR token that stops at the next
// non-identifier character exactly like any other bareword, so a `{` right
// after it still tokenizes as its own L_BRACE and is still found.
func IdentifierParameterInText(e Engine, sql string) bool {
	toks, err := tokenizeRaw(e, sql+"\n"+commandScanSentinel)
	if err != nil {
		return true
	}
	if len(toks) == 0 {
		return true
	}
	last := toks[len(toks)-1]
	if last.TokenType != "VAR" || last.Text != commandScanSentinel {
		return true
	}
	return tokensHoldIdentifierParameter(toks[:len(toks)-1])
}

// tokensHoldIdentifierParameter scans a token slice for an Identifier query
// parameter at every L_BRACE using identifierParameterEnd (lexical.go), the
// same 5-token shape (L_BRACE, identifier, COLON, "Identifier", R_BRACE) the
// real parser requires — including tolerating the whitespace ClickHouse's own
// grammar tolerates there, since identifierParameterEnd works on tokens, not
// source bytes (this is also why no separate handling is needed for the
// engine's own reprint padding of that same shape, e.g. "{ p:Identifier }").
func tokensHoldIdentifierParameter(toks []rawToken) bool {
	for i, tok := range toks {
		if tok.TokenType == "L_BRACE" {
			if _, ok := identifierParameterEnd(toks, i); ok {
				return true
			}
		}
	}
	return false
}
