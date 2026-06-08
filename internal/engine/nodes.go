package engine

import (
	"encoding/json"
	"fmt"
)

// TableTarget is the read view of one real table reference in a SELECT AST.
type TableTarget struct {
	DB    string // schema.name.name; "" if unqualified
	Table string // name.name
	Alias string // alias.name; "" if none
}

// CollectSelectTables returns every real table reference in a SELECT AST, in
// document order, recursing into JOINs, FROM-subqueries, and CTE bodies. Bare
// references whose name matches an in-scope CTE alias are skipped (they are not
// physical tables). Mirrors collectAccessedTablePairsFromAST (select.cc:67-106).
func CollectSelectTables(ast AST) ([]TableTarget, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode select: %w", err)
	}
	var out []TableTarget
	walkTables(root, nil, &out)
	return out, nil
}

// walkTables recurses any decoded JSON node. `scope` is the set of CTE aliases
// visible here (copy-on-fork at each `select` node, like select.cc).
func walkTables(node any, scope map[string]bool, out *[]TableTarget) {
	switch n := node.(type) {
	case map[string]any:
		// Fork the CTE scope when entering a `select` node.
		if sel, ok := n["select"].(map[string]any); ok {
			scope = forkCTEScope(sel, scope)
			for _, v := range sel {
				walkTables(v, scope, out)
			}
			return
		}
		// A `table` node is a concrete table reference.
		// Guard: a real FROM/JOIN table descriptor has its .name field as a nested
		// identifier map ({"name":"t",...}). Column-qualifier "table" fields hold a
		// plain identifier directly, so identName returns "". Skip those.
		if tbl, ok := n["table"].(map[string]any); ok {
			tt := decodeTableTarget(tbl)
			if tt.Table == "" {
				break // not a real table descriptor (e.g. column qualifier); keep walking
			}
			if !(tt.DB == "" && scope[tt.Table]) { // skip in-scope CTE alias refs
				*out = append(*out, tt)
			}
			return // a table node has no table-bearing descendants
		}
		for _, v := range n {
			walkTables(v, scope, out)
		}
	case []any:
		for _, v := range n {
			walkTables(v, scope, out)
		}
	}
}

// forkCTEScope copies the parent scope and adds this select's CTE aliases.
// The returned map MUST be treated read-only: when parent has no new CTEs it is returned by reference (shared with callers up the stack).
func forkCTEScope(sel map[string]any, parent map[string]bool) map[string]bool {
	with, ok := sel["with"].(map[string]any)
	if !ok {
		return parent
	}
	ctes, ok := with["ctes"].([]any)
	if !ok || len(ctes) == 0 {
		return parent
	}
	extended := make(map[string]bool, len(parent)+len(ctes))
	for k := range parent {
		extended[k] = true
	}
	for _, c := range ctes {
		if cm, ok := c.(map[string]any); ok {
			if name := identName(cm["alias"]); name != "" {
				extended[name] = true
			}
		}
	}
	return extended
}

// decodeTableTarget reads {name:{name}, schema:{name}, alias:{name}} from a table node.
func decodeTableTarget(tbl map[string]any) TableTarget {
	return TableTarget{
		DB:    identName(tbl["schema"]),
		Table: identName(tbl["name"]),
		Alias: identName(tbl["alias"]),
	}
}

// identName extracts the .name string from an Identifier-shaped node ({"name":"x",...}).
// Returns "" for null/missing/malformed.
func identName(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["name"].(string)
	return s
}
