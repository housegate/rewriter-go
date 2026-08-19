package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
	ActionSkip     TableAction = iota // leave the node untouched
	ActionRename                      // set table name (+ optionally schema/db)
	ActionRemote                      // replace the table expr with remote(...)
	ActionSubquery                    // replace the table expr with a derived table (Spec G SI surface)
)

// RemoteSpec are the five positional args of a remote() table function.
type RemoteSpec struct{ Addr, DB, Table, User, Password string }

// TableDecision is what a caller returns for a TableTarget.
type TableDecision struct {
	Action   TableAction
	NewDB    string      // ActionRename: new schema; "" keeps the existing schema untouched
	NewTable string      // ActionRename: new table name
	Remote   *RemoteSpec // ActionRemote: the remote() args
	// Subquery is the derived-table body for ActionSubquery: a parsed single
	// statement ({"select":…} or {"union":…}) obtained from Engine.ParseOne.
	// The alias is the user's alias, else the original qualified name —
	// same rule as ActionRemote — so column qualifiers keep resolving.
	Subquery AST
}

// opaqueDerivedTableKey marks a derived table injected by RewriteSelectTables.
// The marker is internal JSON metadata (polyglot ignores unknown AST fields)
// that prevents a later collection/rewrite pass from treating the physical
// tables inside the injected body as additional user-authored references.
const opaqueDerivedTableKey = "_rewriter_go_opaque_derived_table"

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

// NamespaceRefSource identifies an AST surface that carries a ClickHouse
// database/table identity outside an ordinary FROM/JOIN table node.
type NamespaceRefSource string

const (
	NamespaceRefTableFunction    NamespaceRefSource = "table_function"
	NamespaceRefInTable          NamespaceRefSource = "in_table"
	NamespaceRefTableEngine      NamespaceRefSource = "table_engine"
	NamespaceRefDictionarySource NamespaceRefSource = "dictionary_source"
)

// NamespaceRef is one namespace-bearing surface that can reach a local table
// without passing through the ordinary table-name rewriter. This deliberately
// models table functions, IN/GLOBAL IN <table>, CREATE TABLE engine source
// arguments, and local CLICKHOUSE dictionary sources through one shape so
// storage-integrity policy cannot grow another per-command allow-list gap.
type NamespaceRef struct {
	Source              NamespaceRefSource
	Name                string
	Target              TableTarget
	Resolved            bool
	UsesCurrentDatabase bool
}

// TableFunctionRef is the compatibility view of one recognized ClickHouse
// table-function namespace.
// Resolved means both database and table arguments are statically known. When
// only the database is known, Target.DB is preserved while Resolved stays false;
// policy can then reserve a protocol-owned database even if the table argument
// is an expression. A fully dynamic database leaves Target.DB empty and must be
// treated conservatively by storage-integrity policy.
type TableFunctionRef struct {
	Target              TableTarget
	Resolved            bool
	UsesCurrentDatabase bool // one-argument merge(<table-regexp>) overload
}

// CollectNamespaceRefs returns all AST surfaces that carry a database/table
// identity outside normal table nodes. The function families are derived from
// ClickHouse's local-catalog table functions: remote/cluster, merge/loop,
// mergeTree* inspection functions, TimeSeries/Prometheus functions, and
// dictionary. Prefix handling for mergeTree* intentionally covers newly added
// inspection functions such as mergeTreeCodecBlockCounts without a brittle
// one-name patch.
func CollectNamespaceRefs(ast AST) ([]NamespaceRef, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode namespace references: %w", err)
	}
	var out []NamespaceRef
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			if fn, ok := n["function"].(map[string]any); ok {
				if ref, ok := decodeNamespaceFunctionRef(fn); ok {
					out = append(out, ref)
				}
			}
			if in, ok := n["in"].(map[string]any); ok {
				if ref, ok := decodeInNamespaceRef(in); ok {
					out = append(out, ref)
				}
			}
			if property, ok := n["engine_property"].(map[string]any); ok {
				if ref, ok := decodeTableEngineNamespaceRef(property); ok {
					out = append(out, ref)
				}
			}
			if property, ok := n["dict_property"].(map[string]any); ok {
				if ref, ok := decodeDictionarySourceNamespaceRef(property); ok {
					out = append(out, ref)
				}
			}
			for _, child := range n {
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(root)
	return out, nil
}

// CollectTableFunctionRefs returns every recognized remote/cluster/merge
// function, including partially or wholly unresolved namespace arguments.
func CollectTableFunctionRefs(ast AST) ([]TableFunctionRef, error) {
	refs, err := CollectNamespaceRefs(ast)
	if err != nil {
		return nil, err
	}
	var out []TableFunctionRef
	for _, ref := range refs {
		if ref.Source != NamespaceRefTableFunction {
			continue
		}
		out = append(out, TableFunctionRef{
			Target: ref.Target, Resolved: ref.Resolved,
			UsesCurrentDatabase: ref.UsesCurrentDatabase,
		})
	}
	return out, nil
}

// CollectTableFunctionTargets is the compatibility view used by ordinary
// callers that only need fully resolved physical database/table pairs.
func CollectTableFunctionTargets(ast AST) ([]TableTarget, error) {
	refs, err := CollectTableFunctionRefs(ast)
	if err != nil {
		return nil, err
	}
	var out []TableTarget
	for _, ref := range refs {
		if ref.Resolved {
			out = append(out, ref.Target)
		}
	}
	return out, nil
}

func decodeNamespaceFunctionRef(fn map[string]any) (NamespaceRef, bool) {
	name, _ := fn["name"].(string)
	args, _ := fn["args"].([]any)
	lower := strings.ToLower(name)
	switch lower {
	case "in", "notin", "nullin", "notnullin", "globalin", "globalnotin", "globalnullin", "globalnotnullin":
		return decodeCallableInNamespaceRef(lower, args)
	case "remote", "remotesecure", "cluster", "clusterallreplicas":
		return decodeNamespacePair(NamespaceRefTableFunction, name, args, 1), true
	case "merge":
		if len(args) == 1 {
			return decodeNamespaceSingle(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePair(NamespaceRefTableFunction, name, args, 0), true
	case "loop", "dictionary":
		if len(args) == 1 {
			return decodeNamespaceSingle(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePair(NamespaceRefTableFunction, name, args, 0), true
	case "timeseriesdata", "timeseriestags", "timeseriesmetrics":
		if len(args) == 1 {
			return decodeNamespaceSingle(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePair(NamespaceRefTableFunction, name, args, 0), true
	case "timeseriesselector":
		if len(args) == 4 && len(args) > 0 {
			return decodeNamespaceSingle(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePair(NamespaceRefTableFunction, name, args, 0), true
	case "prometheusquery":
		if len(args) == 3 && len(args) > 0 {
			return decodeNamespaceSingle(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePair(NamespaceRefTableFunction, name, args, 0), true
	case "prometheusqueryrange":
		if len(args) == 5 && len(args) > 0 {
			return decodeNamespaceSingle(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePair(NamespaceRefTableFunction, name, args, 0), true
	}
	if strings.HasPrefix(lower, "mergetree") {
		return decodeNamespacePair(NamespaceRefTableFunction, name, args, 0), true
	}
	return NamespaceRef{}, false
}

func decodeCallableInNamespaceRef(name string, args []any) (NamespaceRef, bool) {
	if len(args) != 2 || !isNamespaceIdentifierArg(args[1]) {
		return NamespaceRef{}, false
	}
	display := map[string]string{
		"in": "IN", "notin": "NOT IN", "nullin": "NULL IN", "notnullin": "NOT NULL IN",
		"globalin": "GLOBAL IN", "globalnotin": "GLOBAL NOT IN", "globalnullin": "GLOBAL NULL IN", "globalnotnullin": "GLOBAL NOT NULL IN",
	}[name]
	ref := decodeNamespaceSingle(NamespaceRefInTable, display, args[1])
	if ref.Target.Table == "" && !ref.Resolved {
		return NamespaceRef{}, false
	}
	return ref, true
}

func isNamespaceIdentifierArg(arg any) bool {
	m, ok := arg.(map[string]any)
	if !ok {
		return false
	}
	_, column := m["column"]
	_, dot := m["dot"]
	return column || dot
}

func decodeInNamespaceRef(in map[string]any) (NamespaceRef, bool) {
	isField, _ := in["is_field"].(bool)
	exprs, _ := in["expressions"].([]any)
	if !isField || len(exprs) != 1 {
		return NamespaceRef{}, false
	}
	name := "IN"
	if not, _ := in["not"].(bool); not {
		name = "NOT IN"
	}
	if global, _ := in["global"].(bool); global {
		name = "GLOBAL " + name
	}
	ref := decodeNamespaceSingle(NamespaceRefInTable, name, exprs[0])
	if ref.Target.Table == "" && !ref.Resolved {
		return NamespaceRef{}, false
	}
	return ref, true
}

func decodeTableEngineNamespaceRef(property map[string]any) (NamespaceRef, bool) {
	outer, _ := property["this"].(map[string]any)
	anon, _ := outer["anonymous"].(map[string]any)
	nameHolder, _ := anon["this"].(map[string]any)
	name := identName(nameHolder["identifier"])
	args, _ := anon["expressions"].([]any)
	var first int
	switch strings.ToLower(name) {
	case "remote", "distributed":
		first = 1
	case "merge", "buffer":
		first = 0
	default:
		return NamespaceRef{}, false
	}
	return decodeNamespacePair(NamespaceRefTableEngine, name, args, first), true
}

func decodeDictionarySourceNamespaceRef(property map[string]any) (NamespaceRef, bool) {
	propertyName, _ := property["this"].(map[string]any)
	if !strings.EqualFold(identName(propertyName["identifier"]), "SOURCE") {
		return NamespaceRef{}, false
	}
	kind, _ := property["kind"].(string)
	if !strings.EqualFold(kind, "CLICKHOUSE") {
		return NamespaceRef{}, false
	}
	ref := NamespaceRef{Source: NamespaceRefDictionarySource, Name: "CLICKHOUSE"}
	settings, _ := property["settings"].(map[string]any)
	tuple, _ := settings["tuple"].(map[string]any)
	pairs, _ := tuple["expressions"].([]any)
	var databaseArg, tableArg any
	hasOpaqueSetting := false
	for _, rawPair := range pairs {
		pair, _ := rawPair.(map[string]any)
		body, _ := pair["tuple"].(map[string]any)
		expressions, _ := body["expressions"].([]any)
		if len(expressions) != 2 {
			continue
		}
		keyHolder, _ := expressions[0].(map[string]any)
		key := strings.ToUpper(identName(keyHolder["identifier"]))
		switch key {
		case "DB", "DATABASE":
			databaseArg = expressions[1]
		case "TABLE":
			tableArg = expressions[1]
		case "QUERY", "WHERE", "INVALIDATE_QUERY", "NAME":
			// SQL-bearing filters/probes and named collections can override or
			// extend DB/TABLE. Even when those two fields are constant, the final
			// execution namespace is not proven and SI policy must fail closed.
			hasOpaqueSetting = true
		}
	}
	if hasOpaqueSetting {
		if databaseArg != nil {
			ref = decodeNamespacePair(
				NamespaceRefDictionarySource, "CLICKHOUSE", []any{databaseArg, tableArg}, 0)
			ref.Resolved = false
			ref.UsesCurrentDatabase = false
			return ref, true
		}
		if tableArg != nil {
			ref.Target.Table, _ = tableFunctionArgText(tableArg)
		}
		return ref, true
	}
	if databaseArg == nil && tableArg == nil {
		return ref, true
	}
	if databaseArg == nil {
		ref = decodeNamespaceSingle(NamespaceRefDictionarySource, "CLICKHOUSE", tableArg)
		// A dynamic TABLE expression still executes in the current DB.
		ref.UsesCurrentDatabase = true
		return ref, true
	}
	return decodeNamespacePair(NamespaceRefDictionarySource, "CLICKHOUSE", []any{databaseArg, tableArg}, 0), true
}

func decodeNamespaceSingle(source NamespaceRefSource, name string, arg any) NamespaceRef {
	ref := NamespaceRef{Source: source, Name: name, UsesCurrentDatabase: true}
	if isCurrentDatabaseArg(arg) {
		ref.UsesCurrentDatabase = true
		return ref
	}
	value, ok := tableFunctionArgText(arg)
	if !ok {
		return ref
	}
	if db, table, qualified := exactFunctionQualified(value); qualified {
		ref.Target = TableTarget{DB: db, Table: table}
		ref.Resolved = true
		ref.UsesCurrentDatabase = false
		return ref
	}
	ref.Target.Table = value
	return ref
}

func decodeNamespacePair(source NamespaceRefSource, name string, args []any, first int) NamespaceRef {
	ref := NamespaceRef{Source: source, Name: name}
	if first >= len(args) {
		return ref
	}
	if isCurrentDatabaseArg(args[first]) {
		ref.UsesCurrentDatabase = true
		if first+1 < len(args) {
			ref.Target.Table, _ = tableFunctionArgText(args[first+1])
		}
		return ref
	}
	firstArg, firstOK := tableFunctionArgText(args[first])
	if firstOK {
		if db, table, qualified := exactFunctionQualified(firstArg); qualified {
			ref.Target = TableTarget{DB: db, Table: table}
			ref.Resolved = true
			return ref
		}
		ref.Target.DB = firstArg
	}
	if first+1 >= len(args) {
		return ref
	}
	secondArg, secondOK := tableFunctionArgText(args[first+1])
	if secondOK {
		ref.Target.Table = secondArg
	}
	ref.Resolved = firstOK && secondOK && firstArg != "" && secondArg != ""
	return ref
}

func isCurrentDatabaseArg(arg any) bool {
	m, ok := arg.(map[string]any)
	if !ok {
		return false
	}
	fn, ok := m["function"].(map[string]any)
	if !ok {
		return false
	}
	name, _ := fn["name"].(string)
	return strings.EqualFold(name, "currentDatabase")
}

// CollectEmbeddedSelectSources returns the table nodes and recognized table
// functions inside top-level SELECT/set-operation bodies nested under a write
// AST. Write targets outside those bodies are deliberately excluded.
func CollectEmbeddedSelectSources(ast AST) ([]TableTarget, []TableFunctionRef, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, nil, fmt.Errorf("engine: decode embedded SELECT: %w", err)
	}
	var tables []TableTarget
	var functions []TableFunctionRef
	var walk func(any) error
	walk = func(node any) error {
		switch n := node.(type) {
		case map[string]any:
			if isReadQueryRoot(n) {
				encoded, err := json.Marshal(n)
				if err != nil {
					return fmt.Errorf("engine: encode embedded SELECT: %w", err)
				}
				gotTables, err := CollectSelectTables(AST(encoded))
				if err != nil {
					return err
				}
				gotFunctions, err := CollectTableFunctionRefs(AST(encoded))
				if err != nil {
					return err
				}
				tables = append(tables, gotTables...)
				functions = append(functions, gotFunctions...)
				return nil // the read visitor already recurses into nested subqueries
			}
			for _, child := range n {
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range n {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, nil, err
	}
	return tables, functions, nil
}

func isReadQueryRoot(node map[string]any) bool {
	for _, kind := range []string{NodeSelect, NodeUnion, NodeIntersect, NodeExcept} {
		if _, ok := node[kind]; ok {
			return true
		}
	}
	return false
}

func tableFunctionArgText(arg any) (string, bool) {
	m, ok := arg.(map[string]any)
	if !ok {
		return "", false
	}
	if lit, ok := m["literal"].(map[string]any); ok {
		value, ok := lit["value"].(string)
		return value, ok && value != ""
	}
	if col, ok := m["column"].(map[string]any); ok {
		name := identName(col["name"])
		if name == "" {
			return "", false
		}
		if table := identName(col["table"]); table != "" {
			return table + "." + name, true
		}
		return name, true
	}
	if dot, ok := m["dot"].(map[string]any); ok {
		left, lok := tableFunctionArgText(dot["this"])
		right := identName(dot["field"])
		if lok && right != "" {
			return left + "." + right, true
		}
	}
	if name := identName(m); name != "" {
		return name, true
	}
	return "", false
}

func exactFunctionQualified(s string) (db, table string, ok bool) {
	if strings.Count(s, ".") != 1 {
		return "", "", false
	}
	dot := strings.IndexByte(s, '.')
	db, table = s[:dot], s[dot+1:]
	return db, table, db != "" && table != ""
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
		if subquery, ok := n["subquery"].(map[string]any); ok {
			if opaque, _ := subquery[opaqueDerivedTableKey].(bool); opaque {
				return
			}
		}
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
	case ActionSubquery:
		if len(d.Subquery) == 0 {
			return // misconfigured decision — leave the table untouched
		}
		var body any
		if err := json.Unmarshal(d.Subquery, &body); err != nil {
			return
		}
		aliasName := tt.Alias
		if aliasName == "" {
			aliasName = originName(tt)
		}
		delete(expr, "table")
		// Shape mirrors what polyglot emits for `FROM (SELECT …) AS x`
		// (see testdata/ast-shapes/select_subquery_from.json).
		expr["subquery"] = map[string]any{
			"this":                body,
			"alias":               ident(aliasName),
			"alias_explicit_as":   true,
			"alias_keyword":       "AS",
			"column_aliases":      []any{},
			"lateral":             false,
			"limit":               nil,
			"modifiers_inside":    false,
			"offset":              nil,
			"order_by":            nil,
			"trailing_comments":   []any{},
			opaqueDerivedTableKey: true,
		}
	case ActionSkip:
		// no-op
	}
}

// ReferencesIdentifier reports whether any column reference in the AST has
// the final name part `name` (bare `_hg_row_id`, qualified `t._hg_row_id`,
// in select list / WHERE / ORDER BY / function args / subqueries), or any
// `* EXCEPT|REPLACE|RENAME (...)` entry names it. String literals never
// match. Used for the Spec G reserved-column guard.
func ReferencesIdentifier(ast AST, name string) (bool, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return false, fmt.Errorf("engine: decode: %w", err)
	}
	return refWalk(root, name), nil
}

// QuoteIdentifier forces Identifier-shaped nodes named name to render quoted.
// Polyglot's parser normalizes some quoted ClickHouse keywords (for example
// `from` inside star EXCEPT) back to quoted=false, so trusted synthesized ASTs
// must restore the structural quote before generation. Callers should use this
// only on a generated fragment whose identifier roles they control.
func QuoteIdentifier(ast AST, name string) (AST, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode: %w", err)
	}
	quoteIdentifierWalk(root, name)
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode: %w", err)
	}
	return AST(out), nil
}

func quoteIdentifierWalk(node any, name string) {
	switch n := node.(type) {
	case map[string]any:
		if got, ok := n["name"].(string); ok && got == name {
			n["quoted"] = true
		}
		for _, v := range n {
			quoteIdentifierWalk(v, name)
		}
	case []any:
		for _, v := range n {
			quoteIdentifierWalk(v, name)
		}
	}
}

// UnsupportedTableWrapperTargets returns only the table expressions whose
// wrappers carry semantics ActionSubquery cannot preserve: FINAL, SAMPLE, or an
// alias column list. Keeping the target attached to the finding lets callers
// reject an SI wrapper without falsely rejecting an ordinary JOIN peer's wrapper.
// WITH OFFSET is lost by Polyglot during parse and is token-checked separately.
func UnsupportedTableWrapperTargets(ast AST) ([]TableTarget, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode: %w", err)
	}
	var out []TableTarget
	appendUnique := func(tt TableTarget) {
		for _, existing := range out {
			if existing == tt {
				return
			}
		}
		out = append(out, tt)
	}
	visitTables(root, nil, func(_ map[string]any, tbl map[string]any, tt TableTarget) {
		final, _ := tbl["final_"].(bool)
		sampled := tbl["table_sample"] != nil
		aliases, _ := tbl["column_aliases"].([]any)
		if final || sampled || len(aliases) > 0 {
			appendUnique(tt)
		}
	})
	collectSelectLevelSampleTargets(root, appendUnique)
	return out, nil
}

// HasUnsupportedTableWrapper is retained as the coarse compatibility helper;
// new policy code should use UnsupportedTableWrapperTargets.
func HasUnsupportedTableWrapper(ast AST) (bool, error) {
	targets, err := UnsupportedTableWrapperTargets(ast)
	return len(targets) > 0, err
}

func collectSelectLevelSampleTargets(node any, appendTarget func(TableTarget)) {
	switch n := node.(type) {
	case map[string]any:
		if sel, ok := n["select"].(map[string]any); ok && sel["sample"] != nil {
			if from, ok := sel["from"].(map[string]any); ok {
				found := false
				visitTables(from, nil, func(_ map[string]any, _ map[string]any, tt TableTarget) {
					if !found {
						appendTarget(tt)
						found = true
					}
				})
			}
		}
		for _, v := range n {
			collectSelectLevelSampleTargets(v, appendTarget)
		}
	case []any:
		for _, v := range n {
			collectSelectLevelSampleTargets(v, appendTarget)
		}
	}
}

// HasWithOffset recognizes the actual WITH OFFSET keyword pair from the lexer.
// Whitespace is irrelevant because tokens are adjacent, while string literals
// and comments cannot false-positive because their token types are not WITH and
// OFFSET.
func HasWithOffset(e Engine, sql string) (bool, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return false, err
	}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].TokenType == "WITH" && toks[i+1].TokenType == "OFFSET" {
			return true, nil
		}
	}
	return false, nil
}

// WithOffsetTargets binds each real WITH OFFSET keyword pair to the nearest
// preceding FROM/JOIN table name in the token stream. This keeps wrapper policy
// attached to the table that owns the modifier instead of rejecting an entire
// mixed SI/ordinary SELECT. String literals and comments cannot participate
// because only keyword token types are considered.
func WithOffsetTargets(e Engine, sql string) ([]TableTarget, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return nil, err
	}
	var out []TableTarget
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].TokenType != "WITH" || toks[i+1].TokenType != "OFFSET" {
			continue
		}
		boundary := -1
		nesting := 0
		for j := i - 1; j >= 0; j-- {
			if toks[j].Text == ")" {
				nesting++
				continue
			}
			if toks[j].Text == "(" && nesting > 0 {
				nesting--
				continue
			}
			if nesting == 0 && (toks[j].TokenType == "FROM" || toks[j].TokenType == "JOIN" || toks[j].TokenType == "COMMA") {
				boundary = j
				break
			}
		}
		if boundary < 0 {
			continue
		}
		if target, ok := rawTokenTableTarget(toks, boundary+1); ok {
			out = append(out, target)
		}
	}
	return out, nil
}

func refWalk(node any, name string) bool {
	switch n := node.(type) {
	case map[string]any:
		// Every Polyglot Identifier-shaped node (columns, aliases, CTE names,
		// table aliases, star rename pairs, etc.) carries name+quoted. The SI
		// contract rejects ANY user identifier equal to the reserved RID, not
		// just column.name, so inspect that shape before role-specific fallbacks.
		if got, ok := n["name"].(string); ok && got == name {
			if _, identifierShape := n["quoted"]; identifierShape {
				return true
			}
		}
		if col, ok := n["column"].(map[string]any); ok && identName(col["name"]) == name {
			return true
		}
		if dot, ok := n["dot"].(map[string]any); ok && identName(dot["field"]) == name {
			return true
		}
		if using, ok := n["using"].([]any); ok {
			for _, e := range using {
				if identName(e) == name {
					return true
				}
			}
		}
		if star, ok := n["star"].(map[string]any); ok {
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
					pair, ok := e.([]any)
					if ok {
						for _, side := range pair {
							if identName(side) == name {
								return true
							}
						}
					}
				}
			}
		}
		for _, v := range n {
			if refWalk(v, name) {
				return true
			}
		}
	case []any:
		for _, v := range n {
			if refWalk(v, name) {
				return true
			}
		}
	}
	return false
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
