package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// System-table allowlist, step 1 (spec 2026-09-26 §5, "The system
// database"). Every tenant reaches ClickHouse as one shared user, so
// ClickHouse's own per-user filters on system.* filter nothing: a caller-input
// reference to a system table outside nameresolve's allowlist is refused with
// UnsupportedStatement "system table system.<t> is not accessible", in every
// position and both SI states. Allowed tables keep today's behaviour.

func systemMsg(table string) string { return nameresolve.SystemTableRefusedMessage(table) }

// sysRefused builds one refused case per SI state.
func sysRefused(name, sql, table string) []tablerefCase {
	var out []tablerefCase
	for _, si := range []bool{false, true} {
		out = append(out, tablerefCase{name: name, sql: sql, si: si,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: systemMsg(table), wantSQL: sql})
	}
	return out
}

// sysAllowed builds one Success case per SI state that pins the SQL unchanged.
func sysAllowed(name, sql string) []tablerefCase {
	var out []tablerefCase
	for _, si := range []bool{false, true} {
		out = append(out, tablerefCase{name: name, sql: sql, si: si,
			wantCode: pb.RewriteCode_Success, wantSQL: sql})
	}
	return out
}

func TestSystemTables_AllowlistIsExact(t *testing.T) {
	allowed := []string{
		// class (b)
		"aggregate_function_combinators", "azure_queue_settings", "build_options", "codecs", "collations",
		"contributors", "dashboards", "data_type_families", "database_engines", "formats", "functions",
		"jemalloc_bins", "jemalloc_stats", "keywords", "licenses", "merge_tree_settings", "numbers",
		"numbers_mt", "one", "primes", "privileges", "replicated_merge_tree_settings", "s3_queue_settings",
		"settings", "settings_changes", "table_engines", "table_functions", "time_zones", "tokenizers",
		"unicode", "warnings", "zeros", "zeros_mt",
		// clusters (b*, read by the Sentio driver)
		"clusters",
		// class (c) read by the Sentio driver / clients; step 2 rewrites them
		"tables", "columns", "data_skipping_indices", "projections", "parts", "parts_columns",
		"projection_parts", "projection_parts_columns", "databases", "completions", "mutations",
	}
	for _, table := range allowed {
		if !nameresolve.SystemTableAllowed(table) {
			t.Errorf("system.%s should be allowed", table)
		}
	}
	if got, want := len(nameresolve.SystemTableAllowlist()), len(allowed); got != want {
		t.Errorf("allowlist has %d entries, want %d", got, want)
	}
	for _, table := range []string{
		"processes", "merges", "dictionaries", "query_log", "text_log", "errors", "users", "grants",
		"asynchronous_metrics", "events", "metrics", "detached_parts", "rocksdb", "iceberg_history",
		"database_replicas", "dropped_tables", "dropped_tables_parts", "detached_tables", "part_log",
		"zookeeper", "session_log", "future_table_nobody_knows",
		// case-sensitive, like ClickHouse (measured on 26.2: system.Tables is UNKNOWN_TABLE)
		"Tables", "TABLES", "PROCESSES",
		"", "tables ", "tables\\N",
	} {
		if nameresolve.SystemTableAllowed(table) {
			t.Errorf("system.%q should be refused", table)
		}
	}
}

func TestSystemTables_AllowedTablesKeepTodaysBehaviour(t *testing.T) {
	var cases []tablerefCase
	for _, table := range nameresolve.SystemTableAllowlist() {
		sql := "SELECT * FROM system." + table
		cases = append(cases, sysAllowed(sql, sql)...)
	}
	cases = append(cases, tablerefCase{name: "backtick allowed", sql: "SELECT * FROM `system`.`tables`",
		wantCode: pb.RewriteCode_Success, wantSQL: `SELECT * FROM "system"."tables"`})
	for _, sql := range []string{
		`SELECT * FROM "system".columns`,
		"SELECT * FROM information_schema.tables",
		"SELECT * FROM INFORMATION_SCHEMA.TABLES",
		"SELECT * FROM information_schema.columns WHERE table_schema = 'phys'",
		"SELECT name FROM system.tables WHERE database = 'phys' AND name LIKE 'db1.%'",
		"SELECT * FROM system.mutations WHERE database = 'phys' AND is_done = 0",
		"SELECT * FROM system.parts WHERE database = 'phys' AND active",
		"SELECT * FROM system.clusters",
		"SELECT 1",
		"SELECT * FROM numbers(3)",
		"SHOW SETTINGS LIKE 'max%'",
		"SHOW FUNCTIONS",
		"SHOW ENGINES",
		"SHOW CLUSTERS",
		"SHOW CLUSTER 'default'",
	} {
		cases = append(cases, sysAllowed(sql, sql)...)
	}
	cases = append(cases,
		tablerefCase{name: "IN allowed operand", sql: "SELECT * FROM db1.o WHERE a IN system.tables", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN system.tables`, wantAcc: []string{"db1.o", "system.tables"}},
		tablerefCase{name: "join allowed", sql: "SELECT * FROM db1.o JOIN system.columns USING (a)", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" JOIN system.columns USING (a)`},
		tablerefCase{name: "insert select allowed", sql: "INSERT INTO db1.o SELECT name FROM system.tables", wantCode: pb.RewriteCode_Success,
			wantSQL: `INSERT INTO phys."db1.o" SELECT name FROM system.tables`},
		// Harmless SHOW forms the SI surface does not model keep their
		// inactive-surface pass-through.
		tablerefCase{name: "show changed settings", sql: "SHOW CHANGED SETTINGS", wantCode: pb.RewriteCode_Success, wantSQL: "SHOW CHANGED SETTINGS"},
		tablerefCase{name: "show setting", sql: "SHOW SETTING max_threads", wantCode: pb.RewriteCode_Success, wantSQL: "SHOW SETTING max_threads"},
		tablerefCase{name: "show privileges", sql: "SHOW PRIVILEGES", wantCode: pb.RewriteCode_Success, wantSQL: "SHOW PRIVILEGES"},
		// The rewriter's own system.tables synthesis is engine output, not
		// caller input.
		tablerefCase{name: "show tables synthesis", sql: "SHOW TABLES", wantCode: pb.RewriteCode_Success,
			wantSQL: "SELECT multiIf(startsWith(name, 'db1.'), substring(name, length('db1.') + 1), name) AS name FROM (SELECT name FROM system.tables WHERE database = 'phys' AND startsWith(name, 'db1.'))"},
		tablerefCase{name: "show tables synthesis SI", sql: "SHOW TABLES", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: "SELECT multiIf(startsWith(name, 'db1.'), substring(name, length('db1.') + 1), name) AS name FROM (SELECT name FROM system.tables WHERE database = 'phys' AND startsWith(name, 'db1.'))"},
		tablerefCase{name: "show databases synthesis", sql: "SHOW DATABASES", wantCode: pb.RewriteCode_Success,
			wantSQL: "SELECT name FROM (SELECT 'db1' AS name) ORDER BY name"},
	)
	runTablerefCases(t, cases)
}

func TestSystemTables_RefusedEverywhere(t *testing.T) {
	var cases []tablerefCase
	for _, table := range []string{
		"processes", "merges", "dictionaries", "query_log", "text_log", "errors", "part_log",
		"users", "grants", "asynchronous_metrics", "events", "detached_parts", "rocksdb",
		"iceberg_history", "database_replicas", "dropped_tables", "dropped_tables_parts",
		"detached_tables", "future_table_nobody_knows", "Tables", "PROCESSES",
	} {
		sql := "SELECT * FROM system." + table
		cases = append(cases, sysRefused(sql, sql, table)...)
	}
	for _, c := range []struct{ sql, table string }{
		// quoting and escapes, decoded the way ClickHouse's ParserIdentifier does
		{"SELECT * FROM `system`.`processes`", "processes"},
		{`SELECT * FROM "system"."processes"`, "processes"},
		{"SELECT * FROM system.`\\x70rocesses`", "processes"},
		{"SELECT * FROM `\\x73ystem`.processes", "processes"},
		{"SELECT * FROM `sys\\x74em`.`\\x70rocesses`", "processes"},
		{"SELECT * FROM `system\\N`.processes", "processes"},
		{"SELECT * FROM system.`proc\\Nesses`", "processes"},
		// positions
		{"SELECT * FROM db1.o JOIN system.processes USING (a)", "processes"},
		{"SELECT * FROM db1.o, system.processes", "processes"},
		{"SELECT * FROM system.processes AS p JOIN db1.o USING (a)", "processes"},
		{"SELECT * FROM db1.o WHERE a IN system.processes", "processes"},
		{"SELECT * FROM db1.o WHERE a IN (system.processes)", "processes"},
		{"SELECT * FROM db1.o WHERE a GLOBAL IN system.processes", "processes"},
		{"SELECT * FROM db1.o WHERE in(a, system.processes)", "processes"},
		{"SELECT * FROM db1.o WHERE a NOT IN system.processes", "processes"},
		{"SELECT * FROM db1.o WHERE a IN (SELECT query FROM system.query_log)", "query_log"},
		{"SELECT (SELECT count() FROM system.merges)", "merges"},
		{"SELECT * FROM (SELECT * FROM system.processes)", "processes"},
		{"WITH x AS (SELECT * FROM system.processes) SELECT * FROM x", "processes"},
		{"SELECT * FROM db1.o UNION ALL SELECT * FROM system.processes", "processes"},
		{"SELECT * FROM db1.o ARRAY JOIN (SELECT groupArray(1) FROM system.processes) AS x", "processes"},
		{"SELECT * FROM view(SELECT * FROM system.processes)", "processes"},
		{"INSERT INTO db1.o SELECT query FROM system.query_log", "query_log"},
		{"INSERT INTO db1.o (a) SELECT query FROM system.query_log", "query_log"},
		{"CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM system.processes", "processes"},
		{"CREATE VIEW db1.v AS SELECT query FROM system.query_log", "query_log"},
		{"CREATE MATERIALIZED VIEW db1.mv TO db1.o AS SELECT query FROM system.query_log", "query_log"},
		{"CREATE MATERIALIZED VIEW db1.mv ENGINE = Memory AS SELECT query FROM system.query_log", "query_log"},
		{"CREATE TABLE db1.n AS system.processes", "processes"},
		{"INSERT INTO system.query_log SELECT * FROM db1.o", "query_log"},
		{"DROP TABLE system.query_log", "query_log"},
		{"RENAME TABLE system.query_log TO db1.x", "query_log"},
		{"DELETE FROM db1.o WHERE a IN (SELECT 1 FROM system.processes)", "processes"},
		{"ALTER TABLE db1.o DELETE WHERE a IN (SELECT 1 FROM system.processes)", "processes"},
		{"ALTER TABLE db1.o UPDATE a = 1 WHERE a IN (SELECT 1 FROM system.processes)", "processes"},
		{"CREATE TABLE db1.n (a UInt64 DEFAULT (SELECT count() FROM system.processes)) ENGINE = Memory", "processes"},
		{"CREATE VIEW db1.v (a UInt8 DEFAULT (SELECT count() FROM system.processes)) AS SELECT 1 AS a", "processes"},
		{"CREATE VIEW db1.v (a UInt8, INDEX i a IN system.processes TYPE minmax) AS SELECT 1 AS a", "processes"},
		{"CREATE MATERIALIZED VIEW db1.mv (a UInt8, PROJECTION p (SELECT a FROM system.processes)) ENGINE = Memory AS SELECT 1 AS a", "processes"},
		// DESCRIBE / EXISTS / SHOW CREATE / SHOW COLUMNS family
		{"DESCRIBE system.processes", "processes"},
		{"DESCRIBE TABLE `system`.processes", "processes"},
		{"DESC `system`.`processes`", "processes"},
		{"EXISTS system.processes", "processes"},
		{"EXISTS TABLE `system`.processes", "processes"},
		{"SHOW CREATE TABLE system.processes", "processes"},
		{"SHOW CREATE TABLE `system`.processes", "processes"},
		{"SHOW CREATE DICTIONARY system.d", "d"},
		{"SHOW COLUMNS FROM system.processes", "processes"},
		{"SHOW COLUMNS FROM `system`.`processes`", "processes"},
		{"SHOW COLUMNS FROM `system\\N`.processes", "processes"},
		{"SHOW COLUMNS FROM processes FROM system", "processes"},
		{"SHOW COLUMNS FROM processes IN `system\\N`", "processes"},
		{"SHOW FULL COLUMNS FROM system.processes", "processes"},
		{"SHOW INDEX FROM system.processes", "processes"},
		{"SHOW KEYS FROM system.processes", "processes"},
		// table functions, table engines and string lookups naming system
		{"SELECT * FROM merge('system', '^processes$')", "^processes$"},
		{"SELECT * FROM merge('system', '^tables$')", "^tables$"},
		{"SELECT * FROM remote('127.0.0.1', system.processes)", "processes"},
		{"SELECT * FROM remote('127.0.0.1', 'system', 'processes')", "processes"},
		{"SELECT * FROM remoteSecure('127.0.0.1', system.processes)", "processes"},
		{"SELECT * FROM cluster('default', system.processes)", "processes"},
		{"SELECT * FROM clusterAllReplicas('default', system, processes)", "processes"},
		{"SELECT * FROM loop(system, processes)", "processes"},
		{"SELECT * FROM mergeTreeIndex('system', 'processes')", "processes"},
		{"CREATE TABLE db1.n (a UInt64) ENGINE = Merge('system', '^proc')", "^proc"},
		{"CREATE TABLE db1.n (a UInt64) ENGINE = Distributed(default, system, processes)", "processes"},
		{"CREATE TABLE db1.n (a UInt64) ENGINE = Buffer(system, processes, 1, 1, 1, 1, 1, 1, 1)", "processes"},
		{"SELECT joinGet('system.processes', 'a', 1)", "processes"},
		{"SELECT dictGet('system.d', 'a', toUInt64(1))", "d"},
		{"SELECT hasColumnInTable('system', 'processes', 'query')", "processes"},
	} {
		cases = append(cases, sysRefused(c.sql, c.sql, c.table)...)
	}
	runTablerefCases(t, cases)
}

// TestSystemTables_ShowForms pins the SHOW forms that read a refused system
// table (spec 2026-09-26 §5, SHOW → system mapping measured on 26.2 / 25.8).
func TestSystemTables_ShowForms(t *testing.T) {
	var cases []tablerefCase
	for _, c := range []struct{ sql, table string }{
		{"SHOW PROCESSLIST", "processes"},
		{"SHOW MERGES", "merges"},
		{"SHOW DICTIONARIES FROM db2", "dictionaries"},
		{"SHOW FULL DICTIONARIES FROM db2", "dictionaries"},
		{"SHOW DICTIONARIES FROM system", "dictionaries"},
		{"SHOW DICTIONARIES FROM nosuchdb LIKE '%'", "dictionaries"},
		{"SHOW USERS", "users"},
		{"SHOW ROLES", "roles"},
		{"SHOW GRANTS", "grants"},
		{"SHOW GRANTS FOR default", "grants"},
		{"SHOW ACCESS", "users"},
		{"SHOW PROFILES", "settings_profiles"},
		{"SHOW SETTINGS PROFILES", "settings_profiles"},
		{"SHOW ROW POLICIES", "row_policies"},
		{"SHOW POLICIES", "row_policies"},
		{"SHOW QUOTA", "quota_usage"},
		{"SHOW QUOTAS", "quotas"},
		{"SHOW FILESYSTEM CACHES", "filesystem_cache_settings"},
	} {
		cases = append(cases, sysRefused(c.sql, c.sql, c.table)...)
	}
	// Without FROM, SHOW DICTIONARIES runs in the session database; with
	// the SI surface active and db1 owning an Active table, the SI namespace
	// message keeps precedence.
	for _, sql := range []string{"SHOW DICTIONARIES", "SHOW DICTIONARIES LIKE '%'", "SHOW DICTIONARIES FROM db1"} {
		cases = append(cases,
			tablerefCase{name: sql, sql: sql, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: systemMsg("dictionaries")},
			tablerefCase{name: sql + " SI", sql: sql, si: true, wantCode: pb.RewriteCode_UnsupportedStatement,
				wantMsg: "storage-integrity logical database db1 is not directly addressable"})
	}
	// SHOW kinds the SI surface does not model keep the SI catch-all under V2.
	for _, c := range []struct{ sql, table string }{
		{"SHOW CURRENT ROLES", "current_roles"},
		{"SHOW ENABLED ROLES", "enabled_roles"},
		{"SHOW CURRENT QUOTA", "quota_usage"},
	} {
		cases = append(cases,
			tablerefCase{name: c.sql, sql: c.sql, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: systemMsg(c.table)},
			tablerefCase{name: c.sql + " SI", sql: c.sql, si: true, wantCode: pb.RewriteCode_UnsupportedStatement,
				wantMsg: nameresolve.StorageIntegrityUnmodelledMessage})
	}
	// An unknown SHOW kind reads an unknown system table: refused.
	cases = append(cases, tablerefCase{name: "unknown show kind", sql: "SHOW WIDGETS",
		wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"})
	// SHOW CREATE of an access entity or a dictionary was already refused;
	// the code does not move.
	for _, sql := range []string{
		"SHOW CREATE USER default", "SHOW CREATE ROLE r", "SHOW CREATE QUOTA q",
		"SHOW CREATE ROW POLICY p ON db1.o", "SHOW CREATE SETTINGS PROFILE p", "SHOW CREATE PROFILE p",
		"SHOW CREATE DICTIONARY db1.d",
	} {
		for _, si := range []bool{false, true} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: pb.RewriteCode_UnsupportedStatement})
		}
	}
	runTablerefCases(t, cases)
}

// TestSystemTables_Precedence pins where the check sits: after T2, T7 and
// T3, before T5 / T6 / R5 / R2 with the SI surface inactive; with it active,
// after the SI pipeline, so an SI message keeps precedence.
func TestSystemTables_Precedence(t *testing.T) {
	runTablerefCases(t, []tablerefCase{
		{name: "T2 first", sql: "SELECT * FROM {p:Identifier} JOIN system.processes USING (a)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "query parameters are not supported"},
		{name: "T7 first", sql: "EXPLAIN SELECT * FROM system.processes",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"},
		{name: "T7 first check", sql: "CHECK TABLE system.processes",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"},
		{name: "T7 first SI", sql: "EXPLAIN SELECT * FROM system.processes", si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: nameresolve.StorageIntegrityUnmodelledMessage},
		{name: "T3 first", sql: "SELECT * FROM system.processes JOIN phys.x USING (a)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable"},
		{name: "before T5", sql: "SELECT * FROM system.processes JOIN merge('db1', 'o') USING (a)",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: systemMsg("processes")},
		{name: "before T6", sql: "SELECT joinGet('db1.o', 'a', 1) FROM system.processes",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: systemMsg("processes")},
		{name: "before unresolved", sql: "SELECT * FROM `other.x` JOIN system.processes USING (a)",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: systemMsg("processes")},
		{name: "first in document order", sql: "SELECT * FROM system.merges JOIN system.processes USING (a)",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: systemMsg("merges")},
		{name: "allowed then refused", sql: "SELECT * FROM system.tables JOIN system.processes USING (a)",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: systemMsg("processes")},
		// SI active: the SI pipeline answers first.
		{name: "SI physical wins", sql: "SELECT * FROM hg_safe.db1__t JOIN system.processes USING (a)", si: true,
			wantCode: pb.RewriteCode_RewriteError, wantMsg: "storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		{name: "SI view write wins", sql: "CREATE VIEW db1.v AS SELECT * FROM db1.t JOIN system.processes USING (a)", si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "storage-integrity table db1.t accepts writes only through the signed statement lane"},
		{name: "SI derived read then system", sql: "SELECT * FROM db1.t JOIN system.processes USING (a)", si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: systemMsg("processes")},
		{name: "inactive view", sql: "CREATE VIEW db1.v AS SELECT * FROM db1.t JOIN system.processes USING (a)",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: systemMsg("processes")},
	})
}

// TestSystemTables_UnqualifiedInSystemContext: an unqualified name resolves
// in the session's logical database; when that is `system`, the reference is
// a system table.
func TestSystemTables_UnqualifiedInSystemContext(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, c := range []struct {
			sql, msg string
		}{
			{"SELECT * FROM processes", systemMsg("processes")},
			{"SELECT * FROM db1.o WHERE a IN processes", systemMsg("processes")},
			{"DESCRIBE processes", systemMsg("processes")},
			{"SHOW COLUMNS FROM processes", systemMsg("processes")},
			// An allowed table keeps today's refusal: `system` is not a
			// mapped logical database.
			{"SELECT * FROM tables", nameresolve.UnresolvedUnqualifiedTableMessage("tables")},
		} {
			dyn := tablerefDynamic(si)
			dyn.UpstreamLogicalDatabaseInContext = "system"
			resp, err := doRewrite(e, c.sql, []*pb.RewriteOption{tableRewriteDynamic(dyn)})
			if err != nil {
				t.Fatalf("%s: %v", c.sql, err)
			}
			if resp.GetCode() == pb.RewriteCode_Success || !strings.Contains(resp.GetMessage(), c.msg) {
				t.Errorf("si=%v %q: code=%s msg=%q, want refusal containing %q", si, c.sql, resp.GetCode(), resp.GetMessage(), c.msg)
			}
		}
	}
}

// TestSystemTables_RefusalReportsTheTable: the refusal reports the system
// table it names in original_accessed_tables, so the host sees the object.
func TestSystemTables_RefusalReportsTheTable(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		resp, err := doRewrite(e, "SELECT * FROM system.processes", tablerefOpts(si))
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, a := range resp.GetOriginalAccessedTables() {
			if a.GetOriginalDatabase() == "system" && a.GetOriginalTable() == "processes" {
				found = true
			}
		}
		if !found || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_UNSPECIFIED ||
			len(resp.GetTableRewrites()) != 0 {
			t.Fatalf("si=%v accessed=%v stmt=%s rewrites=%v", si, resp.GetOriginalAccessedTables(), resp.GetStatementType(), resp.GetTableRewrites())
		}
	}
}
