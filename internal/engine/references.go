package engine

import (
	"encoding/json"
	"strings"
)

// CollectDatabaseReferences returns every database name the statement
// addresses, in document order and deduplicated, across every position the
// table-reference policy governs (spec 2026-09-26 §5). Unqualified names
// contribute nothing: the logical context is checked by the caller.
//
// Write targets (the statement's own CREATE/DROP/INSERT/ALTER/RENAME/TO
// target(s), CREATE/DROP DATABASE, table-function clone sources, ...) are
// collected before embedded read sources (FROM/JOIN/subquery/IN/table
// function arguments/...) so a statement's own target database is reported
// ahead of the databases it merely reads — matching the textual order of the
// statements this policy governs (the target always precedes the body it
// reads).
func CollectDatabaseReferences(e Engine, ast AST, sql string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(db string) {
		if db != "" && !seen[db] {
			seen[db] = true
			out = append(out, db)
		}
	}
	kind, err := NodeKind(ast)
	if err != nil {
		return nil, err
	}
	if kind == NodeCommand {
		// USE / SHOW … FROM / EXISTS / SHOW CREATE / DESCRIBE / RENAME / EXCHANGE.
		if info, err := ParseDBLevel(e, sql); err == nil {
			add(info.DB)
		}
		if t, err := ParseObjectTarget(e, sql); err == nil {
			if t.ObjType == "DATABASE" {
				add(t.Table)
			} else {
				add(t.DB)
			}
		}
		if raw, _, err := RawTableRefs(e, ast); err == nil {
			for _, tt := range raw {
				add(tt.DB)
			}
		}
		return out, nil
	}
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, err
	}
	// Write targets first: CREATE/DROP/INSERT/ALTER/CLONE-SOURCE/MV-TO targets
	// and CREATE/DROP DATABASE — the statement's own object(s) — precede the
	// databases its body merely reads.
	targets, err := AllWriteTargets(e, ast)
	if err != nil {
		return nil, err
	}
	for _, tt := range targets {
		add(tt.DB)
	}
	if err := walkStatementObjects(root, readSourceScope{}, readSourceVisitor{
		table:     func(_, _ map[string]any, tt TableTarget) { add(tt.DB) },
		inTable:   func(_ map[string]any, d namespaceRefDetail) { add(d.ref.Target.DB) },
		namespace: func(_ map[string]any, d namespaceRefDetail) { add(d.ref.Target.DB) },
	}); err != nil {
		return nil, err
	}
	// Ruling 1 (controller review, spec 2026-09-26 T3): CollectDatabaseReferences
	// must also see string-form lookup arguments (joinGet/dictGet/hasColumnInTable
	// family) — those are ordinary scalar function calls, not source-role table
	// functions, so the shared read-source visitor above never emits them.
	collectStringLookupDatabases(root, add)
	// A parenthesized single-element IN list `x IN (db.table)` is not
	// is_field-tagged (that flag distinguishes only the syntactic bare-vs-
	// parenthesized IN operand), so the shared namespace/inTable visitor above
	// — which requires is_field precisely so an ordinary multi-element value
	// list is never mistaken for a table reference — does not see it. A
	// single qualified column is the only shape this adds.
	collectParenthesizedInDatabases(root, add)
	return out, nil
}

// CollectSIHandlerBlindDatabaseReferences returns, in document order and
// deduplicated, the subset of CollectDatabaseReferences' result that comes
// from a position no existing storage-integrity handler classifies as a
// table reference: an IsStringLookup call's first string-literal argument
// (joinGet/dictGet/hasColumnInTable family — an ordinary scalar function call
// whose string argument happens to embed a namespace) and a parenthesized
// single-element IN-list (`x IN (db.table)` — not is_field-tagged, so the
// same SI machinery that classifies a bare `x IN db.table` never sees it
// either). PreflightTableReferences must reject a protected hit here even
// while the storage-integrity surface is active, unlike the ordinary table
// positions it otherwise defers to the SI handlers.
func CollectSIHandlerBlindDatabaseReferences(ast AST) ([]string, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	add := func(db string) {
		if db != "" && !seen[db] {
			seen[db] = true
			out = append(out, db)
		}
	}
	collectStringLookupDatabases(root, add)
	collectParenthesizedInDatabases(root, add)
	return out, nil
}

// IsStringLookup reports whether name is a ClickHouse function whose first
// string-literal argument is (or embeds) a database-qualified table/dictionary
// name (spec 2026-09-26 T3, controller ruling 1): the joinGet family, the
// dictGet/dictHas/dictIsIn/dictGetHierarchy/dictGetChildren/dictGetDescendants
// family, and hasColumnInTable.
func IsStringLookup(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "joinget", "joingetornull":
		return true
	}
	for _, prefix := range []string{
		"dictget", "dicthas", "dictgethierarchy", "dictisin",
		"dictgetchildren", "dictgetdescendants", "hascolumnintable",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// collectStringLookupDatabases walks every "function" node anywhere in the
// decoded AST (arbitrary scalar calls are not otherwise visited for namespace
// purposes) and reports the database qualifier of an IsStringLookup call's
// relevant argument.
func collectStringLookupDatabases(node any, add func(string)) {
	switch n := node.(type) {
	case map[string]any:
		if fn, ok := n["function"].(map[string]any); ok {
			name, _ := fn["name"].(string)
			args, _ := fn["args"].([]any)
			if IsStringLookup(name) {
				if db, ok := stringLookupArgDatabase(name, args); ok {
					add(db)
				}
			}
		}
		for _, v := range n {
			collectStringLookupDatabases(v, add)
		}
	case []any:
		for _, v := range n {
			collectStringLookupDatabases(v, add)
		}
	}
}

// stringLookupArgDatabase selects the argument that carries the database for
// an IsStringLookup call and extracts its qualifier. hasColumnInTable's
// database is the third argument from the end — ClickHouse's optional leading
// hostname[, username[, password]] form shifts every other argument (spec
// 2026-09-26 T3/T4 review round 1 finding 2), so fewer than 3 arguments is an
// unresolvable call, not database index 0. Every other recognized name uses
// the first argument.
//
// The selected argument may be a string literal (the whole literal for
// hasColumnInTable, or split on the first '.' for a qualified "db.table"
// string) or an unquoted qualified identifier `db.table` — ClickHouse
// documents both forms for joinGet/dictGet (review round 1 finding 1) — the
// latter decoded structurally via qualifiedColumnArgTarget so a table name
// containing a literal '.' is never mis-split.
func stringLookupArgDatabase(name string, args []any) (string, bool) {
	var arg any
	if strings.HasPrefix(strings.ToLower(name), "hascolumnintable") {
		if len(args) < 3 {
			return "", false
		}
		arg = args[len(args)-3]
	} else {
		if len(args) == 0 {
			return "", false
		}
		arg = args[0]
	}
	if target, _, ok := qualifiedColumnArgTarget(arg); ok {
		return target.DB, target.DB != ""
	}
	if value, origin, ok := tableFunctionArgValue(arg); ok && origin == namespaceValueLiteral {
		return stringLookupDatabase(name, value)
	}
	return "", false
}

// collectParenthesizedInDatabases recognizes the parenthesized single-element
// IN-list form `x IN (db.table)` (spec 2026-09-26 T3/T4): polyglot's is_field
// flag distinguishes only the syntactic bare-vs-parenthesized IN operand, not
// ClickHouse's own catalog-dependent table-vs-column dichotomy for a
// parenthesized single identifier, so the shared namespace/inTable visitor
// (deliberately is_field-gated so an ordinary multi-element value list is
// never mistaken for a table reference) does not see this shape.
func collectParenthesizedInDatabases(node any, add func(string)) {
	switch n := node.(type) {
	case map[string]any:
		if in, ok := n["in"].(map[string]any); ok {
			// is_field is the bare (non-parenthesized) form the shared
			// namespace/inTable visitor already classifies and defers to the
			// SI handlers for; only its ABSENCE marks the genuinely-ambiguous
			// parenthesized form this function exists for.
			isField, _ := in["is_field"].(bool)
			if exprs, ok := in["expressions"].([]any); !isField && ok && len(exprs) == 1 {
				if target, _, ok := qualifiedColumnArgTarget(exprs[0]); ok {
					add(target.DB)
				}
			}
		}
		for _, v := range n {
			collectParenthesizedInDatabases(v, add)
		}
	case []any:
		for _, v := range n {
			collectParenthesizedInDatabases(v, add)
		}
	}
}

// stringLookupDatabase extracts the database qualifier from a string-lookup
// call's first literal argument: for hasColumnInTable the whole (unquoted)
// literal names the database directly; for every other recognized name the
// literal is split on the first '.' and the qualifier half is unquoted.
func stringLookupDatabase(name, literal string) (string, bool) {
	if strings.HasPrefix(strings.ToLower(name), "hascolumnintable") {
		db := unwrapQuoted(literal)
		return db, db != ""
	}
	idx := strings.IndexByte(literal, '.')
	if idx < 0 {
		return "", false
	}
	db := unwrapQuoted(literal[:idx])
	return db, db != ""
}

// unwrapQuoted strips a single layer of surrounding backticks or double
// quotes, mirroring how ClickHouse accepts a quoted identifier segment inside
// a string-lookup call's qualified name argument.
func unwrapQuoted(s string) string {
	if len(s) >= 2 {
		if (s[0] == '`' && s[len(s)-1] == '`') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
