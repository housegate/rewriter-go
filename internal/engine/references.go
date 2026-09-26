package engine

import (
	"encoding/json"
	"fmt"
	"sort"
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

// StringLookup is one detected string-form table/dictionary lookup call
// (spec 2026-09-26 T6): a joinGet/dictGet-family call, whose single first
// argument denotes a table or dictionary by a qualified string or unquoted
// identifier, or hasColumnInTable, whose database and table are two separate
// literal arguments (an optional leading host[, user[, pw]] triple shifts
// every index — the same shift stringLookupArgDatabase already documents).
type StringLookup struct {
	// Function is the call's name exactly as written in the source SQL —
	// used verbatim in the caller's rejection message.
	Function string
	// Arg is the qualified "db.table" text the call names: for the
	// joinGet-family it is that single argument's decoded value (from either
	// a string literal or an unquoted qualified identifier); for
	// hasColumnInTable it is the database and table literals joined with a
	// "." (splitting on the FIRST '.' always recovers the original pair,
	// because the physical database half is a plain identifier and never
	// itself contains a literal '.'). "" when no usable target could be
	// decoded at all.
	Arg string
	// Literal reports whether every argument this decode depends on came
	// from a string literal (hasColumnInTable's own db/table pair, or the
	// joinGet-family's lone literal argument) — false for an unquoted
	// identifier argument (joinGet-family only) or an unresolvable
	// expression.
	Literal bool
}

// StringLookupCalls returns every recognized string-form lookup call in ast,
// in document order, wherever it appears — an ordinary scalar-expression
// position (SELECT list, WHERE, …), not gated by readSourceVisitor's
// source-role traversal, mirroring collectStringLookupDatabases. Only a call
// that names a first argument at all is reported (spec 2026-09-26 T6,
// controller ruling 2): a bare `joinGet()` names nothing to refuse or
// rewrite.
func StringLookupCalls(ast AST) ([]StringLookup, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode string lookups: %w", err)
	}
	var out []StringLookup
	collectStringLookupCalls(root, &out)
	return out, nil
}

func collectStringLookupCalls(node any, out *[]StringLookup) {
	switch n := node.(type) {
	case map[string]any:
		if fn, ok := n["function"].(map[string]any); ok {
			name, _ := fn["name"].(string)
			args, _ := fn["args"].([]any)
			if IsStringLookup(name) && len(args) > 0 {
				*out = append(*out, decodeStringLookupCall(name, args))
			}
		}
		for _, k := range sortedMapKeys(n) {
			collectStringLookupCalls(n[k], out)
		}
	case []any:
		for _, v := range n {
			collectStringLookupCalls(v, out)
		}
	}
}

// sortedMapKeys returns n's keys in sorted order. json.Unmarshal decodes a
// JSON object into a plain Go map, whose range iteration order is randomized
// per the language spec; collectStringLookupCalls/rewriteStringLookupCalls
// walk a map's values to reach nested "function" nodes, so without a fixed
// order the reported/rewritten position of a call among several siblings in
// one statement would vary from run to run (Task 7 fix round 1 finding 1,
// measured over 200 runs: up to 24 distinct accessed orders for one
// statement, and up to 3 distinct first-refusal messages for another). Sorted
// key order is a deterministic — not necessarily source-text — order, which
// is sufficient here: RewriteStringLookups's decide is a pure function of the
// call it receives (see its own doc comment), so nothing downstream depends
// on this order matching document order, only on it being STABLE.
func sortedMapKeys(n map[string]any) []string {
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// decodeStringLookupCall decodes one already-matched (IsStringLookup, len(args)>0)
// call's target into a StringLookup. hasColumnInTable requires BOTH its
// database and table arguments (args[len-3] and args[len-2] — see
// stringLookupArgDatabase) to be string literals; a fully-decoded call always
// carries Literal=true for the join-Get family too, since the joined value is
// meaningless once any half is unresolvable (RewriteStringLookups is never
// asked to rewrite either family that measured out to always-refuse).
func decodeStringLookupCall(name string, args []any) StringLookup {
	if strings.HasPrefix(strings.ToLower(name), "hascolumnintable") {
		if len(args) < 3 {
			return StringLookup{Function: name}
		}
		dbLit, dbOK := literalStringArg(args[len(args)-3])
		tableLit, tableOK := literalStringArg(args[len(args)-2])
		if !dbOK || !tableOK {
			return StringLookup{Function: name}
		}
		return StringLookup{Function: name, Arg: dbLit + "." + tableLit, Literal: true}
	}
	value, origin, ok := tableFunctionArgValue(args[0])
	if !ok {
		return StringLookup{Function: name}
	}
	return StringLookup{Function: name, Arg: value, Literal: origin == namespaceValueLiteral}
}

// literalStringArg decodes arg as a string literal, or reports ok=false for
// anything else (an identifier, an expression, a non-string literal).
func literalStringArg(arg any) (string, bool) {
	m, ok := arg.(map[string]any)
	if !ok {
		return "", false
	}
	lit, ok := m["literal"].(map[string]any)
	if !ok {
		return "", false
	}
	return decodeStringLiteralValue(lit)
}

// RewriteStringLookups mutates every string-lookup call decide accepts,
// re-walking ast in the same shape StringLookupCalls inspects. decide is
// invoked once per call; returning ok=false leaves that call's arguments
// untouched. Because decide is expected to be a pure function of the call's
// own Function/Arg (exactly mirroring how RewriteSelectTables's decide is a
// pure function of the TableTarget it receives), this second, independent
// walk never needs to agree on ORDER with a caller's own earlier
// StringLookupCalls pass — only on each call's own content. The accepted
// replacement is the new qualified "db.table" text: for a single-argument
// call it becomes that argument's whole literal value; for hasColumnInTable
// it is split on the FIRST '.' into the physical database and physical table
// literals (buildDynamicTableName's own "<logical>.<table>" shape, so the
// table half may itself still contain a dot — the database half never does).
func RewriteStringLookups(ast AST, decide func(StringLookup) (string, bool)) (AST, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode string lookups: %w", err)
	}
	rewriteStringLookupCalls(root, decide)
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode string lookups: %w", err)
	}
	return AST(out), nil
}

func rewriteStringLookupCalls(node any, decide func(StringLookup) (string, bool)) {
	switch n := node.(type) {
	case map[string]any:
		if fn, ok := n["function"].(map[string]any); ok {
			name, _ := fn["name"].(string)
			args, _ := fn["args"].([]any)
			if IsStringLookup(name) && len(args) > 0 {
				call := decodeStringLookupCall(name, args)
				if replacement, ok := decide(call); ok {
					fn["args"] = applyStringLookupReplacement(name, args, replacement)
				}
			}
		}
		for _, k := range sortedMapKeys(n) {
			rewriteStringLookupCalls(n[k], decide)
		}
	case []any:
		for _, v := range n {
			rewriteStringLookupCalls(v, decide)
		}
	}
}

// applyStringLookupReplacement builds the mutated args slice for one accepted
// rewrite: replacement is split on the first '.' into the hasColumnInTable
// db/table pair, or used whole for the joinGet-family's single argument.
func applyStringLookupReplacement(name string, args []any, replacement string) []any {
	out := append([]any(nil), args...)
	if strings.HasPrefix(strings.ToLower(name), "hascolumnintable") {
		if len(out) < 3 {
			return out
		}
		db, table := replacement, ""
		if idx := strings.IndexByte(replacement, '.'); idx >= 0 {
			db, table = replacement[:idx], replacement[idx+1:]
		}
		out[len(out)-3] = litStr(db)
		out[len(out)-2] = litStr(table)
		return out
	}
	if len(out) > 0 {
		out[0] = litStr(replacement)
	}
	return out
}

// CollectColumnDefinitionStringLookups returns every string-lookup call in a
// CREATE TABLE's own column definitions — DEFAULT / MATERIALIZED / ALIAS /
// EPHEMERAL expressions (spec 2026-09-26 T6, Task 7 fix round 1 finding 2) —
// in column-then-key order (columns in declaration order, then default,
// materialized_expr, alias_expr, ephemeral for each column; deterministic by
// construction, no sorting needed). ok=false (nil, nil) for anything that is
// not a create_table node. Deliberately narrower than StringLookupCalls: a
// CREATE TABLE ... AS SELECT's embedded body is a different sub-tree
// (as_select) that rewriteSelectCore owns and rewrites hasColumnInTable
// inside — this collector must never also see that body, or its caller could
// not tell "found in a column expression" (always refuse hasColumnInTable
// here) apart from "found in the AS SELECT body" (rewrite it there instead).
func CollectColumnDefinitionStringLookups(ast AST) ([]StringLookup, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode create table columns: %w", err)
	}
	body, ok := root[NodeCreateTable].(map[string]any)
	if !ok {
		return nil, nil
	}
	cols, _ := body["columns"].([]any)
	var out []StringLookup
	for _, c := range cols {
		col, ok := c.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"default", "materialized_expr", "alias_expr", "ephemeral"} {
			collectStringLookupCalls(col[key], &out)
		}
	}
	return out, nil
}

// CollectAlterMutationStringLookups detects an ALTER TABLE ... UPDATE/DELETE
// mutation — opaque in polyglot, either a command node classifyWriteCommand
// reports as CmdAlterUpdate or an alter_table node whose sole action is an
// unstructured Raw UPDATE/DELETE tail — and returns the string-lookup calls
// in its assignment/predicate expressions (spec 2026-09-26 T6, Task 7 fix
// round 1 finding 2): the joinGet/dictGet-family refusal and the
// hasColumnInTable "outside a SELECT body" refusal must reach these
// positions too, not just an embedded SELECT, which this opaque shape has
// none of. Reparses the mutation tail as an equivalent ordinary UPDATE/DELETE
// probe statement against a sentinel target, mirroring
// collectAlterMutationSurface's own technique, and walks the structured
// result for string-lookup calls (deterministic column-then-key order: SET
// assignments in order, then the predicate).
//
// Fails open (nil, nil) for anything that is not this exact opaque mutation
// shape, or whose tail this probe cannot cleanly reparse: this collector
// feeds an ADDITIONAL guard layered on top of the table-reference policy and
// must never turn an unrelated parse/tokenize hiccup into a hard failure for
// a statement that carries no lookup to refuse in the first place — the
// stricter round-trip proof collectAlterMutationSurface itself applies (used
// only under active storage integrity, where a probe mismatch must fail
// closed) is deliberately not reproduced here.
func CollectAlterMutationStringLookups(e Engine, ast AST, sql string) ([]StringLookup, error) {
	kind, body, _, err := bodyOf(ast)
	if err != nil || body == nil {
		return nil, nil
	}
	switch kind {
	case NodeCommand:
		raw, _ := body["this"].(string)
		if classifyWriteCommand(raw) != CmdAlterUpdate {
			return nil, nil
		}
	case NodeAlterTable:
		if !alterBodyHasMutation(body) {
			return nil, nil
		}
	default:
		return nil, nil
	}
	if strings.TrimSpace(sql) == "" {
		return nil, nil
	}
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return nil, nil
	}
	mkind, tailStart, ok := alterMutationTail(toks)
	if !ok || tailStart < 0 || tailStart > len(sql) {
		return nil, nil
	}
	tail := strings.TrimSpace(sql[tailStart:])
	if tail == "" {
		return nil, nil
	}
	probe := "UPDATE " + mutationProbeTable + " SET " + tail
	if mkind == alterMutationDelete {
		probe = "DELETE FROM " + mutationProbeTable + " " + tail
	}
	probeAST, err := e.ParseOne(probe)
	if err != nil {
		return nil, nil
	}
	_, probeBody, _, err := bodyOf(probeAST)
	if err != nil || probeBody == nil {
		return nil, nil
	}
	var out []StringLookup
	if assignments, ok := probeBody["set"].([]any); ok {
		for _, rawAssignment := range assignments {
			assignment, ok := rawAssignment.([]any)
			if !ok || len(assignment) != 2 {
				continue
			}
			collectStringLookupCalls(assignment[1], &out)
		}
	}
	collectStringLookupCalls(probeBody["where_clause"], &out)
	return out, nil
}
