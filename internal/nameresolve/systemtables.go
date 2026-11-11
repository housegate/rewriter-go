package nameresolve

import "sort"

// SystemDatabase is ClickHouse's `system` database. Every housegate tenant
// reaches ClickHouse as one shared user, so ClickHouse's own per-user filters
// on system tables (processes shows only the user's queries, tables only the
// databases the user can see) filter nothing between tenants. Caller SQL may
// therefore name only the system tables on systemTableAllowlist (spec
// 2026-09-26 §5, "The system database", step 1).
//
// ClickHouse database and table names are case-sensitive (measured on 26.2:
// SYSTEM.processes is UNKNOWN_DATABASE, system.Tables is UNKNOWN_TABLE), so
// the database must be spelled exactly `system` and the table must match an
// allowlist entry exactly. `information_schema` / `INFORMATION_SCHEMA` are
// separate databases; every table of them is refused (MetadataDatabaseRefused).
const SystemDatabase = "system"

// systemTableAllowlist is the closed set of system tables caller SQL may
// name. Every other name, including a table a future ClickHouse release
// adds, is refused.
//
//   - Class (b), static or server-global information that shows nothing of
//     another tenant.
//   - clusters: global topology, read by the Sentio driver on every connect.
//   - The class (c) per-database metadata tables the Sentio driver reads by
//     physical name. They still show every tenant's names (and parts
//     partition values, mutations command text): a known residual that
//     step 2 (housegate/housegate#218) closes by serving them as a filtered
//     logical rewrite. The class (c) tables no known client needs
//     (systemTableAwaitingFilteredRead) are refused until then.
var systemTableAllowlist = map[string]bool{
	// class (b)
	"aggregate_function_combinators": true,
	"azure_queue_settings":           true,
	"build_options":                  true,
	"codecs":                         true,
	"collations":                     true,
	"contributors":                   true,
	"dashboards":                     true,
	"data_type_families":             true,
	"database_engines":               true,
	"formats":                        true,
	"functions":                      true,
	"jemalloc_bins":                  true,
	"jemalloc_stats":                 true,
	"keywords":                       true,
	"licenses":                       true,
	"merge_tree_settings":            true,
	"numbers":                        true,
	"numbers_mt":                     true,
	"one":                            true,
	"primes":                         true,
	"privileges":                     true,
	"replicated_merge_tree_settings": true,
	"s3_queue_settings":              true,
	"settings":                       true,
	"settings_changes":               true,
	"table_engines":                  true,
	"table_functions":                true,
	"time_zones":                     true,
	"tokenizers":                     true,
	"unicode":                        true,
	"warnings":                       true,
	"zeros":                          true,
	"zeros_mt":                       true,
	// class (b*), read by the Sentio driver
	"clusters": true,
	// class (c) read by the Sentio driver, passthrough until step 2
	"tables":                true,
	"columns":               true,
	"data_skipping_indices": true,
	"projections":           true,
	"parts":                 true,
	"databases":             true,
	"mutations":             true,
}

// systemTableAwaitingFilteredRead are the class (c) tables no known client
// reads through housegate by physical name: refused (they show every
// tenant's database, table and column names, and partition values) until
// step 2 serves them as filtered logical reads. Their refusal names that
// path. clickhouse-client 25.8+ reads system.completions for interactive
// suggestions and only prints a warning when the read is refused.
var systemTableAwaitingFilteredRead = map[string]bool{
	"completions":              true,
	"parts_columns":            true,
	"projection_parts":         true,
	"projection_parts_columns": true,
}

// FilteredReadIssue is where the tenant-filtered metadata reads that would
// replace a refused class (c) table or information_schema are tracked.
const FilteredReadIssue = "housegate/housegate#218"

// SystemTableAllowed reports whether caller SQL may name system.<table>.
func SystemTableAllowed(table string) bool {
	return systemTableAllowlist[table]
}

// SystemTableAllowlist returns the allowlist, sorted.
func SystemTableAllowlist() []string {
	out := make([]string, 0, len(systemTableAllowlist))
	for t := range systemTableAllowlist {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// SystemTableRefusedMessage is the cross-engine message for a caller-input
// reference to a system table outside the allowlist, and for a SHOW form
// that reads one. table is the name as the statement spells it after
// ClickHouse's identifier decoding (a merge() / Merge regex is quoted
// verbatim).
func SystemTableRefusedMessage(table string) string {
	msg := "system table " + SystemDatabase + "." + table + " is not accessible"
	if systemTableAwaitingFilteredRead[table] {
		msg += "; tenant-filtered metadata reads are tracked in " + FilteredReadIssue
	}
	return msg
}

// MetadataDatabaseRefused reports whether db, a decoded database name, is
// ClickHouse's information_schema in either spelling ClickHouse accepts.
// Its views read system.tables / system.columns / system.databases under
// the shared user, so every table of it shows every tenant's names; caller
// SQL may name none of them, whatever the table or pattern.
func MetadataDatabaseRefused(db string) bool {
	return db == "information_schema" || db == "INFORMATION_SCHEMA"
}

// MetadataDatabaseRefusedMessage is the cross-engine message for a
// caller-input reference to an information_schema table. db is spelled as
// the statement spells it after identifier decoding.
func MetadataDatabaseRefusedMessage(db string) string {
	return "database " + db + " is not accessible; tenant-filtered metadata reads are tracked in " + FilteredReadIssue
}
