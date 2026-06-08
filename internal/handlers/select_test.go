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

// TestRewriteSelect_cteUnreferencedNotInjected verifies that an alias present in
// CteMap but NOT referenced by the query is not injected into the WITH clause.
// Mirrors C++ LargeMap_OnlyUsedInjected_Subset / RunByName_NoneUsedNoInject.
func TestRewriteSelect_cteUnreferencedNotInjected(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT * FROM c")
	opts := []*pb.RewriteOption{{Op: pb.RewriteOp_CommonTableExprRewrite,
		Value: &pb.RewriteOption_CommonTableExprArgs{CommonTableExprArgs: &pb.RewriteCommonTableExprArgs{
			CteMap: map[string]*pb.RewriteCommonTableExprArgs_CommonTableExpr{
				"c":      {Alias: "c", Sql: "SELECT 1"},
				"unused": {Alias: "unused", Sql: "SELECT 2"},
			}}}}}
	resp, err := RewriteSelect(e, ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	sql := resp.GetSqlAfterRewrite()
	t.Logf("sql_after_rewrite: %q", sql)
	if sql == "" {
		t.Fatal("empty sql")
	}
	// Referenced alias must be injected.
	if want := "WITH c AS (SELECT 1)"; !containsNorm(sql, want) {
		t.Fatalf("want %q in %q", want, sql)
	}
	// Unreferenced alias must NOT appear.
	if containsNorm(sql, "unused") {
		t.Fatalf("unreferenced alias 'unused' leaked into output: %q", sql)
	}
}

// TestRewriteSelect_cteNoneReferencedNoInject verifies that when NO alias in
// CteMap is referenced by the query, no WITH clause is injected at all.
// Mirrors C++ LargeMap_NoneUsed_NoInjection / RunByName_NoneUsedNoInject.
func TestRewriteSelect_cteNoneReferencedNoInject(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT 1")
	opts := []*pb.RewriteOption{{Op: pb.RewriteOp_CommonTableExprRewrite,
		Value: &pb.RewriteOption_CommonTableExprArgs{CommonTableExprArgs: &pb.RewriteCommonTableExprArgs{
			CteMap: map[string]*pb.RewriteCommonTableExprArgs_CommonTableExpr{
				"a": {Alias: "a", Sql: "SELECT 1"},
				"b": {Alias: "b", Sql: "SELECT 2"},
			}}}}}
	resp, err := RewriteSelect(e, ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	sql := resp.GetSqlAfterRewrite()
	t.Logf("sql_after_rewrite: %q", sql)
	if containsNorm(sql, "WITH") {
		t.Fatalf("expected no WITH clause, got: %q", sql)
	}
}

// TestRewriteSelect_cteUnreferencedBadParseStillRecorded verifies that a parse
// failure for an UNREFERENCED alias is still recorded in failed_cte_aliases.
// Mirrors C++ select.cc:775 — parseCTEMapToAST records ALL failures upfront,
// not just referenced ones.
func TestRewriteSelect_cteUnreferencedBadParseStillRecorded(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT * FROM good")
	opts := []*pb.RewriteOption{{Op: pb.RewriteOp_CommonTableExprRewrite,
		Value: &pb.RewriteOption_CommonTableExprArgs{CommonTableExprArgs: &pb.RewriteCommonTableExprArgs{
			CteMap: map[string]*pb.RewriteCommonTableExprArgs_CommonTableExpr{
				"good": {Alias: "good", Sql: "SELECT 1"},
				"bad":  {Alias: "bad", Sql: ")("}, // unreferenced AND bad
			}}}}}
	resp, err := RewriteSelect(e, ast, opts)
	if err != nil {
		t.Fatal(err)
	}
	sql := resp.GetSqlAfterRewrite()
	t.Logf("sql_after_rewrite: %q", sql)
	// Referenced good alias injected.
	if want := "WITH good AS (SELECT 1)"; !containsNorm(sql, want) {
		t.Fatalf("want %q in %q", want, sql)
	}
	// Unreferenced bad alias recorded even though it's not referenced.
	failed := resp.GetFailedCteAliases()
	if len(failed) != 1 || failed[0] != "bad" {
		t.Fatalf("failed_cte_aliases = %v, want [bad]", failed)
	}
}

// containsNorm is a simple contains check (no whitespace normalization needed
// for these cases).
func containsNorm(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		findSubstring(s, sub))
}

func findSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
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
