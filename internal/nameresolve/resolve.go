// Package nameresolve maps logical (database, table) names to the physical names
// sent to ClickHouse. It is pure policy: it consumes gen/pb option types and
// returns plain Outcomes. It MUST NOT import internal/engine or the polyglot SDK.
package nameresolve

import (
	"strings"

	"github.com/housegate/rewriter-go/gen/pb"
)

// Status is the disposition of resolving one table target.
type Status int

const (
	StatusPassthrough       Status = iota // no match; leave as written (no error)
	StatusRewrite                         // set physical_db.new_table
	StatusRemote                          // wrap in remote(addr, physical_db, new_table, user, pw)
	StatusRemoteUnsupported               // remote hit in a context that forbids it (non-SELECT)
	StatusInvalid                         // caller policy/request is wrong → InvalidRewriteRequest
)

// Outcome is the result of resolving one (db, table) target. Mirrors the C++
// DynamicRewriteOutcome unified with the static lookup result.
type Outcome struct {
	Status         Status
	PhysicalDB     string // schema to set on the node (Rewrite/Remote)
	NewTable       string // table name to set (Rewrite/Remote); "" for db-only (USE)
	LogicalDB      string // for table_rewrites / accessed bookkeeping
	RemoteAddr     string
	RemoteUser     string
	RemotePassword string
	RejectReason   string // Status==StatusInvalid only
}

// qualify builds the map lookup key: "db.table" when db is set, else "table".
// The separator is always a literal "." (name_rewrite.cc:11-15) — never delim.
func qualify(db, table string) string {
	if db == "" {
		return table
	}
	return db + "." + table
}

// LookupStatic resolves (db, table) through the three static maps in precedence
// order: table_map → remote_table_map → table_with_database_map → passthrough.
// Mirrors lookupStaticTableRewrite / planTableRewrite.
func LookupStatic(db, table string, a *pb.RewriteTableStaticArgs) Outcome {
	key := qualify(db, table)
	if nt, ok := a.GetTableMap()[key]; ok {
		// Rename only; db qualifier preserved (new_db = origin_db).
		return Outcome{Status: StatusRewrite, PhysicalDB: db, NewTable: nt, LogicalDB: db}
	}
	if rt, ok := a.GetRemoteTableMap()[key]; ok {
		return Outcome{
			Status: StatusRemote, PhysicalDB: rt.GetDatabase(), NewTable: rt.GetTable(), LogicalDB: db,
			RemoteAddr: rt.GetAddr(), RemoteUser: rt.GetUser(), RemotePassword: rt.GetPassword(),
		}
	}
	if wd, ok := a.GetTableWithDatabaseMap()[key]; ok {
		newDB := wd.GetDatabase()
		if newDB == "" { // empty database keeps the origin db
			newDB = db
		}
		return Outcome{Status: StatusRewrite, PhysicalDB: newDB, NewTable: wd.GetTable(), LogicalDB: db}
	}
	return Outcome{Status: StatusPassthrough}
}

// resolvePhysicalDatabase maps a logical DB to its physical name, or ok=false
// when unresolvable. Order: database_map, then known_physical (passthrough).
func resolvePhysicalDatabase(logical string, a *pb.RewriteTableDynamicArgs) (string, bool) {
	if logical == "" {
		return "", false
	}
	if phys, ok := a.GetDatabaseMap()[logical]; ok {
		return phys, true
	}
	for _, k := range a.GetKnownPhysicalDatabases() {
		if k == logical {
			return logical, true
		}
	}
	return "", false
}

// buildDynamicTableName constructs "<logical>[<delim><extra>...].<original_table>".
// The separator before original_table is ALWAYS a literal "." (not delim).
// Returns "" when origin_table is "" (the USE sentinel). name_rewrite.cc:112-146.
func buildDynamicTableName(logical, originTable string, a *pb.RewriteTableDynamicArgs) string {
	if originTable == "" {
		return ""
	}
	delim := a.GetDelim()
	if delim == "" {
		delim = "_"
	}
	var b strings.Builder
	b.WriteString(logical)
	for _, extra := range a.GetExtraArguments() {
		b.WriteString(delim)
		b.WriteString(extra)
	}
	b.WriteString(".")
	b.WriteString(originTable)
	return b.String()
}

// ApplyDynamic resolves (db, table) under dynamic args. Mirrors applyDynamicRewrite.
// On any policy failure returns StatusInvalid (SELECT caller treats that as a lenient
// skip; non-SELECT as reject).
func ApplyDynamic(db, table string, a *pb.RewriteTableDynamicArgs) Outcome {
	logical := db
	if logical == "" {
		logical = a.GetUpstreamLogicalDatabaseInContext()
	}
	if logical == "" {
		return Outcome{Status: StatusInvalid, RejectReason: "unqualified target and no upstream_logical_database_in_context"}
	}
	physical, ok := resolvePhysicalDatabase(logical, a)
	if !ok {
		return Outcome{Status: StatusInvalid, RejectReason: "logical db " + logical + " not in database_map and not a known physical database"}
	}
	newTable := buildDynamicTableName(logical, table, a)
	if key, ok := a.GetLogicalDatabaseToRemoteUpstreamIndex()[logical]; ok {
		up, ok := a.GetRemoteUpstreams()[key]
		if !ok {
			return Outcome{Status: StatusInvalid, RejectReason: "remote upstream key " + key + " not in remote_upstreams"}
		}
		return Outcome{
			Status: StatusRemote, PhysicalDB: physical, NewTable: newTable, LogicalDB: logical,
			RemoteAddr: up.GetAddr(), RemoteUser: up.GetUser(), RemotePassword: up.GetPassword(),
		}
	}
	return Outcome{Status: StatusRewrite, PhysicalDB: physical, NewTable: newTable, LogicalDB: logical}
}
