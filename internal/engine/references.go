package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CollectDatabaseReferences returns every database name the statement
// addresses, in document order and deduplicated, across every position the
// table-reference policy governs (spec 2026-09-26 §5). See
// CollectDatabaseReferenceSets.
func CollectDatabaseReferences(e Engine, ast AST, sql string) ([]string, error) {
	all, _, err := CollectDatabaseReferenceSets(e, ast, sql)
	return all, err
}

// CollectDatabaseReferenceSets decodes the statement once and returns every
// database name it addresses (all), in document order and deduplicated, plus
// the SI-handler-blind subset (blind): the databases named only through a
// position no storage-integrity handler classifies as a table reference — a
// joinGet/dictGet/hasColumnInTable-family argument (an ordinary scalar
// function call whose argument embeds a namespace) and a parenthesized
// single-operand IN (`x IN (db.table)`, at any paren depth; the SI handlers
// classify only the bare form). PreflightTableReferences refuses a protected
// blind hit even while the storage-integrity surface is active. Unqualified
// names contribute nothing: the logical context is checked by the caller.
//
// Write targets (the statement's own CREATE/DROP/INSERT/ALTER/RENAME/TO
// target(s), CREATE/DROP DATABASE, table-function clone sources, ...) are
// collected before embedded read sources (FROM/JOIN/subquery/IN/table
// function arguments/column and ALTER-action expressions/...) so a
// statement's own target database is reported ahead of the databases it
// merely reads; opaque ALTER text and string-lookup arguments follow.
func CollectDatabaseReferenceSets(e Engine, ast AST, sql string) (all, blind []string, err error) {
	seen := map[string]bool{}
	add := func(db string) {
		if db != "" && !seen[db] {
			seen[db] = true
			all = append(all, db)
		}
	}
	blindSeen := map[string]bool{}
	addBlind := func(db string) {
		if db != "" && !blindSeen[db] {
			blindSeen[db] = true
			blind = append(blind, db)
		}
		add(db)
	}
	kind, err := NodeKind(ast)
	if err != nil {
		return nil, nil, err
	}
	if kind == NodeCommand {
		// USE / SHOW … FROM / EXISTS / SHOW CREATE / DESCRIBE / RENAME / EXCHANGE.
		if info, err := ParseDBLevel(e, sql); err == nil {
			add(info.DB)
			for _, db := range ShowBodyDatabases(e, info, sql) {
				add(db)
			}
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
		if err := addOpaqueAlterDatabases(e, ast, add); err != nil {
			return nil, nil, err
		}
		return all, blind, nil
	}
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, nil, err
	}
	targets, err := AllWriteTargets(e, ast)
	if err != nil {
		return nil, nil, err
	}
	for _, tt := range targets {
		add(tt.DB)
	}
	if err := walkStatementObjects(root, readSourceScope{}, readSourceVisitor{
		table:     func(_, _ map[string]any, tt TableTarget) { add(tt.DB) },
		inTable:   func(_ map[string]any, d namespaceRefDetail) { add(d.ref.Target.DB) },
		namespace: func(_ map[string]any, d namespaceRefDetail) { add(d.ref.Target.DB) },
	}); err != nil {
		return nil, nil, err
	}
	if err := addOpaqueAlterDatabases(e, ast, add); err != nil {
		return nil, nil, err
	}
	// String-lookup arguments are ordinary scalar function calls, not
	// source-role table functions, so the read-source visitor never emits
	// them (spec 2026-09-26 T3).
	collectStringLookupDatabases(root, addBlind)
	collectParenthesizedInDatabases(root, addBlind)
	return all, blind, nil
}

// addOpaqueAlterDatabases adds the qualifier of every qualified name in the
// statement's opaque ALTER text (spec 2026-09-26 R2), so a protected database
// named in an ALTER … UPDATE tail or a Raw ALTER action is refused with the
// protected-database message before the tail is refused as ungoverned. A
// tokenizer failure is an error the caller seals as UnsupportedStatement.
func addOpaqueAlterDatabases(e Engine, ast AST, add func(string)) error {
	texts, err := OpaqueAlterTexts(ast)
	if err != nil {
		return err
	}
	for _, text := range texts {
		dbs, ok := OpaqueTextDatabases(e, text)
		if !ok {
			return fmt.Errorf("engine: tokenize opaque ALTER text")
		}
		for _, db := range dbs {
			add(db)
		}
	}
	return nil
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

// collectParenthesizedInDatabases reports the database of every
// parenthesized single-operand IN (`x IN (db.table)`, at any paren depth),
// decoded by the shared IN-operand decoder. The storage-integrity handlers
// classify only the bare, is_field-tagged operand, so this parenthesized form
// is "SI-handler-blind": PreflightTableReferences must refuse a protected hit
// here even while the storage-integrity surface is active.
func collectParenthesizedInDatabases(node any, add func(string)) {
	switch n := node.(type) {
	case map[string]any:
		if in, ok := n["in"].(map[string]any); ok {
			if isField, _ := in["is_field"].(bool); !isField {
				if kind, detail := decodeInNodeOperand(in); kind == inOperandTable {
					add(detail.ref.Target.DB)
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
	// InSelectBody reports whether this call is reachable from a
	// select/union/intersect/except subtree (spec 2026-09-26 T6, Task 7 fix
	// round 2 mechanism (a)): CTAS's as_select, an INSERT's query, a CREATE
	// VIEW's body, and an IN subquery all qualify, since polyglot nests a
	// "select"/"union"/"intersect"/"except" key at their root. Only
	// hasColumnInTable consults this (rewriteSelectCore's rewrite pipeline
	// owns it there; everywhere else it is refused like the always-refuse
	// joinGet/dictGet family). Meaningless (always true) for a call found by
	// StringLookupCalls on an ast whose OWN root already is such a subtree —
	// which is exactly the ast rewriteSelectCore hands this package.
	InSelectBody bool
}

// StringLookupCalls returns every recognized string-form lookup call
// anywhere in ast — an ordinary scalar-expression position (SELECT list,
// WHERE, a CREATE TABLE column's DEFAULT/MATERIALIZED/ALIAS/EPHEMERAL
// expression, …), not gated by readSourceVisitor's source-role traversal —
// in the single total order collectStringLookupOccurrences walks the tree
// (SQL-clause rank among one map's keys, then key name, then array index),
// which follows document order for every clause this policy walks. Only a
// call that names a first argument at all is reported (spec 2026-09-26 T6):
// a bare `joinGet()` names nothing to refuse or rewrite.
func StringLookupCalls(ast AST) ([]StringLookup, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode string lookups: %w", err)
	}
	occurrences := collectStringLookupOccurrences(root)
	out := make([]StringLookup, len(occurrences))
	for i, o := range occurrences {
		out[i] = o.call
	}
	return out, nil
}

// stringLookupOccurrence is one matched call plus enough context to mutate
// it in place (fn/args reference the live decoded node) and to order it
// deterministically (walkIndex — the structural walk order below is already a
// single total order, so no secondary sort is applied; Task 7 fix round 3
// minor 3).
type stringLookupOccurrence struct {
	call      StringLookup
	fn        map[string]any // fn["args"] is replaced to rewrite this call
	args      []any
	walkIndex int
}

// selectBodyRootField reports which field of stmtBody — the value of root's
// single top-level "kind" key — is a genuine embedded SELECT-body-rewrite
// root, using the exact same classification ExtractInsertBody /
// ExtractCreateSelectBody / ExtractViewBody rely on: "query" for insert
// (INSERT … SELECT only — a nil query (VALUES) or a {"command":{"this":
// "FORMAT …"}} query doesn't count), "as_select" for create_table (CREATE
// TABLE … AS SELECT only), "query" for create_view. ok=false for every other
// kind, a missing field, or a field that isn't a (optionally parenthesized)
// read body — matching the set of embedded bodies rewriteEmbeddedBody's
// Extract*/Set* pairs actually splice a rewrite into (Task 7 fix round 3
// breakage 2 ruling).
func selectBodyRootField(kind string, stmtBody map[string]any) (field string, ok bool) {
	switch kind {
	case NodeInsert:
		field = "query"
	case NodeCreateTable:
		field = "as_select"
	case NodeCreateView:
		field = "query"
	default:
		return "", false
	}
	q, isMap := stmtBody[field].(map[string]any)
	if !isMap {
		return "", false
	}
	inner, _ := subqueryShells(q)
	if !isReadBody(inner) {
		return "", false
	}
	return field, true
}

// collectStringLookupOccurrences performs the ONE generic recursive walk
// mechanism (a) specifies: every map is visited (children in a SQL-clause-
// aware, otherwise alphabetically sorted order — stringLookupWalkKeys), and
// every "function" node matching IsStringLookup is recorded, in that walk
// order (a single total order: clause rank, then sorted key, then array
// index — Task 7 fix round 3 minor 3 drops the earlier span-based secondary
// sort entirely, since it wasn't a strict weak ordering and the walk order
// alone already reproduces every pinned probe).
//
// InSelectBody is elevated only at a genuine SELECT-body-rewrite root — the
// whole tree when root itself is a top-level select/union/intersect/except
// statement, or the insert/as_select/view "query" field selectBodyRootField
// names for insert/create_table/create_view — and is then sticky for every
// descendant (covering a CTE, and any IN/scalar subquery, nested arbitrarily
// deep beneath that root). A SELECT reached any other way — a structured
// UPDATE SET / DELETE WHERE / INSERT VALUES / CREATE TABLE column expression,
// or any other statement kind — never elevates, because the rewrite pipeline
// never reaches those positions to resolve a logical name there (Task 7 fix
// round 3 breakage 2: the earlier "any select/union/intersect/except key
// anywhere" detection was over-broad and let such a call pass through
// unresolved and unrefused).
func collectStringLookupOccurrences(root any) []stringLookupOccurrence {
	var out []stringLookupOccurrence
	idx := 0
	var walk func(node any, insideSelectBody bool)
	// checkAndDescend is walk's map[string]any case, factored out so the
	// top-level dispatch below can also invoke it directly on a statement's
	// own body map (recording a "function" node hanging directly off that
	// body, exactly like an ordinary recursive walk into it would) while
	// choosing per-child which single field elevates to insideSelectBody=true.
	checkAndDescend := func(n map[string]any, insideSelectBody bool, elevatedField string) {
		if fn, ok := n["function"].(map[string]any); ok {
			name, _ := fn["name"].(string)
			args, _ := fn["args"].([]any)
			if IsStringLookup(name) && len(args) > 0 {
				call := decodeStringLookupCall(name, args)
				call.InSelectBody = insideSelectBody
				out = append(out, stringLookupOccurrence{
					call: call, fn: fn, args: args, walkIndex: idx,
				})
				idx++
			}
		}
		for _, k := range stringLookupWalkKeys(n) {
			child := insideSelectBody || k == elevatedField
			walk(n[k], child)
		}
	}
	walk = func(node any, insideSelectBody bool) {
		switch n := node.(type) {
		case map[string]any:
			checkAndDescend(n, insideSelectBody, "")
		case []any:
			for _, v := range n {
				walk(v, insideSelectBody)
			}
		}
	}

	m, ok := root.(map[string]any)
	if !ok || len(m) != 1 {
		walk(root, false) // defensive fallback; never a genuine polyglot AST shape
		return out
	}
	for kind, bodyVal := range m {
		switch kind {
		case NodeSelect, NodeUnion, NodeIntersect, NodeExcept:
			walk(bodyVal, true)
		default:
			stmtBody, isMap := bodyVal.(map[string]any)
			if !isMap {
				walk(bodyVal, false)
				continue
			}
			elevatedField, _ := selectBodyRootField(kind, stmtBody)
			checkAndDescend(stmtBody, false, elevatedField)
		}
	}
	return out
}

// stringLookupClauseOrder ranks a SELECT statement's own clause field names
// by SQL grammar / textual position; collectStringLookupOccurrences visits a
// map's ranked keys first, in rank order, so a SELECT's WHERE call is reported
// before its ORDER BY call (plain alphabetical key order would reverse them).
// Every other field name (CREATE TABLE properties, an IN node's own fields, …)
// keeps alphabetical order among itself, after every ranked name.
var stringLookupClauseOrder = map[string]int{
	"with": 0, "expressions": 1, "from": 2, "joins": 3, "where_clause": 4,
	"group_by": 5, "having": 6, "qualify": 7, "windows": 8, "order_by": 9,
	"sort_by": 10, "distribute_by": 11, "cluster_by": 12, "limit": 13,
	"offset": 14, "sample": 15, "top": 16, "distinct_on": 17, "fetch": 18,
	"into": 19, "hint": 20, "lateral_views": 21, "connect": 22, "locks": 23,
}

// stringLookupWalkKeys orders n's keys for collectStringLookupOccurrences's
// walk: stringLookupClauseOrder's ranked names first (in rank order), then
// every other key alphabetically.
func stringLookupWalkKeys(n map[string]any) []string {
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, oki := stringLookupClauseOrder[keys[i]]
		rj, okj := stringLookupClauseOrder[keys[j]]
		switch {
		case oki && okj:
			return ri < rj
		case oki:
			return true
		case okj:
			return false
		default:
			return keys[i] < keys[j]
		}
	})
	return keys
}

// sortedMapKeys returns n's keys in plain alphabetical order — used by the
// non-string-lookup callers that only need a deterministic (not
// clause-aware) traversal.
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

// RewriteStringLookups mutates every string-lookup call decide accepts, in
// the same document order StringLookupCalls reports (T6 fix round 2 finding
// 1: the order calls are recorded/rewritten in must be deterministic and
// document-ish, not just stable). decide is invoked once per call, in that
// order; returning ok=false leaves that call's arguments untouched. The
// accepted replacement is the new qualified "db.table" text: for a
// single-argument call it becomes that argument's whole literal value; for
// hasColumnInTable it is split on the FIRST '.' into the physical database
// and physical table literals (buildDynamicTableName's own
// "<logical>.<table>" shape, so the table half may itself still contain a
// dot — the database half never does).
func RewriteStringLookups(ast AST, decide func(StringLookup) (string, bool)) (AST, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode string lookups: %w", err)
	}
	for _, o := range collectStringLookupOccurrences(root) {
		if replacement, ok := decide(o.call); ok {
			o.fn["args"] = applyStringLookupReplacement(o.call.Function, o.args, replacement)
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode string lookups: %w", err)
	}
	return AST(out), nil
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

// CommandTextFindings is everything PreflightTableReferences needs from
// tokenizing a `command` node's raw text exactly once (spec 2026-09-26 T5/T6,
// Task 7 fix round 2): every lookup-family call found by scanning for a name
// immediately followed by "(" (mechanism (b)), and — when the text is an
// EXISTS / SHOW CREATE / DESCRIBE statement — its verb and (when the target
// is a function call) the target's classified name and argument database
// candidates (mechanism (d), plus the DESCRIBE-function-target
// classification from fix round 1 finding 3). Sharing one Tokenize call
// between both concerns avoids tokenizing the same text twice.
type CommandTextFindings struct {
	LookupCalls          []StringLookup
	Verb                 ObjectVerb
	TargetFunctionName   string
	TargetArgDatabases   []string
	TargetIsFunctionCall bool
}

// CollectCommandTextFindings tokenizes text (a `command` node's raw SQL, or
// a {"Raw":{"sql":…}} action's text) exactly once. ok=false means the
// tokenizer failed — the caller must refuse regardless of storage-integrity
// state (T6 mechanism (b): "a tokenizer error fails closed").
func CollectCommandTextFindings(e Engine, text string) (CommandTextFindings, bool) {
	toks, err := tokenizeRaw(e, text)
	if err != nil {
		return CommandTextFindings{}, false
	}
	var f CommandTextFindings
	f.LookupCalls = lookupCallsInRawTokens(toks)
	f.Verb, f.TargetFunctionName, f.TargetArgDatabases, f.TargetIsFunctionCall = parseObjectTargetFunctionCallFromTokens(toks)
	return f, true
}

// CollectRawActionStringLookupCalls scans every {"Raw":{"sql":…}} action
// embedded anywhere in the structured AST (e.g. an ALTER TABLE … MODIFY
// COLUMN … DEFAULT … action, spec 2026-09-26 T6, Task 7 fix round 2 mechanism
// (b)) for a lookup-family call. Unlike a `command` node's own text (see
// CollectCommandTextFindings), a Raw action is never an EXISTS/SHOW
// CREATE/DESCRIBE target, so there is nothing to share a tokenize call with
// here. ok=false (fail closed) on any tokenizer failure.
func CollectRawActionStringLookupCalls(e Engine, ast AST) (calls []StringLookup, ok bool) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, false
	}
	var texts []string
	collectRawActionTexts(root, &texts)
	for _, text := range texts {
		toks, terr := tokenizeRaw(e, text)
		if terr != nil {
			return nil, false
		}
		calls = append(calls, lookupCallsInRawTokens(toks)...)
	}
	return calls, true
}

// collectRawActionTexts recursively collects every {"Raw":{"sql":"…"}}
// action's text anywhere in the decoded tree (capital "Raw" — an ALTER
// action polyglot could not structure; distinct from the lowercase top-level
// "raw" node CollectCommandTextFindings's caller handles separately via
// CommandSQL/NodeRaw).
func collectRawActionTexts(node any, out *[]string) {
	switch n := node.(type) {
	case map[string]any:
		if raw, ok := n["Raw"].(map[string]any); ok {
			if sql, ok := raw["sql"].(string); ok {
				*out = append(*out, sql)
			}
		}
		for _, k := range sortedMapKeys(n) {
			collectRawActionTexts(n[k], out)
		}
	case []any:
		for _, v := range n {
			collectRawActionTexts(v, out)
		}
	}
}

// lookupCallsInRawTokens scans an already-tokenized raw text span for every
// lookup-family name (spec 2026-09-26 T6: joinGet/dictGet-family AND
// hasColumnInTable — this opaque-text position has no SELECT-body rewrite
// pipeline of its own, so hasColumnInTable is treated exactly like the
// always-refuse family here) immediately followed by "(", in token order. The
// reported Arg mirrors decodeStringLookupCall's own convention so the
// message is identical regardless of which mechanism ((a)'s structured walk
// or (b)'s raw-text scan) found the call: hasColumnInTable's last two
// top-level arguments (its db/table pair — an optional leading
// host[, user[, pw]] shifts the index exactly like stringLookupArgDatabase
// documents) joined with '.' when both are string literals; every other
// name's first top-level argument when it is a string literal or a
// (qualified) name. Any other argument is non-literal and reports Arg ""
// (spec 2026-09-26 R10), never a token-joined rendering.
//
// A backquoted or double-quoted function name — for example a backtick-
// quoted dictGet or a double-quote-quoted joinGet immediately followed by an
// argument list — lexes as a single QUOTED_IDENTIFIER token whose .Text is
// already the fully decoded name: quotes stripped, any doubled-quote escape
// resolved to one (measured directly against the engine: a backtick-quoted
// "weird`name" with its embedded backtick doubled decodes to the Go string
// weird`name with one backtick; a double-quote-quoted "weird""name" with its
// embedded quote doubled decodes to weird"name) — so IsStringLookup is
// checked against that decoded text exactly like a plain VAR name (Task 7 fix
// round 3 new breakage 1: unconditionally skipping every QUOTED_IDENTIFIER
// let a quoted lookup name bypass this scan entirely, even though ClickHouse
// accepts a quoted identifier as a function name and runs it as a real
// lookup). Only a STRING token (a quoted string literal, never a callable
// name in ClickHouse) is still skipped outright.
func lookupCallsInRawTokens(toks []rawToken) []StringLookup {
	var out []StringLookup
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].TokenType == "STRING" {
			continue // a string literal's decoded text is never a call name
		}
		if !IsStringLookup(toks[i].Text) || toks[i+1].TokenType != "L_PAREN" {
			continue
		}
		groups, ok := rawCallArgGroups(toks, i+1)
		var arg string
		literal := false
		if ok {
			if strings.HasPrefix(strings.ToLower(toks[i].Text), "hascolumnintable") {
				if len(groups) >= 3 {
					db, dbOK := rawStringArg(groups[len(groups)-3])
					table, tableOK := rawStringArg(groups[len(groups)-2])
					if dbOK && tableOK {
						arg, literal = db+"."+table, true
					}
				}
			} else if len(groups) > 0 {
				if value, isString := rawStringArg(groups[0]); isString {
					arg, literal = value, true
				} else if name, isName := rawQualifiedNameArg(groups[0]); isName {
					arg = name
				}
			}
		}
		out = append(out, StringLookup{Function: toks[i].Text, Arg: arg, Literal: literal})
	}
	return out
}

// rawCallArgGroups splits the token stream from a call's opening "(" (at
// toks[openIdx]) into its top-level (paren-depth-0) comma-separated argument
// groups. ok=false means the call never closes (malformed input).
func rawCallArgGroups(toks []rawToken, openIdx int) (groups [][]rawToken, ok bool) {
	depth := 0
	groupStart := openIdx + 1
	for i := openIdx + 1; i < len(toks); i++ {
		switch toks[i].TokenType {
		case "L_PAREN":
			depth++
		case "R_PAREN":
			if depth == 0 {
				groups = append(groups, toks[groupStart:i])
				return groups, true
			}
			depth--
		case "COMMA":
			if depth == 0 {
				groups = append(groups, toks[groupStart:i])
				groupStart = i + 1
			}
		}
	}
	return nil, false
}

// rawStringArg decodes an argument group that is exactly one string literal.
func rawStringArg(group []rawToken) (string, bool) {
	if len(group) == 1 && group[0].TokenType == "STRING" {
		return group[0].Text, true
	}
	return "", false
}

// rawQualifiedNameArg decodes an argument group that is exactly a name or a
// dot-separated name run (`db.table`), joined with "." — the same text the
// structured path decodes for an unquoted identifier argument. Anything else
// (an expression, a call) is non-literal: the caller reports target "" on
// every path (spec 2026-09-26 R10).
func rawQualifiedNameArg(group []rawToken) (string, bool) {
	if len(group) == 0 || len(group)%2 == 0 {
		return "", false
	}
	parts := make([]string, 0, len(group)/2+1)
	for i, tok := range group {
		if i%2 == 1 {
			if tok.TokenType != "DOT" {
				return "", false
			}
			continue
		}
		if !isNameTok(tok.TokenType) {
			return "", false
		}
		parts = append(parts, tok.Text)
	}
	return strings.Join(parts, "."), true
}
