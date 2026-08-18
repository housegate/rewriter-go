package rewriter

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-proto/gen/pb"
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
	// statement_type is UNSPECIFIED on a reject (oracle parity: C++ sets it only
	// in setSuccessResponse, so a rejected statement carries no type).
	if res.StatementType != pb.StatementType_STATEMENT_TYPE_UNSPECIFIED {
		t.Fatalf("stmt = %v, want UNSPECIFIED (reject)", res.StatementType)
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

func TestNativeRewrite_phase4(t *testing.T) {
	e := newEngine(t)
	optFn := func(string) []*pb.RewriteOption {
		return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
				DynamicArgs: &pb.RewriteTableDynamicArgs{
					DatabaseMap:            map[string]string{"logical1": "phys1"},
					KnownPhysicalDatabases: []string{"phys1"},
					Delim:                  "_",
				}}}}}
	}
	r := New(e, WithOptions(optFn))
	defer r.Close()

	t.Run("exists table rewritten", func(t *testing.T) {
		res, err := r.Rewrite(context.Background(), "EXISTS logical1.t", "acct")
		if err != nil {
			t.Fatal(err)
		}
		if res.Code != pb.RewriteCode_Success || res.StatementType != pb.StatementType_STATEMENT_TYPE_EXISTS_TABLE {
			t.Fatalf("code=%v stmt=%v", res.Code, res.StatementType)
		}
		if res.SQL != "EXISTS TABLE phys1.`logical1.t`" {
			t.Errorf("sql=%q", res.SQL)
		}
	})

	t.Run("show create no longer mis-stamped", func(t *testing.T) {
		res, err := r.Rewrite(context.Background(), "SHOW CREATE TABLE logical1.t", "acct")
		if err != nil {
			t.Fatal(err)
		}
		if res.StatementType != pb.StatementType_STATEMENT_TYPE_SHOW_CREATE_TABLE {
			t.Errorf("stmt=%v", res.StatementType)
		}
		if res.SQL != "SHOW CREATE TABLE phys1.`logical1.t`" {
			t.Errorf("sql=%q", res.SQL)
		}
	})

	t.Run("grant produces deltas + marker", func(t *testing.T) {
		res, err := r.Rewrite(context.Background(), "GRANT SELECT ON logical1.t TO u", "acct")
		if err != nil {
			t.Fatal(err)
		}
		if res.Code != pb.RewriteCode_Success || res.StatementType != pb.StatementType_STATEMENT_TYPE_GRANT {
			t.Fatalf("code=%v stmt=%v", res.Code, res.StatementType)
		}
		if res.SQL != "SELECT 'GRANT SELECT ON logical1.t TO u' AS gstmt" || len(res.PrivilegesDeltas) != 1 {
			t.Errorf("sql=%q deltas=%d", res.SQL, len(res.PrivilegesDeltas))
		}
	})

	t.Run("grant reject echoes input (design §8)", func(t *testing.T) {
		res, _ := r.Rewrite(context.Background(), "GRANT SELECT ON *.* TO u", "acct")
		if res.Code != pb.RewriteCode_UnsupportedStatement {
			t.Fatalf("code=%v", res.Code)
		}
		if res.SQL != "GRANT SELECT ON *.* TO u" {
			t.Errorf("sql=%q want input echo", res.SQL)
		}
	})
}

func TestRewriteErrorMessage(t *testing.T) {
	e := newEngine(t)
	optFn := func(string) []*pb.RewriteOption {
		return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
				DynamicArgs: &pb.RewriteTableDynamicArgs{
					DatabaseMap:            map[string]string{"logical1": "phys1"},
					KnownPhysicalDatabases: []string{"phys1"},
					Delim:                  "_",
				}}}}}
	}
	r := New(e, WithOptions(optFn))
	defer r.Close()
	ctx := context.Background()

	t.Run("inverts table name after EXISTS rewrite", func(t *testing.T) {
		res, err := r.Rewrite(ctx, "EXISTS logical1.t", "acct")
		if err != nil {
			t.Fatal(err)
		}
		// sanity: the rewrite produced the physical name in table_rewrites.
		if res.TableRewrites["logical1.t"] != "phys1.logical1.t" {
			t.Fatalf("table_rewrites=%v", res.TableRewrites)
		}
		inv, err := r.RewriteErrorMessage(ctx, "Table phys1.`logical1.t` does not exist")
		if err != nil {
			t.Fatal(err)
		}
		if inv != "Table logical1.t does not exist" {
			t.Errorf("inv=%q", inv)
		}
	})

	t.Run("inverts database name after USE rewrite", func(t *testing.T) {
		if _, err := r.Rewrite(ctx, "USE logical1", "acct"); err != nil {
			t.Fatal(err)
		}
		inv, _ := r.RewriteErrorMessage(ctx, "Database phys1 does not exist")
		if inv != "Database logical1 does not exist" {
			t.Errorf("inv=%q", inv)
		}
	})

	t.Run("no prior rewrite → unchanged", func(t *testing.T) {
		fresh := New(e, WithOptions(optFn))
		defer fresh.Close()
		inv, _ := fresh.RewriteErrorMessage(ctx, "Table phys1.x does not exist")
		if inv != "Table phys1.x does not exist" {
			t.Errorf("inv=%q", inv)
		}
	})

	t.Run("after a reject → unchanged (last code != Success)", func(t *testing.T) {
		if _, err := r.Rewrite(ctx, "GRANT SELECT ON *.* TO u", "acct"); err != nil {
			t.Fatal(err)
		}
		inv, _ := r.RewriteErrorMessage(ctx, "Database phys1 does not exist")
		if inv != "Database phys1 does not exist" {
			t.Errorf("reject should not invert: %q", inv)
		}
	})

	t.Run("empty message → empty", func(t *testing.T) {
		if _, err := r.Rewrite(ctx, "USE logical1", "acct"); err != nil {
			t.Fatal(err)
		}
		if inv, _ := r.RewriteErrorMessage(ctx, ""); inv != "" {
			t.Errorf("inv=%q", inv)
		}
	})
}

func TestNativeRewrite_deepNestingFailsOpen(t *testing.T) {
	e := newEngine(t)
	r := New(e)
	defer r.Close()
	sql := "SELECT ~" + strings.Repeat("(", 600) + "1" + strings.Repeat(")", 600)
	res, err := r.Rewrite(context.Background(), sql, "acct")
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err) // must be a code, not a crash/Go error
	}
	if res.Code != pb.RewriteCode_SyntaxError {
		t.Errorf("code=%v, want SyntaxError (fail-open)", res.Code)
	}
	if res.SQL != sql {
		t.Errorf("SQL must echo input on SyntaxError")
	}
}

func siContractDynamic(version pb.StorageIntegrityContractVersion) *pb.RewriteTableDynamicArgs {
	return &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"db1": "phys"},
		KnownPhysicalDatabases: []string{"phys"},
		StorageIntegrity: &pb.StorageIntegrityArgs{
			Tables: map[string]*pb.StorageIntegrityArgs_Table{
				"db1.t": {SafeTable: "hg_safe.db1__t", UnsafeTable: "hg_unsafe.db1__t"},
			},
			ReadMode:        pb.StorageIntegrityArgs_READ_MODE_SAFE,
			ContractVersion: version,
		},
	}
}

func tableRewriteDynamic(a *pb.RewriteTableDynamicArgs) *pb.RewriteOption {
	return &pb.RewriteOption{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: a}}}
}

func tableRewriteStatic() *pb.RewriteOption {
	return &pb.RewriteOption{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{StaticArgs: &pb.RewriteTableStaticArgs{}}}}
}

func nativeWithExactOptions(t *testing.T, e engine.Engine, opts []*pb.RewriteOption) *NativeRewriter {
	t.Helper()
	return New(e, WithOptions(func(string) []*pb.RewriteOption { return opts }))
}

func TestStorageIntegrityContract_AcceptedV1AcknowledgedOnSuccessAndSyntaxError(t *testing.T) {
	opts := []*pb.RewriteOption{tableRewriteDynamic(siContractDynamic(
		pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1))}

	e := newEngine(t)
	r := nativeWithExactOptions(t, e, opts)
	defer r.Close()
	res, err := r.Rewrite(context.Background(), "SELECT a FROM db1.t", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success ||
		res.StorageIntegrityContractVersion != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1 {
		t.Fatalf("success = %+v, want Success/V1", res)
	}

	parseFail := nativeWithExactOptions(t, &fakeEngine{parseErr: errors.New("parse boom")}, opts)
	defer parseFail.Close()
	res, err = parseFail.Rewrite(context.Background(), "SELECT (", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_SyntaxError || res.SQL != "SELECT (" ||
		res.StorageIntegrityContractVersion != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1 {
		t.Fatalf("syntax = %+v, want SyntaxError/original/V1", res)
	}
}

func TestStorageIntegrityContract_RejectsMissingOrUnknownVersionBeforeParse(t *testing.T) {
	for name, version := range map[string]pb.StorageIntegrityContractVersion{
		"unspecified": pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED,
		"unknown":     pb.StorageIntegrityContractVersion(99),
	} {
		t.Run(name, func(t *testing.T) {
			r := nativeWithExactOptions(t, &fakeEngine{parseErr: errors.New("must not parse")},
				[]*pb.RewriteOption{tableRewriteDynamic(siContractDynamic(version))})
			defer r.Close()
			res, err := r.Rewrite(context.Background(), "SELECT a FROM db1.t", "acct")
			if err != nil {
				t.Fatal(err)
			}
			if res.Code != pb.RewriteCode_InvalidRewriteRequest || res.SQL != "SELECT a FROM db1.t" ||
				res.StorageIntegrityContractVersion != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED {
				t.Fatalf("res = %+v, want InvalidRewriteRequest/original/unspecified", res)
			}
			if !strings.Contains(res.Message, "storage-integrity contract version V1 is required") {
				t.Fatalf("message = %q", res.Message)
			}
		})
	}
}

func TestStorageIntegrityContract_RejectsMalformedEntriesBeforeAcknowledgement(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*pb.RewriteTableDynamicArgs)
		message string
	}{
		{"nil table", func(d *pb.RewriteTableDynamicArgs) { d.StorageIntegrity.Tables["db1.t"] = nil }, "must not be nil"},
		{"missing safe", func(d *pb.RewriteTableDynamicArgs) { d.StorageIntegrity.Tables["db1.t"].SafeTable = "" }, "requires non-empty safe_table and unsafe_table"},
		{"missing unsafe", func(d *pb.RewriteTableDynamicArgs) { d.StorageIntegrity.Tables["db1.t"].UnsafeTable = "" }, "requires non-empty safe_table and unsafe_table"},
		{"invalid reserved column", func(d *pb.RewriteTableDynamicArgs) { d.StorageIntegrity.ReservedRowIdColumn = "bad-name" }, "reserved_row_id_column must be a simple identifier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dyn := siContractDynamic(pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1)
			tc.mutate(dyn)
			r := nativeWithExactOptions(t, &fakeEngine{parseErr: errors.New("must not parse")},
				[]*pb.RewriteOption{tableRewriteDynamic(dyn)})
			defer r.Close()
			res, err := r.Rewrite(context.Background(), "SELECT a FROM db1.t", "acct")
			if err != nil {
				t.Fatal(err)
			}
			if res.Code != pb.RewriteCode_InvalidRewriteRequest ||
				res.StorageIntegrityContractVersion != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED ||
				!strings.Contains(res.Message, tc.message) {
				t.Fatalf("res=%+v, want InvalidRewriteRequest/no-ack/message %q", res, tc.message)
			}
		})
	}
}

func TestStorageIntegrityContract_NoOrEmptySISurfaceKeepsLegacyZero(t *testing.T) {
	e := newEngine(t)
	for name, opts := range map[string][]*pb.RewriteOption{
		"no block": {tableRewriteDynamic(&pb.RewriteTableDynamicArgs{
			DatabaseMap: map[string]string{"db1": "phys"}})},
		"empty tables": {tableRewriteDynamic(&pb.RewriteTableDynamicArgs{
			DatabaseMap: map[string]string{"db1": "phys"},
			StorageIntegrity: &pb.StorageIntegrityArgs{
				ContractVersion: pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1,
			}})},
	} {
		t.Run(name, func(t *testing.T) {
			r := nativeWithExactOptions(t, e, opts)
			res, err := r.Rewrite(context.Background(), "SELECT a FROM db1.t", "acct")
			if err != nil {
				t.Fatal(err)
			}
			if res.Code != pb.RewriteCode_Success ||
				res.StorageIntegrityContractVersion != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED {
				t.Fatalf("res = %+v, want Success/unspecified", res)
			}
		})
	}
}

func TestStorageIntegrityContract_EffectiveTableRewriteIsLastWins(t *testing.T) {
	v1 := tableRewriteDynamic(siContractDynamic(pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1))
	missing := tableRewriteDynamic(siContractDynamic(pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED))
	static := tableRewriteStatic()
	e := newEngine(t)
	cases := []struct {
		name     string
		opts     []*pb.RewriteOption
		wantCode pb.RewriteCode
		wantAck  pb.StorageIntegrityContractVersion
	}{
		{"v1 shadowed by static", []*pb.RewriteOption{v1, static}, pb.RewriteCode_Success, pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED},
		{"v1 after static", []*pb.RewriteOption{static, v1}, pb.RewriteCode_Success, pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1},
		{"missing shadowed by static", []*pb.RewriteOption{missing, static}, pb.RewriteCode_Success, pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED},
		{"missing after static", []*pb.RewriteOption{static, missing}, pb.RewriteCode_InvalidRewriteRequest, pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := nativeWithExactOptions(t, e, tc.opts)
			res, err := r.Rewrite(context.Background(), "SELECT a FROM db1.t", "acct")
			if err != nil {
				t.Fatal(err)
			}
			if res.Code != tc.wantCode || res.StorageIntegrityContractVersion != tc.wantAck {
				t.Fatalf("res = %+v, want code=%v ack=%v", res, tc.wantCode, tc.wantAck)
			}
		})
	}
}

func TestStorageIntegrityContract_DBLevelUsesDynamicMapWithoutAcknowledgingShadowedSI(t *testing.T) {
	v1 := tableRewriteDynamic(siContractDynamic(pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1))
	static := tableRewriteStatic()
	e := newEngine(t)
	for name, tc := range map[string]struct {
		opts    []*pb.RewriteOption
		wantAck pb.StorageIntegrityContractVersion
	}{
		"SI dynamic shadowed for table policy": {[]*pb.RewriteOption{v1, static}, pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED},
		"SI dynamic active":                    {[]*pb.RewriteOption{static, v1}, pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1},
	} {
		t.Run(name, func(t *testing.T) {
			r := nativeWithExactOptions(t, e, tc.opts)
			res, err := r.Rewrite(context.Background(), "USE db1", "acct")
			if err != nil {
				t.Fatal(err)
			}
			if res.Code != pb.RewriteCode_Success || res.StatementType != pb.StatementType_STATEMENT_TYPE_USE ||
				res.StorageIntegrityContractVersion != tc.wantAck || !strings.Contains(res.SQL, "phys") {
				t.Fatalf("res = %+v", res)
			}
		})
	}
}

func TestStorageIntegrityContract_DescribeRetainsAcknowledgement(t *testing.T) {
	e := newEngine(t)
	r := nativeWithExactOptions(t, e, []*pb.RewriteOption{tableRewriteDynamic(siContractDynamic(
		pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1))})
	defer r.Close()

	res, err := r.Rewrite(context.Background(), "DESCRIBE db1.t", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_Success || res.StatementType != pb.StatementType_STATEMENT_TYPE_DESCRIBE ||
		res.StorageIntegrityContractVersion != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1 {
		t.Fatalf("res = %+v, want Success/DESCRIBE/V1", res)
	}
}
