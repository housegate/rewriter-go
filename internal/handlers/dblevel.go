package handlers

import (
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
// Incremental wiring (Phase-3 Task 3): only USE is dispatched here today. The
// CREATE DATABASE / DROP DATABASE branches land in Task 6 and the SHOW TABLES /
// SHOW DATABASES branches in Tasks 4-5; until then those kinds fall through
// (handled=false) so nothing regresses (Phase-2 RewriteWrite still rejects
// create/drop-db; SHOW passes through; native routing to RewriteDBLevel is wired
// in Task 7).
func RewriteDBLevel(e engine.Engine, ast engine.AST, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, bool, error) {
	kind, err := engine.NodeKind(ast)
	if err != nil {
		return nil, false, err
	}
	dyn := nameresolve.FindDynamicArgs(opts)

	switch kind {
	// case engine.NodeCreateDB: → Task 6 (dispatchCreateDatabase)
	// case engine.NodeDropDB:   → Task 6 (dispatchDropDatabase)
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
				return nil, false, nil // SHOW DATABASES → Task 5 (falls through for now)
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

// escapeSQLLiteral doubles every single quote for embedding inside a single-quoted
// ClickHouse string literal. Mirrors C++ escapeSqlLiteral (common.h).
func escapeSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
