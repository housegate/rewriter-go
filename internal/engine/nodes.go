package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
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

// BareTableNames returns every unqualified (no DB prefix) table name referenced
// in the AST, without recursing into CTE bodies already in scope. This is used
// to seed the referenced-CTE set: CTE aliases appear as bare table refs in the
// outer query before any injection has happened.
// Unlike CollectSelectTables it does NOT skip bare refs that match an in-scope
// CTE alias — those are exactly the refs we want to collect.
func BareTableNames(ast AST) ([]string, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode select: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	var walk func(node any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			if tbl, ok := n["table"].(map[string]any); ok {
				tt := decodeTableTarget(tbl)
				if tt.Table != "" && tt.DB == "" {
					if !seen[tt.Table] {
						seen[tt.Table] = true
						out = append(out, tt.Table)
					}
				}
				// Don't recurse further into this table-expression node;
				// any children are column/subquery nodes, not table refs.
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
	walk(root)
	return out, nil
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
			if tt.Table == "" {
				// Column-qualifier false positive: the "table" key here holds a flat
				// identifier (only name/quoted/trailing_comments), not a real table
				// descriptor. Recursing is safe because there is no nested
				// table-expression wrapper inside such a node — no real table will be
				// re-visited.
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

// originName returns the qualified original table name — "db.table" when the
// source had a db prefix, or bare "table" when it did not. This is the value
// C++ passes to setAlias when the user supplied no alias (origin_table_name /
// origin_full_name in ASTTransformers.cc:157,179,192 and select.cc:201,225).
func originName(tt TableTarget) string {
	if tt.DB != "" {
		return tt.DB + "." + tt.Table
	}
	return tt.Table
}

// applyDecision mutates the table-expression wrapper `expr` (expr["table"]==tbl) per d.
// When the user supplied no alias, a back-alias equal to the original qualified name is
// added to keep qualified column references (e.g. t.col) and result-column names stable
// after renaming — matching ASTReplaceTransformer::transform (ASTTransformers.cc:154-192)
// and dynamicRewriteWalk (select.cc:198-225).
func applyDecision(expr, tbl map[string]any, tt TableTarget, d TableDecision) {
	switch d.Action {
	case ActionRename:
		tbl["name"] = ident(d.NewTable)
		if d.NewDB != "" {
			tbl["schema"] = ident(d.NewDB)
		}
		if tt.Alias != "" {
			// User alias already sits in tbl["alias"] — leave it untouched.
		} else {
			// Back-alias to the original qualified name so qualified column refs stay valid.
			tbl["alias"] = ident(originName(tt))
		}
	case ActionRemote:
		if d.Remote == nil {
			return // misconfigured decision — leave the table untouched
		}
		delete(expr, "table")
		fn := remoteFunc(d.Remote)
		// The alias for a remote() always goes on the wrapper node (not the function
		// itself). Use the user alias when present; otherwise back-alias to the original
		// qualified name (mirrors ASTReplaceTransformer::transform, ASTTransformers.cc:175-179).
		aliasName := tt.Alias
		if aliasName == "" {
			aliasName = originName(tt)
		}
		// Polyglot places the alias on a wrapper node that contains the
		// function under "this", not directly on the function node itself.
		// Empirically: `remote(...) AS x` parses as
		//   expr["alias"] = {"alias":{name:"x",...}, "this":{"function":{...}}}
		// rather than fn["alias"] = {name:"x",...}.
		expr["alias"] = map[string]any{
			"alias":              ident(aliasName),
			"alias_explicit_as":  true,
			"alias_keyword":      "AS",
			"column_aliases":     []any{},
			"pre_alias_comments": []any{},
			"this":               map[string]any{"function": fn},
			"trailing_comments":  []any{},
		}
	case ActionSkip:
		// no-op
	}
}

// needsQuoting reports whether s must be quoted to survive as a single ClickHouse
// identifier — i.e. it is empty or contains a character outside [A-Za-z0-9_] or
// starts with a digit. Mirrors ClickHouse IdentifierQuotingRule::WhenNecessary for
// the cases the rewriter produces (notably dotted dynamic table names).
func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for i, r := range s {
		isLower := r >= 'a' && r <= 'z'
		isUpper := r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		if r == '_' || isLower || isUpper {
			continue
		}
		if isDigit && i > 0 {
			continue
		}
		return true // includes '.', leading digit, and any other char
	}
	return false
}

// ident builds an Identifier node, quoting the name only when necessary (so a
// dotted dynamic table name like `tenant1.events` round-trips as a single
// identifier, not a multi-part db.table.col reference).
func ident(s string) map[string]any {
	return map[string]any{"name": s, "quoted": needsQuoting(s), "trailing_comments": []any{}}
}

// litStr builds a string-literal argument node; used for addr, user, and password in remote().
func litStr(s string) map[string]any {
	return map[string]any{"literal": map[string]any{"literal_type": "string", "value": s}}
}

// colBare builds a bare identifier argument node; used for db and table in remote(), rendered unquoted.
func colBare(s string) map[string]any {
	return map[string]any{"column": map[string]any{
		"name": ident(s), "table": nil, "join_mark": false, "trailing_comments": []any{},
	}}
}

// remoteFunc builds {"name":"remote","args":[addr, db, table, user, pw], ...}.
// All five args are string literals, matching ClickHouse's canonical remote()
// form `remote('addr', 'db', 'table', 'user', 'password')` (the C++ oracle quotes
// db/table as string literals, not bare identifiers).
func remoteFunc(r *RemoteSpec) map[string]any {
	return map[string]any{
		"name": "remote",
		"args": []any{
			litStr(r.Addr), litStr(r.DB), litStr(r.Table), litStr(r.User), litStr(r.Password),
		},
		"distinct": false, "trailing_comments": []any{},
		"use_bracket_syntax": false, "no_parens": false, "quoted": false,
	}
}

// forkCTEScope copies the parent scope and adds this select's CTE aliases.
// The returned map MUST be treated read-only: when parent has no new CTEs it
// is returned by reference (shared with callers up the stack).
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

// numLiteral builds {"literal":{"literal_type":"number","value":"<n>"}}.
func numLiteral(n int64) map[string]any {
	return map[string]any{"literal": map[string]any{"literal_type": "number", "value": strconv.FormatInt(n, 10)}}
}

// outerSelect decodes the AST and returns the outermost select object (value under
// the top-level "select" key) for in-place mutation, plus a re-encode closure.
func outerSelect(ast AST) (sel map[string]any, encode func() (AST, error), err error) {
	var root map[string]any
	if err = json.Unmarshal(ast, &root); err != nil {
		return nil, nil, fmt.Errorf("engine: decode select: %w", err)
	}
	s, ok := root["select"].(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("engine: not a select node")
	}
	encode = func() (AST, error) {
		b, e := json.Marshal(root)
		if e != nil {
			return nil, fmt.Errorf("engine: encode select: %w", e)
		}
		return AST(b), nil
	}
	return s, encode, nil
}

// GetLimit returns the outer select's LIMIT literal value, if present and numeric.
func GetLimit(ast AST) (val int64, ok bool, err error) {
	sel, _, err := outerSelect(ast)
	if err != nil {
		return 0, false, err
	}
	lim, ok := sel["limit"].(map[string]any)
	if !ok {
		return 0, false, nil
	}
	this, ok := lim["this"].(map[string]any)
	if !ok {
		return 0, false, nil
	}
	lit, ok := this["literal"].(map[string]any)
	if !ok {
		return 0, false, nil
	}
	s, _ := lit["value"].(string)
	n, e := strconv.ParseInt(s, 10, 64)
	if e != nil {
		return 0, false, nil // non-literal/expression limit → treat as absent
	}
	return n, true, nil
}

// SetLimit sets the outer select's LIMIT to n.
func SetLimit(ast AST, n int64) (AST, error) {
	sel, encode, err := outerSelect(ast)
	if err != nil {
		return nil, err
	}
	sel["limit"] = map[string]any{"this": numLiteral(n)}
	return encode()
}

// InjectCTEs appends named CTEs (alias → body select AST) to the outer select's
// WITH clause, creating the clause if absent. Aliases are inserted in
// alphabetical order for determinism. Only referenced bodies should be passed
// by the caller (see RewriteSelect). Mirrors ASTRewriteCTETransformer.
func InjectCTEs(ast AST, bodies map[string]AST) (AST, error) {
	if len(bodies) == 0 {
		return ast, nil
	}
	sel, encode, err := outerSelect(ast)
	if err != nil {
		return nil, err
	}
	with, _ := sel["with"].(map[string]any)
	if with == nil {
		with = map[string]any{"ctes": []any{}, "recursive": false, "leading_comments": []any{}}
	}
	ctes, _ := with["ctes"].([]any)

	aliases := make([]string, 0, len(bodies))
	for a := range bodies {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		var bodyNode any
		if err := json.Unmarshal(bodies[alias], &bodyNode); err != nil {
			return nil, fmt.Errorf("engine: decode cte %q: %w", alias, err)
		}
		ctes = append(ctes, map[string]any{
			"alias":        ident(alias),
			"this":         bodyNode,
			"columns":      []any{},
			"materialized": nil,
			"alias_first":  true,
		})
	}
	with["ctes"] = ctes
	sel["with"] = with
	return encode()
}

// SetOffset sets the outer select's OFFSET to n.
func SetOffset(ast AST, n int64) (AST, error) {
	sel, encode, err := outerSelect(ast)
	if err != nil {
		return nil, err
	}
	sel["offset"] = map[string]any{"this": numLiteral(n)}
	return encode()
}

// Setting is one SETTINGS key=value to render. LiteralType is polyglot's
// literal_type ("number"|"string"); Value is the encoded value.
type Setting struct {
	Key         string
	LiteralType string
	Value       string
}

// SetSettings appends settings to the outer select's SETTINGS array (creating it
// if absent). Each renders as {"eq":{"left":{"column":...},"right":{"literal":...}}}.
func SetSettings(ast AST, settings []Setting) (AST, error) {
	sel, encode, err := outerSelect(ast)
	if err != nil {
		return nil, err
	}
	arr, _ := sel["settings"].([]any)
	for _, s := range settings {
		arr = append(arr, map[string]any{"eq": map[string]any{
			"left":          colBare(s.Key),
			"right":         map[string]any{"literal": map[string]any{"literal_type": s.LiteralType, "value": s.Value}},
			"left_comments": []any{}, "operator_comments": []any{}, "trailing_comments": []any{},
		}})
	}
	sel["settings"] = arr
	return encode()
}
