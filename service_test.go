package rewriter

import (
	"context"
	"maps"
	"os"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
)

// dynOpts builds the single dynamic-args RewriteOption shape housegate sends.
func dynOpts(dbMap map[string]string, known []string) []*pb.RewriteOption {
	return []*pb.RewriteOption{{
		Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
			DynamicArgs: &pb.RewriteTableDynamicArgs{
				DatabaseMap:            dbMap,
				KnownPhysicalDatabases: known,
			},
		}},
	}}
}

// TestServiceMatchesNativeRewriter is the parity gate: the stateless
// Service and the per-connection NativeRewriter must produce
// field-identical responses for the same SQL + options.
func TestServiceMatchesNativeRewriter(t *testing.T) {
	e := newEngine(t) // skips without POLYGLOT_SQL_FFI_PATH
	opts := dynOpts(map[string]string{"db1": "phys"}, []string{"phys"})
	nr := New(e, WithOptions(func(string) []*pb.RewriteOption { return opts }))
	svc := &Service{engine: e} // in-package: share the engine, skip a second FFI load

	cases := []string{
		"SELECT a FROM db1.t",
		"USE db1",
		"CREATE TABLE db1.t2 (x Int64) ENGINE = MergeTree ORDER BY x",
		"GRANT SELECT ON db1.* TO bob",
		"SET max_threads = 4",
		"SELECT FROM WHERE ((", // SyntaxError path
	}
	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			res, rerr := nr.Rewrite(context.Background(), sql, "acct")
			resp, serr := svc.Rewrite(context.Background(), &pb.RewriteSQLRequest{Sql: sql, Options: opts})
			if (rerr != nil) != (serr != nil) {
				t.Fatalf("error mismatch: native=%v service=%v", rerr, serr)
			}
			if rerr != nil {
				return
			}
			if res.Code != resp.GetCode() {
				t.Errorf("code: native=%v service=%v", res.Code, resp.GetCode())
			}
			if res.SQL != resp.GetSqlAfterRewrite() {
				t.Errorf("sql: native=%q service=%q", res.SQL, resp.GetSqlAfterRewrite())
			}
			if res.StatementType != resp.GetStatementType() {
				t.Errorf("stmt: native=%v service=%v", res.StatementType, resp.GetStatementType())
			}
			if !maps.Equal(res.TableRewrites, resp.GetTableRewrites()) {
				t.Errorf("table_rewrites: native=%v service=%v", res.TableRewrites, resp.GetTableRewrites())
			}
			if !maps.Equal(res.DatabaseRewrites, resp.GetDatabaseRewrites()) {
				t.Errorf("database_rewrites: native=%v service=%v", res.DatabaseRewrites, resp.GetDatabaseRewrites())
			}
			if res.ExistenceClause != resp.GetExistenceClause() {
				t.Errorf("existence_clause: native=%v service=%v", res.ExistenceClause, resp.GetExistenceClause())
			}
		})
	}
}

// TestNewServiceLoadsAndCloses exercises the public constructor end to end.
func TestNewServiceLoadsAndCloses(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("POLYGLOT_SQL_FFI_PATH not set; run via `make test`")
	}
	svc, err := NewService("") // "" → OpenDefault → honors POLYGLOT_SQL_FFI_PATH
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	resp, err := svc.Rewrite(context.Background(), &pb.RewriteSQLRequest{Sql: "SELECT 1"})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code = %v, want Success", resp.GetCode())
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
