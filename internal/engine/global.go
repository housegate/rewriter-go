package engine

import (
	"encoding/json"
	"fmt"
)

var remoteFuncNames = map[string]bool{
	"remote": true, "remoteSecure": true, "cluster": true, "clusterAllReplicas": true,
}

// ForceGlobalForRemoteAsymmetry promotes JOIN→GLOBAL JOIN and IN→GLOBAL IN where a
// remote/local asymmetry exists. Bottom-up, per select. Port of
// forceGlobalForRemoteAsymmetry (select.cc:505-563).
//
// LIMITATION: GLOBAL JOIN is represented by an alias "GLOBAL" on the join's LEFT
// operand, which polyglot only round-trips for a plain-table left operand. When
// the left operand is a table function (e.g. remote()), the GLOBAL modifier is
// NOT synthesizable (an alias there renders as `AS GLOBAL`, corrupting the query),
// so such joins are left un-GLOBAL — a known divergence registered with the
// differential harness.
// (Task 11 allow-lists: function-left GLOBAL joins, and any GLOBAL the C++ emits
// that polyglot cannot represent.)
func ForceGlobalForRemoteAsymmetry(ast AST) (AST, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode select: %w", err)
	}
	globalWalk(root, nil)
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode select: %w", err)
	}
	return AST(out), nil
}

// globalWalk recurses bottom-up; at each `select` it runs the two promotion passes.
func globalWalk(node any, scope map[string]bool) {
	switch n := node.(type) {
	case map[string]any:
		if sel, ok := n["select"].(map[string]any); ok {
			scope = forkCTEScope(sel, scope)
			for _, v := range sel { // children first (bottom-up)
				globalWalk(v, scope)
			}
			promoteJoins(sel, scope)
			promoteINs(sel, scope)
			return
		}
		for _, v := range n {
			globalWalk(v, scope)
		}
	case []any:
		for _, v := range n {
			globalWalk(v, scope)
		}
	}
}

// promoteJoins walks FROM+joins left→right; marks a join GLOBAL when accumulated
// locality is asymmetric with the current element. select.cc:526-547.
func promoteJoins(sel map[string]any, scope map[string]bool) {
	from, _ := sel["from"].(map[string]any)
	joins, _ := sel["joins"].([]any)
	if from == nil || len(joins) == 0 {
		return
	}
	fexprs, _ := from["expressions"].([]any)
	accRemote, accLocal := false, false
	for _, fe := range fexprs {
		accRemote = accRemote || subtreeHasRemoteSource(fe)
		accLocal = accLocal || subtreeHasLocalSource(fe, scope)
	}
	for i, j := range joins {
		jm, ok := j.(map[string]any)
		if !ok {
			continue
		}
		thisRemote := subtreeHasRemoteSource(jm["this"])
		thisLocal := subtreeHasLocalSource(jm["this"], scope)
		if (accRemote && thisLocal) || (accLocal && thisRemote) {
			markJoinGlobalAt(sel, i)
		}
		accRemote = accRemote || thisRemote
		accLocal = accLocal || thisLocal
	}
}

// markJoinGlobalAt sets GLOBAL on join i by giving its LEFT operand the alias
// "GLOBAL" — but ONLY when that operand is a plain table with no existing alias.
// A function/subquery left operand cannot carry the modifier (see LIMITATION), so
// it is left untouched.
//
// In multi-join chains where an intermediate function operand blocks marking
// (e.g. local JOIN remote JOIN local2), join[1] cannot be marked even if
// accumulators detect asymmetry — this is a silent no-op, not corruption.
// Registered with the differential harness.
func markJoinGlobalAt(sel map[string]any, joinIdx int) {
	left := leftTableByIndex(sel, joinIdx)
	if left == nil || left["alias"] != nil {
		return
	}
	left["alias"] = map[string]any{"name": "GLOBAL", "quoted": false, "trailing_comments": []any{}}
}

// leftTableByIndex returns the plain-table node immediately left of join i, or nil
// if that operand is not a plain table (function/subquery → cannot be marked).
func leftTableByIndex(sel map[string]any, joinIdx int) map[string]any {
	if joinIdx == 0 {
		from, _ := sel["from"].(map[string]any)
		fexprs, _ := from["expressions"].([]any)
		if len(fexprs) > 0 {
			if fe, ok := fexprs[0].(map[string]any); ok {
				if tbl, ok := fe["table"].(map[string]any); ok {
					return tbl
				}
			}
		}
		return nil
	}
	joins, _ := sel["joins"].([]any)
	if prev, ok := joins[joinIdx-1].(map[string]any); ok {
		if this, ok := prev["this"].(map[string]any); ok {
			if tbl, ok := this["table"].(map[string]any); ok {
				return tbl
			}
		}
	}
	return nil
}

// promoteINs: when the select's FROM or JOINs has a remote source, set in.global=true on
// any IN-family node (outside FROM/JOINS/WITH) whose RHS has a local source.
// select.cc:550-562 + forceGlobalInWalk:474-497.
//
// "Remote source present" considers the whole table list (FROM + JOINs),
// matching the C++ which tests subtreeHasRemoteSource(tables).
func promoteINs(sel map[string]any, scope map[string]bool) {
	if !subtreeHasRemoteSource(sel["from"]) && !subtreeHasRemoteSource(sel["joins"]) {
		return
	}
	for k, v := range sel {
		if k == "from" || k == "joins" || k == "with" {
			continue
		}
		globalInWalk(v, scope)
	}
}

// globalInWalk sets in.global=true where the IN's RHS subtree has a local source.
// Stops at nested selects/subqueries (handled at their own scope by globalWalk).
func globalInWalk(node any, scope map[string]bool) {
	switch n := node.(type) {
	case map[string]any:
		if _, ok := n["select"]; ok {
			return // a nested select — handled at its own scope by globalWalk
		}
		if in, ok := n["in"].(map[string]any); ok {
			rhsLocal := false
			if exprs, ok := in["expressions"].([]any); ok {
				for _, ex := range exprs {
					rhsLocal = rhsLocal || subtreeHasLocalSource(ex, scope)
				}
			}
			if q := in["query"]; q != nil {
				rhsLocal = rhsLocal || subtreeHasLocalSource(q, scope)
			}
			if rhsLocal {
				in["global"] = true
				n["in"] = in
			}
		}
		for _, v := range n {
			globalInWalk(v, scope)
		}
	case []any:
		for _, v := range n {
			globalInWalk(v, scope)
		}
	}
}

// subtreeHasRemoteSource reports whether any descendant is a remote/cluster function.
func subtreeHasRemoteSource(node any) bool {
	found := false
	visit(node, func(m map[string]any) {
		if fn, ok := m["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); remoteFuncNames[name] {
				found = true
			}
		}
	})
	return found
}

// subtreeHasLocalSource reports whether any descendant is a real (non-CTE) table.
// The tt.Table != "" guard excludes column-qualifier nodes (which share the
// "table" JSON key but carry a flat-string name → empty Table).
func subtreeHasLocalSource(node any, scope map[string]bool) bool {
	found := false
	visit(node, func(m map[string]any) {
		if tbl, ok := m["table"].(map[string]any); ok {
			tt := decodeTableTarget(tbl)
			if tt.Table != "" && !(tt.DB == "" && scope[tt.Table]) {
				found = true
			}
		}
	})
	return found
}

// visit calls fn on every map node in the subtree (pre-order).
func visit(node any, fn func(map[string]any)) {
	switch n := node.(type) {
	case map[string]any:
		fn(n)
		for _, v := range n {
			visit(v, fn)
		}
	case []any:
		for _, v := range n {
			visit(v, fn)
		}
	}
}
