package handlers

import (
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

func TestDescribeMetadataSQL(t *testing.T) {
	want := "SELECT name, type, default_kind AS default_type, default_expression, comment, codec_expression, ttl_expression FROM system.columns WHERE database = 'hg_safe' AND table = 'db1__t' AND name != '_hg_row_id' ORDER BY position"
	if got := describeMetadataSQL("hg_safe.db1__t", "_hg_row_id"); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestRewriteDescribe_storageIntegrity(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{"DESCRIBE TABLE db1.t", "DESCRIBE db1.t", "DESC db1.t"} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		resp, handled, err := RewriteDescribe(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
		if err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", sql, handled, err)
		}
		if resp.StatementType != pb.StatementType_STATEMENT_TYPE_DESCRIBE || resp.Code != pb.RewriteCode_Success {
			t.Fatalf("%q: stmt=%v code=%v", sql, resp.StatementType, resp.Code)
		}
		if resp.SqlAfterRewrite != describeMetadataSQL("hg_safe.db1__t", "_hg_row_id") {
			t.Fatalf("%q: sql=%q", sql, resp.SqlAfterRewrite)
		}
		if len(resp.OriginalAccessedTables) != 1 || !resp.OriginalAccessedTables[0].IsStorageIntegrity {
			t.Fatalf("%q: accessed=%+v", sql, resp.OriginalAccessedTables)
		}
	}
}

func TestRewriteDescribe_useDefaultDatabase(t *testing.T) {
	e := newEngine(t)
	dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
	dyn.UpstreamLogicalDatabaseInContext = "db1"
	ast, _ := e.ParseOne("DESCRIBE t")
	resp, _, err := RewriteDescribe(e, ast, "DESCRIBE t", dynOpt(dyn))
	if err != nil {
		t.Fatal(err)
	}
	if resp.SqlAfterRewrite != describeMetadataSQL("hg_safe.db1__t", "_hg_row_id") {
		t.Fatalf("sql=%q", resp.SqlAfterRewrite)
	}
}

func TestRewriteDescribe_nonSIPassesThrough(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("DESCRIBE TABLE other.u")
	resp, handled, err := RewriteDescribe(e, ast, "DESCRIBE TABLE other.u", dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.StatementType != pb.StatementType_STATEMENT_TYPE_DESCRIBE || resp.SqlAfterRewrite != "DESCRIBE TABLE other.u" {
		t.Fatalf("stmt=%v sql=%q (G-minimal: non-SI DESCRIBE passes through until Spec E D6)", resp.StatementType, resp.SqlAfterRewrite)
	}
}
