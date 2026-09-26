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
// MV TO target, and — for opaque command nodes — any Identifier parameter in
// the command text.
func TablePositionParameter(ast AST) (bool, error) {
	kind, err := NodeKind(ast)
	if err != nil {
		return false, err
	}
	if kind == NodeCommand {
		sql, err := CommandSQL(ast)
		if err != nil {
			return false, err
		}
		return IdentifierParameterInText(sql), nil
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

// IdentifierParameterInText scans SQL text for a `{name:Identifier}` token
// outside string literals, quoted identifiers and comments. It reuses the
// lexical scanner's startsIdentifierQueryParameter so the accepted spelling
// matches the parser's.
func IdentifierParameterInText(sql string) bool {
	for i := 0; i < len(sql); i++ {
		switch sql[i] {
		case '\'', '`', '"':
			i = skipQuotedSpan(sql, i)
		case '-':
			if i+1 < len(sql) && sql[i+1] == '-' {
				i = skipLineComment(sql, i)
			}
		case '/':
			if i+1 < len(sql) && sql[i+1] == '*' {
				i = skipBlockCommentSpan(sql, i)
			}
		case '{':
			if startsIdentifierQueryParameter(sql[i:]) { // lexical.go:1947, string-based
				return true
			}
			// Tolerate the engine's own reprint spacing for a brace parameter
			// embedded in an identifier position on the RENAME/EXCHANGE command
			// grammar (measured on rewriter-go v0.13.0): parsing
			// "RENAME TABLE db1.{p:Identifier} TO db1.z" and reading command.this
			// back yields "RENAME TABLE db1.{ p:Identifier } TO db1.z" — a single
			// space padded inside the braces, with "name:Identifier" itself left
			// unspaced. That padding is an artifact of the engine's own
			// command-node reprint, not something a caller typed, so a command
			// node's raw text (the only input this function has for that node
			// kind) must be scanned tolerating it too, or every RENAME/EXCHANGE
			// carrying an Identifier-typed target silently escapes detection.
			// This is strictly narrower than tolerating arbitrary whitespace: the
			// colon itself must stay unspaced, so "USE {d : Identifier}" (space
			// around the colon, not the braces) is unaffected and still false.
			if startsIdentifierQueryParameterSpaced(sql[i:]) {
				return true
			}
		}
	}
	return false
}

// startsIdentifierQueryParameterSpaced matches startsIdentifierQueryParameter's
// exact spelling with one added tolerance: a single space immediately after
// '{' and immediately before '}'. See IdentifierParameterInText's comment for
// why this narrow padding (and only this padding) is tolerated.
func startsIdentifierQueryParameterSpaced(value string) bool {
	if len(value) < 2 || value[0] != '{' || value[1] != ' ' {
		return false
	}
	rest := value[2:]
	end := -1
	for j := 0; j < len(rest); j++ {
		if rest[j] == '}' {
			end = j
			break
		}
	}
	if end <= 0 || rest[end-1] != ' ' {
		return false
	}
	inner := rest[:end-1]
	return startsIdentifierQueryParameter("{" + inner + "}")
}

// skipQuotedSpan returns the index of the closing delimiter of the span that
// opens at sql[i] ('…', `…` or "…"), honouring doubled delimiters and
// backslash escapes; an unterminated span skips to the end.
func skipQuotedSpan(sql string, i int) int {
	q := sql[i]
	for j := i + 1; j < len(sql); j++ {
		switch sql[j] {
		case '\\':
			j++
		case q:
			if j+1 < len(sql) && sql[j+1] == q {
				j++
				continue
			}
			return j
		}
	}
	return len(sql)
}

func skipLineComment(sql string, i int) int {
	for ; i < len(sql) && sql[i] != '\n'; i++ {
	}
	return i
}

func skipBlockCommentSpan(sql string, i int) int {
	depth := 0
	for j := i; j+1 < len(sql); j++ {
		switch {
		case sql[j] == '/' && sql[j+1] == '*':
			depth++
			j++
		case sql[j] == '*' && sql[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j
			}
		}
	}
	return len(sql)
}
