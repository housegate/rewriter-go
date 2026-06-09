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
