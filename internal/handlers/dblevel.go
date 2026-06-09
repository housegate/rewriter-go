package handlers

import (
	"sort"
	"strings"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
)

// RewriteDBLevel ports the database-level handlers (USE / SHOW TABLES / SHOW
// DATABASES / CREATE DATABASE / DROP DATABASE). Returns (resp, handled, err) with
// the same contract as RewriteWrite. native.go calls it after RewriteWrite and
// before the SELECT/pass-through fallback.
//
// CREATE DATABASE / DROP DATABASE are validated against database_map and rewritten
// to a synthetic debug SELECT (the proxy does its own database bookkeeping; we
// never ship the DDL to the physical ClickHouse). Everything else this handler
// doesn't recognize falls through (handled=false) so the caller routes it onward
// (native wires RewriteDBLevel after RewriteWrite in Task 7).
func RewriteDBLevel(e engine.Engine, ast engine.AST, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, bool, error) {
	kind, err := engine.NodeKind(ast)
	if err != nil {
		return nil, false, err
	}
	dyn := nameresolve.FindDynamicArgs(opts)

	switch kind {
	case engine.NodeCreateDB:
		return dispatchCreateDatabase(e, ast, dyn)
	case engine.NodeDropDB:
		return dispatchDropDatabase(e, ast, dyn)
	case engine.NodeCommand:
		info, perr := engine.ParseDBLevel(e, sql)
		if perr != nil {
			return nil, false, perr
		}
		if info.Kind == engine.DBUse {
			return dispatchUse(e, ast, sql, info, dyn)
		}
		if info.Kind == engine.DBShow {
			if info.ShowWhat == "DATABASES" {
				return dispatchShowDatabases(e, ast, sql, info, dyn)
			}
			if info.ShowWhat == "CREATE" {
				// SHOW CREATE {TABLE|DATABASE|VIEW|...} is NOT an ASTShowTablesQuery in
				// ClickHouse — C++ routes it to a dedicated show_create handler (Phase 4),
				// not show_tables. Don't let dispatchShowTables mis-stamp it SHOW_TABLES;
				// defer so the native pass-through classifies it (SHOW_CREATE_TABLE) until
				// the Phase-4 handler lands.
				return nil, false, nil
			}
			return dispatchShowTables(e, ast, sql, info, dyn)
		}
	}
	return nil, false, nil // not handled yet → caller falls through
}

func newDBResp(stmt pb.StatementType) *pb.RewriteSQLResponse {
	return &pb.RewriteSQLResponse{
		Code: pb.RewriteCode_Success, Message: "success",
		StatementType: stmt, DatabaseRewrites: map[string]string{},
	}
}

func rejectDBInvalid(resp *pb.RewriteSQLResponse, msg string) {
	resp.Code, resp.Message = pb.RewriteCode_InvalidRewriteRequest, msg
}

func rejectDBUnsupported(resp *pb.RewriteSQLResponse, msg string) {
	resp.Code, resp.Message = pb.RewriteCode_UnsupportedStatement, msg
}

// recordDatabaseRewrite appends {origin → new} to database_rewrites (no-op when
// origin is empty or unchanged). Mirrors C++ recordDatabaseRewrite
// (name_rewrite.cc:349). Shared by USE / SHOW.
func recordDatabaseRewrite(resp *pb.RewriteSQLResponse, origin, newDB string) {
	if origin == "" || origin == newDB {
		return
	}
	if resp.DatabaseRewrites == nil {
		resp.DatabaseRewrites = map[string]string{}
	}
	resp.DatabaseRewrites[origin] = newDB
}

// recordAccessedDatabase appends one db-level AccessedTable (original_table="").
// physical = ResolvePhysicalDatabase(target) when resolvable, else "". Mirrors C++
// recordAccessedDatabase (name_rewrite.h:254-276).
func recordAccessedDatabase(resp *pb.RewriteSQLResponse, target string, dyn *pb.RewriteTableDynamicArgs) {
	phys := ""
	if dyn != nil {
		if p, ok := nameresolve.ResolvePhysicalDatabase(target, dyn); ok {
			phys = p
		}
	}
	resp.OriginalAccessedTables = append(resp.OriginalAccessedTables, &pb.AccessedTable{
		OriginalDatabase: target, OriginalTable: "",
		LogicalDatabase: target, PhysicalDatabase: phys, IsRemote: false,
	})
}

// dispatchCreateDatabase ports the C++ CREATE DATABASE branch (writes.cc:277-345).
// CREATE DATABASE is never shipped to the physical ClickHouse — the proxy does its
// own database bookkeeping. We validate the request against database_map /
// known_physical_databases and rewrite to a debug `SELECT '<canonical DDL>' AS
// cdstmt` so the user's intent lands in query_log. recordAccessedDatabase runs
// BEFORE validation so a rejected request still surfaces the target. The debug
// literal embeds Generate(ast) (canonical formatAst), NOT the raw sql.
func dispatchCreateDatabase(e engine.Engine, ast engine.AST, dyn *pb.RewriteTableDynamicArgs) (*pb.RewriteSQLResponse, bool, error) {
	resp := newDBResp(pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE)
	target, ifNotExists, _, err := engine.DatabaseTarget(ast)
	if err != nil {
		return nil, false, err
	}
	if dyn == nil {
		rejectDBUnsupported(resp, "CREATE DATABASE requires a TableNameRewrite/dynamic_args option to validate against")
		return resp, true, nil
	}
	recordAccessedDatabase(resp, target, dyn) // before validation
	if phys, hit := dyn.GetDatabaseMap()[target]; hit && !ifNotExists {
		rejectDBInvalid(resp, "CREATE DATABASE target '"+target+"' already exists (mapped to physical '"+phys+"'); use IF NOT EXISTS to suppress this error")
		return resp, true, nil
	}
	if phys := dyn.GetUpstreamPhysicalDatabaseInContext(); phys != "" {
		known := false
		for _, p := range dyn.GetKnownPhysicalDatabases() {
			if p == phys {
				known = true
				break
			}
		}
		if !known {
			rejectDBInvalid(resp, "CREATE DATABASE: upstream_physical_database_in_context '"+phys+"' is not in known_physical_databases")
			return resp, true, nil
		}
	}
	gen, gerr := e.Generate(ast)
	if gerr != nil {
		return nil, false, gerr
	}
	resp.SqlAfterRewrite = "SELECT '" + escapeSQLLiteral(gen) + "' AS cdstmt"
	return resp, true, nil
}

// dispatchDropDatabase ports the C++ DROP DATABASE branch (writes.cc:403-463).
// Symmetric to CREATE: never emitted against the physical CH (a logical DB shares
// its physical with siblings via prefix-sharing; a CH-level DROP DATABASE would
// nuke unrelated tenants). On a database_map hit it records the logical→physical
// rewrite so an external GC can reclaim the prefixed tables asynchronously; a miss
// without IF EXISTS rejects (we don't manage that logical DB). The result is a
// debug `SELECT '<canonical DDL>' AS ddstmt`.
func dispatchDropDatabase(e engine.Engine, ast engine.AST, dyn *pb.RewriteTableDynamicArgs) (*pb.RewriteSQLResponse, bool, error) {
	resp := newDBResp(pb.StatementType_STATEMENT_TYPE_DROP_DATABASE)
	target, _, ifExists, err := engine.DatabaseTarget(ast)
	if err != nil {
		return nil, false, err
	}
	if dyn == nil {
		rejectDBUnsupported(resp, "DROP DATABASE requires a TableNameRewrite/dynamic_args option to validate against")
		return resp, true, nil
	}
	recordAccessedDatabase(resp, target, dyn)
	if phys, hit := dyn.GetDatabaseMap()[target]; !hit {
		if !ifExists {
			rejectDBInvalid(resp, "DROP DATABASE target '"+target+"' is not in database_map; use IF EXISTS to suppress this error")
			return resp, true, nil
		}
	} else {
		recordDatabaseRewrite(resp, target, phys)
	}
	gen, gerr := e.Generate(ast)
	if gerr != nil {
		return nil, false, gerr
	}
	resp.SqlAfterRewrite = "SELECT '" + escapeSQLLiteral(gen) + "' AS ddstmt"
	return resp, true, nil
}

// passthroughDB regenerates the original AST (canonical form) for a db-level
// passthrough; on a generate hiccup it echoes the original sql.
func passthroughDB(e engine.Engine, ast engine.AST, sql string, resp *pb.RewriteSQLResponse) (*pb.RewriteSQLResponse, bool, error) {
	if gen, gerr := e.Generate(ast); gerr == nil && gen != "" {
		resp.SqlAfterRewrite = gen
	} else {
		resp.SqlAfterRewrite = sql
	}
	return resp, true, nil
}

// dispatchUse ports use.cc handleUseQuery. No dynamic_args → passthrough.
// Unresolvable physical → InvalidRewriteRequest. Logical mapped to a remote
// upstream → UnsupportedStatement (USE has no remote analog). physical != origin
// → `USE <physical>` + recordDatabaseRewrite(origin→physical). Else passthrough.
func dispatchUse(e engine.Engine, ast engine.AST, sql string, info engine.DBLevelInfo, dyn *pb.RewriteTableDynamicArgs) (*pb.RewriteSQLResponse, bool, error) {
	resp := newDBResp(pb.StatementType_STATEMENT_TYPE_USE)
	origin := info.DB
	if dyn == nil {
		// No TableNameRewrite / dynamic_args in the request → passthrough.
		return passthroughDB(e, ast, sql, resp)
	}
	physical, ok := nameresolve.ResolvePhysicalDatabase(origin, dyn)
	if !ok {
		rejectDBInvalid(resp, "USE target '"+origin+"' is not in database_map and not a known physical database; user does not have this database")
		return resp, true, nil
	}
	if nameresolve.IsLogicalRemoteMapped(origin, dyn) {
		// USE has no remote analog — rewriting to `USE <local physical>` would
		// silently misroute every subsequent unqualified query to the local
		// physical instead of the remote cluster. Reject.
		rejectDBUnsupported(resp, "USE target '"+origin+"' is mapped to a remote upstream via dynamic_args.logical_database_to_remote_upstream_index; USE has no remote analog")
		return resp, true, nil
	}
	if physical != origin {
		resp.SqlAfterRewrite = "USE " + physical
		recordDatabaseRewrite(resp, origin, physical)
		return resp, true, nil
	}
	return passthroughDB(e, ast, sql, resp)
}

// dispatchShowTables ports show_tables.cc handleShowTablesQuery. Only SHOW TABLES
// proper is rewritten into a synthetic system.tables enumeration; SHOW CLUSTERS/
// DICTIONARIES/SETTINGS/MERGES/CACHES (and a no-dynamic request) pass through.
// The FROM clause (logical db) wins over upstream_logical_database_in_context; an
// unresolvable physical or a dangling remote-upstream key is InvalidRewriteRequest.
// A remote-mapped logical routes the enumeration through remote(...), but the
// (database, prefix) filter still uses the database_map physical name.
func dispatchShowTables(e engine.Engine, ast engine.AST, sql string, info engine.DBLevelInfo, dyn *pb.RewriteTableDynamicArgs) (*pb.RewriteSQLResponse, bool, error) {
	resp := newDBResp(pb.StatementType_STATEMENT_TYPE_SHOW_TABLES)
	// Only SHOW TABLES proper is rewritten; SHOW CLUSTERS/DICTIONARIES/SETTINGS/
	// MERGES/CACHES (and a no-dynamic request) pass through.
	if info.ShowWhat != "TABLES" || dyn == nil {
		return passthroughDB(e, ast, sql, resp)
	}
	fromLogical := info.DB
	if dot := strings.IndexByte(fromLogical, '.'); dot >= 0 {
		fromLogical = fromLogical[:dot]
	}
	logical := fromLogical
	if logical == "" {
		logical = dyn.GetUpstreamLogicalDatabaseInContext()
	}
	if logical == "" {
		rejectDBInvalid(resp, "SHOW TABLES has no FROM clause and no upstream_logical_database_in_context is set; caller must send `USE <db>` or use `SHOW TABLES FROM <db>`")
		return resp, true, nil
	}
	physical, ok := nameresolve.ResolvePhysicalDatabase(logical, dyn)
	if !ok {
		rejectDBInvalid(resp, "SHOW TABLES target logical database '"+logical+"' is not in database_map and not a known physical database; user does not have this database")
		return resp, true, nil
	}
	source := "system.tables"
	if key, ok := dyn.GetLogicalDatabaseToRemoteUpstreamIndex()[logical]; ok {
		up, ok := dyn.GetRemoteUpstreams()[key]
		if !ok {
			rejectDBInvalid(resp, "SHOW TABLES target logical database '"+logical+"' is mapped to remote upstream key '"+key+"' but that key is not in remote_upstreams")
			return resp, true, nil
		}
		source = "remote('" + escapeSQLLiteral(up.GetAddr()) + "', system, tables, '" + escapeSQLLiteral(up.GetUser()) + "', '" + escapeSQLLiteral(up.GetPassword()) + "')"
	}
	prefix := nameresolve.BuildDynamicTablePrefix(logical, dyn)
	ep := escapeSQLLiteral(prefix)
	ephys := escapeSQLLiteral(physical)
	resp.SqlAfterRewrite = "SELECT multiIf(startsWith(name, '" + ep + "'), substring(name, length('" + ep + "') + 1), name) AS name FROM (SELECT name FROM " + source + " WHERE database = '" + ephys + "' AND startsWith(name, '" + ep + "'))"
	recordDatabaseRewrite(resp, logical, physical)
	return resp, true, nil
}

// dispatchShowDatabases ports show_databases.cc handleShowDatabasesQuery. With no
// dynamic_args it passes through. Otherwise it enumerates the LOGICAL database
// names from database_map (sorted by logical name, since protobuf map order is
// unspecified) as a UNION ALL of synthetic `SELECT '<logical>' AS name` rows.
// Trust anchor: an entry is surfaced only when its physical DB is declared in
// known_physical_databases — otherwise it is skipped (no subquery, no
// database_rewrites entry), and physical names never leak to the caller. When no
// entry survives, an empty-body sentinel (a SELECT of the empty string with
// WHERE 0) is used. An optional outer LIKE/ILIKE clause filters the logical names.
func dispatchShowDatabases(e engine.Engine, ast engine.AST, sql string, info engine.DBLevelInfo, dyn *pb.RewriteTableDynamicArgs) (*pb.RewriteSQLResponse, bool, error) {
	resp := newDBResp(pb.StatementType_STATEMENT_TYPE_SHOW_DATABASES)
	if dyn == nil {
		return passthroughDB(e, ast, sql, resp)
	}
	// Sort database_map by logical (protobuf map order is unspecified).
	type ent struct{ logical, physical string }
	var entries []ent
	for l, p := range dyn.GetDatabaseMap() {
		entries = append(entries, ent{l, p})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].logical < entries[j].logical })

	known := map[string]bool{}
	for _, p := range dyn.GetKnownPhysicalDatabases() {
		known[p] = true
	}
	var subqueries []string
	for _, en := range entries {
		if !known[en.physical] {
			continue // trust anchor: physical must be declared known
		}
		subqueries = append(subqueries, "SELECT '"+escapeSQLLiteral(en.logical)+"' AS name")
		recordDatabaseRewrite(resp, en.logical, en.physical)
	}
	body := "SELECT '' AS name WHERE 0"
	if len(subqueries) > 0 {
		body = strings.Join(subqueries, " UNION ALL ")
	}
	resp.SqlAfterRewrite = "SELECT name FROM (" + body + ")" + buildLikeClause(info) + " ORDER BY name"
	return resp, true, nil
}

// buildLikeClause renders " WHERE name <op> '<pat>'" or "" when no LIKE. Mirrors
// the C++ buildLikeClause (show_databases.cc:20-29).
func buildLikeClause(info engine.DBLevelInfo) string {
	if !info.HasLike {
		return ""
	}
	op := "LIKE"
	if info.LikeCaseInsensitive {
		op = "ILIKE"
	}
	if info.LikeNot {
		op = "NOT " + op
	}
	return " WHERE name " + op + " '" + escapeSQLLiteral(info.Like) + "'"
}

// escapeSQLLiteral doubles every single quote for embedding inside a single-quoted
// ClickHouse string literal. Mirrors C++ escapeSqlLiteral (common.h).
func escapeSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
