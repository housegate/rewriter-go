package rewriter

import (
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// TestTableRef_ClickHouse268SettingsAreRefused pins the settings a
// ClickHouse 26.8 server honours (rewriter-grpc sqlBearingSetting): SQL text
// the server wraps around the query, the current database, format overrides
// and the other switches. Refused whatever the embedded parser, since the
// backend decides.
func TestTableRef_ClickHouse268SettingsAreRefused(t *testing.T) {
	setting := func(name string) string { return "table setting " + name + " is not accepted" }
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, name string }{
			{"SELECT * FROM db1.o SETTINGS filter = 'a IN (SELECT a FROM db2.x)'", "filter"},
			{"SELECT * FROM db1.o SETTINGS select = '(SELECT 1 FROM db2.x)'", "select"},
			{"SELECT * FROM db1.o SETTINGS `order` = 'a'", "order"},
			{"SELECT * FROM db1.o SETTINGS sort = 'a'", "sort"},
			{"SELECT * FROM db1.o SETTINGS database = 'db2'", "database"},
			{"SELECT * FROM db1.o SETTINGS DATABASE = 'db2'", "DATABASE"},
			{"INSERT INTO db1.o SETTINGS input_format = 'Values' FORMAT CSV 1", "input_format"},
			{"INSERT INTO db1.o SETTINGS format = 'Values' FORMAT CSV 1", "format"},
			{"SELECT * FROM db1.o SETTINGS output_format = 'JSON'", "output_format"},
			{"SELECT * FROM db1.o SETTINGS default_format = 'JSON'", "default_format"},
			{"SELECT * FROM db1.o SETTINGS asterisk_include_virtual_columns = 1", "asterisk_include_virtual_columns"},
			{"SELECT * FROM db1.o SETTINGS s3_allow_server_credentials_in_user_queries = 1", "s3_allow_server_credentials_in_user_queries"},
			{"SELECT * FROM db1.o SETTINGS dynamic_disk_allow_from_env = 1", "dynamic_disk_allow_from_env"},
			{"SELECT * FROM db1.o SETTINGS url_base = 'file://'", "url_base"},
			{"SELECT * FROM db1.o SETTINGS s3_base = 'https://b/'", "s3_base"},
			{"SELECT * FROM db1.o SETTINGS analyzer_compatibility_allow_cte_redefinition = 1", "analyzer_compatibility_allow_cte_redefinition"},
			{"SELECT * FROM db1.o SETTINGS analyzer_inline_views = 1", "analyzer_inline_views"},
			{"SELECT * FROM db1.o SETTINGS enable_streaming_queries = 1", "enable_streaming_queries"},
			{"SELECT * FROM db1.o SETTINGS ai_function_text_default_credentials = 'c'", "ai_function_text_default_credentials"},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: setting(c.name), wantSQL: c.sql})
		}
	}
	// A session SET is modelled only while the SI surface is inactive. These
	// are refused whatever the message: polyglot reads neither as a SET
	// statement, so they leave through the generic refusal, where
	// rewriter-grpc names the setting.
	for _, sql := range []string{
		"SET filter = 'a IN (SELECT a FROM db2.x)'",
		"SET database = 'db2'",
	} {
		cases = append(cases, tablerefCase{name: sql, sql: sql,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantSQL: sql})
	}
	runTablerefCases(t, cases)
}

// TestDeploymentIdentity_ClickHouse268 pins the identity functions
// authenticatedUser / authUser and ClickHouse 26.8's pgGetUserById,
// currentRequestURL and currentHandler, and the `_database` virtual column
// 26.8 gives MergeTree, Memory, Log, View, MaterializedView and system
// tables (rewriter-grpc firstDeploymentIdentityCall /
// firstDeploymentIdentityColumn), refused in dynamic mode, both SI states.
func TestDeploymentIdentity_ClickHouse268(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		dyn := &pb.RewriteTableDynamicArgs{
			DatabaseMap:                      map[string]string{"tenant1": "physical_db_1"},
			UpstreamLogicalDatabaseInContext: "tenant1",
		}
		if si {
			dyn.StorageIntegrity = &pb.StorageIntegrityArgs{
				Tables: map[string]*pb.StorageIntegrityArgs_Table{
					"tenant1.si": {SafeTable: "hg_safe.tenant1__si", UnsafeTable: "hg_unsafe.tenant1__si"},
				},
				ReadMode:            pb.StorageIntegrityArgs_READ_MODE_SAFE,
				ReservedRowIdColumn: "_hg_row_id",
				ContractVersion:     pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2,
				ReservedDatabases:   []string{"hg_safe", "hg_unsafe", "hg_promote"},
			}
		}
		opts := []*pb.RewriteOption{tableRewriteDynamic(dyn)}
		for sql, want := range map[string]string{
			"SELECT authenticatedUser()":                              "function authenticatedUser is not accessible",
			"SELECT authUser()":                                       "function authUser is not accessible",
			"SELECT pgGetUserById(1)":                                 "function pgGetUserById is not accessible",
			"SELECT pg_get_userbyid(1)":                               "function pg_get_userbyid is not accessible",
			"SELECT currentRequestURL()":                              "function currentRequestURL is not accessible",
			"SELECT currentHandler()":                                 "function currentHandler is not accessible",
			"SELECT _database FROM tenant1.o":                         "virtual column _database is not accessible",
			"SELECT o._database FROM tenant1.o":                       "virtual column _database is not accessible",
			"SELECT `_database` FROM tenant1.o":                       "virtual column _database is not accessible",
			"SELECT a FROM tenant1.o WHERE _database != ''":           "virtual column _database is not accessible",
			"SELECT a FROM (SELECT a, _database AS d FROM tenant1.o)": "virtual column _database is not accessible",
			"INSERT INTO tenant1.p SELECT _database FROM tenant1.o":   "virtual column _database is not accessible",
			"SELECT hostName(), _database FROM tenant1.o":             "function hostName is not accessible",
			"SELECT currentDatabase() FROM tenant1.o":                 "function currentDatabase is not accessible",
			"SELECT currentUser() FROM tenant1.si":                    "function currentUser is not accessible",
		} {
			resp, err := doRewrite(e, sql, opts)
			if err != nil {
				t.Fatalf("si=%v %s: %v", si, sql, err)
			}
			if resp.GetCode() != pb.RewriteCode_UnsupportedStatement || resp.GetMessage() != want || resp.GetSqlAfterRewrite() != sql {
				t.Errorf("si=%v %s: code=%v message=%q sql=%q", si, sql, resp.GetCode(), resp.GetMessage(), resp.GetSqlAfterRewrite())
			}
			// Like rewriter-grpc, the refusal carries no rewrite maps.
			if len(resp.GetTableRewrites()) != 0 || len(resp.GetDatabaseRewrites()) != 0 || len(resp.GetPrivilegesDeltas()) != 0 {
				t.Errorf("si=%v %s: refusal carries rewrite maps: tables=%v databases=%v privileges=%v",
					si, sql, resp.GetTableRewrites(), resp.GetDatabaseRewrites(), resp.GetPrivilegesDeltas())
			}
		}
		// A string, an alias that only spells the name, and another case are
		// not the column.
		for _, sql := range []string{
			"SELECT '_database' FROM tenant1.o",
			"SELECT _DATABASE FROM tenant1.o",
			"SELECT a AS _database FROM tenant1.o",
		} {
			resp, err := doRewrite(e, sql, opts)
			if err != nil {
				t.Fatalf("si=%v %s: %v", si, sql, err)
			}
			if resp.GetCode() != pb.RewriteCode_Success {
				t.Errorf("si=%v %s: code=%v message=%q, want Success", si, sql, resp.GetCode(), resp.GetMessage())
			}
		}
	}
}
