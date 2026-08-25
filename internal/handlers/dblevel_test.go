package handlers

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-proto/gen/pb"
	"google.golang.org/protobuf/proto"
)

type showGenerateOverrideEngine struct {
	engine.Engine
	generated string
}

func (e showGenerateOverrideEngine) Generate(engine.AST) (string, error) {
	return e.generated, nil
}

func TestRewriteDBLevel_usePhysicalRewrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "USE tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_USE {
		t.Fatalf("code=%v stmt=%v", resp.GetCode(), resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "USE testnet") {
		t.Errorf("sql=%q", resp.GetSqlAfterRewrite())
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_usePassthroughNoDynamic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE tenant1")
	resp, handled, _ := RewriteDBLevel(e, ast, "USE tenant1", nil)
	if !handled || resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("handled=%v code=%v", handled, resp.GetCode())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "USE tenant1") || len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("sql=%q rewrites=%v", resp.GetSqlAfterRewrite(), resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_useSamePhysicalPassthrough(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE prod")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{KnownPhysicalDatabases: []string{"prod"}})
	resp, _, _ := RewriteDBLevel(e, ast, "USE prod", opts)
	// physical == origin → passthrough, no database_rewrites entry.
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "USE prod") || len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("sql=%q rewrites=%v", resp.GetSqlAfterRewrite(), resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_useUnresolvableInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE nope")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, _, _ := RewriteDBLevel(e, ast, "USE nope", opts)
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v", resp.GetCode())
	}
}

func TestRewriteDBLevel_useRemoteMappedUnsupported(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "USE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap:                          map[string]string{"tenant1": "testnet"},
		LogicalDatabaseToRemoteUpstreamIndex: map[string]string{"tenant1": "up0"},
	})
	resp, _, _ := RewriteDBLevel(e, ast, "USE tenant1", opts)
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("code=%v", resp.GetCode())
	}
}

func TestRewriteDBLevel_notDBLevel(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SELECT 1")
	_, handled, _ := RewriteDBLevel(e, ast, "SELECT 1", nil)
	if handled {
		t.Errorf("SELECT must not be handled by RewriteDBLevel")
	}
}

// SHOW TABLES FROM <logical> → synthetic system.tables enumeration that strips
// the per-table prefix. database_map resolves tenant1→testnet; the prefix is
// "tenant1." (logical + trailing dot, no extra_arguments).
func TestRewriteDBLevel_showTablesSynthetic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW TABLES FROM tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW TABLES FROM tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_TABLES {
		t.Fatalf("code=%v stmt=%v msg=%q", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	want := "SELECT multiIf(startsWith(name, 'tenant1.'), substring(name, length('tenant1.') + 1), name) AS name " +
		"FROM (SELECT name FROM system.tables WHERE database = 'testnet' AND startsWith(name, 'tenant1.'))"
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("sql=%q\nwant=%q", resp.GetSqlAfterRewrite(), want)
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_showTablePrefixesRetainPolicySemantics(t *testing.T) {
	e := newEngine(t)
	dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
	for _, tc := range []struct {
		name      string
		baseSQL   string
		prefixSQL string
	}{
		{name: "physical", baseSQL: "SHOW TABLES FROM hg_safe", prefixSQL: "SHOW FULL TABLES FROM hg_safe"},
		{name: "logical protected", baseSQL: "SHOW TABLES FROM db1", prefixSQL: "SHOW FULL TEMPORARY TABLES FROM db1"},
		{name: "ordinary", baseSQL: "SHOW TABLES FROM other", prefixSQL: "SHOW TEMPORARY TABLES FROM other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rewrite := func(sql string) *pb.RewriteSQLResponse {
				t.Helper()
				ast := mustParse(t, e, sql)
				resp, handled, err := RewriteDBLevel(e, ast, sql, dynOpt(dyn))
				if err != nil || !handled {
					t.Fatalf("%q: handled=%v err=%v", sql, handled, err)
				}
				return resp
			}
			base, prefixed := rewrite(tc.baseSQL), rewrite(tc.prefixSQL)
			if !proto.Equal(prefixed, base) {
				t.Fatalf("prefixed=%+v\nbase=%+v", prefixed, base)
			}
		})
	}
}

// Bare SHOW TABLES (no FROM) falls back to upstream_logical_database_in_context.
func TestRewriteDBLevel_showTablesUpstreamContext(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW TABLES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap:                      map[string]string{"tenant1": "testnet"},
		UpstreamLogicalDatabaseInContext: "tenant1",
	})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW TABLES", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	want := "SELECT multiIf(startsWith(name, 'tenant1.'), substring(name, length('tenant1.') + 1), name) AS name " +
		"FROM (SELECT name FROM system.tables WHERE database = 'testnet' AND startsWith(name, 'tenant1.'))"
	if resp.GetCode() != pb.RewriteCode_Success || !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("code=%v sql=%q", resp.GetCode(), resp.GetSqlAfterRewrite())
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

// Bare SHOW TABLES with neither FROM nor upstream context → InvalidRewriteRequest.
func TestRewriteDBLevel_showTablesNoContextInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW TABLES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW TABLES", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
	}
}

func TestRewriteDBLevel_showTablesExplicitUnresolvedStorageIntegrityFailsClosed(t *testing.T) {
	e := newEngine(t)
	sql := "SHOW TABLES FROM {db:Identifier}"
	ast := mustParse(t, e, sql)
	dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
	dyn.UpstreamLogicalDatabaseInContext = "other"
	resp, handled, err := RewriteDBLevel(e, ast, sql, dynOpt(dyn))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
		resp.GetMessage() != "storage-integrity SHOW TABLES database is not statically resolvable" {
		t.Fatalf("code=%v message=%q", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_UNSPECIFIED || resp.GetSqlAfterRewrite() != sql {
		t.Fatalf("stmt=%v sql=%q", resp.GetStatementType(), resp.GetSqlAfterRewrite())
	}
	if len(resp.GetOriginalAccessedTables()) != 0 || len(resp.GetDatabaseRewrites()) != 0 {
		t.Fatalf("accessed=%+v rewrites=%v, want empty", resp.GetOriginalAccessedTables(), resp.GetDatabaseRewrites())
	}
}

// SHOW CLUSTERS is not SHOW TABLES proper → passthrough (Success, stmt SHOW_TABLES,
// verbatim SHOW CLUSTERS).
func TestRewriteDBLevel_showClustersPassthrough(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW CLUSTERS")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW CLUSTERS", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_TABLES {
		t.Fatalf("code=%v stmt=%v", resp.GetCode(), resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SHOW CLUSTERS") {
		t.Errorf("sql=%q want SHOW CLUSTERS", resp.GetSqlAfterRewrite())
	}
	if len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

func TestRewriteDBLevel_showDictionariesStorageIntegrityNamespaces(t *testing.T) {
	e := newEngine(t)
	tests := []struct {
		name         string
		sql          string
		contextDB    string
		physicalDB   string
		unauthorized bool
		mapContext   bool
		wantCode     pb.RewriteCode
		wantMessage  string
		wantDB       string
		wantLogical  string
		wantPhysical string
		wantSI       bool
	}{
		{
			name:         "physical FROM namespace",
			sql:          "SHOW DICTIONARIES FROM hg_safe",
			wantCode:     pb.RewriteCode_UnsupportedStatement,
			wantMessage:  "storage-integrity physical database hg_safe is not directly addressable",
			wantDB:       "hg_safe",
			wantPhysical: "hg_safe",
			wantSI:       true,
		},
		{
			name:         "FULL physical FROM namespace",
			sql:          "SHOW FULL DICTIONARIES FROM hg_safe",
			wantCode:     pb.RewriteCode_UnsupportedStatement,
			wantMessage:  "storage-integrity physical database hg_safe is not directly addressable",
			wantDB:       "hg_safe",
			wantPhysical: "hg_safe",
			wantSI:       true,
		},
		{
			name:         "physical IN namespace",
			sql:          "SHOW DICTIONARIES IN hg_unsafe",
			wantCode:     pb.RewriteCode_UnsupportedStatement,
			wantMessage:  "storage-integrity physical database hg_unsafe is not directly addressable",
			wantDB:       "hg_unsafe",
			wantPhysical: "hg_unsafe",
			wantSI:       true,
		},
		{
			name:        "explicit unresolved FROM namespace fails closed",
			sql:         "SHOW DICTIONARIES FROM {db:Identifier}",
			contextDB:   "other",
			wantCode:    pb.RewriteCode_UnsupportedStatement,
			wantMessage: "storage-integrity SHOW DICTIONARIES database is not statically resolvable",
		},
		{
			name:        "explicit unresolved IN namespace fails closed",
			sql:         "SHOW DICTIONARIES IN {db:Identifier}",
			contextDB:   "other",
			wantCode:    pb.RewriteCode_UnsupportedStatement,
			wantMessage: "storage-integrity SHOW DICTIONARIES database is not statically resolvable",
		},
		{
			name:         "escaped physical namespace",
			sql:          "SHOW DICTIONARIES FROM `hg\\x5Fsafe`",
			wantCode:     pb.RewriteCode_UnsupportedStatement,
			wantMessage:  "storage-integrity physical database hg_safe is not directly addressable",
			wantDB:       "hg_safe",
			wantPhysical: "hg_safe",
			wantSI:       true,
		},
		{
			name:         "logical protected namespace",
			sql:          "SHOW DICTIONARIES FROM db1",
			wantCode:     pb.RewriteCode_UnsupportedStatement,
			wantMessage:  "storage-integrity logical database db1 is not directly addressable",
			wantDB:       "db1",
			wantLogical:  "db1",
			wantPhysical: "phys",
			wantSI:       true,
		},
		{
			name:         "FULL TEMPORARY logical protected namespace",
			sql:          "SHOW FULL TEMPORARY DICTIONARIES FROM db1",
			wantCode:     pb.RewriteCode_UnsupportedStatement,
			wantMessage:  "storage-integrity logical database db1 is not directly addressable",
			wantDB:       "db1",
			wantLogical:  "db1",
			wantPhysical: "phys",
			wantSI:       true,
		},
		{
			name:         "logical protected context",
			sql:          "SHOW DICTIONARIES",
			contextDB:    "db1",
			physicalDB:   "phys",
			wantCode:     pb.RewriteCode_UnsupportedStatement,
			wantMessage:  "storage-integrity logical database db1 is not directly addressable",
			wantDB:       "db1",
			wantLogical:  "db1",
			wantPhysical: "phys",
			wantSI:       true,
		},
		{
			name:         "physical context with empty logical context",
			sql:          "SHOW DICTIONARIES",
			physicalDB:   "hg_safe",
			wantCode:     pb.RewriteCode_UnsupportedStatement,
			wantMessage:  "storage-integrity physical database hg_safe is not directly addressable",
			wantDB:       "hg_safe",
			wantPhysical: "hg_safe",
			wantSI:       true,
		},
		{
			name:         "physical context wins over ordinary logical context",
			sql:          "SHOW DICTIONARIES",
			contextDB:    "other",
			physicalDB:   "hg_unsafe",
			wantCode:     pb.RewriteCode_UnsupportedStatement,
			wantMessage:  "storage-integrity physical database hg_unsafe is not directly addressable",
			wantDB:       "hg_unsafe",
			wantPhysical: "hg_unsafe",
			wantSI:       true,
		},
		{
			name:         "escaped logical protected namespace",
			sql:          "SHOW DICTIONARIES FROM `\\x64b1`",
			wantCode:     pb.RewriteCode_UnsupportedStatement,
			wantMessage:  "storage-integrity logical database db1 is not directly addressable",
			wantDB:       "db1",
			wantLogical:  "db1",
			wantPhysical: "phys",
			wantSI:       true,
		},
		{
			name:         "logical protected namespace requires authorization",
			sql:          "SHOW DICTIONARIES FROM db1",
			unauthorized: true,
			wantCode:     pb.RewriteCode_InvalidRewriteRequest,
			wantMessage:  "storage-integrity logical database db1 is not authorized by database_map",
			wantDB:       "db1",
			wantLogical:  "db1",
			wantSI:       true,
		},
		{
			name:        "ordinary namespace remains passthrough",
			sql:         "SHOW DICTIONARIES FROM other",
			physicalDB:  "hg_safe",
			wantCode:    pb.RewriteCode_Success,
			wantMessage: "success",
		},
		{
			name:        "TEMPORARY ordinary namespace remains passthrough",
			sql:         "SHOW TEMPORARY DICTIONARIES FROM other",
			wantCode:    pb.RewriteCode_Success,
			wantMessage: "success",
		},
		{
			name:        "FULL ordinary namespace preserves request SQL",
			sql:         "sHoW FULL DICTIONARIES   FROM other",
			wantCode:    pb.RewriteCode_Success,
			wantMessage: "success",
		},
		{
			name:        "semantic context is not decoded again",
			sql:         "SHOW DICTIONARIES",
			contextDB:   `hg\x5Fsafe`,
			mapContext:  true,
			wantCode:    pb.RewriteCode_Success,
			wantMessage: "success",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
			dyn.UpstreamLogicalDatabaseInContext = tt.contextDB
			if tt.physicalDB != "" {
				physical := tt.physicalDB
				dyn.UpstreamPhysicalDatabaseInContext = &physical
			}
			if tt.mapContext {
				dyn.DatabaseMap[tt.contextDB] = "phys"
			}
			if tt.unauthorized {
				delete(dyn.DatabaseMap, "db1")
			}
			ast := mustParse(t, e, tt.sql)
			resp, handled, err := RewriteDBLevel(e, ast, tt.sql, dynOpt(dyn))
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if resp.GetCode() != tt.wantCode || resp.GetMessage() != tt.wantMessage {
				t.Fatalf("code=%v message=%q, want code=%v message=%q", resp.GetCode(), resp.GetMessage(), tt.wantCode, tt.wantMessage)
			}
			if resp.GetSqlAfterRewrite() != tt.sql {
				t.Fatalf("sql=%q, want exact original %q", resp.GetSqlAfterRewrite(), tt.sql)
			}
			if tt.wantCode == pb.RewriteCode_Success {
				if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_TABLES {
					t.Fatalf("statement_type=%v, want SHOW_TABLES", resp.GetStatementType())
				}
				if len(resp.GetOriginalAccessedTables()) != 0 || len(resp.GetDatabaseRewrites()) != 0 {
					t.Fatalf("ordinary accessed=%+v rewrites=%v", resp.GetOriginalAccessedTables(), resp.GetDatabaseRewrites())
				}
				return
			}
			if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_UNSPECIFIED {
				t.Fatalf("statement_type=%v, want UNSPECIFIED", resp.GetStatementType())
			}
			if tt.wantDB == "" {
				if len(resp.GetOriginalAccessedTables()) != 0 || len(resp.GetDatabaseRewrites()) != 0 {
					t.Fatalf("unresolved accessed=%+v rewrites=%v, want empty", resp.GetOriginalAccessedTables(), resp.GetDatabaseRewrites())
				}
				return
			}
			if len(resp.GetOriginalAccessedTables()) != 1 {
				t.Fatalf("accessed=%+v, want exactly one", resp.GetOriginalAccessedTables())
			}
			got := resp.GetOriginalAccessedTables()[0]
			if got.GetOriginalDatabase() != tt.wantDB || got.GetOriginalTable() != "" ||
				got.GetLogicalDatabase() != tt.wantLogical || got.GetPhysicalDatabase() != tt.wantPhysical ||
				got.GetIsRemote() || got.GetIsStorageIntegrity() != tt.wantSI {
				t.Fatalf("accessed=%+v", got)
			}
		})
	}
}

func TestRewriteDBLevel_showDictionariesWithoutStorageIntegrityPassthrough(t *testing.T) {
	e := newEngine(t)
	dyn := &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"other": "phys"},
		KnownPhysicalDatabases: []string{"phys"},
	}
	for _, sql := range []string{
		"SHOW DICTIONARIES FROM other",
		"SHOW DICTIONARIES FROM {db:Identifier}",
		"SHOW FULL TEMPORARY DICTIONARIES FROM other",
		"SHOW FULL DICTIONARIES FROM {db:Identifier}",
		"sHoW FULL TEMPORARY DICTIONARIES   FROM other",
	} {
		t.Run(sql, func(t *testing.T) {
			ast := mustParse(t, e, sql)
			resp, handled, err := RewriteDBLevel(e, ast, sql, dynOpt(dyn))
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_TABLES ||
				resp.GetSqlAfterRewrite() != sql {
				t.Fatalf("code=%v stmt=%v sql=%q message=%q", resp.GetCode(), resp.GetStatementType(), resp.GetSqlAfterRewrite(), resp.GetMessage())
			}
			if len(resp.GetOriginalAccessedTables()) != 0 || len(resp.GetDatabaseRewrites()) != 0 {
				t.Fatalf("accessed=%+v rewrites=%v", resp.GetOriginalAccessedTables(), resp.GetDatabaseRewrites())
			}
		})
	}
}

func TestRewriteDBLevel_prefixedShowDictionariesPreservesRequestWithoutRegeneration(t *testing.T) {
	base := newEngine(t)
	sql := "sHoW FULL TEMPORARY DICTIONARIES   FROM other"
	ast := mustParse(t, base, sql)
	e := showGenerateOverrideEngine{Engine: base, generated: "SHOW DICTIONARIES FROM formatter_output"}
	for _, tc := range []struct {
		name string
		opts []*pb.RewriteOption
	}{
		{name: "active SI ordinary namespace", opts: dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE))},
		{name: "no SI dynamic args", opts: dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"other": "phys"}})},
		{name: "no dynamic args"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, handled, err := RewriteDBLevel(e, ast, sql, tc.opts)
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_TABLES ||
				resp.GetSqlAfterRewrite() != sql {
				t.Fatalf("code=%v stmt=%v sql=%q message=%q", resp.GetCode(), resp.GetStatementType(), resp.GetSqlAfterRewrite(), resp.GetMessage())
			}
		})
	}
}

func TestRewriteDBLevel_showDictionariesKeywordDatabaseNamesRemainOrdinary(t *testing.T) {
	e := newEngine(t)
	dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
	for _, db := range []string{"system", "default", "select", "from", "table", "settings", "123db"} {
		sql := "SHOW FULL DICTIONARIES FROM " + db
		t.Run(db, func(t *testing.T) {
			ast := mustParse(t, e, sql)
			resp, handled, err := RewriteDBLevel(e, ast, sql, dynOpt(dyn))
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_TABLES ||
				resp.GetSqlAfterRewrite() != sql {
				t.Fatalf("code=%v stmt=%v sql=%q message=%q", resp.GetCode(), resp.GetStatementType(), resp.GetSqlAfterRewrite(), resp.GetMessage())
			}
			if len(resp.GetOriginalAccessedTables()) != 0 || len(resp.GetDatabaseRewrites()) != 0 {
				t.Fatalf("accessed=%+v rewrites=%v", resp.GetOriginalAccessedTables(), resp.GetDatabaseRewrites())
			}
		})
	}
}

func TestRewriteDBLevel_nonDatabaseShowIgnoresStorageIntegrityContext(t *testing.T) {
	e := newEngine(t)
	dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
	dyn.UpstreamLogicalDatabaseInContext = "hg_safe"
	for _, sql := range []string{"SHOW CLUSTERS", "SHOW SETTINGS", "SHOW MERGES", "SHOW CACHES"} {
		ast := mustParse(t, e, sql)
		resp, handled, err := RewriteDBLevel(e, ast, sql, dynOpt(dyn))
		if err != nil || !handled || resp.GetCode() != pb.RewriteCode_Success {
			t.Fatalf("%q: handled=%v err=%v resp=%+v", sql, handled, err, resp)
		}
		if resp.GetSqlAfterRewrite() != sql || len(resp.GetOriginalAccessedTables()) != 0 {
			t.Fatalf("%q: sql=%q accessed=%+v", sql, resp.GetSqlAfterRewrite(), resp.GetOriginalAccessedTables())
		}
	}
}

// TestRewriteDBLevel_showCreateDefers: SHOW CREATE TABLE is NOT an
// ASTShowTablesQuery in ClickHouse (C++ routes it to a dedicated show_create
// handler — Phase 4), so RewriteDBLevel must NOT claim it as SHOW_TABLES. It
// returns handled=false → native pass-through classifies it SHOW_CREATE_TABLE.
func TestRewriteDBLevel_showCreateDefers(t *testing.T) {
	e := newEngine(t)
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	for _, sql := range []string{"SHOW CREATE TABLE db.t", "SHOW CREATE DATABASE db"} {
		ast := mustParse(t, e, sql)
		_, handled, err := RewriteDBLevel(e, ast, sql, opts)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		if handled {
			t.Errorf("%q: handled=true, want false (SHOW CREATE must defer, not be stamped SHOW_TABLES)", sql)
		}
	}
}

// SHOW DATABASES with a 3-entry database_map enumerates the LOGICAL names as a
// UNION ALL of synthetic SELECTs, sorted by logical name. The "ghost" entry's
// physical ("orphan") is NOT in known_physical_databases, so it is skipped — no
// subquery and no database_rewrites entry. tenant1/tenant2 survive (alphabetical).
func TestRewriteDBLevel_showDatabasesSynthetic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{
			"tenant2": "phys2",
			"tenant1": "phys1",
			"ghost":   "orphan",
		},
		KnownPhysicalDatabases: []string{"phys1", "phys2"},
	})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW DATABASES", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_DATABASES {
		t.Fatalf("code=%v stmt=%v msg=%q", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	want := "SELECT name FROM (" +
		"SELECT 'tenant1' AS name UNION ALL SELECT 'tenant2' AS name" +
		") ORDER BY name"
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("sql=%q\nwant=%q", resp.GetSqlAfterRewrite(), want)
	}
	rw := resp.GetDatabaseRewrites()
	if rw["tenant1"] != "phys1" || rw["tenant2"] != "phys2" {
		t.Errorf("database_rewrites missing tenant1/tenant2: %v", rw)
	}
	if _, ok := rw["ghost"]; ok {
		t.Errorf("ghost (untrusted physical) must not appear in database_rewrites: %v", rw)
	}
	if len(rw) != 2 {
		t.Errorf("database_rewrites should have exactly 2 entries, got %v", rw)
	}
}

// SHOW DATABASES NOT ILIKE '<pat>' renders the outer " WHERE name NOT ILIKE 'p%'"
// clause (case_insensitive + not_like → "NOT ILIKE"), with the pattern escaped.
func TestRewriteDBLevel_showDatabasesLike(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES NOT ILIKE 'p%'")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"tenant1": "phys1"},
		KnownPhysicalDatabases: []string{"phys1"},
	})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW DATABASES NOT ILIKE 'p%'", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
	}
	want := "SELECT name FROM (SELECT 'tenant1' AS name) WHERE name NOT ILIKE 'p%' ORDER BY name"
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("sql=%q\nwant=%q", resp.GetSqlAfterRewrite(), want)
	}
}

// SHOW DATABASES with an empty database_map → empty-body sentinel
// (SELECT empty-string AS name WHERE 0); no database_rewrites entries.
func TestRewriteDBLevel_showDatabasesEmpty(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{KnownPhysicalDatabases: []string{"phys1"}})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW DATABASES", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	want := "SELECT name FROM (SELECT '' AS name WHERE 0) ORDER BY name"
	if resp.GetCode() != pb.RewriteCode_Success || !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), want) {
		t.Errorf("code=%v sql=%q\nwant=%q", resp.GetCode(), resp.GetSqlAfterRewrite(), want)
	}
	if len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

// SHOW DATABASES with no dynamic_args → passthrough (Success, stmt SHOW_DATABASES,
// verbatim SHOW DATABASES), no database_rewrites.
func TestRewriteDBLevel_showDatabasesPassthroughNoDynamic(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW DATABASES")
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW DATABASES", nil)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_SHOW_DATABASES {
		t.Fatalf("code=%v stmt=%v", resp.GetCode(), resp.GetStatementType())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SHOW DATABASES") || len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("sql=%q rewrites=%v", resp.GetSqlAfterRewrite(), resp.GetDatabaseRewrites())
	}
}

// A remote-mapped logical routes the enumeration through remote('addr', system,
// tables, user, password); the (database, prefix) filter still uses the physical
// name resolved from database_map.
func TestRewriteDBLevel_showTablesRemoteSource(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "SHOW TABLES FROM tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{
		DatabaseMap:                          map[string]string{"tenant1": "testnet"},
		LogicalDatabaseToRemoteUpstreamIndex: map[string]string{"tenant1": "up0"},
		RemoteUpstreams: map[string]*pb.RewriteTableDynamicArgs_RemoteUpstream{
			"up0": {Addr: "h:9000", User: "u", Password: "p"},
		},
	})
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW TABLES FROM tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
	}
	if !strings.Contains(resp.GetSqlAfterRewrite(), "remote('h:9000', system, tables, 'u', 'p')") {
		t.Errorf("expected remote() source, sql=%q", resp.GetSqlAfterRewrite())
	}
	// physical filter still uses the database_map result, not the remote key.
	if !strings.Contains(resp.GetSqlAfterRewrite(), "database = 'testnet'") {
		t.Errorf("expected physical filter database = 'testnet', sql=%q", resp.GetSqlAfterRewrite())
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Errorf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}

// ---- Task 6: CREATE DATABASE / DROP DATABASE debug rewrite ----

// CREATE DATABASE newdb (not in database_map) + dynamic_args → validated and
// rewritten to the debug SELECT `SELECT '<canonical DDL>' AS cdstmt`. One db-level
// accessed table is recorded BEFORE validation {newdb, "", physical=""} (newdb is
// not in database_map by precondition, so physical stays empty). No
// database_rewrites (CREATE never records a rewrite).
func TestRewriteDBLevel_createDatabaseDebugRewrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE newdb")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "CREATE DATABASE newdb", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE {
		t.Fatalf("code=%v stmt=%v msg=%q", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SELECT 'CREATE DATABASE newdb' AS cdstmt") {
		t.Errorf("sql=%q want SELECT 'CREATE DATABASE newdb' AS cdstmt", resp.GetSqlAfterRewrite())
	}
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 {
		t.Fatalf("accessed=%+v, want exactly 1", ats)
	}
	if ats[0].GetOriginalDatabase() != "newdb" || ats[0].GetOriginalTable() != "" ||
		ats[0].GetLogicalDatabase() != "newdb" || ats[0].GetPhysicalDatabase() != "" {
		t.Errorf("accessed[0]=%+v, want {orig_db=newdb, orig_table=\"\", logical=newdb, physical=\"\"}", ats[0])
	}
	if len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("database_rewrites=%v, want empty (CREATE records none)", resp.GetDatabaseRewrites())
	}
}

// CREATE DATABASE with no dynamic_args → UnsupportedStatement (can't validate or
// build the debug rewrite without policy context). No access recorded (the
// no-dynamic guard short-circuits before recordAccessedDatabase).
func TestRewriteDBLevel_createDatabaseNoDynamicUnsupported(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE newdb")
	resp, handled, err := RewriteDBLevel(e, ast, "CREATE DATABASE newdb", nil)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("code=%v msg=%q, want UnsupportedStatement", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE {
		t.Errorf("stmt=%v, want CREATE_DATABASE", resp.GetStatementType())
	}
	if len(resp.GetOriginalAccessedTables()) != 0 {
		t.Errorf("accessed=%+v, want empty (no-dynamic guard precedes record)", resp.GetOriginalAccessedTables())
	}
}

// CREATE DATABASE tenant1 where tenant1 is ALREADY in database_map and no
// IF NOT EXISTS → InvalidRewriteRequest. The access is still recorded BEFORE the
// reject — but because the db IS in database_map, its physical resolves (testnet).
func TestRewriteDBLevel_createDatabaseAlreadyExistsInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "CREATE DATABASE tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v msg=%q, want InvalidRewriteRequest", resp.GetCode(), resp.GetMessage())
	}
	if !strings.Contains(resp.GetMessage(), "already exists") {
		t.Errorf("message=%q, want to mention 'already exists'", resp.GetMessage())
	}
	// Access recorded before the reject; tenant1 is in database_map → physical=testnet.
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalDatabase() != "tenant1" || ats[0].GetOriginalTable() != "" {
		t.Fatalf("accessed=%+v, want 1 {tenant1, \"\"} recorded before reject", ats)
	}
	if ats[0].GetPhysicalDatabase() != "testnet" {
		t.Errorf("accessed[0].physical=%q, want testnet (resolvable on a db_map hit)", ats[0].GetPhysicalDatabase())
	}
}

// CREATE DATABASE IF NOT EXISTS tenant1 where tenant1 IS mapped → the IF NOT
// EXISTS suppresses the already-exists reject and falls through to the debug
// rewrite (Success). Access still recorded.
func TestRewriteDBLevel_createDatabaseIfNotExistsSuppresses(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "CREATE DATABASE IF NOT EXISTS tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "CREATE DATABASE IF NOT EXISTS tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_CREATE_DATABASE {
		t.Fatalf("code=%v stmt=%v msg=%q, want Success/CREATE_DATABASE", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SELECT 'CREATE DATABASE IF NOT EXISTS tenant1' AS cdstmt") {
		t.Errorf("sql=%q want SELECT 'CREATE DATABASE IF NOT EXISTS tenant1' AS cdstmt", resp.GetSqlAfterRewrite())
	}
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalDatabase() != "tenant1" {
		t.Errorf("accessed=%+v, want 1 {tenant1}", ats)
	}
}

// DROP DATABASE tenant1 where tenant1 IS in database_map → validated and rewritten
// to `SELECT '<canonical DDL>' AS ddstmt`. On the db_map hit a database_rewrite
// {tenant1:testnet} is recorded (so an external GC can find the prefixed tables),
// and the accessed table's physical resolves to testnet.
func TestRewriteDBLevel_dropDatabaseDebugRewrite(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE tenant1")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "DROP DATABASE tenant1", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DROP_DATABASE {
		t.Fatalf("code=%v stmt=%v msg=%q", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SELECT 'DROP DATABASE tenant1' AS ddstmt") {
		t.Errorf("sql=%q want SELECT 'DROP DATABASE tenant1' AS ddstmt", resp.GetSqlAfterRewrite())
	}
	if got := resp.GetDatabaseRewrites(); !mapEq(got, map[string]string{"tenant1": "testnet"}) {
		t.Errorf("database_rewrites=%v, want {tenant1:testnet}", got)
	}
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalDatabase() != "tenant1" || ats[0].GetOriginalTable() != "" {
		t.Fatalf("accessed=%+v, want 1 {tenant1, \"\"}", ats)
	}
	if ats[0].GetPhysicalDatabase() != "testnet" {
		t.Errorf("accessed[0].physical=%q, want testnet", ats[0].GetPhysicalDatabase())
	}
}

// DROP DATABASE nope where nope is NOT in database_map and no IF EXISTS →
// InvalidRewriteRequest (rewriter-managed DROP is logical-level; we don't manage
// it). Access recorded before the reject; physical stays empty (unresolvable).
func TestRewriteDBLevel_dropDatabaseNotManagedInvalid(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE nope")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "DROP DATABASE nope", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
		t.Errorf("code=%v msg=%q, want InvalidRewriteRequest", resp.GetCode(), resp.GetMessage())
	}
	if !strings.Contains(resp.GetMessage(), "not in database_map") {
		t.Errorf("message=%q, want to mention 'not in database_map'", resp.GetMessage())
	}
	ats := resp.GetOriginalAccessedTables()
	if len(ats) != 1 || ats[0].GetOriginalDatabase() != "nope" {
		t.Fatalf("accessed=%+v, want 1 {nope} recorded before reject", ats)
	}
	if ats[0].GetPhysicalDatabase() != "" {
		t.Errorf("accessed[0].physical=%q, want empty (nope unresolvable)", ats[0].GetPhysicalDatabase())
	}
	// A not-managed reject records NO database_rewrite (no logical→physical to log).
	if len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("database_rewrites=%v, want empty on a not-managed reject", resp.GetDatabaseRewrites())
	}
}

// DROP DATABASE IF EXISTS nope where nope is NOT mapped → IF EXISTS suppresses the
// not-managed reject; falls through to the debug rewrite (Success). No
// database_rewrite (db_map miss → nothing to record).
func TestRewriteDBLevel_dropDatabaseIfExistsSuppresses(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE IF EXISTS nope")
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"tenant1": "testnet"}})
	resp, handled, err := RewriteDBLevel(e, ast, "DROP DATABASE IF EXISTS nope", opts)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DROP_DATABASE {
		t.Fatalf("code=%v stmt=%v msg=%q, want Success/DROP_DATABASE", resp.GetCode(), resp.GetStatementType(), resp.GetMessage())
	}
	if !sqlSemEq(t, e, resp.GetSqlAfterRewrite(), "SELECT 'DROP DATABASE IF EXISTS nope' AS ddstmt") {
		t.Errorf("sql=%q want SELECT 'DROP DATABASE IF EXISTS nope' AS ddstmt", resp.GetSqlAfterRewrite())
	}
	// db_map miss → no database_rewrite recorded even though it succeeds.
	if len(resp.GetDatabaseRewrites()) != 0 {
		t.Errorf("database_rewrites=%v, want empty (IF EXISTS miss records none)", resp.GetDatabaseRewrites())
	}
	if ats := resp.GetOriginalAccessedTables(); len(ats) != 1 || ats[0].GetOriginalDatabase() != "nope" {
		t.Errorf("accessed=%+v, want 1 {nope}", ats)
	}
}

// DROP DATABASE with no dynamic_args → UnsupportedStatement (mirrors CREATE).
func TestRewriteDBLevel_dropDatabaseNoDynamicUnsupported(t *testing.T) {
	e := newEngine(t)
	ast := mustParse(t, e, "DROP DATABASE tenant1")
	resp, handled, err := RewriteDBLevel(e, ast, "DROP DATABASE tenant1", nil)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
		t.Errorf("code=%v msg=%q, want UnsupportedStatement", resp.GetCode(), resp.GetMessage())
	}
	if resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DROP_DATABASE {
		t.Errorf("stmt=%v, want DROP_DATABASE", resp.GetStatementType())
	}
	if len(resp.GetOriginalAccessedTables()) != 0 {
		t.Errorf("accessed=%+v, want empty (no-dynamic guard precedes record)", resp.GetOriginalAccessedTables())
	}
}

// TestRewriteDBLevel_unknownShowKindFallsThroughUnderStorageIntegrity pins the
// Spec N D2 catch-all. A SHOW kind in none of the three classification lists
// must not be assumed target-less: under an active storage-integrity contract
// dispatchShowTables declines to handle it, so native.go's pass-through tail
// answers with the Spec I D1 generic refusal instead of forwarding a statement
// whose target was never inspected. This assertion is deliberately engine-local
// (plan deviation D-4): ClickHouse's own ParserShowTablesQuery accepts a fixed
// keyword set, so the C++ engine may answer SyntaxError and the shared corpus
// schema has exactly one want_code per case.
func TestRewriteDBLevel_unknownShowKindFallsThroughUnderStorageIntegrity(t *testing.T) {
	e := newEngine(t)
	dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
	for _, sql := range []string{"SHOW SOMETHINGNEW FROM hg_safe", "SHOW SOMETHINGNEW FROM other", "SHOW SOMETHINGNEW"} {
		ast := mustParse(t, e, sql)
		resp, handled, err := RewriteDBLevel(e, ast, sql, dynOpt(dyn))
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		if handled {
			t.Errorf("%q: handled=true (code=%v sql=%q), want false so the D1 catch-all answers",
				sql, resp.GetCode(), resp.GetSqlAfterRewrite())
		}
	}
}

// TestRewriteDBLevel_unknownShowKindStillPassesThroughWithoutStorageIntegrity
// is the other half: an empty-SI request keeps the legacy pass-through, so the
// catch-all narrows nothing outside the storage-integrity surface.
func TestRewriteDBLevel_unknownShowKindStillPassesThroughWithoutStorageIntegrity(t *testing.T) {
	e := newEngine(t)
	opts := dynOpt(&pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"other": "phys"}})
	for _, sql := range []string{"SHOW SOMETHINGNEW FROM other", "SHOW SOMETHINGNEW"} {
		ast := mustParse(t, e, sql)
		resp, handled, err := RewriteDBLevel(e, ast, sql, opts)
		if err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", sql, handled, err)
		}
		if resp.GetCode() != pb.RewriteCode_Success || resp.GetSqlAfterRewrite() != sql {
			t.Errorf("%q: code=%v sql=%q, want Success with the statement unchanged", sql, resp.GetCode(), resp.GetSqlAfterRewrite())
		}
	}
}
