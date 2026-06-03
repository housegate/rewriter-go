package rewriter

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

// newNative builds a NativeRewriter over the real polyglot engine (needs FFI).
func newNative(t *testing.T) *NativeRewriter {
	t.Helper()
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("POLYGLOT_SQL_FFI_PATH not set; run via `make test`")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return New(e)
}

func TestPassThroughClassifiesAndEchoes(t *testing.T) {
	r := newNative(t)
	defer r.Close()
	res, err := r.Rewrite(context.Background(), "SELECT a FROM db.t", "acct")
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if res.Code != pb.RewriteCode_Success {
		t.Fatalf("code = %v, want Success", res.Code)
	}
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_SELECT {
		t.Fatalf("stmt = %v, want SELECT", res.StatementType)
	}
	if res.SQL == "" {
		t.Fatal("SQL must always be set on success")
	}
}

// fakeEngine is a deterministic Engine for contract tests that must not depend
// on polyglot's (lenient) parser behavior or the native FFI lib.
type fakeEngine struct {
	parseErr error
}

func (f *fakeEngine) ParseOne(sql string) (engine.AST, error) {
	if f.parseErr != nil {
		return nil, f.parseErr
	}
	return engine.AST(`{"select":{}}`), nil
}
func (f *fakeEngine) Generate(ast engine.AST) (string, error) { return "GENERATED", nil }
func (f *fakeEngine) RenameTables(a engine.AST, m map[string]string) (engine.AST, error) {
	return a, nil
}
func (f *fakeEngine) QualifyTables(a engine.AST, db string) (engine.AST, error) { return a, nil }
func (f *fakeEngine) Tokenize(string) (engine.AST, error)                       { return engine.AST("[]"), nil }
func (f *fakeEngine) DiffSQL(string, string) (engine.AST, error)               { return engine.AST("{}"), nil }
func (f *fakeEngine) Close() error                                             { return nil }

// A parse failure must surface as RewriteResult.code == SyntaxError with a NIL
// Go error (the Go error channel is reserved for unexpected/internal failures),
// and SQL must echo the input. Uses a fake engine so the contract is tested
// independently of polyglot's lenient parser (which, e.g., accepts "SELECT FROM").
func TestSyntaxErrorIsCodeNotGoError(t *testing.T) {
	r := New(&fakeEngine{parseErr: errors.New("parse boom")})
	defer r.Close()
	res, err := r.Rewrite(context.Background(), "SELECT FROM", "acct")
	if err != nil {
		t.Fatalf("syntax error must be returned in code, not as a Go error: %v", err)
	}
	if res.Code != pb.RewriteCode_SyntaxError {
		t.Fatalf("code = %v, want SyntaxError", res.Code)
	}
	if res.SQL != "SELECT FROM" {
		t.Fatalf("on non-Success, SQL must echo input; got %q", res.SQL)
	}
}
