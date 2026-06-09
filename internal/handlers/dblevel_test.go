package handlers

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
)

func TestRewriteDBLevel_usePhysicalRewrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "USE tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_USE {
		t.Fatalf("code=%v stmt=%v", resp.GetCode(), resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "USE testnet") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_usePassthroughNoDynamic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE tenant1")
	resp, handled, _ := RewriteDBLevel(e, ast, "USE tenant1", nil)
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v", handled, resp.GetCode())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "USE tenant1") || len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("sql=%q rewrites=%v", resp.GetSqlAfterRewrite(), resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_useSamePhysicalPassthrough(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE prod")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{KnownPhysicalDatabases: []string{"prod"}})
	resp, _, _ := RewriteDBLevel(e, ast, "USE prod", opts)
	// physical == origin → passthrough, no database_rewrites entry.
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "USE prod") || len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("sql=%q rewrites=%v", resp.GetSqlAfterRewrite(), resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_useUnresolvableInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE nope")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, _, _ := RewriteDBLevel(e, ast, "USE nope", opts)
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v", resp.GetCode())
	}
}

func TestRewriteDBLevel_useRemoteMappedUnsupported(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap:                          map[string]string{"tenant1": "testnet"},
		LogicalDatabaseToRemoteUpstreamIndex: map[string]string{"tenant1": "up0"},
	})
	resp, _, _ := RewriteDBLevel(e, ast, "USE tenant1", opts)
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("code=%v", resp.GetCode())
	}
}

func TestRewriteDBLevel_notDBLevel(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SELECT 1")
	_, handled, _ := RewriteDBLevel(e, ast, "SELECT 1", nil)
	if handled {
		t.Errorf("SELECT must not be handled by RewriteDBLevel")
	}
}

// SHOW TABLES FROM <logical> → synthetic system.tables enumeration that strips
// the per-table prefix. database_map resolves tenant1→testnet; the prefix is
// "tenant1." (logical + trailing dot, no extra_arguments).
func TestRewriteDBLevel_showTablesSynthetic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW TABLES FROM tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW TABLES FROM tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_TABLES {
		t.Fatalf("code=%v stmt=%v msg=%q", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	want := "SELECT multiIf(startsWith(name, 'tenant1.'), substring(name, length('tenant1.') + 1), name) AS name " +
		"FROM (SELECT name FROM system.tables WHERE database = 'testnet' AND startsWith(name, 'tenant1.'))"
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("sql=%q\nwant=%q", resp.GetSqlAfterRewrite(), want)
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

// Bare SHOW TABLES (no FROM) falls back to upstream_logical_database_in_context.
func TestRewriteDBLevel_showTablesUpstreamContext(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW TABLES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap:                      map[string]string{"tenant1": "testnet"},
		UpstreamLogicalDatabaseInContext: "tenant1",
	})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW TABLES", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	want := "SELECT multiIf(startsWith(name, 'tenant1.'), substring(name, length('tenant1.') + 1), name) AS name " +
		"FROM (SELECT name FROM system.tables WHERE database = 'testnet' AND startsWith(name, 'tenant1.'))"
	if resp.GetCode() != pb.RewriteCode_Success || !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("code=%v sql=%q", resp.GetCode(), resp.GetSqlAfterRewrite())
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

// Bare SHOW TABLES with neither FROM nor upstream context → InvalidRewriteRequest.
func TestRewriteDBLevel_showTablesNoContextInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW TABLES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW TABLES", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
	}
}

// SHOW CLUSTERS is not SHOW TABLES proper → passthrough (Success, stmt SHOW_TABLES,
// verbatim SHOW CLUSTERS).
func TestRewriteDBLevel_showClustersPassthrough(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW CLUSTERS")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW CLUSTERS", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_TABLES {
		t.Fatalf("code=%v stmt=%v", resp.GetCode(), resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SHOW CLUSTERS") {
		t.Errorf("sql=%q want SHOW CLUSTERS", resp.GetSqlAfterRewrite())
	}
	if len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

// TestRewriteDBLevel_showCreateDefers: SHOW CREATE TABLE is NOT an
// ASTShowTablesQuery in ClickHouse (C++ routes it to a dedicated show_create
// handler — Phase 4), so RewriteDBLevel must NOT claim it as SHOW_TABLES. It
// returns handled=false → native pass-through classifies it SHOW_CREATE_TABLE.
func TestRewriteDBLevel_showCreateDefers(t *testing.T) {
	e := newEngine(t)
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	for _, sql := range []string{"SHOW CREATE TABLE db.t", "SHOW CREATE DATABASE db"} {
		ast := mustParse(t, e, sql)
		_, handled, err := RewriteDBLevel(e, ast, sql, opts)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		if handled {
			t.Errorf("%q: handled=true, want false (SHOW CREATE must defer, not be stamped SHOW_TABLES)", sql)
		}
	}
}

// SHOW DATABASES with a 3-entry database_map enumerates the LOGICAL names as a
// UNION ALL of synthetic SELECTs, sorted by logical name. The "ghost" entry's
// physical ("orphan") is NOT in known_physical_databases, so it is skipped — no
// subquery and no database_rewrites entry. tenant1/tenant2 survive (alphabetical).
func TestRewriteDBLevel_showDatabasesSynthetic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{
			"tenant2": "phys2",
			"tenant1": "phys1",
			"ghost":   "orphan",
		},
		KnownPhysicalDatabases: []string{"phys1", "phys2"},
	})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW DATABASES", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_DATABASES {
		t.Fatalf("code=%v stmt=%v msg=%q", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	want := "SELECT name FROM (" +
		"SELECT 'tenant1' AS name UNION ALL SELECT 'tenant2' AS name" +
		") ORDER BY name"
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("sql=%q\nwant=%q", resp.GetSqlAfterRewrite(), want)
	}
	rw := resp.GetDatabaseRewrites()
	if rw["tenant1"] != "phys1" || rw["tenant2"] != "phys2" {
		t.Errorf("database_rewrites missing tenant1/tenant2: %v", rw)
	}
	if _, ok := rw["ghost"]; ok {
		t.Errorf("ghost (untrusted physical) must not appear in database_rewrites: %v", rw)
	}
	if len(rw) != 2 {
		t.Errorf("database_rewrites should have exactly 2 entries, got %v", rw)
	}
}

// SHOW DATABASES NOT ILIKE '<pat>' renders the outer " WHERE name NOT ILIKE 'p%'"
// clause (case_insensitive + not_like → "NOT ILIKE"), with the pattern escaped.
func TestRewriteDBLevel_showDatabasesLike(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES NOT ILIKE 'p%'")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"tenant1": "phys1"},
		KnownPhysicalDatabases: []string{"phys1"},
	})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW DATABASES NOT ILIKE 'p%'", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
	}
	want := "SELECT name FROM (SELECT 'tenant1' AS name) WHERE name NOT ILIKE 'p%' ORDER BY name"
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("sql=%q\nwant=%q", resp.GetSqlAfterRewrite(), want)
	}
}

// SHOW DATABASES with an empty database_map → empty-body sentinel
// (SELECT empty-string AS name WHERE 0); no database_rewrites entries.
func TestRewriteDBLevel_showDatabasesEmpty(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{KnownPhysicalDatabases: []string{"phys1"}})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW DATABASES", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	want := "SELECT name FROM (SELECT '' AS name WHERE 0) ORDER BY name"
	if resp.GetCode() != pb.RewriteCode_Success || !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("code=%v sql=%q\nwant=%q", resp.GetCode(), resp.GetSqlAfterRewrite(), want)
	}
	if len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

// SHOW DATABASES with no dynamic_args → passthrough (Success, stmt SHOW_DATABASES,
// verbatim SHOW DATABASES), no database_rewrites.
func TestRewriteDBLevel_showDatabasesPassthroughNoDynamic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES")
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW DATABASES", nil)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_DATABASES {
		t.Fatalf("code=%v stmt=%v", resp.GetCode(), resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SHOW DATABASES") || len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("sql=%q rewrites=%v", resp.GetSqlAfterRewrite(), resp.GetDatabaseRewrites())
	}
}

// A remote-mapped logical routes the enumeration through remote('addr', system,
// tables, user, password); the (database, prefix) filter still uses the physical
// name resolved from database_map.
func TestRewriteDBLevel_showTablesRemoteSource(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW TABLES FROM tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap:                          map[string]string{"tenant1": "testnet"},
		LogicalDatabaseToRemoteUpstreamIndex: map[string]string{"tenant1": "up0"},
		RemoteUpstreams: map[string]*pb.RewriteTableDynamicArgs_RemoteUpstream{
			"up0": {Addr: "h:9000", User: "u", Password: "p"},
		},
	})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW TABLES FROM tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
	}
	if !strings.Contains(resp.GetSqlAfterRewrite(), "remote('h:9000', system, tables, 'u', 'p')") {
		t.Errorf("expected remote() source, sql=%q", resp.GetSqlAfterRewrite())
	}
	// physical filter still uses the database_map result, not the remote key.
	if !strings.Contains(resp.GetSqlAfterRewrite(), "database = 'testnet'") {
		t.Errorf("expected physical filter database = 'testnet', sql=%q", resp.GetSqlAfterRewrite())
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

// ---- Task 6: CREATE DATABASE / DROP DATABASE debug rewrite ----

// CREATE DATABASE newdb (not in database_map) + dynamic_args → validated and
// rewritten to the debug SELECT `SELECT '<canonical DDL>' AS cdstmt`. One db-level
// accessed table is recorded BEFORE validation {newdb, "", physical=""} (newdb is
// not in database_map by precondition, so physical stays empty). No
// database_rewrites (CREATE never records a rewrite).
func TestRewriteDBLevel_createDatabaseDebugRewrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE newdb")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "CREATE DATABASE newdb", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE {
		t.Fatalf("code=%v stmt=%v msg=%q", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SELECT 'CREATE DATABASE newdb' AS cdstmt") {
		t.Errorf("sql=%q want SELECT 'CREATE DATABASE newdb' AS cdstmt", resp.GetSqlAfterRewrite())
	}
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 {
		t.Fatalf("accessed=%+v, want exactly 1", ats)
	}
	if ats[0].GetOriginalDatabase() != "newdb" || ats[0].GetOriginalTable() != "" ||
		ats[0].GetLogicalDatabase() != "newdb" || ats[0].GetPhysicalDatabase() != "" {
		t.Errorf("accessed[0]=%+v, want {orig_db=newdb, orig_table=\"\", logical=newdb, physical=\"\"}", ats[0])
	}
	if len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("database_rewrites=%v, want empty (CREATE records none)", resp.GetDatabaseRewrites())
	}
}

// CREATE DATABASE with no dynamic_args → UnsupportedStatement (can't validate or
// build the debug rewrite without policy context). No access recorded (the
// no-dynamic guard short-circuits before recordAccessedDatabase).
func TestRewriteDBLevel_createDatabaseNoDynamicUnsupported(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE newdb")
	resp, handled, err := RewriteDBLevel(e, ast, "CREATE DATABASE newdb", nil)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("code=%v msg=%q, want UnsupportedStatement", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE {
		t.Errorf("stmt=%v, want CREATE_DATABASE", resp.GetStatementType())
	}
	if len(resp.GetOriginalAccessedTables()) != 0 {
		t.Errorf("accessed=%+v, want empty (no-dynamic guard precedes record)", resp.GetOriginalAccessedTables())
	}
}

// CREATE DATABASE tenant1 where tenant1 is ALREADY in database_map and no
// IF NOT EXISTS → InvalidRewriteRequest. The access is still recorded BEFORE the
// reject — but because the db IS in database_map, its physical resolves (testnet).
func TestRewriteDBLevel_createDatabaseAlreadyExistsInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "CREATE DATABASE tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v msg=%q, want InvalidRewriteRequest", resp.GetCode(), resp.GetMessage())
	}
	if !strings.Contains(resp.GetMessage(), "already exists") {
		t.Errorf("message=%q, want to mention 'already exists'", resp.GetMessage())
	}
	// Access recorded before the reject; tenant1 is in database_map → physical=testnet.
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalDatabase() != "tenant1" || ats[0].GetOriginalTable() != "" {
		t.Fatalf("accessed=%+v, want 1 {tenant1, \"\"} recorded before reject", ats)
	}
	if ats[0].GetPhysicalDatabase() != "testnet" {
		t.Errorf("accessed[0].physical=%q, want testnet (resolvable on a db_map hit)", ats[0].GetPhysicalDatabase())
	}
}

// CREATE DATABASE IF NOT EXISTS tenant1 where tenant1 IS mapped → the IF NOT
// EXISTS suppresses the already-exists reject and falls through to the debug
// rewrite (Success). Access still recorded.
func TestRewriteDBLevel_createDatabaseIfNotExistsSuppresses(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE IF NOT EXISTS tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "CREATE DATABASE IF NOT EXISTS tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE {
		t.Fatalf("code=%v stmt=%v msg=%q, want Success/CREATE_DATABASE", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SELECT 'CREATE DATABASE IF NOT EXISTS tenant1' AS cdstmt") {
		t.Errorf("sql=%q want SELECT 'CREATE DATABASE IF NOT EXISTS tenant1' AS cdstmt", resp.GetSqlAfterRewrite())
	}
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalDatabase() != "tenant1" {
		t.Errorf("accessed=%+v, want 1 {tenant1}", ats)
	}
}

// DROP DATABASE tenant1 where tenant1 IS in database_map → validated and rewritten
// to `SELECT '<canonical DDL>' AS ddstmt`. On the db_map hit a database_rewrite
// {tenant1:testnet} is recorded (so an external GC can find the prefixed tables),
// and the accessed table's physical resolves to testnet.
func TestRewriteDBLevel_dropDatabaseDebugRewrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "DROP DATABASE tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DROP_DATABASE {
		t.Fatalf("code=%v stmt=%v msg=%q", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SELECT 'DROP DATABASE tenant1' AS ddstmt") {
		t.Errorf("sql=%q want SELECT 'DROP DATABASE tenant1' AS ddstmt", resp.GetSqlAfterRewrite())
	}
	if got := resp.GetDatabaseRewrites(); !mapEq(got, map[string]string{"tenant1": "testnet"}) {
		t.Errorf("database_rewrites=%v, want {tenant1:testnet}", got)
	}
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalDatabase() != "tenant1" || ats[0].GetOriginalTable() != "" {
		t.Fatalf("accessed=%+v, want 1 {tenant1, \"\"}", ats)
	}
	if ats[0].GetPhysicalDatabase() != "testnet" {
		t.Errorf("accessed[0].physical=%q, want testnet", ats[0].GetPhysicalDatabase())
	}
}

// DROP DATABASE nope where nope is NOT in database_map and no IF EXISTS →
// InvalidRewriteRequest (rewriter-managed DROP is logical-level; we don't manage
// it). Access recorded before the reject; physical stays empty (unresolvable).
func TestRewriteDBLevel_dropDatabaseNotManagedInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE nope")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "DROP DATABASE nope", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v msg=%q, want InvalidRewriteRequest", resp.GetCode(), resp.GetMessage())
	}
	if !strings.Contains(resp.GetMessage(), "not in database_map") {
		t.Errorf("message=%q, want to mention 'not in database_map'", resp.GetMessage())
	}
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalDatabase() != "nope" {
		t.Fatalf("accessed=%+v, want 1 {nope} recorded before reject", ats)
	}
	if ats[0].GetPhysicalDatabase() != "" {
		t.Errorf("accessed[0].physical=%q, want empty (nope unresolvable)", ats[0].GetPhysicalDatabase())
	}
	// A not-managed reject records NO database_rewrite (no logical→physical to log).
	if len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("database_rewrites=%v, want empty on a not-managed reject", resp.GetDatabaseRewrites())
	}
}

// DROP DATABASE IF EXISTS nope where nope is NOT mapped → IF EXISTS suppresses the
// not-managed reject; falls through to the debug rewrite (Success). No
// database_rewrite (db_map miss → nothing to record).
func TestRewriteDBLevel_dropDatabaseIfExistsSuppresses(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE IF EXISTS nope")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "DROP DATABASE IF EXISTS nope", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DROP_DATABASE {
		t.Fatalf("code=%v stmt=%v msg=%q, want Success/DROP_DATABASE", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SELECT 'DROP DATABASE IF EXISTS nope' AS ddstmt") {
		t.Errorf("sql=%q want SELECT 'DROP DATABASE IF EXISTS nope' AS ddstmt", resp.GetSqlAfterRewrite())
	}
	// db_map miss → no database_rewrite recorded even though it succeeds.
	if len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("database_rewrites=%v, want empty (IF EXISTS miss records none)", resp.GetDatabaseRewrites())
	}
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalDatabase() != "nope" {
		t.Errorf("accessed=%+v, want 1 {nope}", ats)
	}
}

// DROP DATABASE with no dynamic_args → UnsupportedStatement (mirrors CREATE).
func TestRewriteDBLevel_dropDatabaseNoDynamicUnsupported(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE tenant1")
	resp, handled, err := RewriteDBLevel(e, ast, "DROP DATABASE tenant1", nil)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("code=%v msg=%q, want UnsupportedStatement", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DROP_DATABASE {
		t.Errorf("stmt=%v, want DROP_DATABASE", resp.GetStatementType())
	}
	if len(resp.GetOriginalAccessedTables()) != 0 {
		t.Errorf("accessed=%+v, want empty (no-dynamic guard precedes record)", resp.GetOriginalAccessedTables())
	}
}
