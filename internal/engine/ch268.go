package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// UnmodelledClickHouse268Shape reports a ClickHouse 26.8 shape that Polyglot
// parses as something else, so no handler models what the server runs:
//
//   - a STREAM table modifier (`FROM t STREAM`, a continuous query). Polyglot
//     reads an unquoted bare `STREAM` as the implicit alias of whatever
//     precedes it (a table, subquery or table function, and a select-list
//     expression too, `SELECT a stream FROM t`, which 26.8 does not parse)
//     and regenerates it in place, where 26.8 reads the modifier. Every such
//     implicit alias is refused; an explicit `AS stream` or a quoted
//     `STREAM` stays an alias for both engines;
//   - a NATURAL join, which 26.4 added and which has no ON / USING the
//     25.8 / 26.3 servers accept.
//
// Mirrors rewriter-grpc unmodelledClickHouse268Shape, which also refuses a
// LOCAL join, a MergeTree UNIQUE KEY and DESCRIBE TEMPORARY TABLE; Polyglot
// does not parse those in full, so the whole-statement parse gate refuses
// them here first.
func UnmodelledClickHouse268Shape(ast AST) (bool, error) {
	if len(ast) == 0 {
		return false, nil
	}
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return false, fmt.Errorf("engine: decode: %w", err)
	}
	return hasClickHouse268Shape(root), nil
}

func hasClickHouse268Shape(node any) bool {
	switch n := node.(type) {
	case []any:
		for _, item := range n {
			if hasClickHouse268Shape(item) {
				return true
			}
		}
	case map[string]any:
		if explicit, ok := n["alias_explicit_as"].(bool); ok && !explicit {
			if alias, ok := n["alias"].(map[string]any); ok && alias["quoted"] == false {
				if name, ok := alias["name"].(string); ok && strings.EqualFold(name, "stream") {
					return true
				}
			}
		}
		if kind, ok := n["kind"].(string); ok && strings.HasPrefix(kind, "Natural") {
			if _, join := n["using"]; join {
				return true
			}
		}
		for _, v := range n {
			if hasClickHouse268Shape(v) {
				return true
			}
		}
	}
	return false
}

// HasIdentifierParameter reports whether ast holds an Identifier query
// parameter (`{f:Identifier}`) in any structured position. ClickHouse
// substitutes it before analysis, so in an expression it is a bare name the
// request's SQL does not show (measured on 26.8.37.168: `SELECT
// {f:Identifier}` with f = session_user returns the shared user, and
// `arrayMap({f:Identifier}, [true])` with f = currentSchemas the physical
// database), and the request carries no parameter values. T2 refuses a
// database or table position, and any Identifier parameter in an opaque
// text, before this; finalize refuses the rest with T7, as rewriter-grpc
// hasIdentifierQueryParameter.
func HasIdentifierParameter(ast AST) (bool, error) {
	if len(ast) == 0 {
		return false, nil
	}
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return false, fmt.Errorf("engine: decode: %w", err)
	}
	return hasIdentifierParameter(root), nil
}

func hasIdentifierParameter(node any) bool {
	switch n := node.(type) {
	case []any:
		for _, item := range n {
			if hasIdentifierParameter(item) {
				return true
			}
		}
	case map[string]any:
		if p, ok := n["parameter"].(map[string]any); ok && p["expression"] == "Identifier" {
			return true
		}
		for _, v := range n {
			if hasIdentifierParameter(v) {
				return true
			}
		}
	}
	return false
}
