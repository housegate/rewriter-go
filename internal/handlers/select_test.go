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
	// The rewritten table name contains a dot, so it must be a single quoted
	// identifier in the output (db=testnet, table="tenant1.events"), not a 3-part name.
	reparsed, perr := e.ParseOne(resp.GetSqlAfterRewrite())
	if perr != nil {
		t.Fatalf("re-parse rewritten sql %q: %v", resp.GetSqlAfterRewrite(), perr)
	}
	refs, _ := engine.CollectSelectTables(reparsed)
	if len(refs) != 1 || refs[0].DB != "testnet" || refs[0].Table != "tenant1.events" {
		t.Fatalf("rewritten table not preserved as single identifier: sql=%q refs=%+v", resp.GetSqlAfterRewrite(), refs)
	}
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalDatabase() != "tenant1" || ats[0].GetPhysicalDatabase() != "testnet" {
		t.Fatalf("accessed = %+v", ats)
	}
}

func TestRewriteSelect_cteInjectAndFailedAliases(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT * FROM c")
	opts := []*pb.RewriteOption{{Op: pb.RewriteOp_CommonTableExprRewrite,
		Value: &pb.RewriteOption_CommonTableExprArgs{CommonTableExprArgs: &pb.RewriteCommonTableExprArgs{
			CteMap: map[string]*pb.RewriteCommonTableExprArgs_CommonTableExpr{
				"c":   {Alias: "c", Sql: "SELECT 1"},
				"bad": {Alias: "bad", Sql: ")("},
			}}}}}
	resp, err := RewriteSelect(e, ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSqlAfterRewrite() == "" {
		t.Fatal("empty sql")
	}
	t.Logf("sql_after_rewrite: %q", resp.GetSqlAfterRewrite())
	if len(resp.GetFailedCteAliases()) != 1 || resp.GetFailedCteAliases()[0] != "bad" {
		t.Fatalf("failed_cte_aliases = %v, want [bad]", resp.GetFailedCteAliases())
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

func TestRewriteSelect_cteBodyTableRewritten(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT * FROM c")
	opts := []*pb.RewriteOption{
		{Op: pb.RewriteOp_CommonTableExprRewrite, Value: &pb.RewriteOption_CommonTableExprArgs{
			CommonTableExprArgs: &pb.RewriteCommonTableExprArgs{
				CteMap: map[string]*pb.RewriteCommonTableExprArgs_CommonTableExpr{
					"c": {Alias: "c", Sql: "SELECT * FROM tenant1.events"},
				}}}},
		{Op: pb.RewriteOp_TableNameRewrite, Value: &pb.RewriteOption_TableNameArgs{
			TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: &pb.RewriteTableDynamicArgs{
				DatabaseMap: map[string]string{"tenant1": "testnet"}, Delim: "_"}}}},
	}
	resp, err := RewriteSelect(e, ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	// The CTE body's table was rewritten by the same walk:
	if resp.GetTableRewrites()["tenant1.events"] != "testnet.tenant1.events" {
		t.Fatalf("cte body table not rewritten: %v", resp.GetTableRewrites())
	}
	// The outer `c` reference is scoped out — re-parsing the output yields only
	// the rewritten body table, never `c` as a physical table:
	reparsed, perr := e.ParseOne(resp.GetSqlAfterRewrite())
	if perr != nil {
		t.Fatalf("re-parse %q: %v", resp.GetSqlAfterRewrite(), perr)
	}
	refs, _ := engine.CollectSelectTables(reparsed)
	if len(refs) != 1 || refs[0].DB != "testnet" || refs[0].Table != "tenant1.events" {
		t.Fatalf("expected 1 ref {testnet, tenant1.events}, got %+v (sql=%q)", refs, resp.GetSqlAfterRewrite())
	}
}
