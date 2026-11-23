package rewriter

import (
	"context"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// RewriteSQLRequest.refuse_metadata_access: the opt-in metadata refusal for
// static-mode callers. The cases mirror rewriter-grpc's
// tests/static_metadata_access_test.cc.

// sentioRequest is the Sentio analytics engine's request shape: settings,
// limit, then a static TableNameRewrite with table_with_database_map and
// remote_table_map.
func sentioRequest(sql string, refuse bool) *pb.RewriteSQLRequest {
	return &pb.RewriteSQLRequest{
		Sql:                  sql,
		RefuseMetadataAccess: refuse,
		Options: []*pb.RewriteOption{
			{Op: pb.RewriteOp_SettingsRewrite, Value: &pb.RewriteOption_SettingsArgs{SettingsArgs: &pb.RewriteSettingsArgs{
				Settings: []*pb.RewriteSettingsArgs_Setting{{
					Key: "log_comment", Type: pb.SettingType_String,
					Value: &pb.RewriteSettingsArgs_Setting_StringValue{StringValue: "sig"},
				}},
			}}},
			{Op: pb.RewriteOp_LimitRewrite, Value: &pb.RewriteOption_LimitArgs{LimitArgs: &pb.RewriteLimitArgs{
				Value: &pb.RewriteLimitArgs_ForceLimit{ForceLimit: 1000},
			}}},
			{Op: pb.RewriteOp_TableNameRewrite, Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{
				StaticArgs: &pb.RewriteTableStaticArgs{
					TableWithDatabaseMap: map[string]*pb.RewriteTableStaticArgs_TableWithDatabase{
						"Transfer": {Database: "sentio_proc_1", Table: "event_transfer_1"},
					},
					RemoteTableMap: map[string]*pb.RewriteTableStaticArgs_RemoteTable{
						"other.Transfer": {Addr: "chain-a:9000", Database: "sentio_proc_2", Table: "event_transfer_2", User: "reader", Password: "pw"},
					},
				},
			}}},
		},
	}
}

func metadataService(t *testing.T) *Service {
	t.Helper()
	return &Service{engine: newEngine(t)} // skips without POLYGLOT_SQL_FFI_PATH
}

func rewriteReq(t *testing.T, svc *Service, req *pb.RewriteSQLRequest) *pb.RewriteSQLResponse {
	t.Helper()
	resp, err := svc.Rewrite(context.Background(), req)
	if err != nil {
		t.Fatalf("%q: unexpected error: %v", req.GetSql(), err)
	}
	return resp
}

func expectMetadataRefused(t *testing.T, svc *Service, req *pb.RewriteSQLRequest) {
	t.Helper()
	resp := rewriteReq(t, svc, req)
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement || resp.GetMessage() != "metadata access is disabled" {
		t.Errorf("%q: got %v %q, want UnsupportedStatement %q", req.GetSql(), resp.GetCode(), resp.GetMessage(), "metadata access is disabled")
	}
	if resp.GetSqlAfterRewrite() != req.GetSql() {
		t.Errorf("%q: sql_after_rewrite %q, want the input echoed", req.GetSql(), resp.GetSqlAfterRewrite())
	}
	if len(resp.GetTableRewrites()) != 0 || len(resp.GetOriginalAccessedTables()) != 0 {
		t.Errorf("%q: a refusal carries no rewrite maps", req.GetSql())
	}
	if !resp.GetMetadataAccessRefusalApplied() {
		t.Errorf("%q: metadata_access_refusal_applied not set", req.GetSql())
	}
}

func TestStaticMetadataAccess_MetadataReadsAreRefused(t *testing.T) {
	svc := metadataService(t)
	refused := []string{
		// The metadata databases, in every read position.
		"SELECT * FROM system.tables",
		"SELECT 1 FROM system.one",
		"SELECT * FROM system.numbers LIMIT 3",
		"SELECT * FROM system.query_log",
		"SELECT * FROM `system`.`columns`",
		"SELECT * FROM SYSTEM.USERS",
		"SELECT * FROM information_schema.columns",
		"SELECT * FROM INFORMATION_SCHEMA.TABLES",
		"SELECT * FROM (SELECT * FROM system.users)",
		"SELECT * FROM Transfer UNION ALL SELECT * FROM system.query_log",
		"SELECT metadata.name FROM Transfer JOIN system.tables AS metadata ON 1",
		"WITH metadata AS (SELECT name FROM system.tables) SELECT * FROM metadata",
		"SELECT * FROM Transfer WHERE `from` IN (SELECT name FROM system.tables)",
		"SELECT * FROM numbers(1) WHERE 1 IN system.users",
		"SELECT (SELECT count() FROM system.processes) AS n",
		"SELECT * FROM view(SELECT * FROM system.query_log)",
		"SELECT name, row_number() OVER w FROM Transfer WINDOW w AS (ORDER BY (SELECT 1 FROM system.one))",
		"EXPLAIN SELECT * FROM system.tables",
		// Table functions and engines that can reach them.
		"SELECT * FROM remote('localhost:9000', 'system', 'tables')",
		"SELECT * FROM remote('localhost:9000', system.query_log)",
		"SELECT * FROM remote('localhost:9000', concat('sys', 'tem'), 'query_log')",
		"SELECT * FROM remote('localhost:9000')",
		"SELECT * FROM remote(my_collection, table='query_log', database='system')",
		"SELECT * FROM remoteSecure('localhost:9440', 'system', 'columns')",
		"SELECT * FROM cluster('default')",
		"SELECT * FROM clusterAllReplicas('default', 'system', 'users')",
		"SELECT * FROM merge('system', '^tables$')",
		"SELECT * FROM merge('^tables$')",
		"SELECT * FROM merge(REGEXP('system'), '^query_log$')",
		"SELECT * FROM loop(system.query_log)",
		"SELECT * FROM loop(query_log)",
		"SELECT * FROM mergeTreeIndex('system', 'columns')",
		"SELECT * FROM mergeTreeAnalyzeIndexes(system, query_log, 1)",
		"CREATE TABLE copy ENGINE = Merge('system', '^query_log$')",
		"SELECT * FROM mysql(my_collection)",
		"SELECT * FROM mysql('127.0.0.1:9004', 'system', 'query_log', 'u', 'p')",
		"SELECT * FROM postgresql('127.0.0.1:9005', 'information_schema', 'tables', 'u', 'p')",
		"SELECT * FROM jdbc('datasource', 'system', 'users')",
		"CREATE TABLE copy (a UInt8) ENGINE = Distributed('default', 'system', 'query_log')",
		// String lookups.
		"SELECT dictGet('system.x', 'a', 1)",
		"SELECT dictGet(concat('sys', 'tem.x'), 'a', 1)",
		"SELECT * FROM dictionary('system.x')",
		"SELECT joinGet('system.t', 'a', 1)",
		"SELECT naiveBayesClassifier('system.model', 'text')",
		"SELECT hasColumnInTable('system', 'users', 'name')",
		"SELECT hasColumnInTable('host', 'default', 't', 'c')",
		// Statements over them.
		"SHOW TABLES",
		"SHOW TABLES FROM system",
		"SHOW DATABASES",
		"SHOW DICTIONARIES",
		"SHOW CLUSTERS",
		"SHOW MERGES",
		"SHOW FILESYSTEM CACHES",
		"SHOW QUOTAS",
		"SHOW GRANTS FOR default",
		"SHOW INDEXES FROM tables FROM system",
		"SHOW CREATE DATABASE system",
		"DROP DATABASE system",
		"ALTER TABLE leak MOVE PARTITION 1 TO TABLE system.query_log",
		"ALTER TABLE leak REPLACE PARTITION 1 FROM system.query_log",
		"SHOW SETTINGS LIKE 'max%'",
		"SHOW PROCESSLIST",
		"SHOW USERS",
		"SHOW ROLES",
		"SHOW GRANTS",
		"SHOW ACCESS",
		"SHOW CREATE USER default",
		"SHOW FUNCTIONS",
		"SHOW ENGINES",
		"SHOW SETTING max_threads",
		"SHOW PRIVILEGES",
		"SHOW COLUMNS FROM tables FROM system",
		"SHOW CREATE TABLE system.tables",
		"DESCRIBE TABLE system.tables",
		"EXISTS TABLE system.tables",
		"USE system",
		"USE `INFORMATION_SCHEMA`",
		"DROP TABLE system.tables",
		"INSERT INTO system.query_log VALUES (1)",
		"CREATE TABLE copy AS system.query_log",
		"RENAME TABLE system.query_log TO leak",
		"EXCHANGE TABLES leak AND system.query_log",
		// Calls and bare names that reveal the deployment.
		"SELECT version()",
		"SELECT VeRsIoN()",
		"SELECT currentDatabase(), 1",
		"SELECT * FROM (SELECT currentUser())",
		"SELECT currentUser",
		"SELECT CURRENT_USER",
		"SELECT user()",
		"SELECT hostName()",
		"SELECT FQDN()",
		"SELECT serverUUID()",
		"SELECT getMacro('shard')",
		"WITH metadata AS (SELECT hostName()) SELECT * FROM metadata",
	}
	for _, sql := range refused {
		expectMetadataRefused(t, svc, sentioRequest(sql, true))
	}
}

func TestStaticMetadataAccess_OrdinaryReadsAreAccepted(t *testing.T) {
	svc := metadataService(t)
	accepted := []string{
		"SELECT * FROM Transfer",
		"SELECT * FROM `other.Transfer`",
		"SELECT * FROM Transfer AS a JOIN `other.Transfer` AS b ON a.x = b.x",
		"SELECT number FROM numbers(10)",
		"SELECT * FROM remote('evil:9000', default.t)",
		"SELECT * FROM merge('sentio_proc_1', '^event_')",
		"SELECT * FROM mysql('db:3306', 'shop', 'orders', 'u', 'p')",
		"SELECT * FROM jdbc('datasource', 'orders')",
		"SELECT dictGet('sentio_proc_1.d', 'a', 1)",
		"SELECT dictGet('d', 'a', 1)",
		"SELECT hasColumnInTable('sentio_proc_1', 't', 'c')",
		"SELECT user FROM project_users",
		"SELECT displayName(), timezone(), uptime()",
		"SELECT 'system.tables', 'version()' FROM Transfer",
		"SELECT * FROM system_backup.t",
		"SELECT toDecimal256(value, 18) FROM Transfer WHERE `from` IN ('a', 'b')",
	}
	for _, sql := range accepted {
		resp := rewriteReq(t, svc, sentioRequest(sql, true))
		if resp.GetCode() != pb.RewriteCode_Success {
			t.Errorf("%q: got %v %q, want Success", sql, resp.GetCode(), resp.GetMessage())
		}
		if !resp.GetMetadataAccessRefusalApplied() {
			t.Errorf("%q: metadata_access_refusal_applied not set", sql)
		}
	}
}

func TestStaticMetadataAccess_WithoutTheFlagStaticModeIsUnchanged(t *testing.T) {
	svc := metadataService(t)
	for _, sql := range []string{"SELECT * FROM system.tables", "SELECT version()"} {
		off := rewriteReq(t, svc, sentioRequest(sql, false))
		if off.GetCode() != pb.RewriteCode_Success {
			t.Errorf("%q without the flag: got %v %q, want Success", sql, off.GetCode(), off.GetMessage())
		}
		if off.GetMetadataAccessRefusalApplied() {
			t.Errorf("%q without the flag: metadata_access_refusal_applied set", sql)
		}
	}
}

func TestStaticMetadataAccess_RewriteOptionsCannotIntroduceAMetadataRead(t *testing.T) {
	svc := metadataService(t)
	staticArgs := func(req *pb.RewriteSQLRequest) *pb.RewriteTableStaticArgs {
		return req.GetOptions()[2].GetTableNameArgs().GetStaticArgs()
	}
	// A table map whose target is a metadata table.
	req := sentioRequest("SELECT * FROM Transfer", true)
	staticArgs(req).TableWithDatabaseMap["Transfer"] = &pb.RewriteTableStaticArgs_TableWithDatabase{Database: "system", Table: "query_log"}
	expectMetadataRefused(t, svc, req)
	// A remote map entry that addresses a metadata database.
	req = sentioRequest("SELECT * FROM `other.Transfer`", true)
	staticArgs(req).RemoteTableMap["other.Transfer"].Database = "system"
	expectMetadataRefused(t, svc, req)
	// An injected CTE body that reads one, or reveals the deployment.
	for _, body := range []string{"SELECT name FROM system.tables", "SELECT currentDatabase() AS db"} {
		req := sentioRequest("SELECT * FROM leak", true)
		req.Options = append(req.Options, &pb.RewriteOption{
			Op: pb.RewriteOp_CommonTableExprRewrite,
			Value: &pb.RewriteOption_CommonTableExprArgs{CommonTableExprArgs: &pb.RewriteCommonTableExprArgs{
				CteMap: map[string]*pb.RewriteCommonTableExprArgs_CommonTableExpr{"leak": {Alias: "leak", Sql: body}},
			}},
		})
		expectMetadataRefused(t, svc, req)
	}
}

func TestStaticMetadataAccess_EveryResponsePathAcknowledgesTheFlag(t *testing.T) {
	svc := metadataService(t)
	for _, sql := range []string{"SELEC * FROM Transfer", "", "SELECT * FROM system.one"} {
		resp := rewriteReq(t, svc, sentioRequest(sql, true))
		if resp.GetCode() == pb.RewriteCode_Success {
			t.Errorf("%q: got Success", sql)
		}
		if !resp.GetMetadataAccessRefusalApplied() {
			t.Errorf("%q: metadata_access_refusal_applied not set", sql)
		}
	}
	// No table option at all (Sentio's cursor path).
	expectMetadataRefused(t, svc, &pb.RewriteSQLRequest{Sql: "SELECT * FROM system.tables", RefuseMetadataAccess: true})
}

func TestStaticMetadataAccess_AppliesInDynamicModeToo(t *testing.T) {
	svc := metadataService(t)
	// system.settings is on the dynamic-mode allowlist; the flag refuses it.
	req := &pb.RewriteSQLRequest{
		Sql:                  "SELECT * FROM system.settings",
		RefuseMetadataAccess: true,
		Options:              dynOpts(map[string]string{"tenant1": "physical_db"}, nil),
	}
	expectMetadataRefused(t, svc, req)
}
