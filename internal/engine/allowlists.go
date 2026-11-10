package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// TableFunctionClass is the spec 2026-09-26 T5 classification of a
// source-role table function.
type TableFunctionClass int

const (
	TableFunctionUnknown  TableFunctionClass = iota // not on any list: refused as "not recognised"
	TableFunctionRefused                            // carries a table identity: refused as "not accepted"
	TableFunctionDataOnly                           // produces rows from its arguments only: allowed
	TableFunctionExternal                           // reads external data: non-goal, unchanged
)

var refusedTableFunctions = map[string]bool{
	"merge": true, "remote": true, "remotesecure": true, "cluster": true, "clusterallreplicas": true,
	"loop": true, "dictionary": true, "mergetreeindex": true, "mergetreeprojection": true,
	"timeseriesdata": true, "timeseriestags": true, "timeseriesmetrics": true, "timeseriesselector": true,
	"prometheusquery": true, "prometheusqueryrange": true, "clickhouse": true,
	"mysql": true, "postgresql": true, "mongodb": true, "jdbc": true, "odbc": true,
	"executable": true, "fuzzquery": true, "fuzzjson": true,
}

var dataOnlyTableFunctions = map[string]bool{
	"numbers": true, "numbers_mt": true, "generate_series": true, "generateseries": true,
	"generaterandom": true, "zeros": true, "zeros_mt": true, "null": true, "values": true,
	"format": true, "input": true, "view": true,
}

var externalTableFunctions = map[string]bool{
	"url": true, "s3": true, "gcs": true, "oss": true, "cosn": true, "file": true, "hdfs": true,
	"azureblobstorage": true, "iceberg": true, "deltalake": true, "hudi": true,
	"urlcluster": true, "s3cluster": true, "filecluster": true, "hdfscluster": true,
	"azureblobstoragecluster": true, "icebergcluster": true, "deltalakecluster": true, "hudicluster": true,
	"sqlite": true, "redis": true,
}

// ClassifyTableFunction classifies by ClickHouse's case-insensitive name.
func ClassifyTableFunction(name string) TableFunctionClass {
	lower := strings.ToLower(name)
	switch {
	case refusedTableFunctions[lower] || strings.HasPrefix(lower, "mergetree"):
		return TableFunctionRefused
	case dataOnlyTableFunctions[lower]:
		return TableFunctionDataOnly
	case externalTableFunctions[lower]:
		return TableFunctionExternal
	default:
		return TableFunctionUnknown
	}
}

// allowedTableEngines lists the spec's allowed engines in ClickHouse's own
// spelling: engine names are case-sensitive in ClickHouse (`ENGINE = memory`
// is an unknown engine), so a name is matched exactly.
var allowedTableEngines = map[string]bool{
	"MergeTree": true, "ReplacingMergeTree": true, "SummingMergeTree": true, "AggregatingMergeTree": true,
	"CollapsingMergeTree": true, "VersionedCollapsingMergeTree": true, "GraphiteMergeTree": true,
	"Memory": true, "Log": true, "TinyLog": true, "StripeLog": true, "Null": true, "Set": true, "Join": true,
	"View": true, "MaterializedView": true, "LiveView": true,
}

// TableEngineClass is the spec 2026-09-26 T5 classification of a table
// engine name.
type TableEngineClass int

const (
	TableEngineRefused      TableEngineClass = iota // not on the list: "not accepted"
	TableEngineAllowedClass                         // on the list, in ClickHouse's spelling
	TableEngineUnknown                              // a list name in the wrong case: ClickHouse does not know it
)

// ClassifyTableEngine classifies a CREATE TABLE / materialized-view engine.
// A Replicated* MergeTree engine is allowed only without arguments. A name
// that matches an allowed engine only case-insensitively is Unknown (spec
// 2026-09-26 R12): ClickHouse rejects it as an unknown engine.
func ClassifyTableEngine(name string, argCount int) TableEngineClass {
	if strings.HasPrefix(name, "Replicated") && allowedTableEngines[strings.TrimPrefix(name, "Replicated")] {
		if argCount == 0 {
			return TableEngineAllowedClass
		}
		return TableEngineRefused
	}
	if allowedTableEngines[name] {
		return TableEngineAllowedClass
	}
	lower := strings.ToLower(name)
	for allowed := range allowedTableEngines {
		if strings.ToLower(allowed) == lower || "replicated"+strings.ToLower(allowed) == lower {
			return TableEngineUnknown
		}
	}
	return TableEngineRefused
}

// TableEngineAllowed reports whether a CREATE TABLE engine is on the spec's
// list, in ClickHouse's spelling (ClassifyTableEngine).
func TableEngineAllowed(name string, argCount int) bool {
	return ClassifyTableEngine(name, argCount) == TableEngineAllowedClass
}

var refusedTableSettings = map[string]bool{"disk": true, "storage_policy": true}

// RefusedTableSetting reports whether a CREATE / ALTER … MODIFY SETTING name
// stores data by reference.
func RefusedTableSetting(name string) bool { return refusedTableSettings[strings.ToLower(name)] }

// Cross-engine messages (spec 2026-09-26 §5).
const UnsupportedStatementMessage = "statement is not supported"

func TableFunctionRefusedMessage(name string) string {
	return "table function " + name + " is not accepted"
}
func TableFunctionUnknownMessage(name string) string {
	return "table function " + name + " is not recognised"
}
func TableEngineRefusedMessage(name string) string {
	return "table engine " + name + " is not accepted"
}
func TableEngineUnknownMessage(name string) string {
	return "table engine " + name + " is not recognised"
}
func TableSettingRefusedMessage(name string) string {
	return "table setting " + name + " is not accepted"
}

// deploymentIdentityFunctions name the shared ClickHouse deployment behind
// housegate rather than the caller's own view of it: the physical session
// database, the shared ClickHouse user, the host, the server UUID and its
// macros. Matched case-insensitively. version(), displayName() and
// timezone() are not here: the server hello already gives them to the client.
// The names are ClickHouse 26.3's registrations and aliases (DATABASE,
// SCHEMA and current_database for currentDatabase; currentSchemas and
// current_schemas; user and current_user for currentUser; fullHostName for
// FQDN; authenticatedUser and its alias authUser), plus ClickHouse 26.8's
// pgGetUserById / pg_get_userbyid (the session's initial user) and
// currentRequestURL / currentHandler (the gateway's HTTP request), as in
// rewriter-grpc firstDeploymentIdentityCall.
var deploymentIdentityFunctions = map[string]bool{
	"currentdatabase": true, "database": true, "schema": true, "current_database": true,
	"currentschemas": true, "current_schemas": true,
	"currentuser": true, "user": true, "current_user": true, "session_user": true,
	"authenticateduser": true, "authuser": true,
	"hostname": true, "fqdn": true, "fullhostname": true, "serveruuid": true, "getmacro": true,
	"pggetuserbyid": true, "pg_get_userbyid": true, "currentrequesturl": true, "currenthandler": true,
}

// tableNameIntroducers are the words after which a name followed by "(" is a
// table and its column list rather than a call.
var tableNameIntroducers = map[string]bool{
	"into": true, "table": true, "exists": true, "view": true, "dictionary": true, "to": true,
}

// bareDeploymentIdentityNames are the deployment-identity functions
// ClickHouse's analyzer also resolves without parentheses (they allow
// omitting them) when no column or alias of that name is in scope, spelled
// so that an ordinary column is unlikely to carry the name. The aliases
// user, DATABASE and SCHEMA resolve the same way but are ordinary column
// names, so a bare one is left alone (a known residual: it reaches the
// function only when the statement has no such column).
var bareDeploymentIdentityNames = map[string]bool{
	"currentdatabase": true, "current_database": true, "currentuser": true, "current_user": true,
}

// DeploymentIdentityFunction reports whether a call of name reveals the
// shared deployment (deploymentIdentityFunctions).
func DeploymentIdentityFunction(name string) bool {
	return deploymentIdentityFunctions[strings.ToLower(name)]
}

// FunctionNotAccessibleMessage is the refusal of a deployment-identity call.
func FunctionNotAccessibleMessage(name string) string {
	return "function " + name + " is not accessible"
}

// deploymentIdentityColumn is the virtual column ClickHouse 26.8 gives
// MergeTree, Memory, Log, View, MaterializedView and system tables, holding
// the physical database name. Matched as written: ClickHouse column names
// are case-sensitive, so `_DATABASE` is an ordinary column.
const deploymentIdentityColumn = "_database"

// HasDeploymentIdentityColumn reports whether ast references the
// deploymentIdentityColumn as a column, bare or qualified (`t._database`),
// anywhere in the statement. Mirrors rewriter-grpc
// firstDeploymentIdentityColumn; a string literal or an alias that only
// spells the name is not a column reference.
func HasDeploymentIdentityColumn(ast AST) (bool, error) {
	if len(ast) == 0 {
		return false, nil
	}
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return false, fmt.Errorf("engine: decode: %w", err)
	}
	return hasIdentityColumn(root), nil
}

func hasIdentityColumn(node any) bool {
	switch n := node.(type) {
	case []any:
		for _, item := range n {
			if hasIdentityColumn(item) {
				return true
			}
		}
	case map[string]any:
		if col, ok := n["column"].(map[string]any); ok {
			if nm, ok := col["name"].(map[string]any); ok && nm["name"] == deploymentIdentityColumn {
				return true
			}
		}
		for _, v := range n {
			if hasIdentityColumn(v) {
				return true
			}
		}
	}
	return false
}

// ColumnNotAccessibleMessage is the refusal of a deployment-identity column.
func ColumnNotAccessibleMessage(name string) string {
	return "virtual column " + name + " is not accessible"
}

// DeploymentIdentityColumnName is the column HasDeploymentIdentityColumn
// looks for, for the refusal message.
func DeploymentIdentityColumnName() string { return deploymentIdentityColumn }

// FirstDeploymentIdentityCall returns the name of the first call in sql, in
// text order, of a deployment-identity function: a bare word or a quoted
// identifier directly followed by "(" (the call shape callSpellings reads).
// A quoted name is returned unquoted. ok is false when there is none.
func FirstDeploymentIdentityCall(e Engine, sql string) (name string, ok bool, err error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return "", false, err
	}
	for i := 0; i+1 < len(toks); i++ {
		tk := toks[i]
		if toks[i+1].TokenType != "L_PAREN" || tk.Source == "" {
			continue
		}
		if i > 0 && toks[i-1].TokenType == "DOT" {
			continue
		}
		// A table name before its column list (`INSERT INTO user (a)`,
		// `CREATE TABLE IF NOT EXISTS schema (…)`, an MV's `TO database (…)`)
		// is not a call, as in rewriter-grpc.
		if i > 0 && !isQuotedLexeme(toks[i-1].TokenType) && tableNameIntroducers[strings.ToLower(toks[i-1].Source)] {
			continue
		}
		switch {
		case isQuotedLexeme(tk.TokenType) && isQuotedIdentifier(tk.Source):
			unquoted, ok := clickhouseUnquote(tk.Source)
			if !ok {
				unquoted = tk.Text
			}
			if DeploymentIdentityFunction(unquoted) {
				return unquoted, true, nil
			}
		case !isQuotedLexeme(tk.TokenType) && isWordStart(tk.Source):
			if DeploymentIdentityFunction(tk.Source) {
				return tk.Source, true, nil
			}
		}
	}
	return "", false, nil
}

// FirstDeploymentIdentityName returns the first deployment-identity reference
// in a statement: a call (FirstDeploymentIdentityCall) or else an unqualified
// bare name of bareDeploymentIdentityNames in an expression of ast (a column
// reference without a table, or Polyglot's parenthesis-free function node),
// found in a fixed walk order. Mirrors rewriter-grpc
// firstDeploymentIdentityCall / firstBareDeploymentIdentityName.
func FirstDeploymentIdentityName(e Engine, ast AST, sql string) (name string, ok bool, err error) {
	if name, ok, err = FirstDeploymentIdentityCall(e, sql); err != nil || ok {
		return name, ok, err
	}
	if len(ast) == 0 {
		return "", false, nil
	}
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return "", false, fmt.Errorf("engine: decode: %w", err)
	}
	name, ok = firstBareIdentityName(root)
	return name, ok, nil
}

func firstBareIdentityName(node any) (string, bool) {
	switch n := node.(type) {
	case []any:
		for _, item := range n {
			if name, ok := firstBareIdentityName(item); ok {
				return name, true
			}
		}
	case map[string]any:
		if col, ok := n["column"].(map[string]any); ok && col["table"] == nil {
			if nm, ok := col["name"].(map[string]any); ok {
				if s, ok := nm["name"].(string); ok && bareDeploymentIdentityNames[strings.ToLower(s)] {
					return s, true
				}
			}
		}
		if fn, ok := n["function"].(map[string]any); ok && fn["no_parens"] == true {
			if s, ok := fn["name"].(string); ok && bareDeploymentIdentityNames[strings.ToLower(s)] {
				return s, true
			}
		}
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if name, ok := firstBareIdentityName(n[k]); ok {
				return name, true
			}
		}
	}
	return "", false
}
