// Package nameresolve maps logical (database, table) names to the physical names
// sent to ClickHouse. It is pure policy: it consumes rewriter-proto option
// types and returns plain Outcomes. It MUST NOT import internal/engine or the
// polyglot SDK.
package nameresolve

import (
	"fmt"
	"sort"
	"strings"

	"github.com/housegate/rewriter-proto/gen/pb"
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

// FindDynamicArgs returns the dynamic_args from the last TableNameRewrite option
// that carries them, or nil. Unlike FindActive, it ignores static_args entirely —
// db-level handlers (USE/SHOW/CREATE-DB/DROP-DB) only consult dynamic_args.
// Mirrors C++ findDynamicArgs.
func FindDynamicArgs(opts []*pb.RewriteOption) *pb.RewriteTableDynamicArgs {
	var found *pb.RewriteTableDynamicArgs
	for _, o := range opts {
		if o.GetOp() != pb.RewriteOp_TableNameRewrite {
			continue
		}
		if d := o.GetTableNameArgs().GetDynamicArgs(); d != nil {
			found = d
		}
	}
	return found
}

// ResolvePhysicalDatabase is the exported wrapper over resolvePhysicalDatabase
// (database_map, then known_physical passthrough). ok=false when unresolvable.
func ResolvePhysicalDatabase(logical string, a *pb.RewriteTableDynamicArgs) (string, bool) {
	return resolvePhysicalDatabase(logical, a)
}

// IsLogicalRemoteMapped reports whether logical is in
// logical_database_to_remote_upstream_index. Mirrors C++ isLogicalRemoteMapped.
func IsLogicalRemoteMapped(logical string, a *pb.RewriteTableDynamicArgs) bool {
	if logical == "" {
		return false
	}
	_, ok := a.GetLogicalDatabaseToRemoteUpstreamIndex()[logical]
	return ok
}

// BuildDynamicTablePrefix returns "<logical>[<delim><extra>...]." — the physical
// table-name prefix (everything buildDynamicTableName emits before original_table,
// including the trailing "."). NOTE: buildDynamicTableName short-circuits to "" on
// an empty original_table (the USE sentinel), so this builds the prefix directly.
// Mirrors C++ buildDynamicTablePrefix.
func BuildDynamicTablePrefix(logical string, a *pb.RewriteTableDynamicArgs) string {
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
	return b.String()
}

// Mode is the active table-rewrite mode for a statement.
type Mode int

const (
	ModeNone    Mode = iota // no TableNameRewrite option had either sub-message
	ModeStatic              // static_args present (even if all maps empty)
	ModeDynamic             // dynamic_args present, static_args absent
)

// Selection is the active rewrite policy for one statement (last-wins across options).
type Selection struct {
	Mode    Mode
	Static  *pb.RewriteTableStaticArgs
	Dynamic *pb.RewriteTableDynamicArgs
}

// Accessed is the best-effort resolution used to populate original_accessed_tables.
type Accessed struct {
	LogicalDB          string
	PhysicalDB         string
	IsRemote           bool
	IsStorageIntegrity bool
}

// FindActive scans options for the active TableNameRewrite policy. Last-wins
// across options; within an option, static_args beats dynamic_args. Options with
// neither sub-message do not reset a prior selection. name_rewrite.cc:37-61.
func FindActive(opts []*pb.RewriteOption) Selection {
	sel := Selection{Mode: ModeNone}
	for _, o := range opts {
		if o.GetOp() != pb.RewriteOp_TableNameRewrite {
			continue
		}
		na := o.GetTableNameArgs()
		if na == nil {
			continue
		}
		switch {
		case na.GetStaticArgs() != nil:
			sel = Selection{Mode: ModeStatic, Static: na.GetStaticArgs()}
		case na.GetDynamicArgs() != nil:
			sel = Selection{Mode: ModeDynamic, Dynamic: na.GetDynamicArgs()}
		}
	}
	return sel
}

// Resolve dispatches one (db, table) through the active selection.
func Resolve(db, table string, sel Selection) Outcome {
	switch sel.Mode {
	case ModeStatic:
		return LookupStatic(db, table, sel.Static)
	case ModeDynamic:
		return ApplyDynamic(db, table, sel.Dynamic)
	default:
		return Outcome{Status: StatusPassthrough}
	}
}

// ResolveAccessed computes best-effort (logical, physical, is_remote) for
// original_accessed_tables. Never rejects; leaves fields empty when unresolvable.
// name_rewrite.cc:229-288.
func ResolveAccessed(db, table string, sel Selection) Accessed {
	switch sel.Mode {
	case ModeStatic:
		// Static mode tracks no logical DB. Physical and is_remote come from the
		// same static lookup: table_map hit → origin db; remote hit → remote db +
		// is_remote; table_with_database hit → entry db; UNMATCHED → empty physical.
		o := LookupStatic(db, table, sel.Static)
		return Accessed{LogicalDB: "", PhysicalDB: o.PhysicalDB, IsRemote: o.Status == StatusRemote}
	case ModeDynamic:
		if _, ok := LookupStorageIntegrityPhysical(db, table, sel.Dynamic); ok {
			physicalDB := db
			if physicalDB == "" {
				physicalDB = sel.Dynamic.GetUpstreamLogicalDatabaseInContext()
			}
			return Accessed{PhysicalDB: physicalDB, IsStorageIntegrity: true}
		}
		logical := db
		if logical == "" {
			logical = sel.Dynamic.GetUpstreamLogicalDatabaseInContext()
		}
		phys, _ := resolvePhysicalDatabase(logical, sel.Dynamic)
		_, isRemote := sel.Dynamic.GetLogicalDatabaseToRemoteUpstreamIndex()[logical]
		_, _, isSI := LookupStorageIntegrity(db, table, sel.Dynamic)
		if isSI {
			isRemote = false
		}
		return Accessed{LogicalDB: logical, PhysicalDB: phys, IsRemote: isRemote, IsStorageIntegrity: isSI}
	default:
		return Accessed{}
	}
}

func splitQualified(name string) (string, string) {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}

// DefaultReservedRowIDColumn is the protocol row-identity column hidden from
// the logical surface (Spec G D3).
const DefaultReservedRowIDColumn = "_hg_row_id"

// LookupStorageIntegrity reports whether (db, table) — db resolved through
// upstream_logical_database_in_context when empty — is a key of
// dynamic_args.storage_integrity.tables. It is consulted BEFORE the ordinary
// dynamic resolution by every table-targeting handler; the SI mapping wins.
// It never rejects: an unqualified target with no context simply misses.
func LookupStorageIntegrity(db, table string, a *pb.RewriteTableDynamicArgs) (*pb.StorageIntegrityArgs_Table, string, bool) {
	tables := a.GetStorageIntegrity().GetTables()
	if len(tables) == 0 || table == "" {
		return nil, "", false
	}
	logical := db
	if logical == "" {
		logical = a.GetUpstreamLogicalDatabaseInContext()
	}
	if logical == "" {
		return nil, "", false
	}
	key := logical + "." + table
	tbl, ok := tables[key]
	if !ok || tbl == nil {
		return nil, "", false
	}
	return tbl, key, true
}

// LookupStorageIntegrityPhysical reports whether the caller addressed one of
// the protocol-owned safe/unsafe physical table names directly. Those names are
// never part of the public SQL surface, even when the physical database is not
// listed in known_physical_databases.
func LookupStorageIntegrityPhysical(db, table string, a *pb.RewriteTableDynamicArgs) (string, bool) {
	if a == nil || table == "" {
		return "", false
	}
	effectiveDB := db
	if effectiveDB == "" {
		effectiveDB = a.GetUpstreamLogicalDatabaseInContext()
	}
	if effectiveDB == "" {
		return "", false
	}
	for logicalKey, tbl := range a.GetStorageIntegrity().GetTables() {
		if tbl == nil {
			continue
		}
		for _, physical := range []string{tbl.GetSafeTable(), tbl.GetUnsafeTable()} {
			physicalDB, _, ok := exactQualifiedTable(physical)
			if ok && effectiveDB == physicalDB {
				return logicalKey, true
			}
		}
	}
	return "", false
}

// IsStorageIntegrityPhysicalDatabase reports whether db is one of the
// configured safe/unsafe physical namespaces. The reservation is database-wide:
// once a database hosts a protocol-owned SI table, no user-authored SQL may
// address any object in that database directly (including table functions and
// database-scope DDL/DCL).
func IsStorageIntegrityPhysicalDatabase(db string, a *pb.RewriteTableDynamicArgs) bool {
	if a == nil || db == "" {
		return false
	}
	for _, tbl := range a.GetStorageIntegrity().GetTables() {
		if tbl == nil {
			continue
		}
		for _, physical := range []string{tbl.GetSafeTable(), tbl.GetUnsafeTable()} {
			physicalDB, _, ok := exactQualifiedTable(physical)
			if ok && db == physicalDB {
				return true
			}
		}
	}
	return false
}

// IsStorageIntegrityLogicalDatabase reports whether db owns at least one
// storage_integrity.tables key. Namespace-bearing functions and table engines
// are not passed through the ordinary table rewriter, so a database-level match
// must be reserved even when their table argument is a regexp or expression.
func IsStorageIntegrityLogicalDatabase(db string, a *pb.RewriteTableDynamicArgs) bool {
	if a == nil || db == "" {
		return false
	}
	for key, tbl := range a.GetStorageIntegrity().GetTables() {
		if tbl == nil {
			continue
		}
		logicalDB, _, ok := exactQualifiedTable(key)
		if ok && logicalDB == db {
			return true
		}
	}
	return false
}

// StorageIntegrityPhysicalDatabases returns the configured protocol-owned
// safe/unsafe database namespaces in deterministic order. It is used to attach
// conservative SI classification metadata when a table-function database
// expression cannot be resolved statically.
func StorageIntegrityPhysicalDatabases(a *pb.RewriteTableDynamicArgs) []string {
	seen := map[string]bool{}
	for _, tbl := range a.GetStorageIntegrity().GetTables() {
		if tbl == nil {
			continue
		}
		for _, physical := range []string{tbl.GetSafeTable(), tbl.GetUnsafeTable()} {
			db, _, ok := exactQualifiedTable(physical)
			if ok {
				seen[db] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for db := range seen {
		out = append(out, db)
	}
	sort.Strings(out)
	return out
}

// AuthorizeStorageIntegrityLogical requires the logical database selected by a
// storage_integrity.tables key to be present in the account-filtered
// database_map. known_physical_databases is deliberately not an authorization
// source for a global logical SI key; it only preserves ordinary direct-physical
// compatibility outside the protocol-owned safe/unsafe namespace.
func AuthorizeStorageIntegrityLogical(db string, a *pb.RewriteTableDynamicArgs) (string, bool) {
	if a == nil {
		return "", false
	}
	logical := db
	if logical == "" {
		logical = a.GetUpstreamLogicalDatabaseInContext()
	}
	if logical == "" {
		return "", false
	}
	_, ok := a.GetDatabaseMap()[logical]
	return logical, ok
}

// ReservedRowIDColumn returns storage_integrity.reserved_row_id_column, or the
// protocol default when unset.
func ReservedRowIDColumn(a *pb.RewriteTableDynamicArgs) string {
	if rid := a.GetStorageIntegrity().GetReservedRowIdColumn(); rid != "" {
		return rid
	}
	return DefaultReservedRowIDColumn
}

// StorageIntegrityWriteRejectMessage is the shared (Go + C++) message for any
// non-lane write/DDL touching an SI table (Spec G §4.4).
func StorageIntegrityWriteRejectMessage(logicalKey string) string {
	return "storage-integrity table " + logicalKey + " accepts writes only through the signed statement lane"
}

func StorageIntegrityUnauthorizedMessage(logical string) string {
	return "storage-integrity logical database " + logical + " is not authorized by database_map"
}

func StorageIntegrityPhysicalRejectMessage(physical string) string {
	return "storage-integrity physical table " + physical + " is not directly addressable"
}

func StorageIntegrityPhysicalDatabaseRejectMessage(physical string) string {
	return "storage-integrity physical database " + physical + " is not directly addressable"
}

func StorageIntegrityTableFunctionNamespaceRejectMessage(physical string) string {
	return "storage-integrity table function namespace " + physical + " is not directly addressable"
}

func StorageIntegrityUnresolvedTableFunctionNamespaceRejectMessage() string {
	return "storage-integrity table function namespace is not statically resolvable"
}

// ValidateStorageIntegrity checks the v1 configuration before the service
// publishes an acknowledgement. Invalid entries must be a structured request
// rejection, never a nil/non-SI fallthrough or a later parser error.
func ValidateStorageIntegrity(a *pb.RewriteTableDynamicArgs) error {
	si := a.GetStorageIntegrity()
	if si == nil {
		return nil
	}
	rid := si.GetReservedRowIdColumn()
	if rid != "" && !simpleIdentifier(rid) {
		return fmt.Errorf("storage-integrity reserved_row_id_column must be a simple identifier")
	}
	switch si.GetReadMode() {
	case pb.StorageIntegrityArgs_READ_MODE_UNSPECIFIED,
		pb.StorageIntegrityArgs_READ_MODE_SAFE,
		pb.StorageIntegrityArgs_READ_MODE_UNSAFE_LATEST:
	default:
		return fmt.Errorf("storage-integrity read_mode %d is not supported", si.GetReadMode())
	}
	keys := make([]string, 0, len(si.GetTables()))
	for key := range si.GetTables() {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, _, ok := exactQualifiedTable(key); !ok {
			return fmt.Errorf("storage-integrity logical table key %s must have exact <database>.<table> shape", key)
		}
		tbl := si.GetTables()[key]
		if tbl == nil {
			return fmt.Errorf("storage-integrity table %s must not be nil", key)
		}
		if tbl.GetSafeTable() == "" || tbl.GetUnsafeTable() == "" {
			return fmt.Errorf("storage-integrity table %s requires non-empty safe_table and unsafe_table", key)
		}
		if _, _, ok := exactQualifiedTable(tbl.GetSafeTable()); !ok {
			return fmt.Errorf("storage-integrity table %s safe_table %s must have exact <database>.<table> shape", key, tbl.GetSafeTable())
		}
		if _, _, ok := exactQualifiedTable(tbl.GetUnsafeTable()); !ok {
			return fmt.Errorf("storage-integrity table %s unsafe_table %s must have exact <database>.<table> shape", key, tbl.GetUnsafeTable())
		}
	}
	return nil
}

func exactQualifiedTable(s string) (db, table string, ok bool) {
	if strings.Count(s, ".") != 1 {
		return "", "", false
	}
	db, table = splitQualified(s)
	return db, table, simpleIdentifier(db) && simpleIdentifier(table)
}

func simpleIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alpha := r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		digit := r >= '0' && r <= '9'
		if !alpha && !(i > 0 && digit) {
			return false
		}
	}
	return true
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
