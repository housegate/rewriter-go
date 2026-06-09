package handlers

import (
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// dynOpts builds a single dynamic-args TableNameRewrite option for handler tests.
func dynOpts(da *pb.RewriteTableDynamicArgs) []*pb.RewriteOption {
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: da}}}}
}

func parse(t *testing.T, e engine.Engine, sql string) engine.AST {
	t.Helper()
	ast, err := e.ParseOne(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	return ast
}

func TestRewriteExistsShowCreate(t *testing.T) {
	e := newEngine(t)
	dyn := &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"logical1": "phys1"},
		KnownPhysicalDatabases: []string{"phys1"},
		Delim:                  "_",
	}

	t.Run("exists none-mode bare", func(t *testing.T) {
		resp, handled, err := RewriteExistsShowCreate(e, parse(t, e, "EXISTS t"), "EXISTS t", nil)
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if resp.Code != pb.RewriteCode_Success || resp.StatementType != pb.StatementType_STATEMENT_TYPE_EXISTS_TABLE {
			t.Fatalf("code=%v stmt=%v", resp.Code, resp.StatementType)
		}
		if resp.SqlAfterRewrite != "EXISTS TABLE t" {
			t.Errorf("sql=%q", resp.SqlAfterRewrite)
		}
		if len(resp.OriginalAccessedTables) != 1 || resp.OriginalAccessedTables[0].GetOriginalTable() != "t" {
			t.Errorf("accessed=%+v", resp.OriginalAccessedTables)
		}
	})

	t.Run("exists dynamic rewrite", func(t *testing.T) {
		resp, handled, err := RewriteExistsShowCreate(e, parse(t, e, "EXISTS logical1.t"), "EXISTS logical1.t", dynOpts(dyn))
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		// physical db = phys1; physical table = buildDynamicTableName("logical1","t") = "logical1.t"
		// → quoted WhenNecessary because of the dot.
		if resp.SqlAfterRewrite != "EXISTS TABLE phys1.`logical1.t`" {
			t.Errorf("sql=%q", resp.SqlAfterRewrite)
		}
		if resp.TableRewrites["logical1.t"] != "phys1.logical1.t" {
			t.Errorf("table_rewrites=%v", resp.TableRewrites)
		}
	})

	t.Run("exists database rejected", func(t *testing.T) {
		resp, handled, _ := RewriteExistsShowCreate(e, parse(t, e, "EXISTS DATABASE db"), "EXISTS DATABASE db", dynOpts(dyn))
		if !handled || resp.Code != pb.RewriteCode_UnsupportedStatement {
			t.Fatalf("handled=%v code=%v", handled, resp.Code)
		}
	})

	t.Run("show create table dynamic", func(t *testing.T) {
		resp, handled, _ := RewriteExistsShowCreate(e, parse(t, e, "SHOW CREATE TABLE logical1.t"), "SHOW CREATE TABLE logical1.t", dynOpts(dyn))
		if !handled || resp.StatementType != pb.StatementType_STATEMENT_TYPE_SHOW_CREATE_TABLE {
			t.Fatalf("handled=%v stmt=%v", handled, resp.StatementType)
		}
		if resp.SqlAfterRewrite != "SHOW CREATE TABLE phys1.`logical1.t`" {
			t.Errorf("sql=%q", resp.SqlAfterRewrite)
		}
	})

	t.Run("show create view rejected", func(t *testing.T) {
		resp, handled, _ := RewriteExistsShowCreate(e, parse(t, e, "SHOW CREATE VIEW v"), "SHOW CREATE VIEW v", dynOpts(dyn))
		if !handled || resp.Code != pb.RewriteCode_UnsupportedStatement {
			t.Fatalf("handled=%v code=%v", handled, resp.Code)
		}
	})

	t.Run("exists unresolvable logical invalid", func(t *testing.T) {
		resp, handled, _ := RewriteExistsShowCreate(e, parse(t, e, "EXISTS unknown.t"), "EXISTS unknown.t", dynOpts(dyn))
		if !handled || resp.Code != pb.RewriteCode_InvalidRewriteRequest {
			t.Fatalf("handled=%v code=%v", handled, resp.Code)
		}
	})

	t.Run("not exists falls through", func(t *testing.T) {
		_, handled, err := RewriteExistsShowCreate(e, parse(t, e, "USE db"), "USE db", dynOpts(dyn))
		if handled || err != nil {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
	})
}
