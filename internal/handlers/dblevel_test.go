package handlers

import (
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
