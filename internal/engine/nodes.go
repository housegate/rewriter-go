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

// TableAction is the rewrite a caller chose for a TableTarget.
type TableAction int

const (
	ActionSkip   TableAction = iota // leave the node untouched
	ActionRename                    // set table name (+ optionally schema/db)
	ActionRemote                    // replace the table expr with remote(...)
)

// RemoteSpec are the five positional args of a remote() table function.
type RemoteSpec struct{ Addr, DB, Table, User, Password string }

// TableDecision is what a caller returns for a TableTarget.
type TableDecision struct {
	Action   TableAction
	NewDB    string      // ActionRename: new schema; "" keeps the existing schema untouched
	NewTable string      // ActionRename: new table name
	Remote   *RemoteSpec // ActionRemote: the remote() args
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
	visitTables(root, nil, func(_, _ map[string]any, tt TableTarget) {
		out = append(out, tt)
	})
	return out, nil
}

// RewriteSelectTables walks every real table reference (same traversal as
// CollectSelectTables) and applies the TableDecision returned by decide. The AST
// is decoded once, mutated in place (Go maps are references), and re-encoded.
// Mirrors ASTReplaceTransformer::transform.
func RewriteSelectTables(ast AST, decide func(TableTarget) TableDecision) (AST, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode select: %w", err)
	}
	visitTables(root, nil, func(expr, tbl map[string]any, tt TableTarget) {
		applyDecision(expr, tbl, tt, decide(tt))
	})
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode select: %w", err)
	}
	return AST(out), nil
}

// visitTables walks a decoded SELECT AST and calls visit for every REAL table
// reference (recursing into JOINs, FROM-subqueries, CTE bodies), skipping
// column-qualifier nodes (tt.Table=="") and bare refs matching an in-scope CTE
// alias. visit receives the table-expression wrapper `expr` (mutable) and the
// `tbl` node (mutable), so callers can rewrite in place.
// scope is treated read-only: it is never mutated (copy-on-fork via forkCTEScope).
func visitTables(node any, scope map[string]bool, visit func(expr, tbl map[string]any, tt TableTarget)) {
	switch n := node.(type) {
	case map[string]any:
		if sel, ok := n["select"].(map[string]any); ok {
			scope = forkCTEScope(sel, scope)
			for _, v := range sel {
				visitTables(v, scope, visit)
			}
			return
		}
		if tbl, ok := n["table"].(map[string]any); ok {
			tt := decodeTableTarget(tbl)
			if tt.Table == "" { // column-qualifier false positive — recurse, don't visit
				for _, v := range n {
					visitTables(v, scope, visit)
				}
				return
			}
			if tt.DB == "" && scope[tt.Table] {
				return // in-scope CTE alias — leave untouched
			}
			visit(n, tbl, tt)
			return
		}
		for _, v := range n {
			visitTables(v, scope, visit)
		}
	case []any:
		for _, v := range n {
			visitTables(v, scope, visit)
		}
	}
}

// applyDecision mutates the table-expression wrapper `expr` (expr["table"]==tbl) per d.
func applyDecision(expr, tbl map[string]any, tt TableTarget, d TableDecision) {
	switch d.Action {
	case ActionRename:
		tbl["name"] = ident(d.NewTable)
		if d.NewDB != "" {
			tbl["schema"] = ident(d.NewDB)
		}
		// existing tbl["alias"] is left untouched (preserves the user's alias).
	case ActionRemote:
		delete(expr, "table")
		fn := remoteFunc(d.Remote)
		if tt.Alias != "" {
			fn["alias"] = ident(tt.Alias)
		}
		expr["function"] = fn
	case ActionSkip:
		// no-op
	}
}

// ident builds an Identifier node {"name":s,"quoted":false,"trailing_comments":[]}.
func ident(s string) map[string]any {
	return map[string]any{"name": s, "quoted": false, "trailing_comments": []any{}}
}

func litStr(s string) map[string]any {
	return map[string]any{"literal": map[string]any{"literal_type": "string", "value": s}}
}

func colBare(s string) map[string]any {
	return map[string]any{"column": map[string]any{
		"name": ident(s), "table": nil, "join_mark": false, "trailing_comments": []any{},
	}}
}

// remoteFunc builds {"name":"remote","args":[addr, db, table, user, pw], ...}.
// addr/user/pw are string literals; db/table are bare column identifiers.
func remoteFunc(r *RemoteSpec) map[string]any {
	return map[string]any{
		"name": "remote",
		"args": []any{
			litStr(r.Addr), colBare(r.DB), colBare(r.Table), litStr(r.User), litStr(r.Password),
		},
		"distinct": false, "trailing_comments": []any{},
		"use_bracket_syntax": false, "no_parens": false, "quoted": false,
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
