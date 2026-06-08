package handlers

import (
	"os"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

func newEngine(t *testing.T) engine.Engine {
	t.Helper()
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func dynOpt(a *pb.RewriteTableDynamicArgs) []*pb.RewriteOption {
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: a}}}}
}

func mapEq(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestRewriteSelect_dynamicRename(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT a FROM tenant1.events WHERE x IN (1, 2)")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{"tenant1": "testnet"}, Delim: "_",
	})
	resp, err := RewriteSelect(e, ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s)", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SELECT {
		t.Fatalf("stmt type = %v", resp.GetStatementType())
	}
	want := map[string]string{"tenant1.events": "testnet.tenant1.events"}
	if got := resp.GetTableRewrites(); !mapEq(got, want) {
		t.Fatalf("table_rewrites = %v, want %v", got, want)
	}
	if resp.GetSqlAfterRewrite() == "" {
		t.Fatal("empty sql")
	}
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalDatabase() != "tenant1" || ats[0].GetPhysicalDatabase() != "testnet" {
		t.Fatalf("accessed = %+v", ats)
	}
}

func TestRewriteSelect_invalidUnqualified_skipsLeniently(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT a FROM events")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, err := RewriteSelect(e, ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code = %v, want Success (lenient)", resp.GetCode())
	}
	if len(resp.GetTableRewrites()) != 0 {
		t.Fatalf("expected no rewrites, got %v", resp.GetTableRewrites())
	}
}
