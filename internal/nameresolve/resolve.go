// Package nameresolve maps logical (database, table) names to the physical names
// sent to ClickHouse. It is pure policy: it consumes gen/pb option types and
// returns plain Outcomes. It MUST NOT import internal/engine or the polyglot SDK.
package nameresolve

import "github.com/housegate/rewriter-go/gen/pb"

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
