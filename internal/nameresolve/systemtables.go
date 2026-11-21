package nameresolve

import (
	"sort"
	"strings"

	"github.com/housegate/rewriter-proto/gen/pb"
)

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
//     physical name. In dynamic mode each read of one is served from a
//     tenant-filtered derived table (SystemTableTenantFilterSQL, step 2,
//     housegate/housegate#218): only the rows of the caller's own logical
//     databases reach it, in ClickHouse's physical shape. The class (c)
//     tables no known client needs (systemTableAwaitingFilteredRead) are
//     refused.
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
	// class (c) read by the Sentio driver, served tenant-filtered (step 2)
	"tables":                true,
	"columns":               true,
	"data_skipping_indices": true,
	"projections":           true,
	"parts":                 true,
	"databases":             true,
	"mutations":             true,
}

// systemTableTenantColumn maps each allowed class (c) table to the column
// that carries a physical table name, which starts with the owning logical
// database's prefix (BuildDynamicTablePrefix). system.databases has no such
// column ("" here): its rows are filtered by `name` against the physical
// databases of the caller's database_map instead.
var systemTableTenantColumn = map[string]string{
	"tables":                "name",
	"columns":               "table",
	"data_skipping_indices": "table",
	"projections":           "table",
	"parts":                 "table",
	"mutations":             "table",
	"databases":             "",
}

// SystemTableTenantFiltered reports whether a read of system.<table> is
// served from a tenant-filtered derived table in dynamic mode (step 2,
// housegate/housegate#218). table is the decoded name.
func SystemTableTenantFiltered(table string) bool {
	_, ok := systemTableTenantColumn[table]
	return ok
}

// SystemTableTenantFilterSQL renders the derived-table body that replaces a
// read of system.<table> (SystemTableTenantFiltered) in dynamic mode:
//
//	SELECT * FROM system.<table> WHERE <filter>
//
// For a table with a tenant column the filter keeps the rows of every
// logical database in database_map: `database = '<physical>' AND
// startsWith(<column>, '<prefix>')` per logical, OR-ed in (physical,
// logical) order, each term parenthesised when there are several. For
// system.databases it is `name IN ('<physical>', …)`. An empty database_map
// (an unauthenticated session) renders `WHERE 0`, so nothing is shown.
//
// The prefix is BuildDynamicTablePrefix's, which always ends in "." while
// logical names never contain a dot, so one logical's prefix never matches
// another logical's tables. The filter sits inside the derived table, where
// no alias of the caller's query is in scope: a predicate added to the
// caller's own WHERE would resolve `database` / `name` against the caller's
// select-list aliases first (measured on 26.8.37.168: `SELECT 'l1.x' AS
// name … WHERE startsWith(name, 'l1.')` keeps every row). The body keeps
// ClickHouse's physical shape and its `*` column set; ALIAS columns
// (parts.bytes, tables.table, columns.column, databases.database) are not in
// `*`, so a caller that names one gets UNKNOWN_IDENTIFIER (a declared
// limitation). Literals are escaped with escape.
func SystemTableTenantFilterSQL(table string, a *pb.RewriteTableDynamicArgs, escape func(string) string) string {
	column := systemTableTenantColumn[table]
	type entry struct{ physical, logical string }
	var entries []entry
	physicals := map[string]bool{}
	for logical, physical := range a.GetDatabaseMap() {
		if logical == "" || physical == "" {
			continue
		}
		entries = append(entries, entry{physical, logical})
		physicals[physical] = true
	}
	body := "SELECT * FROM " + SystemDatabase + "." + table + " WHERE "
	if len(entries) == 0 {
		return body + "0"
	}
	if column == "" {
		names := make([]string, 0, len(physicals))
		for p := range physicals {
			names = append(names, "'"+escape(p)+"'")
		}
		sort.Strings(names)
		return body + "name IN (" + strings.Join(names, ", ") + ")"
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].physical != entries[j].physical {
			return entries[i].physical < entries[j].physical
		}
		return entries[i].logical < entries[j].logical
	})
	terms := make([]string, len(entries))
	for i, en := range entries {
		terms[i] = "database = '" + escape(en.physical) + "' AND startsWith(" + column + ", '" +
			escape(BuildDynamicTablePrefix(en.logical, a)) + "')"
		if len(entries) > 1 {
			terms[i] = "(" + terms[i] + ")"
		}
	}
	return body + strings.Join(terms, " OR ")
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
