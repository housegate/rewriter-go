package engine

import (
	"encoding/json"
	"fmt"
)

// ReferencesIdentifierInScope reports whether the AST addresses `name` in a
// position that can resolve to a table `protected` accepts.
//
// A reference counts when either:
//   - the query block that owns it reads at least one protected table (its own
//     FROM/JOIN list, after in-scope CTE aliases are removed), or
//   - the reference is qualified and its qualifier is bound to a protected
//     table in that block or any enclosing one.
//
// An unqualified reference inside a block that reads only ordinary tables does
// NOT count: an identically named column on an ordinary table is legitimate
// (Spec I D7a). Hiding is still enforced structurally — the protected table is
// replaced by a derived table that projects the column away — so this scoping
// narrows the error message, never the protection.
func ReferencesIdentifierInScope(ast AST, name string, protected func(TableTarget) bool) (bool, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return false, fmt.Errorf("engine: decode: %w", err)
	}
	return scopeWalk(root, name, protected, map[string]bool{}, false, nil), nil
}

// scopeWalk descends `node`, which belongs to a query block whose own tables
// make `blockProtected` true and whose qualifier bindings are `bindings`
// (qualifier -> is-protected, innermost binding wins).
func scopeWalk(node any, name string, protected func(TableTarget) bool,
	bindings map[string]bool, blockProtected bool, cteScope map[string]bool) bool {
	switch n := node.(type) {
	case map[string]any:
		// A nested query block re-derives its own tables and bindings.
		if sel, ok := n["select"].(map[string]any); ok {
			scope := forkCTEScope(sel, cteScope)
			nextBindings, nextProtected := blockScope(sel, scope, protected, bindings)
			for _, v := range sel {
				if scopeWalk(v, name, protected, nextBindings, nextProtected, scope) {
					return true
				}
			}
			return false
		}
		// Polyglot represents ordinary qualified columns as
		// {"column":{"name":...,"table":...}} rather than a dot node.
		// Decide that whole reference here so its child identifier is not
		// re-judged as an unqualified hit.
		if col, ok := n["column"].(map[string]any); ok && identName(col["name"]) == name {
			if q := identName(col["table"]); q != "" {
				if p, bound := bindings[q]; bound {
					return p
				}
			}
			return blockProtected
		}
		// A qualified reference (`alias.name`, `db.table.name`) is decided by
		// its qualifier and never descends further: the child identifier node
		// is the same reference and must not be re-judged as unqualified.
		if dot, ok := n["dot"].(map[string]any); ok && identName(dot["field"]) == name {
			if q := identName(dot["this"]); q != "" {
				if p, bound := bindings[q]; bound {
					return p
				}
			}
			return blockProtected
		}
		if unqualifiedIdentifierHit(n, name) {
			if blockProtected {
				return true
			}
			return false
		}
		for _, v := range n {
			if scopeWalk(v, name, protected, bindings, blockProtected, cteScope) {
				return true
			}
		}
	case []any:
		for _, v := range n {
			if scopeWalk(v, name, protected, bindings, blockProtected, cteScope) {
				return true
			}
		}
	}
	return false
}

// blockScope returns the qualifier bindings visible inside one select block
// (enclosing bindings plus this block's own tables) and whether the block
// itself reads a protected table.
func blockScope(sel map[string]any, cteScope map[string]bool, protected func(TableTarget) bool,
	parent map[string]bool) (map[string]bool, bool) {
	out := make(map[string]bool, len(parent)+2)
	for k, v := range parent {
		out[k] = v
	}
	blockProtected := false
	for _, tt := range blockTables(sel, cteScope) {
		p := protected(tt)
		blockProtected = blockProtected || p
		switch {
		case tt.Alias != "":
			out[tt.Alias] = p
		case tt.Table != "":
			out[tt.Table] = p
		}
	}
	return out, blockProtected
}

// blockTables returns the tables one select block reads directly. It never
// descends into a nested block (`select` / `subquery`) — those own their own
// scope — and skips bare references that match an in-scope CTE alias.
func blockTables(sel map[string]any, cteScope map[string]bool) []TableTarget {
	var out []TableTarget
	var walk func(node any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			if _, nested := n["select"]; nested {
				return
			}
			if _, nested := n["subquery"]; nested {
				return
			}
			if tbl, ok := n["table"].(map[string]any); ok {
				tt := decodeTableTarget(tbl)
				if tt.Table == "" {
					for _, v := range n {
						walk(v)
					}
					return
				}
				if tt.DB == "" && cteScope[tt.Table] {
					return
				}
				out = append(out, tt)
				return
			}
			for _, v := range n {
				walk(v)
			}
		case []any:
			for _, v := range n {
				walk(v)
			}
		}
	}
	walk(sel)
	return out
}

// unqualifiedIdentifierHit recognizes the remaining unqualified reference
// shapes after `column` and `dot` are handled by the caller: a bare Identifier
// node, a JOIN USING entry, and star EXCEPT / REPLACE / RENAME entries. String
// literals never match — they are not Identifier-shaped.
func unqualifiedIdentifierHit(n map[string]any, name string) bool {
	if got, ok := n["name"].(string); ok && got == name {
		if _, identifierShape := n["quoted"]; identifierShape {
			return true
		}
	}
	if using, ok := n["using"].([]any); ok {
		for _, e := range using {
			if identName(e) == name {
				return true
			}
		}
	}
	star, ok := n["star"].(map[string]any)
	if !ok {
		return false
	}
	if list, ok := star["except"].([]any); ok {
		for _, e := range list {
			if identName(e) == name {
				return true
			}
		}
	}
	if list, ok := star["replace"].([]any); ok {
		for _, e := range list {
			if m, ok := e.(map[string]any); ok && identName(m["alias"]) == name {
				return true
			}
		}
	}
	if list, ok := star["rename"].([]any); ok {
		for _, e := range list {
			if pair, ok := e.([]any); ok {
				for _, side := range pair {
					if identName(side) == name {
						return true
					}
				}
			}
		}
	}
	return false
}
