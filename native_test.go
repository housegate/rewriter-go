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

// newEngine builds the real polyglot engine for tests that need both a rewriter
// (via New) and direct engine access (for the SQL-equivalence check). Mirrors
// handlers.newEngine: skips without the FFI lib and closes on cleanup. Tests that
// pass it to New(e) own the engine lifetime via r.Close(); the t.Cleanup close is
// idempotent (engine.Close double-call is safe).
func newEngine(t *testing.T) engine.Engine {
	t.Helper()
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
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
func (f *fakeEngine) ParseGeneric(sql string) (engine.AST, error) {
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
func (f *fakeEngine) DiffSQL(string, string) (engine.AST, error)                { return engine.AST("{}"), nil }
func (f *fakeEngine) Close() error                                              { return nil }

func TestNativeRewrite_selectDynamic(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, _ := engine.NewPolyglot("")
	defer e.Close()
	r := New(e, WithOptions(func(account string) []*pb.RewriteOption {
		return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
				DynamicArgs: &pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}, Delim: "_"}}}}}
	}))
	res, err := r.Rewrite(context.Background(), "SELECT a FROM tenant1.events", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success || len(res.TableRewrites) != 1 {
		t.Fatalf("res = %+v", res)
	}
}

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

// --- Task 11: write routing in NativeRewriter.Rewrite ----------------------
//
// These exercise the dispatch added BEFORE the SELECT/pass-through branches:
// writes go to handlers.RewriteWrite, SELECT/USE/SHOW/etc still fall through.

// statOptFn wraps a static table_map into a WithOptions builder (ignores account).
func statOptFn(m map[string]string) func(string) []*pb.RewriteOption {
	return func(string) []*pb.RewriteOption {
		return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
				StaticArgs: &pb.RewriteTableStaticArgs{TableMap: m}}}}}
	}
}

// dynOptFn wraps dynamic args into a WithOptions builder (ignores account).
func dynOptFn(a *pb.RewriteTableDynamicArgs) func(string) []*pb.RewriteOption {
	return func(string) []*pb.RewriteOption {
		return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
				DynamicArgs: a}}}}
	}
}

// nativeSQLSemEq reports whether got and want are semantically equal after a
// parse+regenerate round-trip through the engine (writes aren't SELECTs, so a
// canonical-form compare is the right fidelity check). Mirrors handlers.sqlSemEq.
func nativeSQLSemEq(t *testing.T, e engine.Engine, got, want string) bool {
	t.Helper()
	canon := func(sql string) string {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		gen, err := e.Generate(ast)
		if err != nil {
			t.Fatalf("gen %q: %v", sql, err)
		}
		return gen
	}
	return canon(got) == canon(want)
}

// nativeMapEq is an order-independent map compare.
func nativeMapEq(a, b map[string]string) bool {
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

// 1. DROP TABLE db.t routes through RewriteWrite (not pass-through): table renamed.
func TestNativeRewrite_dropTableRouted(t *testing.T) {
	e := newEngine(t)
	r := New(e, WithOptions(statOptFn(map[string]string{"db.t": "t_phys"})))
	defer r.Close()

	res, err := r.Rewrite(context.Background(), "DROP TABLE db.t", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s), want Success", res.Code, res.Message)
	}
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_DROP_TABLE {
		t.Fatalf("stmt = %v, want DROP_TABLE", res.StatementType)
	}
	if !nativeSQLSemEq(t, e, res.SQL, "DROP TABLE db.t_phys") {
		t.Fatalf("sql = %q, want ≈ DROP TABLE db.t_phys", res.SQL)
	}
	if want := map[string]string{"db.t": "db.t_phys"}; !nativeMapEq(res.TableRewrites, want) {
		t.Fatalf("table_rewrites = %v, want %v", res.TableRewrites, want)
	}
}

// 2. INSERT INTO db.t (x) VALUES (1) routes through RewriteWrite: target renamed.
func TestNativeRewrite_insertRouted(t *testing.T) {
	e := newEngine(t)
	r := New(e, WithOptions(statOptFn(map[string]string{"db.t": "t_phys"})))
	defer r.Close()

	res, err := r.Rewrite(context.Background(), "INSERT INTO db.t (x) VALUES (1)", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s), want Success", res.Code, res.Message)
	}
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_INSERT {
		t.Fatalf("stmt = %v, want INSERT", res.StatementType)
	}
	if !nativeSQLSemEq(t, e, res.SQL, "INSERT INTO db.t_phys (x) VALUES (1)") {
		t.Fatalf("sql = %q, want ≈ INSERT INTO db.t_phys (x) VALUES (1)", res.SQL)
	}
}

// 3. RENAME TABLE db.a TO db.b routes through the tier-C raw path: both renamed.
func TestNativeRewrite_renameRouted(t *testing.T) {
	e := newEngine(t)
	r := New(e, WithOptions(statOptFn(map[string]string{"db.a": "a_phys", "db.b": "b_phys"})))
	defer r.Close()

	res, err := r.Rewrite(context.Background(), "RENAME TABLE db.a TO db.b", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s), want Success", res.Code, res.Message)
	}
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_RENAME_TABLE {
		t.Fatalf("stmt = %v, want RENAME_TABLE", res.StatementType)
	}
	if !nativeSQLSemEq(t, e, res.SQL, "RENAME TABLE db.a_phys TO db.b_phys") {
		t.Fatalf("sql = %q, want ≈ RENAME TABLE db.a_phys TO db.b_phys", res.SQL)
	}
}

// 4. SELECT still routes to the SELECT handler (no regression from write routing).
func TestNativeRewrite_selectStillWorks(t *testing.T) {
	e := newEngine(t)
	r := New(e) // no opts
	defer r.Close()

	res, err := r.Rewrite(context.Background(), "SELECT 1", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s), want Success", res.Code, res.Message)
	}
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_SELECT {
		t.Fatalf("stmt = %v, want SELECT", res.StatementType)
	}
}

//  5. An UnsupportedStatement write (multi-table DROP) surfaces the reject CODE,
//     and SQL must echo the ORIGINAL input so the caller can still forward it (§8).
func TestNativeRewrite_unsupportedWriteSurfacesCode(t *testing.T) {
	e := newEngine(t)
	r := New(e) // opts irrelevant; rejected before resolution
	defer r.Close()

	const src = "DROP TABLE a, b"
	res, err := r.Rewrite(context.Background(), src, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("code = %v (%s), want UnsupportedStatement", res.Code, res.Message)
	}
	if res.SQL != src {
		t.Fatalf("on non-Success write, SQL must echo input; got %q, want %q", res.SQL, src)
	}
}

//  6. An InvalidRewriteRequest write (unqualified DROP under dynamic args with no
//     upstream_logical_database_in_context) surfaces the reject CODE; SQL == input.
func TestNativeRewrite_invalidWriteSurfacesCode(t *testing.T) {
	e := newEngine(t)
	r := New(e, WithOptions(dynOptFn(&pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{"tenant1": "testnet"},
	})))
	defer r.Close()

	const src = "DROP TABLE t"
	res, err := r.Rewrite(context.Background(), src, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_InvalidRewriteRequest {
		t.Fatalf("code = %v (%s), want InvalidRewriteRequest", res.Code, res.Message)
	}
	if res.SQL != src {
		t.Fatalf("on non-Success write, SQL must echo input; got %q, want %q", res.SQL, src)
	}
}

//  7. CREATE DATABASE with NO dynamic_args → RewriteDBLevel's no-dynamic reject
//     (UnsupportedStatement), stmt still CREATE_DATABASE, and §8 echoes the input
//     SQL so the result stays runnable.
func TestNativeRewrite_createDatabaseNoDynamicArgsRejected(t *testing.T) {
	e := newEngine(t)
	r := New(e)
	defer r.Close()

	res, err := r.Rewrite(context.Background(), "CREATE DATABASE db", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_UnsupportedStatement {
		t.Fatalf("code = %v (%s), want UnsupportedStatement", res.Code, res.Message)
	}
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE {
		t.Fatalf("stmt = %v, want CREATE_DATABASE", res.StatementType)
	}
	if res.SQL != "CREATE DATABASE db" {
		t.Errorf("SQL = %q, want input echoed (§8)", res.SQL)
	}
}

//  8. USE db with no opts → RewriteWrite handled=false → RewriteDBLevel's USE
//     passthrough (regenerate). Confirms write routing did NOT swallow non-write
//     commands and db-level routing handles a no-dynamic USE. classify labels it USE.
func TestNativeRewrite_useStillPassthrough(t *testing.T) {
	e := newEngine(t)
	r := New(e)
	defer r.Close()

	res, err := r.Rewrite(context.Background(), "USE db", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s), want Success (pass-through)", res.Code, res.Message)
	}
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_USE {
		t.Fatalf("stmt = %v, want USE", res.StatementType)
	}
	if res.SQL == "" {
		t.Fatal("pass-through SQL must always be set")
	}
}

// --- Task 7: db-level routing in NativeRewriter.Rewrite ---------------------
//
// These exercise the dispatch added AFTER RewriteWrite and BEFORE the SELECT/
// pass-through branches: USE / SHOW DATABASES / SHOW TABLES / CREATE DATABASE /
// DROP DATABASE go to handlers.RewriteDBLevel; SELECT and writes still route
// first (no regression).

//  1. USE tenant1 under a database_map {tenant1→testnet} routes through
//     RewriteDBLevel (no longer the bare pass-through): rewritten to `USE testnet`
//     with a database_rewrites{tenant1:testnet} entry.
func TestNativeRewrite_useRouted(t *testing.T) {
	e := newEngine(t)
	r := New(e, WithOptions(dynOptFn(&pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{"tenant1": "testnet"},
	})))
	defer r.Close()

	res, err := r.Rewrite(context.Background(), "USE tenant1", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s), want Success", res.Code, res.Message)
	}
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_USE {
		t.Fatalf("stmt = %v, want USE", res.StatementType)
	}
	if !nativeSQLSemEq(t, e, res.SQL, "USE testnet") {
		t.Fatalf("sql = %q, want ≈ USE testnet", res.SQL)
	}
	if want := map[string]string{"tenant1": "testnet"}; !nativeMapEq(res.DatabaseRewrites, want) {
		t.Fatalf("database_rewrites = %v, want %v", res.DatabaseRewrites, want)
	}
}

//  2. SHOW DATABASES under a database_map (with known_physical_databases) routes
//     through RewriteDBLevel and gets the synthetic enumeration: Success,
//     SHOW_DATABASES (no longer a bare pass-through).
func TestNativeRewrite_showDatabasesRouted(t *testing.T) {
	e := newEngine(t)
	r := New(e, WithOptions(dynOptFn(&pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"tenant1": "testnet"},
		KnownPhysicalDatabases: []string{"testnet"},
	})))
	defer r.Close()

	res, err := r.Rewrite(context.Background(), "SHOW DATABASES", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s), want Success", res.Code, res.Message)
	}
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_SHOW_DATABASES {
		t.Fatalf("stmt = %v, want SHOW_DATABASES", res.StatementType)
	}
	if !nativeSQLSemEq(t, e, res.SQL,
		"SELECT name FROM (SELECT 'tenant1' AS name) ORDER BY name") {
		t.Fatalf("sql = %q, want synthetic SHOW DATABASES enumeration", res.SQL)
	}
}

//  3. CREATE DATABASE newdb under dynamic_args routes through RewriteDBLevel and
//     gets the Task-6 DEBUG rewrite (`SELECT '...' AS cdstmt`) — NOT the removed
//     Phase-2 out-of-phase UnsupportedStatement. This is the key end-to-end proof
//     that db-level routing replaced the old write-path reject.
func TestNativeRewrite_createDatabaseRouted(t *testing.T) {
	e := newEngine(t)
	r := New(e, WithOptions(dynOptFn(&pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{"tenant1": "testnet"},
	})))
	defer r.Close()

	res, err := r.Rewrite(context.Background(), "CREATE DATABASE newdb", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s), want Success (debug rewrite, not out-of-phase reject)", res.Code, res.Message)
	}
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE {
		t.Fatalf("stmt = %v, want CREATE_DATABASE", res.StatementType)
	}
	if !nativeSQLSemEq(t, e, res.SQL, "SELECT 'CREATE DATABASE newdb' AS cdstmt") {
		t.Fatalf("sql = %q, want ≈ SELECT 'CREATE DATABASE newdb' AS cdstmt", res.SQL)
	}
}

//  4. SELECT and writes still route FIRST (no regression): SELECT 1 → SELECT
//     handler; DROP TABLE db.t → RewriteWrite (DROP_TABLE), never reaching the
//     db-level branch.
func TestNativeRewrite_selectAndWriteStillWork(t *testing.T) {
	e := newEngine(t)
	r := New(e, WithOptions(statOptFn(map[string]string{"db.t": "t_phys"})))
	defer r.Close()

	sel, err := r.Rewrite(context.Background(), "SELECT 1", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if sel.Code != pb.RewriteCode_Success {
		t.Fatalf("SELECT code = %v (%s), want Success", sel.Code, sel.Message)
	}
	if sel.StatementType != pb.StatementType_STATEMENT_TYPE_SELECT {
		t.Fatalf("SELECT stmt = %v, want SELECT", sel.StatementType)
	}

	drop, err := r.Rewrite(context.Background(), "DROP TABLE db.t", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if drop.Code != pb.RewriteCode_Success {
		t.Fatalf("DROP code = %v (%s), want Success", drop.Code, drop.Message)
	}
	if drop.StatementType != pb.StatementType_STATEMENT_TYPE_DROP_TABLE {
		t.Fatalf("DROP stmt = %v, want DROP_TABLE (writes route before db-level)", drop.StatementType)
	}
	if !nativeSQLSemEq(t, e, drop.SQL, "DROP TABLE db.t_phys") {
		t.Fatalf("DROP sql = %q, want ≈ DROP TABLE db.t_phys", drop.SQL)
	}
}
