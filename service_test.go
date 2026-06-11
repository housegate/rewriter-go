package rewriter

import (
	"context"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"google.golang.org/protobuf/proto"
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
			if res.Message != resp.GetMessage() {
				t.Errorf("message: native=%q service=%q", res.Message, resp.GetMessage())
			}
			if !slices.Equal(res.FailedCTEAliases, resp.GetFailedCteAliases()) {
				t.Errorf("failed_cte_aliases: native=%v service=%v", res.FailedCTEAliases, resp.GetFailedCteAliases())
			}
			if !slices.EqualFunc(res.OriginalAccessedTables, resp.GetOriginalAccessedTables(),
				func(a, b *pb.AccessedTable) bool { return proto.Equal(a, b) }) {
				t.Errorf("original_accessed_tables: native=%v service=%v", res.OriginalAccessedTables, resp.GetOriginalAccessedTables())
			}
			if !slices.EqualFunc(res.PrivilegesDeltas, resp.GetPrivilegesDeltas(),
				func(a, b *pb.PrivilegeDelta) bool { return proto.Equal(a, b) }) {
				t.Errorf("privileges_deltas: native=%v service=%v", res.PrivilegesDeltas, resp.GetPrivilegesDeltas())
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

// TestServiceRewriteErrorMessage: physical names in an error message are
// inverted back to logical ones using maps re-derived from sql+options;
// unparseable SQL (non-Success forward rewrite) passes through unchanged.
func TestServiceRewriteErrorMessage(t *testing.T) {
	e := newEngine(t)
	svc := &Service{engine: e}
	opts := dynOpts(map[string]string{"db1": "phys"}, []string{"phys"})

	// The rewrite maps db1.t → phys.db1.t (dot delimiter); ClickHouse quotes
	// dotted table names with backticks in error messages (phys.`db1.t`), which
	// is the form reverse.Invert actually matches against the table_rewrites map.
	resp, err := svc.RewriteErrorMessage(context.Background(), &pb.RewriteErrorMessageRequest{
		Sql:          "SELECT a FROM db1.t",
		ErrorMessage: "Table phys.`db1.t` does not exist",
		Options:      opts,
	})
	if err != nil {
		t.Fatalf("RewriteErrorMessage: %v", err)
	}
	if got := resp.GetErrorAfterRewrite(); !strings.Contains(got, "db1.t") || strings.Contains(got, "phys.") {
		t.Errorf("inversion failed: %q", got)
	}

	resp2, err := svc.RewriteErrorMessage(context.Background(), &pb.RewriteErrorMessageRequest{
		Sql:          "SELECT FROM WHERE ((",
		ErrorMessage: "boom",
		Options:      opts,
	})
	if err != nil {
		t.Fatalf("RewriteErrorMessage(passthrough): %v", err)
	}
	if resp2.GetErrorAfterRewrite() != "boom" {
		t.Errorf("passthrough = %q, want \"boom\"", resp2.GetErrorAfterRewrite())
	}
}

// TestServiceConcurrentUse backs the "safe for concurrent use" godoc
// claim: one process-shared Service hammered from many goroutines must
// be race-free (run under -race) and deterministic per call.
func TestServiceConcurrentUse(t *testing.T) {
	e := newEngine(t)
	svc := &Service{engine: e}
	opts := dynOpts(map[string]string{"db1": "phys"}, []string{"phys"})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				resp, err := svc.Rewrite(context.Background(), &pb.RewriteSQLRequest{Sql: "SELECT a FROM db1.t", Options: opts})
				if err != nil || resp.GetCode() != pb.RewriteCode_Success {
					t.Errorf("Rewrite: err=%v code=%v", err, resp.GetCode())
					return
				}
				em, err := svc.RewriteErrorMessage(context.Background(), &pb.RewriteErrorMessageRequest{
					Sql: "SELECT a FROM db1.t", ErrorMessage: "Table phys.`db1.t` does not exist", Options: opts,
				})
				if err != nil || em.GetErrorAfterRewrite() == "" {
					t.Errorf("RewriteErrorMessage: err=%v out=%q", err, em.GetErrorAfterRewrite())
					return
				}
			}
		}()
	}
	wg.Wait()
}
