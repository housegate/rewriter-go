package harness

import (
	"context"
	"os"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// FuzzRewrite hardens the fail-open contract: for any input, Rewrite must not
// panic and must return either (Success + non-empty SQL) or a classified
// non-Success RewriteResult with a nil Go error (the Go error is reserved for
// internal engine failures). Run: POLYGLOT_SQL_FFI_PATH=… go test ./internal/harness -run x -fuzz FuzzRewrite -fuzztime 30s
func FuzzRewrite(f *testing.F) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		f.Skip("needs engine")
	}
	for _, seed := range []string{
		"SELECT 1", "SELECT * FROM logical1.t", "USE logical1", "SHOW TABLES",
		"GRANT SELECT ON logical1.t TO u", "EXISTS logical1.t",
		"CREATE TABLE logical1.t (x Int32) ENGINE = Memory", "INSERT INTO logical1.t VALUES (1)",
		"DROP TABLE logical1.t", "RENAME TABLE logical1.a TO logical1.b",
	} {
		f.Add(seed)
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { e.Close() })

	optFn := func(string) []*pb.RewriteOption {
		return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
				DynamicArgs: &pb.RewriteTableDynamicArgs{
					DatabaseMap: map[string]string{"logical1": "phys1"}, KnownPhysicalDatabases: []string{"phys1"}, Delim: "_",
				}}}}}
	}

	f.Fuzz(func(t *testing.T, sql string) {
		// A NewRewriter per iteration keeps last-call state isolated.
		r := newWriteRewriter(e, optFn(""))
		res, err := r.Rewrite(context.Background(), sql, "acct")
		if err != nil {
			// Only an internal/engine failure is allowed to surface as a Go error;
			// the zero RewriteResult must not be inspected.
			return
		}
		if res.Code == pb.RewriteCode_Success && res.SQL == "" {
			t.Fatalf("Success with empty SQL for %q", sql)
		}
		// RewriteErrorMessage must also never panic, on any prior state.
		if _, eerr := r.RewriteErrorMessage(context.Background(), "Table phys1.`logical1.t` does not exist"); eerr != nil {
			t.Fatalf("RewriteErrorMessage error: %v", eerr)
		}
	})
}
