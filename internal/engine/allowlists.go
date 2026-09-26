package engine

import "strings"

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

var allowedTableEngines = map[string]bool{
	"mergetree": true, "replacingmergetree": true, "summingmergetree": true, "aggregatingmergetree": true,
	"collapsingmergetree": true, "versionedcollapsingmergetree": true, "graphitemergetree": true,
	"memory": true, "log": true, "tinylog": true, "stripelog": true, "null": true, "set": true, "join": true,
	"view": true, "materializedview": true, "liveview": true,
}

// TableEngineAllowed reports whether a CREATE TABLE engine is on the spec's
// list. A Replicated* MergeTree engine is allowed only without arguments.
func TableEngineAllowed(name string, argCount int) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "replicated") && allowedTableEngines[strings.TrimPrefix(lower, "replicated")] {
		return argCount == 0
	}
	return allowedTableEngines[lower]
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
func TableSettingRefusedMessage(name string) string {
	return "table setting " + name + " is not accepted"
}
