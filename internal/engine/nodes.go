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

// opaqueIdentifierParameterKey marks a synthetic parser probe identifier that
// stands in for an otherwise unsupported {x:Identifier} alias. It is retained
// only in the in-memory AST used for read-source collection and must never be
// treated as a concrete alias or namespace component.
const opaqueIdentifierParameterKey = "_rewriter_go_opaque_identifier_parameter"

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
	if err := walkStatementObjects(root, readSourceScope{}, readSourceVisitor{
		table: func(_, _ map[string]any, tt TableTarget) { out = append(out, tt) },
	}); err != nil {
		return nil, err
	}
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
	databaseIdentifier  bool
	tableIdentifier     bool
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

// ReadSourceKind distinguishes ordinary table nodes, recognized table
// functions reached through a real FROM/JOIN source role, and parser-proven
// IN/GLOBAL IN table operands. Arbitrary scalar arguments are not read sources.
type ReadSourceKind uint8

const (
	ReadSourceTable ReadSourceKind = iota + 1
	ReadSourceTableFunction
	ReadSourceInTable
)

// ReadSourceRef is the ordered union of ordinary tables, source-role table
// functions, and IN-table operands found in embedded SELECT/set-operation
// bodies. The origin flags are intentionally package-private: D2 uses them to
// normalize parser-preserved identifier escapes without ever decoding an
// already-semantic string literal a second time.
type ReadSourceRef struct {
	Kind                ReadSourceKind
	Target              TableTarget
	Resolved            bool
	UsesCurrentDatabase bool
	databaseIdentifier  bool
	tableIdentifier     bool
}

type namespaceValueOrigin uint8

const (
	namespaceValueUnknown namespaceValueOrigin = iota
	namespaceValueLiteral
	namespaceValueIdentifier
)

type namespaceRefDetail struct {
	ref            NamespaceRef
	databaseOrigin namespaceValueOrigin
	tableOrigin    namespaceValueOrigin
}

func (detail namespaceRefDetail) refWithOrigins() NamespaceRef {
	ref := detail.ref
	ref.databaseIdentifier = ref.databaseIdentifier || detail.databaseOrigin == namespaceValueIdentifier
	ref.tableIdentifier = ref.tableIdentifier || detail.tableOrigin == namespaceValueIdentifier
	return ref
}

// CollectNamespaceRefs returns all AST surfaces that carry a database/table
// identity outside normal table nodes. The function families are derived from
// ClickHouse's local-catalog table functions: remote/cluster, merge/loop,
// mergeTree* inspection functions, TimeSeries/Prometheus functions, and
// dictionary. Prefix handling for mergeTree* intentionally covers newly added
// inspection functions such as mergeTreeCodecBlockCounts without a brittle
// one-name patch. The shared statement walker carries forked CTE scope into
// every SELECT block: a bare IN target bound to an in-scope CTE is not a
// namespace reference, while a qualified target and an unbound bare target
// remain real references (Spec I D7b).
func CollectNamespaceRefs(ast AST) ([]NamespaceRef, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode namespace references: %w", err)
	}
	var out []NamespaceRef
	if err := walkStatementObjects(root, readSourceScope{}, readSourceVisitor{
		namespace: func(_ map[string]any, detail namespaceRefDetail) {
			out = append(out, detail.refWithOrigins())
		},
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// CollectTableFunctionRefs returns recognized source-role table functions and
// explicit INSERT FUNCTION targets, including partially or wholly unresolved
// namespace arguments. Scalar functions with the same spelling are excluded.
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
	detail, ok := decodeNamespaceFunctionRefDetail(fn)
	return detail.ref, ok
}

func decodeNamespaceFunctionRefDetail(fn map[string]any) (namespaceRefDetail, bool) {
	name, _ := fn["name"].(string)
	args, _ := fn["args"].([]any)
	lower := strings.ToLower(name)
	if canonical, ok := canonicalCallableInName(lower); ok {
		return decodeCallableInNamespaceRefDetail(canonical, args)
	}
	switch lower {
	case "remote", "remotesecure", "cluster", "clusterallreplicas":
		return decodeNamespacePairDetail(NamespaceRefTableFunction, name, args, 1), true
	case "merge":
		if len(args) == 1 {
			return decodeNamespaceSingleDetail(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePairDetail(NamespaceRefTableFunction, name, args, 0), true
	case "loop", "dictionary":
		if len(args) == 1 {
			return decodeNamespaceSingleDetail(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePairDetail(NamespaceRefTableFunction, name, args, 0), true
	case "timeseriesdata", "timeseriestags", "timeseriesmetrics":
		if len(args) == 1 {
			return decodeNamespaceSingleDetail(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePairDetail(NamespaceRefTableFunction, name, args, 0), true
	case "timeseriesselector":
		if len(args) == 4 && len(args) > 0 {
			return decodeNamespaceSingleDetail(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePairDetail(NamespaceRefTableFunction, name, args, 0), true
	case "prometheusquery":
		if len(args) == 3 && len(args) > 0 {
			return decodeNamespaceSingleDetail(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePairDetail(NamespaceRefTableFunction, name, args, 0), true
	case "prometheusqueryrange":
		if len(args) == 5 && len(args) > 0 {
			return decodeNamespaceSingleDetail(NamespaceRefTableFunction, name, args[0]), true
		}
		return decodeNamespacePairDetail(NamespaceRefTableFunction, name, args, 0), true
	}
	if strings.HasPrefix(lower, "mergetree") {
		return decodeNamespacePairDetail(NamespaceRefTableFunction, name, args, 0), true
	}
	return namespaceRefDetail{}, false
}

func canonicalCallableInName(name string) (string, bool) {
	// ClickHouse registers the IgnoreSet implementations as callable aliases of
	// the same IN family. Normalize that implementation suffix before applying
	// the namespace-target policy so every alias follows one recognition path.
	canonical := strings.TrimSuffix(name, "ignoreset")
	switch canonical {
	case "in", "notin", "nullin", "notnullin", "globalin", "globalnotin", "globalnullin", "globalnotnullin":
		return canonical, true
	default:
		return "", false
	}
}

func decodeCallableInNamespaceRefDetail(name string, args []any) (namespaceRefDetail, bool) {
	if len(args) != 2 || !isNamespaceIdentifierArg(args[1]) {
		return namespaceRefDetail{}, false
	}
	display := map[string]string{
		"in": "IN", "notin": "NOT IN", "nullin": "NULL IN", "notnullin": "NOT NULL IN",
		"globalin": "GLOBAL IN", "globalnotin": "GLOBAL NOT IN", "globalnullin": "GLOBAL NULL IN", "globalnotnullin": "GLOBAL NOT NULL IN",
	}[name]
	detail := decodeNamespaceSingleDetail(NamespaceRefInTable, display, args[1])
	if detail.ref.Target.Table == "" && !detail.ref.Resolved {
		return namespaceRefDetail{}, false
	}
	return detail, true
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
	detail, ok := decodeInNamespaceRefDetail(in)
	return detail.refWithOrigins(), ok
}

func decodeInNamespaceRefDetail(in map[string]any) (namespaceRefDetail, bool) {
	isField, _ := in["is_field"].(bool)
	exprs, _ := in["expressions"].([]any)
	if !isField || len(exprs) != 1 {
		return namespaceRefDetail{}, false
	}
	name := "IN"
	if not, _ := in["not"].(bool); not {
		name = "NOT IN"
	}
	if global, _ := in["global"].(bool); global {
		name = "GLOBAL " + name
	}
	detail := decodeNamespaceSingleDetail(NamespaceRefInTable, name, exprs[0])
	if detail.ref.Target.Table == "" && !detail.ref.Resolved {
		return namespaceRefDetail{}, false
	}
	return detail, true
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
	return decodeNamespaceSingleDetail(source, name, arg).refWithOrigins()
}

func decodeNamespaceSingleDetail(source NamespaceRefSource, name string, arg any) namespaceRefDetail {
	detail := namespaceRefDetail{ref: NamespaceRef{Source: source, Name: name, UsesCurrentDatabase: true}}
	if isCurrentDatabaseArg(arg) {
		detail.ref.UsesCurrentDatabase = true
		return detail
	}
	value, origin, ok := tableFunctionArgValue(arg)
	if !ok {
		return detail
	}
	if db, table, qualified := exactFunctionQualified(value); qualified {
		detail.ref.Target = TableTarget{DB: db, Table: table}
		detail.ref.Resolved = true
		detail.ref.UsesCurrentDatabase = false
		detail.databaseOrigin = origin
		detail.tableOrigin = origin
		return detail
	}
	detail.ref.Target.Table = value
	detail.tableOrigin = origin
	return detail
}

func decodeNamespacePair(source NamespaceRefSource, name string, args []any, first int) NamespaceRef {
	return decodeNamespacePairDetail(source, name, args, first).refWithOrigins()
}

func decodeNamespacePairDetail(source NamespaceRefSource, name string, args []any, first int) namespaceRefDetail {
	detail := namespaceRefDetail{ref: NamespaceRef{Source: source, Name: name}}
	if first >= len(args) {
		return detail
	}
	if isCurrentDatabaseArg(args[first]) {
		detail.ref.UsesCurrentDatabase = true
		if first+1 < len(args) {
			detail.ref.Target.Table, detail.tableOrigin, _ = tableFunctionArgValue(args[first+1])
		}
		return detail
	}
	firstArg, firstOrigin, firstOK := tableFunctionArgValue(args[first])
	if firstOK {
		if db, table, qualified := exactFunctionQualified(firstArg); qualified {
			detail.ref.Target = TableTarget{DB: db, Table: table}
			detail.ref.Resolved = true
			detail.databaseOrigin = firstOrigin
			detail.tableOrigin = firstOrigin
			return detail
		}
		detail.ref.Target.DB = firstArg
		detail.databaseOrigin = firstOrigin
	}
	if first+1 >= len(args) {
		return detail
	}
	secondArg, secondOrigin, secondOK := tableFunctionArgValue(args[first+1])
	if secondOK {
		detail.ref.Target.Table = secondArg
		detail.tableOrigin = secondOrigin
	}
	detail.ref.Resolved = firstOK && secondOK && firstArg != "" && secondArg != ""
	return detail
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

// CollectEmbeddedReadSources returns one ordered, role-aware union of ordinary
// table nodes, source-role table functions, and parser-proven IN-table operands
// inside SELECT/set-operation bodies. Write targets outside those bodies are
// excluded. A table function is a source only through FROM/JOIN; arbitrary
// scalar projection/predicate arguments are never promoted to table references.
func CollectEmbeddedReadSources(ast AST) ([]ReadSourceRef, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode embedded SELECT sources: %w", err)
	}
	var out []ReadSourceRef
	err := walkStatementObjects(root, readSourceScope{}, readSourceVisitor{
		table: func(_, _ map[string]any, target TableTarget) {
			out = append(out, ReadSourceRef{
				Kind: ReadSourceTable, Target: target, Resolved: true,
				databaseIdentifier: target.DB != "", tableIdentifier: true,
			})
		},
		function: func(_ map[string]any, detail namespaceRefDetail) {
			out = append(out, ReadSourceRef{
				Kind: ReadSourceTableFunction, Target: detail.ref.Target,
				Resolved: detail.ref.Resolved, UsesCurrentDatabase: detail.ref.UsesCurrentDatabase,
				databaseIdentifier: detail.databaseOrigin == namespaceValueIdentifier,
				tableIdentifier:    detail.tableOrigin == namespaceValueIdentifier,
			})
		},
		inTable: func(_ map[string]any, detail namespaceRefDetail) {
			out = append(out, ReadSourceRef{
				Kind: ReadSourceInTable, Target: detail.ref.Target,
				Resolved: detail.ref.Resolved, UsesCurrentDatabase: detail.ref.UsesCurrentDatabase,
				databaseIdentifier: detail.databaseOrigin == namespaceValueIdentifier,
				tableIdentifier:    detail.tableOrigin == namespaceValueIdentifier,
			})
		},
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CollectEmbeddedSelectSources is the compatibility split view for ordinary
// tables and table functions. IN-table events remain in the ordered union only.
func CollectEmbeddedSelectSources(ast AST) ([]TableTarget, []TableFunctionRef, error) {
	refs, err := CollectEmbeddedReadSources(ast)
	if err != nil {
		return nil, nil, err
	}
	var tables []TableTarget
	var functions []TableFunctionRef
	for _, ref := range refs {
		switch ref.Kind {
		case ReadSourceTable:
			tables = append(tables, ref.Target)
		case ReadSourceTableFunction:
			functions = append(functions, TableFunctionRef{
				Target: ref.Target, Resolved: ref.Resolved,
				UsesCurrentDatabase: ref.UsesCurrentDatabase,
			})
		}
	}
	return tables, functions, nil
}

type readSourceVisitor struct {
	table     func(expr, table map[string]any, target TableTarget)
	function  func(function map[string]any, detail namespaceRefDetail)
	inTable   func(expression map[string]any, detail namespaceRefDetail)
	namespace func(expression map[string]any, detail namespaceRefDetail)
}

type readSourceScope struct {
	ctes    map[string]bool
	aliases map[string]bool
}

// walkStatementObjects is the sole statement-level dispatcher behind the
// ordered table, read-source, namespace, and rewrite projections.
func walkStatementObjects(node any, scope readSourceScope, visitor readSourceVisitor) error {
	if node == nil {
		return nil
	}
	if handled, err := walkReadQuery(node, scope, visitor); handled {
		return err
	}

	switch n := node.(type) {
	case []any:
		for _, child := range n {
			if err := walkStatementObjects(child, scope, visitor); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		switch {
		case statementMap(n, NodeInsert) != nil:
			return walkInsertObjects(statementMap(n, NodeInsert), scope, visitor)
		case statementMap(n, NodeCreateTable) != nil:
			return walkCreateTableObjects(statementMap(n, NodeCreateTable), scope, visitor)
		case statementMap(n, NodeCreateView) != nil:
			body := statementMap(n, NodeCreateView)
			if err := walkExpression(body["query"], scope, visitor); err != nil {
				return err
			}
			if err := walkExpression(body["options"], scope, visitor); err != nil {
				return err
			}
			return walkCreateProperties(body["table_properties"], scope, visitor)
		case statementMap(n, NodeAlterTable) != nil:
			body := statementMap(n, NodeAlterTable)
			if err := walkExpression(body["actions"], scope, visitor); err != nil {
				return err
			}
			return walkExpression(body["partition"], scope, visitor)
		case statementMap(n, NodeDelete) != nil:
			return walkDeleteObjects(statementMap(n, NodeDelete), scope, visitor)
		case statementMap(n, NodeUpdate) != nil:
			return walkUpdateObjects(statementMap(n, NodeUpdate), scope, visitor)
		case statementMap(n, NodeCopy) != nil:
			body := statementMap(n, NodeCopy)
			if err := walkExpression(body["this"], scope, visitor); err != nil {
				return err
			}
			if err := walkExpression(body["files"], scope, visitor); err != nil {
				return err
			}
			return walkExpression(body["params"], scope, visitor)
		case n[NodeRaw] != nil, n[NodeCommand] != nil, n[NodeDropTable] != nil,
			n[NodeDropView] != nil, n[NodeTruncate] != nil, n[NodeCreateDB] != nil,
			n[NodeDropDB] != nil:
			return nil
		default:
			return rejectUnknownReadCarrier(n, "statement")
		}
	default:
		return nil
	}
}

func statementMap(node map[string]any, kind string) map[string]any {
	body, _ := node[kind].(map[string]any)
	return body
}

func walkInsertObjects(body map[string]any, parent readSourceScope, visitor readSourceVisitor) error {
	scope, err := walkWithObjects(body["with"], parent, visitor)
	if err != nil {
		return err
	}
	if target, ok := body["function_target"].(map[string]any); ok {
		if function, ok := target["function"].(map[string]any); ok {
			if detail, recognized := decodeNamespaceFunctionRefDetail(function); recognized &&
				detail.ref.Source == NamespaceRefTableFunction && visitor.namespace != nil {
				visitor.namespace(function, detail)
			}
			if err := walkExpression(function["args"], scope, visitor); err != nil {
				return err
			}
			if err := rejectUnknownReadFields(function, fields("name", "args", "distinct",
				"trailing_comments", "use_bracket_syntax", "no_parens", "quoted",
				"span", "inferred_type"), "INSERT FUNCTION target"); err != nil {
				return err
			}
		} else if err := rejectUnknownReadCarrier(target, "INSERT FUNCTION target"); err != nil {
			return err
		}
	}
	for _, key := range []string{
		"values", "query", "partition", "returning", "output", "on_conflict",
		"replace_where", "source", "partition_by", "settings", "hint",
	} {
		if err := walkExpression(body[key], scope, visitor); err != nil {
			return err
		}
	}
	return rejectUnknownReadFields(body, fields(
		"table", "columns", "values", "query", "overwrite", "partition", "directory",
		"returning", "output", "on_conflict", "leading_comments", "if_exists", "with",
		"ignore", "source_alias", "alias", "alias_explicit_as", "default_values",
		"by_name", "conflict_action", "is_replace", "hint", "replace_where", "source",
		"function_target", "partition_by", "settings",
	), "INSERT")
}

func walkCreateTableObjects(body map[string]any, scope readSourceScope, visitor readSourceVisitor) error {
	var err error
	scope, err = walkWithObjects(body["with_cte"], scope, visitor)
	if err != nil {
		return err
	}
	if err := walkCreateTableFunctionSource(body["clone_source"], scope, visitor); err != nil {
		return err
	}
	for _, key := range []string{"as_select", "clone_at_clause", "partition_of", "using_template"} {
		if err := walkExpression(body[key], scope, visitor); err != nil {
			return err
		}
	}
	if err := walkCreateProperties(body["properties"], scope, visitor); err != nil {
		return err
	}
	return walkCreateProperties(body["post_table_properties"], scope, visitor)
}

// Polyglot stores CREATE TABLE target AS table_function(...) in the otherwise
// table-shaped clone_source slot. Only the exact identifier_func.function path
// has source semantics; ordinary scalar functions and plain clone tables do not.
// This is a namespace projection (like INSERT FUNCTION), not an embedded SELECT
// read-source event: the write preflight must reject the SI namespace before its
// generic AsTableFunction rejection erases that metadata.
func walkCreateTableFunctionSource(node any, scope readSourceScope, visitor readSourceVisitor) error {
	cloneSource, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	identifierFunc, ok := cloneSource["identifier_func"].(map[string]any)
	if !ok {
		return nil
	}
	function, ok := identifierFunc["function"].(map[string]any)
	if !ok {
		return rejectUnknownReadCarrier(identifierFunc, "CREATE AS table function")
	}
	if detail, recognized := decodeNamespaceFunctionRefDetail(function); recognized &&
		detail.ref.Source == NamespaceRefTableFunction && visitor.namespace != nil {
		visitor.namespace(function, detail)
	}
	return walkExpression(function["args"], scope, visitor)
}

// CREATE adapters are deliberately the only place where engine and dictionary
// namespace carriers are decoded. A scalar lookalike never reaches this path.
func walkCreateProperties(node any, scope readSourceScope, visitor readSourceVisitor) error {
	switch n := node.(type) {
	case nil:
		return nil
	case []any:
		for _, child := range n {
			if err := walkCreateProperties(child, scope, visitor); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if property, ok := n["engine_property"].(map[string]any); ok {
			if ref, ok := decodeTableEngineNamespaceRef(property); ok && visitor.namespace != nil {
				visitor.namespace(property, namespaceRefDetail{ref: ref})
			}
			return rejectUnknownReadFields(n, fields("engine_property"), "CREATE engine property")
		}
		if property, ok := n["dict_property"].(map[string]any); ok {
			if ref, ok := decodeDictionarySourceNamespaceRef(property); ok && visitor.namespace != nil {
				visitor.namespace(property, namespaceRefDetail{ref: ref})
			}
			return rejectUnknownReadFields(n, fields("dict_property"), "CREATE dictionary property")
		}
		return rejectUnknownReadCarrier(n, "CREATE property")
	default:
		return nil
	}
}

func walkDeleteObjects(body map[string]any, parent readSourceScope, visitor readSourceVisitor) error {
	scope, err := walkWithObjects(body["with"], parent, visitor)
	if err != nil {
		return err
	}
	if err := walkTableSource(body["using"], scope, visitor); err != nil {
		return err
	}
	if err := walkJoinObjects(body["joins"], scope, visitor); err != nil {
		return err
	}
	for _, key := range []string{"where_clause", "output", "limit", "order_by", "returning", "hint"} {
		if err := walkExpression(body[key], scope, visitor); err != nil {
			return err
		}
	}
	return nil
}

func walkUpdateObjects(body map[string]any, parent readSourceScope, visitor readSourceVisitor) error {
	scope, err := walkWithObjects(body["with"], parent, visitor)
	if err != nil {
		return err
	}
	fromFirst, _ := body["from_before_set"].(bool)
	walkSet := func() error { return walkExpression(body["set"], scope, visitor) }
	walkFrom := func() error {
		if err := walkTableSource(body["from_clause"], scope, visitor); err != nil {
			return err
		}
		return walkJoinObjects(body["from_joins"], scope, visitor)
	}
	if fromFirst {
		if err := walkFrom(); err != nil {
			return err
		}
		if err := walkSet(); err != nil {
			return err
		}
	} else {
		if err := walkSet(); err != nil {
			return err
		}
		if err := walkFrom(); err != nil {
			return err
		}
	}
	for _, key := range []string{"where_clause", "returning", "output", "limit", "order_by", "hint"} {
		if err := walkExpression(body[key], scope, visitor); err != nil {
			return err
		}
	}
	return nil
}

// transparentReadQueryBody unwraps only grammar-transparent query wrappers.
// It is shared by query traversal and CTE role classification.
func transparentReadQueryBody(node any) (inner any, wrapper map[string]any, kind string, ok bool) {
	m, ok := node.(map[string]any)
	if !ok {
		return nil, nil, "", false
	}
	if subquery, ok := m["subquery"].(map[string]any); ok {
		if opaque, _ := subquery[opaqueDerivedTableKey].(bool); opaque {
			return nil, subquery, "subquery", true
		}
		return subquery["this"], subquery, "subquery", true
	}
	if paren, ok := m["paren"].(map[string]any); ok {
		return paren["this"], paren, "paren", true
	}
	return nil, nil, "", false
}

// walkReadQuery handles SELECT/set roots and transparent parenthesized/subquery
// wrappers. The bool reports whether node was a read-query carrier.
func walkReadQuery(node any, parent readSourceScope, visitor readSourceVisitor) (bool, error) {
	m, ok := node.(map[string]any)
	if !ok {
		return false, nil
	}
	if selectNode, ok := m[NodeSelect].(map[string]any); ok {
		return true, walkSelectObjects(selectNode, parent, visitor)
	}
	for _, kind := range []string{NodeUnion, NodeIntersect, NodeExcept} {
		if setNode, ok := m[kind].(map[string]any); ok {
			return true, walkSetObjects(setNode, parent, visitor)
		}
	}
	inner, wrapper, kind, transparent := transparentReadQueryBody(m)
	if !transparent {
		return false, nil
	}
	if kind == "subquery" {
		if opaque, _ := wrapper[opaqueDerivedTableKey].(bool); opaque {
			return true, nil
		}
	}
	handled, err := walkReadQuery(inner, parent, visitor)
	if err != nil {
		return true, err
	}
	if !handled {
		return false, nil
	}
	if kind == "subquery" {
		for _, key := range []string{"order_by", "limit", "offset", "distribute_by", "sort_by", "cluster_by"} {
			if err := walkExpression(wrapper[key], parent, visitor); err != nil {
				return true, err
			}
		}
		return true, rejectUnknownReadFields(wrapper, fields(
			"this", "alias", "column_aliases", "alias_explicit_as", "alias_keyword",
			"order_by", "limit", "offset", "distribute_by", "sort_by", "cluster_by",
			"lateral", "pattern", "quoted", opaqueDerivedTableKey,
		), "subquery")
	}
	return true, rejectUnknownReadFields(wrapper, fields("this", "trailing_comments"), "parenthesized query")
}

func walkSelectObjects(selectNode map[string]any, parent readSourceScope, visitor readSourceVisitor) error {
	scope, err := walkWithObjects(selectNode["with"], parent, visitor)
	if err != nil {
		return err
	}
	scope = selectAliasScope(selectNode, scope)

	if err := walkExpression(selectNode["expressions"], scope, visitor); err != nil {
		return err
	}
	if err := walkTableSource(selectNode["from"], scope, visitor); err != nil {
		return err
	}
	if err := walkJoinObjects(selectNode["joins"], scope, visitor); err != nil {
		return err
	}
	if err := walkExpression(selectNode["lateral_views"], scope, visitor); err != nil {
		return err
	}
	if err := walkExpression(selectNode["prewhere"], scope, visitor); err != nil {
		return err
	}
	if err := walkExpression(selectNode["where_clause"], scope, visitor); err != nil {
		return err
	}
	if err := walkExpression(selectNode["group_by"], scope, visitor); err != nil {
		return err
	}
	if err := walkExpression(selectNode["having"], scope, visitor); err != nil {
		return err
	}

	qualifyAfterWindow, _ := selectNode["qualify_after_window"].(bool)
	if qualifyAfterWindow {
		if err := walkNamedWindows(selectNode["windows"], scope, visitor); err != nil {
			return err
		}
		if err := walkExpression(selectNode["qualify"], scope, visitor); err != nil {
			return err
		}
	} else {
		if err := walkExpression(selectNode["qualify"], scope, visitor); err != nil {
			return err
		}
		if err := walkNamedWindows(selectNode["windows"], scope, visitor); err != nil {
			return err
		}
	}

	for _, key := range []string{
		"order_by", "distribute_by", "cluster_by", "sort_by", "limit_by",
		"limit", "offset", "fetch", "distinct_on", "top", "sample", "settings",
		"format", "hint", "connect", "into", "locks", "for_xml", "for_json", "exclude",
	} {
		if err := walkExpression(selectNode[key], scope, visitor); err != nil {
			return err
		}
	}
	return rejectUnknownReadFields(selectNode, fields(
		"expressions", "from", "joins", "lateral_views", "prewhere", "where_clause",
		"group_by", "having", "qualify", "order_by", "distribute_by", "cluster_by",
		"sort_by", "limit", "offset", "limit_by", "fetch", "distinct", "distinct_on",
		"top", "with", "sample", "settings", "format", "windows", "hint", "connect",
		"into", "locks", "for_xml", "for_json", "leading_comments",
		"post_select_comments", "kind", "operation_modifiers",
		"qualify_after_window", "option", "exclude",
	), "SELECT")
}

func walkSetObjects(setNode map[string]any, parent readSourceScope, visitor readSourceVisitor) error {
	scope, err := walkWithObjects(setNode["with"], parent, visitor)
	if err != nil {
		return err
	}
	if err := walkExpression(setNode["left"], scope, visitor); err != nil {
		return err
	}
	if err := walkExpression(setNode["right"], scope, visitor); err != nil {
		return err
	}
	for _, key := range []string{
		"order_by", "limit", "offset", "distribute_by", "sort_by", "cluster_by", "on_columns",
	} {
		if err := walkExpression(setNode[key], scope, visitor); err != nil {
			return err
		}
	}
	return rejectUnknownReadFields(setNode, fields(
		"left", "right", "all", "distinct", "with", "order_by", "limit", "offset",
		"distribute_by", "sort_by", "cluster_by", "by_name", "side", "kind",
		"corresponding", "strict", "on_columns",
	), "set operation")
}

func walkWithObjects(withNode any, parent readSourceScope, visitor readSourceVisitor) (readSourceScope, error) {
	if wrapped, ok := withNode.(map[string]any); ok {
		if body, exists := wrapped["with"].(map[string]any); exists {
			withNode = body
		}
	}
	with, _ := withNode.(map[string]any)
	ctes, _ := with["ctes"].([]any)
	if len(ctes) == 0 {
		return parent, walkExpression(with["search"], parent, visitor)
	}
	scope := readSourceScope{
		ctes:    cloneReadSourceNames(parent.ctes, len(ctes)),
		aliases: cloneReadSourceNames(parent.aliases, len(ctes)),
	}
	recursive, _ := with["recursive"].(bool)
	if recursive {
		for _, raw := range ctes {
			cte, _ := raw.(map[string]any)
			declareCTEBinding(scope, cte)
		}
	}
	for _, raw := range ctes {
		cte, _ := raw.(map[string]any)
		if err := walkExpression(cte["this"], scope, visitor); err != nil {
			return scope, err
		}
		if !recursive {
			declareCTEBinding(scope, cte)
		}
		if err := rejectUnknownReadFields(cte, fields(
			"alias", "this", "columns", "materialized", "key_expressions",
			"alias_first", "comments",
		), "CTE"); err != nil {
			return scope, err
		}
	}
	if err := walkExpression(with["search"], scope, visitor); err != nil {
		return scope, err
	}
	return scope, rejectUnknownReadFields(with, fields(
		"ctes", "recursive", "leading_comments", "search",
	), "WITH")
}

func declareCTEBinding(scope readSourceScope, cte map[string]any) {
	name := concreteIdentifierName(cte["alias"])
	if name == "" {
		return
	}
	aliasFirst, _ := cte["alias_first"].(bool)
	if aliasFirst && cteBodyIsReadQuery(cte["this"]) {
		scope.ctes[name] = true
		return
	}
	scope.aliases[name] = true
}

func cteBodyIsReadQuery(node any) bool {
	for {
		m, ok := node.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := m[NodeSelect]; ok {
			return true
		}
		for _, kind := range []string{NodeUnion, NodeIntersect, NodeExcept} {
			if _, ok := m[kind]; ok {
				return true
			}
		}
		inner, wrapper, kind, transparent := transparentReadQueryBody(node)
		if !transparent {
			return false
		}
		if kind == "subquery" {
			if opaque, _ := wrapper[opaqueDerivedTableKey].(bool); opaque {
				return false
			}
		}
		node = inner
	}
}

func cloneReadSourceNames(names map[string]bool, extra int) map[string]bool {
	cloned := make(map[string]bool, len(names)+extra)
	for name := range names {
		cloned[name] = true
	}
	return cloned
}

func isScopedCurrentDatabaseRef(ref NamespaceRef, scope readSourceScope) bool {
	if !ref.UsesCurrentDatabase || ref.Target.DB != "" || ref.Target.Table == "" {
		return false
	}
	return scope.ctes[ref.Target.Table] || scope.aliases[ref.Target.Table]
}

func selectAliasScope(selectNode map[string]any, parent readSourceScope) readSourceScope {
	aliases := cloneReadSourceNames(parent.aliases, 4)
	collectProjectionAliases(selectNode["expressions"], aliases)
	collectTableSourceAliases(selectNode["from"], aliases)
	collectJoinSourceAliases(selectNode["joins"], aliases)
	parent.aliases = aliases
	return parent
}

func collectProjectionAliases(node any, aliases map[string]bool) {
	expressions, _ := node.([]any)
	for _, raw := range expressions {
		expression, _ := raw.(map[string]any)
		alias, _ := expression["alias"].(map[string]any)
		if name := concreteIdentifierName(alias["alias"]); name != "" {
			aliases[name] = true
		}
	}
}

func collectTableSourceAliases(node any, aliases map[string]bool) {
	switch n := node.(type) {
	case []any:
		for _, child := range n {
			collectTableSourceAliases(child, aliases)
		}
	case map[string]any:
		if from, ok := n["from"].(map[string]any); ok {
			collectTableSourceAliases(from["expressions"], aliases)
			return
		}
		if expressions, ok := n["expressions"].([]any); ok && n["name"] == nil {
			collectTableSourceAliases(expressions, aliases)
			return
		}
		if table, ok := n["table"].(map[string]any); ok {
			if name := concreteIdentifierName(table["alias"]); name != "" {
				aliases[name] = true
			}
			return
		}
		if alias, ok := n["alias"].(map[string]any); ok {
			if name := concreteIdentifierName(alias["alias"]); name != "" {
				aliases[name] = true
			}
			return
		}
		if subquery, ok := n["subquery"].(map[string]any); ok {
			if name := concreteIdentifierName(subquery["alias"]); name != "" {
				aliases[name] = true
			}
			return
		}
		if paren, ok := n["paren"].(map[string]any); ok {
			collectTableSourceAliases(paren["this"], aliases)
			return
		}
		if joined, ok := n["joined_table"].(map[string]any); ok {
			collectTableSourceAliases(joined["left"], aliases)
			collectJoinSourceAliases(joined["joins"], aliases)
		}
	}
}

func collectJoinSourceAliases(node any, aliases map[string]bool) {
	switch n := node.(type) {
	case []any:
		for _, child := range n {
			collectJoinSourceAliases(child, aliases)
		}
	case map[string]any:
		collectTableSourceAliases(n["this"], aliases)
	}
}

// walkTableSource is the only source-role entry point. It may emit recognized
// table functions; walkExpression never does.
func walkTableSource(node any, scope readSourceScope, visitor readSourceVisitor) error {
	switch n := node.(type) {
	case nil:
		return nil
	case []any:
		for _, child := range n {
			if err := walkTableSource(child, scope, visitor); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if opaqueSubquery(n) {
			return nil
		}
		if handled, err := walkReadQuery(n, scope, visitor); handled {
			return err
		}
		if from, ok := n["from"].(map[string]any); ok {
			return walkTableSource(from["expressions"], scope, visitor)
		}
		if expressions, ok := n["expressions"].([]any); ok {
			return walkTableSource(expressions, scope, visitor)
		}
		if table, ok := n["table"].(map[string]any); ok {
			emitTableSource(n, table, scope, visitor)
			return nil
		}
		if isTableRefPayload(n) {
			emitTableSource(n, n, scope, visitor)
			return nil
		}
		if function, ok := n["function"].(map[string]any); ok {
			if detail, recognized := decodeNamespaceFunctionRefDetail(function); recognized &&
				detail.ref.Source == NamespaceRefTableFunction {
				if visitor.namespace != nil {
					visitor.namespace(function, detail)
				}
				if visitor.function != nil {
					visitor.function(function, detail)
				}
			}
			return walkExpression(function["args"], scope, visitor)
		}
		if alias, ok := n["alias"].(map[string]any); ok {
			return walkTableSource(alias["this"], scope, visitor)
		}
		if paren, ok := n["paren"].(map[string]any); ok {
			if handled, err := walkReadQuery(paren["this"], scope, visitor); handled {
				return err
			}
			return walkTableSource(paren["this"], scope, visitor)
		}
		if joined, ok := n["joined_table"].(map[string]any); ok {
			if err := walkTableSource(joined["left"], scope, visitor); err != nil {
				return err
			}
			if err := walkJoinObjects(joined["joins"], scope, visitor); err != nil {
				return err
			}
			return walkExpression(joined["lateral_views"], scope, visitor)
		}
		for _, kind := range []string{"pivot", "pivot_alias", "unpivot"} {
			if body, ok := n[kind].(map[string]any); ok {
				if err := walkTableSource(body["this"], scope, visitor); err != nil {
					return err
				}
				return walkExpression(body["expressions"], scope, visitor)
			}
		}
		if rows, ok := n["rows_from"].(map[string]any); ok {
			return walkTableSource(rows["expressions"], scope, visitor)
		}
		return rejectUnknownReadCarrier(n, "table source")
	default:
		return nil
	}
}

func isTableRefPayload(node map[string]any) bool {
	name, ok := node["name"].(map[string]any)
	return ok && identName(name) != ""
}

func emitTableSource(expr, table map[string]any, scope readSourceScope, visitor readSourceVisitor) {
	target := decodeTableTarget(table)
	if target.Table == "" || (target.DB == "" && scope.ctes[target.Table]) {
		return
	}
	if visitor.table != nil {
		visitor.table(expr, table, target)
	}
}

func walkJoinObjects(node any, scope readSourceScope, visitor readSourceVisitor) error {
	switch n := node.(type) {
	case nil:
		return nil
	case []any:
		for _, child := range n {
			if err := walkJoinObjects(child, scope, visitor); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if join, ok := n["join"].(map[string]any); ok {
			n = join
		}
		if err := walkTableSource(n["this"], scope, visitor); err != nil {
			return err
		}
		for _, key := range []string{"on", "using", "pivots", "sample"} {
			if err := walkExpression(n[key], scope, visitor); err != nil {
				return err
			}
		}
		return rejectUnknownReadFields(n, fields(
			"this", "on", "using", "kind", "pivots", "sample",
		), "JOIN")
	default:
		return nil
	}
}

// walkExpression traverses scalar grammar roles and embedded read-query roots.
// Generic scalar functions recurse their arguments only; recognized table
// functions are intentionally not emitted here.
func walkExpression(node any, scope readSourceScope, visitor readSourceVisitor) error {
	switch n := node.(type) {
	case nil:
		return nil
	case []any:
		for _, child := range n {
			if err := walkExpression(child, scope, visitor); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if opaqueSubquery(n) {
			return nil
		}
		if handled, err := walkReadQuery(n, scope, visitor); handled {
			return err
		}
		if _, ordered := n["with_fill"]; ordered || n["desc"] != nil || n["nulls_first"] != nil {
			return walkOrderedExpression(n, scope, visitor)
		}
		if handled, err := walkExpressionStruct(n, scope, visitor); handled {
			return err
		}
		kind := expressionNodeKind(n)
		if kind == "" {
			return rejectUnknownReadCarrier(n, "expression carrier")
		}
		body, _ := n[kind].(map[string]any)
		switch kind {
		case "in":
			return walkInExpression(body, scope, visitor)
		case "function":
			return walkFunctionExpression(body, scope, visitor)
		case "aggregate_function":
			return walkAggregateExpression(body, scope, visitor)
		case "window_function":
			return walkWindowFunction(body, scope, visitor)
		case "case":
			return walkCaseExpression(body, scope, visitor)
		case "if_func":
			return walkIfExpression(body, scope, visitor)
		case "ordered":
			return walkOrderedExpression(body, scope, visitor)
		case "window", "over":
			return walkWindowSpec(body, scope, visitor)
		case "between":
			return walkExpressionFields(body, scope, visitor, kind, "this", "low", "high")
		case "like", "i_like":
			return walkExpressionFields(body, scope, visitor, kind, "left", "right", "escape")
		case "cast", "try_cast", "safe_cast":
			return walkExpressionFields(body, scope, visitor, kind, "this", "format", "default")
		case "values":
			return walkExpressionFields(body, scope, visitor, kind, "rows", "expressions")
		}
		if binaryExpressionKind(kind) {
			return walkExpressionFields(body, scope, visitor, kind, "left", "right")
		}
		if unaryExpressionKind(kind) {
			return walkExpressionFields(body, scope, visitor, kind, "this")
		}
		if expressionListKind(kind) {
			return walkExpressionFields(body, scope, visitor, kind, "expressions")
		}
		if typedAggregateExpressionKind(kind) {
			return walkExpressionFields(body, scope, visitor, kind,
				"this", "condition", "filter", "order_by", "separator", "limit",
				"percentile", "accuracy", "having_max")
		}
		if typedWindowExpressionKind(kind) {
			return walkExpressionFields(body, scope, visitor, kind,
				"this", "num_buckets", "offset", "default", "order_by", "args")
		}
		if scalarFieldExpressionKind(kind) {
			return walkExpressionFields(body, scope, visitor, kind,
				"this", "expression", "expressions", "args", "start", "length",
				"replacement", "position", "format", "default", "separator",
				"condition", "true_value", "false_value")
		}
		return rejectUnknownReadCarrier(n, "expression "+kind)
	default:
		return nil
	}
}

// walkExpressionStruct handles grammar structs reached through an already
// explicit parent field (WHERE, ORDER BY, LIMIT, SAMPLE, and similar clauses).
// It never dispatches an externally tagged expression variant.
func walkExpressionStruct(node map[string]any, scope readSourceScope, visitor readSourceVisitor) (bool, error) {
	if _, ok := node["this"]; ok {
		for _, key := range []string{
			"this", "expressions", "format", "default", "value", "on", "order_by",
		} {
			if err := walkExpression(node[key], scope, visitor); err != nil {
				return true, err
			}
		}
		return true, rejectUnknownReadFields(node, fields(
			"this", "expressions", "format", "default", "value", "on", "order_by",
			"all", "totals", "siblings", "percent", "rows", "comments", "count",
			"direction", "with_ties", "parenthesized", "table_alias", "column_aliases",
			"outer", "temporary", "unlogged", "bulk_collect",
		), "clause expression")
	}
	if _, ok := node["expressions"]; ok {
		if err := walkExpression(node["expressions"], scope, visitor); err != nil {
			return true, err
		}
		return true, rejectUnknownReadFields(node, fields(
			"expressions", "all", "totals", "siblings", "comments",
		), "expression list clause")
	}
	if _, ok := node["count"]; ok {
		if err := walkExpression(node["count"], scope, visitor); err != nil {
			return true, err
		}
		return true, rejectUnknownReadFields(node, fields(
			"count", "direction", "percent", "rows", "with_ties",
		), "FETCH")
	}
	if _, ok := node["size"]; ok {
		for _, key := range []string{
			"size", "seed", "offset", "bucket_numerator", "bucket_denominator", "bucket_field",
		} {
			if err := walkExpression(node[key], scope, visitor); err != nil {
				return true, err
			}
		}
		return true, rejectUnknownReadFields(node, fields(
			"method", "size", "seed", "offset", "unit_after_size", "use_sample_keyword",
			"explicit_method", "method_before_size", "use_seed_keyword", "bucket_numerator",
			"bucket_denominator", "bucket_field", "is_using_sample", "is_percent",
			"suppress_method_output",
		), "SAMPLE")
	}
	if _, ok := node["connect"]; ok {
		if err := walkExpression(node["start"], scope, visitor); err != nil {
			return true, err
		}
		if err := walkExpression(node["connect"], scope, visitor); err != nil {
			return true, err
		}
		return true, rejectUnknownReadFields(node, fields(
			"start", "connect", "nocycle",
		), "CONNECT")
	}
	if _, ok := node["columns"]; ok {
		if err := walkExpression(node["columns"], scope, visitor); err != nil {
			return true, err
		}
		if err := walkExpression(node["into_table"], scope, visitor); err != nil {
			return true, err
		}
		return true, rejectUnknownReadFields(node, fields(
			"columns", "into_table",
		), "OUTPUT")
	}
	return false, nil
}

func expressionNodeKind(node map[string]any) string {
	kind := ""
	for key := range node {
		if key == opaqueDerivedTableKey || key == opaqueIdentifierParameterKey ||
			strings.HasPrefix(key, "_rewriter_go_") {
			continue
		}
		if kind != "" {
			return ""
		}
		kind = key
	}
	return kind
}

func walkInExpression(inNode map[string]any, scope readSourceScope, visitor readSourceVisitor) error {
	if err := walkExpression(inNode["this"], scope, visitor); err != nil {
		return err
	}
	if detail, ok := decodeInNamespaceRefDetail(inNode); ok {
		if !isScopedCurrentDatabaseRef(detail.ref, scope) {
			if visitor.namespace != nil {
				visitor.namespace(inNode, detail)
			}
			if visitor.inTable != nil {
				visitor.inTable(inNode, detail)
			}
		}
	} else {
		if err := walkExpression(inNode["expressions"], scope, visitor); err != nil {
			return err
		}
	}
	if err := walkExpression(inNode["query"], scope, visitor); err != nil {
		return err
	}
	if err := walkExpression(inNode["unnest"], scope, visitor); err != nil {
		return err
	}
	return rejectUnknownReadFields(inNode, fields(
		"this", "expressions", "query", "not", "global", "unnest", "is_field",
	), "IN")
}

func walkFunctionExpression(function map[string]any, scope readSourceScope, visitor readSourceVisitor) error {
	args, _ := function["args"].([]any)
	if detail, recognized := decodeNamespaceFunctionRefDetail(function); recognized &&
		detail.ref.Source == NamespaceRefInTable {
		if len(args) > 0 {
			if err := walkExpression(args[0], scope, visitor); err != nil {
				return err
			}
		}
		if !isScopedCurrentDatabaseRef(detail.ref, scope) {
			if visitor.namespace != nil {
				visitor.namespace(function, detail)
			}
			if visitor.inTable != nil {
				visitor.inTable(function, detail)
			}
		}
	} else if err := walkExpression(args, scope, visitor); err != nil {
		return err
	}
	return rejectUnknownReadFields(function, fields(
		"name", "args", "distinct", "trailing_comments", "use_bracket_syntax",
		"no_parens", "quoted", "span", "inferred_type",
	), "scalar function")
}

func walkAggregateExpression(function map[string]any, scope readSourceScope, visitor readSourceVisitor) error {
	for _, key := range []string{"args", "filter", "order_by", "limit"} {
		if err := walkExpression(function[key], scope, visitor); err != nil {
			return err
		}
	}
	return rejectUnknownReadFields(function, fields(
		"name", "args", "distinct", "filter", "order_by", "limit",
		"ignore_nulls", "inferred_type",
	), "aggregate function")
}

func walkCaseExpression(caseNode map[string]any, scope readSourceScope, visitor readSourceVisitor) error {
	if err := walkExpression(caseNode["operand"], scope, visitor); err != nil {
		return err
	}
	if whens, ok := caseNode["whens"].([]any); ok {
		for _, pair := range whens {
			if err := walkExpression(pair, scope, visitor); err != nil {
				return err
			}
		}
	} else if err := walkExpression(caseNode["whens"], scope, visitor); err != nil {
		return err
	}
	if err := walkExpression(caseNode["else_"], scope, visitor); err != nil {
		return err
	}
	return rejectUnknownReadFields(caseNode, fields(
		"operand", "whens", "else_", "comments", "inferred_type",
	), "CASE")
}

func walkIfExpression(ifNode map[string]any, scope readSourceScope, visitor readSourceVisitor) error {
	for _, key := range []string{"condition", "true_value", "false_value"} {
		if err := walkExpression(ifNode[key], scope, visitor); err != nil {
			return err
		}
	}
	return rejectUnknownReadFields(ifNode, fields(
		"condition", "true_value", "false_value", "original_name", "inferred_type",
	), "IF")
}

func walkWindowFunction(windowFunction map[string]any, scope readSourceScope, visitor readSourceVisitor) error {
	if err := walkExpression(windowFunction["this"], scope, visitor); err != nil {
		return err
	}
	if err := walkWindowSpec(windowFunction["over"], scope, visitor); err != nil {
		return err
	}
	if keep, ok := windowFunction["keep"].(map[string]any); ok {
		if err := walkExpression(keep["order_by"], scope, visitor); err != nil {
			return err
		}
	}
	return rejectUnknownReadFields(windowFunction, fields(
		"this", "over", "keep", "inferred_type", "comments",
	), "window function")
}

func walkNamedWindows(node any, scope readSourceScope, visitor readSourceVisitor) error {
	switch n := node.(type) {
	case nil:
		return nil
	case []any:
		for _, child := range n {
			if err := walkNamedWindows(child, scope, visitor); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if err := walkWindowSpec(n["spec"], scope, visitor); err != nil {
			return err
		}
		return rejectUnknownReadFields(n, fields("name", "spec"), "named WINDOW")
	default:
		return nil
	}
}

func walkWindowSpec(node any, scope readSourceScope, visitor readSourceVisitor) error {
	if wrapped, ok := node.(map[string]any); ok {
		if body, exists := wrapped["window"].(map[string]any); exists {
			node = body
		} else if body, exists := wrapped["over"].(map[string]any); exists {
			node = body
		}
	}
	spec, _ := node.(map[string]any)
	if spec == nil {
		return nil
	}
	if err := walkExpression(spec["partition_by"], scope, visitor); err != nil {
		return err
	}
	if err := walkExpression(spec["order_by"], scope, visitor); err != nil {
		return err
	}
	if err := walkWindowFrame(spec["frame"], scope, visitor); err != nil {
		return err
	}
	return rejectUnknownReadFields(spec, fields(
		"window_name", "partition_by", "order_by", "frame", "alias",
	), "window specification")
}

func walkWindowFrame(node any, scope readSourceScope, visitor readSourceVisitor) error {
	frame, _ := node.(map[string]any)
	if frame == nil {
		return nil
	}
	if err := walkFrameBound(frame["start"], scope, visitor); err != nil {
		return err
	}
	if err := walkFrameBound(frame["end"], scope, visitor); err != nil {
		return err
	}
	return rejectUnknownReadFields(frame, fields(
		"kind", "start", "end", "exclude", "kind_text", "start_side_text", "end_side_text",
	), "window frame")
}

func walkFrameBound(node any, scope readSourceScope, visitor readSourceVisitor) error {
	bound, _ := node.(map[string]any)
	if bound == nil {
		return nil
	}
	for _, key := range []string{"preceding", "following", "value"} {
		if child, ok := bound[key]; ok {
			return walkExpression(child, scope, visitor)
		}
	}
	return rejectUnknownReadCarrier(bound, "window frame bound")
}

func walkOrderedExpression(ordered map[string]any, scope readSourceScope, visitor readSourceVisitor) error {
	if err := walkExpression(ordered["this"], scope, visitor); err != nil {
		return err
	}
	withFill, _ := ordered["with_fill"].(map[string]any)
	if withFill != nil {
		for _, key := range []string{"from_", "to", "step", "staleness", "interpolate"} {
			if err := walkExpression(withFill[key], scope, visitor); err != nil {
				return err
			}
		}
		if err := rejectUnknownReadFields(withFill, fields(
			"from_", "to", "step", "staleness", "interpolate",
		), "WITH FILL"); err != nil {
			return err
		}
	}
	return rejectUnknownReadFields(ordered, fields(
		"this", "desc", "nulls_first", "explicit_asc", "with_fill", "comments",
	), "ordered expression")
}

func walkExpressionFields(body map[string]any, scope readSourceScope, visitor readSourceVisitor, context string, keys ...string) error {
	for _, key := range keys {
		if err := walkExpression(body[key], scope, visitor); err != nil {
			return err
		}
	}
	allowed := fields(keys...)
	for _, key := range []string{
		"inferred_type", "trailing_comments", "comments", "distinct", "not",
		"kind", "quoted", "literal_type", "value", "name", "alias",
	} {
		allowed[key] = true
	}
	return rejectUnknownReadFields(body, allowed, context)
}

func binaryExpressionKind(kind string) bool {
	switch kind {
	case "and", "or", "add", "sub", "mul", "div", "mod", "eq", "neq", "lt",
		"lte", "gt", "gte", "match", "bitwise_and", "bitwise_or", "bitwise_xor",
		"concat", "adjacent", "ts_match", "property_eq", "array_contains_all",
		"array_contained_by", "array_overlaps", "jsonb_contains_all_top_keys",
		"jsonb_contains_any_top_keys", "jsonb_delete_at_path", "extends_left",
		"extends_right", "is", "member_of", "null_safe_eq", "null_safe_neq",
		"glob", "similar_to", "overlaps", "bitwise_left_shift",
		"bitwise_right_shift":
		return true
	default:
		return false
	}
}

func unaryExpressionKind(kind string) bool {
	switch kind {
	case "alias", "paren", "annotated", "braced_wildcard", "not", "neg",
		"bitwise_not", "is_null", "is_true", "is_false", "is_json", "exists",
		"pre_where", "where", "having", "qualify", "prior", "connect_by_root",
		"collation", "upper", "lower", "length", "ltrim", "rtrim", "reverse",
		"abs", "floor", "ceil", "sqrt", "cbrt", "ln", "exp", "sign",
		"array_length", "array_size", "cardinality", "array_reverse",
		"array_distinct", "array_flatten", "array_compact", "to_array",
		"json_array_length", "json_keys", "json_type", "parse_json", "to_json":
		return true
	default:
		return false
	}
}

func expressionListKind(kind string) bool {
	switch kind {
	case "array", "struct", "tuple", "coalesce", "greatest", "least",
		"group_by", "order_by", "distribute_by", "cluster_by", "sort_by",
		"from", "hint", "array_func", "map_func", "json_array", "json_object",
		"named_struct":
		return true
	default:
		return false
	}
}

func typedAggregateExpressionKind(kind string) bool {
	switch kind {
	case "count", "sum", "avg", "min", "max", "group_concat", "string_agg",
		"list_agg", "array_agg", "count_if", "sum_if", "stddev", "stddev_pop",
		"stddev_samp", "variance", "var_pop", "var_samp", "median", "mode",
		"first", "last", "any_value", "approx_distinct", "approx_count_distinct",
		"approx_percentile", "percentile", "logical_and", "logical_or", "skewness",
		"array_concat_agg", "array_unique_agg", "bool_xor_agg", "percentile_cont",
		"percentile_disc":
		return true
	default:
		return false
	}
}

func typedWindowExpressionKind(kind string) bool {
	switch kind {
	case "rank", "dense_rank", "n_tile", "lead", "lag", "first_value",
		"last_value", "nth_value", "percent_rank", "cume_dist":
		return true
	default:
		return false
	}
}

func scalarFieldExpressionKind(kind string) bool {
	switch kind {
	case "substring", "trim", "replace", "left", "right", "repeat", "lpad",
		"rpad", "split", "regexp_like", "regexp_replace", "regexp_extract",
		"overlay", "round", "power", "log", "at_time_zone", "date_add",
		"date_sub", "date_diff", "date_trunc", "extract", "to_date",
		"to_timestamp", "null_if", "if_null", "nvl", "nvl2", "contains",
		"starts_with", "ends_with", "position", "array_contains",
		"array_position", "array_append", "array_prepend", "array_concat",
		"array_sort", "array_join", "array_to_string", "array_intersect",
		"array_union", "array_except", "array_remove", "array_zip", "sequence",
		"generate", "struct_extract", "map_from_entries", "map_from_arrays",
		"map_keys", "map_values", "map_contains_key", "map_concat",
		"element_at", "transform_keys", "transform_values", "json_extract",
		"json_extract_scalar", "json_extract_path", "json_query", "json_value",
		"json_set", "json_insert", "json_remove", "json_merge_patch",
		"named_argument", "subscript", "dot", "method_call", "array_slice",
		"lambda", "interval":
		return true
	default:
		return false
	}
}

func rejectUnknownReadCarrier(node any, context string) error {
	if containsReadBearingNode(node) {
		return fmt.Errorf("engine: ordered object walk: unmodeled %s contains a read object", context)
	}
	return nil
}

func rejectUnknownReadFields(node map[string]any, handled map[string]bool, context string) error {
	for key, child := range node {
		if handled[key] {
			continue
		}
		if containsReadBearingNode(child) {
			return fmt.Errorf("engine: ordered object walk: unmodeled %s field %q contains a read object", context, key)
		}
	}
	return nil
}

func fields(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		out[key] = true
	}
	return out
}

func containsReadBearingNode(node any) bool {
	switch n := node.(type) {
	case []any:
		for _, child := range n {
			if containsReadBearingNode(child) {
				return true
			}
		}
	case map[string]any:
		if opaqueSubquery(n) {
			return false
		}
		for _, key := range []string{NodeSelect, NodeUnion, NodeIntersect, NodeExcept} {
			if _, ok := n[key].(map[string]any); ok {
				return true
			}
		}
		if subquery, ok := n["subquery"].(map[string]any); ok && subquery["this"] != nil {
			return true
		}
		if inNode, ok := n["in"].(map[string]any); ok && inNode["this"] != nil {
			return true
		}
		if table, ok := n["table"].(map[string]any); ok && isTableRefPayload(table) {
			return true
		}
		if _, ok := n["engine_property"].(map[string]any); ok {
			return true
		}
		if _, ok := n["dict_property"].(map[string]any); ok {
			return true
		}
		if function, ok := n["function"].(map[string]any); ok {
			if _, recognized := decodeNamespaceFunctionRefDetail(function); recognized {
				return true
			}
		}
		for _, child := range n {
			if containsReadBearingNode(child) {
				return true
			}
		}
	}
	return false
}

func opaqueSubquery(node map[string]any) bool {
	subquery, ok := node["subquery"].(map[string]any)
	if !ok {
		return false
	}
	opaque, _ := subquery[opaqueDerivedTableKey].(bool)
	return opaque
}

func tableFunctionArgText(arg any) (string, bool) {
	value, _, ok := tableFunctionArgValue(arg)
	return value, ok
}

// decodeStringLiteralValue returns the semantic string a polyglot literal node
// denotes, and whether the node is a literal kind whose value is safe to use as
// a namespace name. ClickHouse heredocs arrive as literal_type "dollar_string";
// the tagged form $tag$body$tag$ encodes as "<tag>\x00<body>", so reading
// lit["value"] raw made storage-integrity policy see a different string than
// Generate emits for ClickHouse to execute (Spec N D6).
//
// Any other literal type is deliberately NOT decoded. Treating an unmodelled
// encoding as an opaque, harmless value is exactly how the tagged heredoc got
// through; an unrecognized kind must reach the caller as unresolvable so
// storage-integrity policy fails closed. Widening this whitelist requires
// proving that the decoded value equals the value Generate emits — the
// invariant TestTableFunctionArgValue_PolicyValueMatchesGeneratedValueOrRefuses
// enforces for every literal_type polyglot can produce here.
func decodeStringLiteralValue(lit map[string]any) (string, bool) {
	value, ok := lit["value"].(string)
	if !ok {
		return "", false
	}
	switch lit["literal_type"] {
	case "string":
		return value, true
	case "dollar_string":
		if nul := strings.IndexByte(value, 0); nul >= 0 {
			return value[nul+1:], true // strip the "<tag>\x00" prefix
		}
		return value, true
	default:
		return "", false
	}
}

func tableFunctionArgValue(arg any) (string, namespaceValueOrigin, bool) {
	m, ok := arg.(map[string]any)
	if !ok {
		return "", namespaceValueUnknown, false
	}
	if lit, ok := m["literal"].(map[string]any); ok {
		value, ok := decodeStringLiteralValue(lit)
		return value, namespaceValueLiteral, ok && value != ""
	}
	if col, ok := m["column"].(map[string]any); ok {
		if unresolvedIdentifierNode(col["name"]) || unresolvedIdentifierNode(col["table"]) {
			return "", namespaceValueUnknown, false
		}
		name := identName(col["name"])
		if name == "" {
			return "", namespaceValueUnknown, false
		}
		if table := identName(col["table"]); table != "" {
			return table + "." + name, namespaceValueIdentifier, true
		}
		return name, namespaceValueIdentifier, true
	}
	if dot, ok := m["dot"].(map[string]any); ok {
		left, origin, lok := tableFunctionArgValue(dot["this"])
		if unresolvedIdentifierNode(dot["field"]) {
			return "", namespaceValueUnknown, false
		}
		right := identName(dot["field"])
		if lok && origin == namespaceValueIdentifier && right != "" {
			return left + "." + right, namespaceValueIdentifier, true
		}
	}
	if unresolvedIdentifierNode(m) {
		return "", namespaceValueUnknown, false
	}
	if name := identName(m); name != "" {
		return name, namespaceValueIdentifier, true
	}
	return "", namespaceValueUnknown, false
}

// Polyglot preserves a fully dynamic {x:Identifier} as a parameter node, but
// flattens a qualified db.{x:Identifier} field into an unquoted Identifier
// named "{x: Identifier}". Neither shape proves a concrete namespace value.
// Quoted identifiers with the same visible spelling remain ordinary names.
func unresolvedIdentifierNode(node any) bool {
	m, ok := node.(map[string]any)
	if !ok {
		return false
	}
	if opaque, _ := m[opaqueIdentifierParameterKey].(bool); opaque {
		return true
	}
	if _, parameter := m["parameter"]; parameter {
		return true
	}
	quoted, _ := m["quoted"].(bool)
	name, _ := m["name"].(string)
	if quoted || len(name) < len("{x: Identifier}") || name[0] != '{' || name[len(name)-1] != '}' {
		return false
	}
	colon := strings.LastIndexByte(name, ':')
	return colon > 1 && strings.EqualFold(strings.TrimSpace(name[colon+1:len(name)-1]), "Identifier")
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
	if err := walkStatementObjects(root, readSourceScope{}, readSourceVisitor{
		table: func(expr, tbl map[string]any, tt TableTarget) {
			applyDecision(expr, tbl, tt, decide(tt))
		},
	}); err != nil {
		return nil, err
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode select: %w", err)
	}
	return AST(out), nil
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
	if err := walkStatementObjects(root, readSourceScope{}, readSourceVisitor{table: func(_ map[string]any, tbl map[string]any, tt TableTarget) {
		final, _ := tbl["final_"].(bool)
		sampled := tbl["table_sample"] != nil
		aliases, _ := tbl["column_aliases"].([]any)
		if final || sampled || len(aliases) > 0 {
			appendUnique(tt)
		}
	}}); err != nil {
		return nil, err
	}
	if err := collectSelectLevelSampleTargets(root, appendUnique); err != nil {
		return nil, err
	}
	return out, nil
}

// HasUnsupportedTableWrapper is retained as the coarse compatibility helper;
// new policy code should use UnsupportedTableWrapperTargets.
func HasUnsupportedTableWrapper(ast AST) (bool, error) {
	targets, err := UnsupportedTableWrapperTargets(ast)
	return len(targets) > 0, err
}

func collectSelectLevelSampleTargets(node any, appendTarget func(TableTarget)) error {
	switch n := node.(type) {
	case map[string]any:
		if sel, ok := n["select"].(map[string]any); ok && sel["sample"] != nil {
			if from, ok := sel["from"].(map[string]any); ok {
				found := false
				if err := walkTableSource(from, readSourceScope{}, readSourceVisitor{table: func(_ map[string]any, _ map[string]any, tt TableTarget) {
					if !found {
						appendTarget(tt)
						found = true
					}
				}}); err != nil {
					return err
				}
			}
		}
		for _, v := range n {
			if err := collectSelectLevelSampleTargets(v, appendTarget); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range n {
			if err := collectSelectLevelSampleTargets(v, appendTarget); err != nil {
				return err
			}
		}
	}
	return nil
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
			aliasFirst, _ := cm["alias_first"].(bool)
			if aliasFirst && cteBodyIsReadQuery(cm["this"]) {
				name := concreteIdentifierName(cm["alias"])
				if name == "" {
					continue
				}
				extended[name] = true
			}
		}
	}
	return extended
}

// decodeTableTarget reads {name:{name}, schema:{name}, alias:{name}} from a table node.
func decodeTableTarget(tbl map[string]any) TableTarget {
	if unresolvedIdentifierNode(tbl["name"]) ||
		(tbl["schema"] != nil && unresolvedIdentifierNode(tbl["schema"])) {
		return TableTarget{}
	}
	return TableTarget{
		DB:    identName(tbl["schema"]),
		Table: identName(tbl["name"]),
		Alias: concreteIdentifierName(tbl["alias"]),
	}
}

func concreteIdentifierName(v any) string {
	if unresolvedIdentifierNode(v) {
		return ""
	}
	return identName(v)
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
