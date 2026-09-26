package engine

import (
	"encoding/json"
	"fmt"
	"strings"
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
	info, err := InspectWrite(ast)
	if err != nil {
		return false, err
	}
	return info.ParameterTarget, nil
}

// commandTextParameterHit applies the position-scoped T2 policy to an opaque
// `command` node, dispatched by command class so a column/value-position
// parameter in a class that also carries one (ALTER ... UPDATE's assignments)
// is never refused:
//
//   - EXISTS / SHOW CREATE / DESCRIBE (ParseObjectTarget's Verb != VerbNone):
//     these carry nothing but the target — no WHERE, no assignment list — so
//     the whole text is scanned. Deliberately NOT ParseObjectTarget's own
//     DB/Table fields: those mis-extract a qualified parameter target instead
//     of reporting it (measured: "SHOW CREATE TABLE db1.{p:Identifier}"
//     resolves Table="db1" and silently drops the ".{p:Identifier}" suffix,
//     because its name-run grammar requires BOTH sides of a dot to be a name
//     token and falls back to treating the qualifier alone as a bare name
//     when the second side isn't one). ParseObjectTarget here answers only
//     "is this one of these three verbs", not "where is the parameter".
//   - USE / SHOW ... FROM|IN (ParseDBLevel's Kind != DBNone): reuses its
//     existing HasDBClause/DBResolved/DB extraction — an explicit clause that
//     did not resolve to a plain name is the parameter shape. Checked AFTER
//     ParseObjectTarget: ParseDBLevel classifies ANY leading "SHOW ..." as
//     DBShow regardless of what follows, so "SHOW CREATE ..." must be routed
//     to the whole-text scan above first or it is wrongly treated as a
//     database-target statement whose (never populated for CREATE) DB clause
//     looks clean.
//   - RENAME TABLE / EXCHANGE TABLES (InspectWrite's Sub == CmdRename /
//     CmdExchange): whole text scanned, same reasoning as EXISTS/DESCRIBE —
//     both carry only names after the TABLE/TABLES keyword.
//   - ALTER ... UPDATE (Sub == CmdAlterUpdate): only the target — the span
//     between ALTER TABLE and the UPDATE keyword — is scanned, via the
//     existing production alterMutationTail/consumeMutationQualifiedName
//     grammar (mutation_reads.go) reused as-is; anything after UPDATE
//     (assignments, WHERE) is a column/value position and must stay allowed.
//   - Every other command class (SET, SYSTEM, KILL, CHECK, EXPLAIN, CREATE
//     USER, GRANT, REVOKE, the CmdBareReject family, CmdNone, ...) is NOT
//     text-scanned here at all: this policy only refuses table/database
//     positions this switch can name, and Task 7 separately refuses those
//     classes as unmodelled statements.
//
// sql must be the caller's original source text (the same argument doRewrite
// passes to every other command-node handler), never CommandSQL(ast): measured
// on rewriter-go v0.13.0, RENAME's command.this reprint pads a space inside an
// embedded Identifier-typed brace parameter ("db1.{p:Identifier}" round-trips
// as "db1.{ p:Identifier }"), which the original source text never has.
func commandTextParameterHit(e Engine, ast AST, sql string) (bool, error) {
	objTarget, err := ParseObjectTarget(e, sql)
	if err != nil {
		return true, nil // tokenizer error: fail closed, not a propagated Go error
	}
	if objTarget.Verb != VerbNone {
		return IdentifierParameterInText(e, sql), nil
	}
	dbInfo, err := ParseDBLevel(e, sql)
	if err != nil {
		return true, nil
	}
	if dbInfo.Kind != DBNone {
		return dbLevelHoldsParameter(dbInfo), nil
	}
	info, err := InspectWrite(ast)
	if err != nil {
		return false, err
	}
	switch info.Sub {
	case CmdRename, CmdExchange:
		return IdentifierParameterInText(e, sql), nil
	case CmdAlterUpdate:
		return alterUpdateTargetHoldsParameter(e, sql), nil
	default:
		return false, nil
	}
}

// dbLevelHoldsParameter reads a USE/SHOW ... FROM|IN (and, for the COLUMNS/
// INDEX family, SHOW ... FROM <table> [FROM <database>]) target for an
// explicit but unresolved shape: ParseDBLevel only ever populates DB/ShowTable
// when the subsequent token(s) resolved to a plain name (see ParseDBLevel's
// USE branch, parseShowTableThenDatabase, and parsedIdentifierAt), so an
// explicit clause that stayed unresolved is the Identifier-parameter shape.
// DB/ShowTable containing '{' guards any future ParseDBLevel change that
// starts echoing the raw span into one of them instead of leaving it empty.
func dbLevelHoldsParameter(info DBLevelInfo) bool {
	switch info.Kind {
	case DBUse:
		return info.DB == "" || strings.Contains(info.DB, "{")
	case DBShow:
		if info.HasDBClause && !info.DBResolved {
			return true
		}
		// The COLUMNS/INDEX family's FIRST FROM/IN clause names a TABLE, kept
		// in ShowTable/ShowTableResolved rather than DB/DBResolved (dblevel.go's
		// parseShowTableThenDatabase) — checked independently of the database
		// clause above, since either can carry the parameter on its own (e.g.
		// "SHOW COLUMNS FROM {p:Identifier}" never reaches a database clause at
		// all, while "SHOW COLUMNS FROM t FROM {d:Identifier}" resolves the
		// table and leaves only the database clause unresolved).
		if info.HasTableClause && !info.ShowTableResolved {
			return true
		}
		return strings.Contains(info.DB, "{") || strings.Contains(info.ShowTable, "{")
	default:
		return false
	}
}

// alterUpdateTargetHoldsParameter reports whether an ALTER ... UPDATE
// statement's target (the span between ALTER TABLE and UPDATE) holds an
// Identifier parameter, reusing mutation_reads.go's own
// alterMutationTail/consumeMutationQualifiedName grammar instead of a second,
// independently-written boundary scanner: consumeMutationQualifiedName
// requires BOTH sides of a DOT to be a name token, so a target such as
// "db1.{p:Identifier}" already fails that grammar (verified: the L_BRACE after
// the dot is not a name token, and the trailing DOT-without-a-following-name
// check catches the bare "{p:Identifier}" form too) — alterMutationTail
// reports that as ok==false, which this function treats as a hit. A
// tokenizer error is also a hit (fail closed); the tail after UPDATE
// (assignments, WHERE) is never inspected here, so a genuinely allowed
// column-position parameter there cannot trip this.
func alterUpdateTargetHoldsParameter(e Engine, sql string) bool {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return true
	}
	_, _, ok := alterMutationTail(toks)
	return !ok
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
