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
// separate databases and are not checked in step 1.
const SystemDatabase = "system"

// systemTableAllowlist is the closed set of system tables caller SQL may
// name. Every other name, including a table a future ClickHouse release
// adds, is refused.
//
//   - Class (b), static or server-global information that shows nothing of
//     another tenant.
//   - clusters: global topology, read by the Sentio driver on every connect.
//   - The class (c) per-database metadata tables the Sentio driver and
//     clients read by physical name. They still show every tenant's names
//     (and parts* partition values, mutations command text): a known
//     residual that step 2 closes by serving them as a filtered logical
//     rewrite.
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
	// class (c), passthrough until step 2
	"tables":                   true,
	"columns":                  true,
	"data_skipping_indices":    true,
	"projections":              true,
	"parts":                    true,
	"parts_columns":            true,
	"projection_parts":         true,
	"projection_parts_columns": true,
	"databases":                true,
	"completions":              true,
	"mutations":                true,
}

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
	return "system table " + SystemDatabase + "." + table + " is not accessible"
}
