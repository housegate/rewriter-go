package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// Statements, clauses, functions and settings ClickHouse 26.4 – 26.8 added,
// as reviewed against the upstream changelog when the servers moved to the
// official 26.8.37.168 image (internal design doc 2026-11-14 official
// ClickHouse switch, Step 5). rewriter-grpc pins the same statements in
// TableRefFidelity.ClickHouse264To268StatementForms,
// TableRefLookup.NaiveBayesClassifierIsAStringLookup and
// MetadataAccess.{ClickHouse268BareIdentityNamesAreRefused,
// AIFunctionsAreRefusedInDynamicMode}; the storage-integrity corpus holds the
// shared answers over an SI table.

// TestDeploymentIdentity_ClickHouse268BareNames: since 26.5 a bare function
// name as a higher-order function's first argument is that function applied
// to the lambda arguments when no column or alias has the name and the
// function takes arguments. Measured on 26.8.37.168,
// `SELECT arrayMap(currentSchemas, [true])` returns the physical session
// database and `arrayMap(pgGetUserById, [toUInt32(0)])` the shared ClickHouse
// user. A function that allows omitting its parentheses is called by its bare
// name: `SELECT SESSION_USER` (the 26.6 alias of currentUser) returns the
// shared user, and the 26.8 currentRequestURL / currentHandler resolve the
// same way. Every spelling is refused like the call.
func TestDeploymentIdentity_ClickHouse268BareNames(t *testing.T) {
	refused := []struct{ sql, name string }{
		{"SELECT arrayMap(currentSchemas, [true])", "currentSchemas"},
		{"SELECT arrayMap(current_schemas, [true])", "current_schemas"},
		{"SELECT arrayMap(CURRENTSCHEMAS, [true])", "CURRENTSCHEMAS"},
		{"SELECT arrayMap(`currentSchemas`, [true])", "currentSchemas"},
		{"SELECT arrayFilter(x -> has(x, 'a'), arrayMap(currentSchemas, [true]))", "currentSchemas"},
		{"SELECT arrayMap(pgGetUserById, [toUInt32(0)])", "pgGetUserById"},
		{"SELECT arrayMap(pg_get_userbyid, [1])", "pg_get_userbyid"},
		{"SELECT a FROM db1.o WHERE has(arrayMap(currentSchemas, [true]), [s])", "currentSchemas"},
		{"SELECT SESSION_USER", "SESSION_USER"},
		{"SELECT session_user", "session_user"},
		{"SELECT a FROM db1.o WHERE s = SESSION_USER", "SESSION_USER"},
		{"SELECT currentRequestURL", "currentRequestURL"},
		{"SELECT currentHandler", "currentHandler"},
		{"SELECT current_schemas FROM db1.o", "current_schemas"},
	}
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range refused {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement,
				wantMsg:  "function " + c.name + " is not accessible", wantSQL: c.sql})
		}
		for _, sql := range []string{
			"SELECT arrayMap(negate, [1, 2])", "SELECT arrayMap(toString, [a]) FROM db1.o",
			"SELECT o.currentSchemas FROM db1.o AS o", "SELECT pgTableIsVisible(1)",
			"SELECT arrayMap(pg_table_is_visible, [1])", "SELECT 'SESSION_USER'",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: pb.RewriteCode_Success})
		}
	}
	runTablerefCases(t, cases)
}

// TestExternalCall_AIFunctionsAreRefused: ClickHouse 26.8's AI functions send
// their text to the LLM provider of a named collection (the call's
// `credentials` parameter or the ai_function_*_default_credentials setting;
// generally available, a SQL-created collection admitted by default).
// Whether a collection the shared ClickHouse user may use exists is
// deployment configuration the engine cannot see, so every spelling is
// refused in dynamic mode, as a call and as a higher-order function's bare
// argument, with the deployment-identity message (rewriter-grpc
// isExternalCallFunctionName).
func TestExternalCall_AIFunctionsAreRefused(t *testing.T) {
	refused := []struct{ sql, name string }{
		{"SELECT aiGenerate('hi')", "aiGenerate"},
		{"SELECT AIGenerate('hi')", "AIGenerate"},
		{"SELECT aiClassify('x', ['a', 'b'])", "aiClassify"},
		{"SELECT AIClassify('x', ['a', 'b'])", "AIClassify"},
		{"SELECT aiExtract('x', 'name')", "aiExtract"},
		{"SELECT AIExtract('x', 'name')", "AIExtract"},
		{"SELECT aiTranslate('x', 'fr')", "aiTranslate"},
		{"SELECT AITranslate('x', 'fr')", "AITranslate"},
		{"SELECT aiEmbed('x')", "aiEmbed"},
		{"SELECT AIEmbed('x')", "AIEmbed"},
		{"SELECT aiFilter('x', 'is it')", "aiFilter"},
		{"SELECT AIFilter('x', 'is it')", "AIFilter"},
		{"SELECT aiRedact('x', ['email'])", "aiRedact"},
		{"SELECT AIRedact('x', ['email'])", "AIRedact"},
		{"SELECT aiSimilarity('x', 'y')", "aiSimilarity"},
		{"SELECT AISimilarity('x', 'y')", "AISimilarity"},
		{"SELECT `aiGenerate`('hi')", "aiGenerate"},
		{"SELECT aigenerate('hi')", "aigenerate"},
		{"SELECT aiGenerate('hi', map('credentials', 'c'))", "aiGenerate"},
		{"SELECT a FROM db1.o WHERE aiFilter(s, 'cond')", "aiFilter"},
		{"INSERT INTO db1.o (s) VALUES (aiGenerate('x'))", "aiGenerate"},
		{"INSERT INTO db1.p SELECT aiGenerate(s) FROM db1.o", "aiGenerate"},
		{"CREATE TABLE db1.n (a String DEFAULT aiGenerate('x')) ENGINE = Memory", "aiGenerate"},
		{"CREATE VIEW db1.v AS SELECT aiGenerate(s) FROM db1.o", "aiGenerate"},
		{"SELECT arrayMap(x -> aiGenerate(x), ['a'])", "aiGenerate"},
		{"SELECT arrayMap(aiGenerate, ['x'])", "aiGenerate"},
		{"SELECT arrayMap(AIEmbed, ['x'])", "AIEmbed"},
		{"SELECT arrayFilter(aiFilter, ['a'], ['c'])", "aiFilter"},
	}
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range refused {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement,
				wantMsg:  "function " + c.name + " is not accessible", wantSQL: c.sql})
		}
		for _, sql := range []string{
			"SELECT 'aiGenerate(1)'", "SELECT o.aiEmbed FROM db1.o AS o", "SELECT aiGenerated(1) FROM db1.o",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: pb.RewriteCode_Success})
		}
	}
	runTablerefCases(t, cases)
	// Static mode keeps its legacy surface, as for the identity calls.
	e := newEngine(t)
	resp, err := doRewrite(e, "SELECT aiGenerate('hi')", []*pb.RewriteOption{tableRewriteStatic()})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Errorf("static mode: code=%v message=%q", resp.GetCode(), resp.GetMessage())
	}
}

// TestTableRef_NaiveBayesClassifierIsAStringLookup: ClickHouse 26.7 made the
// naiveBayesClassifier family read the NAIVE_BAYES dictionary its first
// argument names, resolved like a dictGet target (measured on 26.8.37.168:
// `naiveBayesClassifier('nosuch', 't')` is `Dictionary (`nosuch`) not
// found`), so it joins the T6 string-lookup family. naiveBayesNgrams reads no
// dictionary.
func TestTableRef_NaiveBayesClassifierIsAStringLookup(t *testing.T) {
	const tail = " does not resolve through the caller's databases"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, msg string }{
			{"SELECT naiveBayesClassifier('d', s) FROM db1.o", `naiveBayesClassifier target "d"`},
			{"SELECT naiveBayesClassifier('db2.d', s) FROM db1.o", `naiveBayesClassifier target "db2.d"`},
			{"SELECT naiveBayesClassifierWithProb(db1.d, s) FROM db1.o", `naiveBayesClassifierWithProb target "db1.d"`},
			{"SELECT naiveBayesClassifierWithAllProbs('db1.d', 'text')", `naiveBayesClassifierWithAllProbs target "db1.d"`},
			{"INSERT INTO db1.o (s) VALUES (naiveBayesClassifier('d', 'x'))", `naiveBayesClassifier target "d"`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: c.msg + tail, wantSQL: c.sql})
		}
		cases = append(cases,
			tablerefCase{name: "phys", sql: "SELECT naiveBayesClassifier('phys.d', s) FROM db1.o", si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable"},
			tablerefCase{name: "ngrams", sql: "SELECT naiveBayesNgrams('text', 3)", si: si,
				wantCode: pb.RewriteCode_Success})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_ClickHouse268ShapesAreRefused mirrors rewriter-grpc
// TableRefFidelity.ClickHouse268ShapesAreRefused: a STREAM table modifier
// (Polyglot reads an unquoted bare `STREAM` after a table expression as its
// alias; 26.8 reads a continuous query) and a NATURAL join are refused with
// `statement is not supported` in both SI states before any name is
// examined, as are a LOCAL join after an aliased table, a MergeTree UNIQUE
// KEY and DESCRIBE TEMPORARY TABLE. An explicit or quoted `stream` alias and
// a table aliased `natural` stay ordinary.
func TestTableRef_ClickHouse268ShapesAreRefused(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"SELECT a FROM db1.o STREAM",
			"SELECT a FROM db1.o stream",
			"SELECT a FROM db1.o Stream",
			"SELECT a FROM db1.t STREAM",
			"SELECT a FROM (SELECT a FROM db1.o) STREAM",
			"SELECT * FROM numbers(3) STREAM",
			"SELECT a FROM db1.o STREAM BOUNDED UNORDERED",
			"SELECT a FROM db1.o STREAM WATERMARK FOR a AS a - 1",
			"SELECT a FROM (SELECT a FROM db1.o STREAM) AS s",
			"SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.o STREAM)",
			"WITH c AS (SELECT a FROM db1.o STREAM) SELECT a FROM c",
			"SELECT a FROM db1.o AS l JOIN db1.o AS r STREAM ON l.a = r.a",
			"INSERT INTO db1.q SELECT a FROM db1.o STREAM",
			"CREATE VIEW db1.v AS SELECT a FROM db1.o STREAM",
			"DESCRIBE TEMPORARY TABLE o",
			"DESC TEMPORARY TABLE o",
			"SELECT * FROM db1.o AS x LOCAL JOIN db1.p AS y ON x.a = y.a",
			"SELECT * FROM db1.o AS x NATURAL JOIN db1.p AS y",
			"SELECT * FROM db1.o NATURAL JOIN db1.p",
			"SELECT * FROM db1.o natural join db1.p",
			"SELECT * FROM db1.o NATURAL LEFT JOIN db1.p",
			"SELECT * FROM db1.t NATURAL JOIN db1.o",
			"CREATE TABLE db1.n (a UInt8) ENGINE = MergeTree ORDER BY a UNIQUE KEY a",
			"CREATE TABLE db1.n (a UInt8) ENGINE = MergeTree ORDER BY a UNIQUE KEY (a IN (SELECT 1 FROM db2.x))",
			"CREATE MATERIALIZED VIEW db1.mv ENGINE = MergeTree ORDER BY a UNIQUE KEY a AS SELECT a FROM db1.o",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported", wantSQL: sql})
		}
		for _, sql := range []string{
			"SELECT a FROM db1.o AS stream",
			"SELECT a FROM db1.o `STREAM`",
			"SELECT * FROM db1.o AS natural JOIN db1.p USING (a)",
			"SELECT a FROM db1.o",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: pb.RewriteCode_Success})
		}
		// Polyglot cannot parse a LOCAL join inside a subquery at all, so this
		// engine answers SyntaxError where rewriter-grpc answers `statement is
		// not supported`.
		cases = append(cases, tablerefCase{name: "local join in subquery", si: si,
			sql:      "SELECT a FROM db1.o WHERE a IN (SELECT a FROM db1.o AS x LOCAL JOIN db1.p AS y USING (a))",
			wantCode: pb.RewriteCode_SyntaxError})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_ClickHouse264To268StatementForms pins this engine's answer to
// the other 26.4 – 26.8 forms. rewriter-grpc's embedded 26.8 parser lowers
// `|>` pipe operators, a SELECT that starts with FROM, a string literal in a
// table position, `x = SOME([…])`, `LIMIT -n WITH TIES`, `SET TIME ZONE`,
// `MODIFY PROJECTION` and the MultiPoint type to SQL its checks read and
// rewrites them; Polyglot does not take them, so they are refused here
// (`statement is not supported`), a declared engine-local difference.
// Everything that reaches outside the tenant's databases or that no handler
// models is refused by both engines.
func TestTableRef_ClickHouse264To268StatementForms(t *testing.T) {
	const unsupported = "statement is not supported"
	unmappedDB2 := `database "db2" does not resolve through the caller's databases (it is not mapped for this account, or the account has no access)`
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		// Accepted by rewriter-grpc only.
		for _, sql := range []string{
			"FROM db1.o |> WHERE a > 1",
			"SELECT a FROM db1.o |> AGGREGATE count() AS c GROUP BY a",
			"FROM db1.o |> EXTEND a + 1 AS b",
			"FROM o",
			"INSERT INTO db1.p FROM db1.o |> SELECT a",
			"SELECT * FROM 'data.csv'",
			"SELECT * FROM 'db2.x'",
			"SELECT a = SOME([1, 2]) FROM db1.o",
			"SELECT a FROM db1.o ORDER BY a LIMIT -1 WITH TIES",
			"ALTER TABLE db1.o MODIFY PROJECTION p (SELECT a ORDER BY a) WITH SETTINGS (index_granularity = 1)",
			"CREATE TABLE db1.n (p MultiPoint) ENGINE = Memory",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: sql})
		}
		// Accepted by both engines.
		for _, sql := range []string{
			"SELECT * LIKE 'a%' FROM db1.o",
			"SELECT o.* ILIKE 'a%' FROM db1.o",
			"SELECT a FROM db1.o WHERE s LIKE 'a!_%' ESCAPE '!'",
			"SELECT date_part('year', now())",
			"SELECT EXTRACT(EPOCH FROM now())",
			"SELECT now() AT TIME ZONE 'UTC'",
			"SELECT now() AT LOCAL",
			"SELECT a < ALL([1, 2]) FROM db1.o",
			"SELECT a FROM db1.o LIMIT -2 BY a",
			"SELECT any(a) OVER (ORDER BY a GROUPS BETWEEN CURRENT ROW AND 1 FOLLOWING) FROM db1.o",
			"SELECT [1, 2, 3][[1, 2]]",
			"SELECT sumTuple((a, a)) FROM db1.o",
			"SELECT arrayMap(negate, [1, 2])",
			"SELECT notHas([1], 2)",
			"SELECT parseQueryToJSON('SELECT 1')",
			"ALTER TABLE db1.o MODIFY CONSTRAINT c CHECK a > 1",
			"ALTER TABLE db1.o MODIFY COLUMN e ADD ENUM VALUES ('c' = 3)",
			"ALTER TABLE db1.o ADD PROJECTION p (SELECT a WHERE a > 1)",
			"ALTER TABLE db1.o ADD COLUMN b UInt8 NOT NULL",
			"CREATE OR REPLACE MATERIALIZED VIEW db1.mv TO db1.p AS SELECT a FROM db1.o",
			"CREATE MATERIALIZED VIEW db1.mv TO db1.p POPULATE AS SELECT a FROM db1.o",
			"SELECT a FROM db1.o SETTINGS run_query_in_background = 1",
			"SELECT a FROM db1.o SETTINGS page = 2",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: pb.RewriteCode_Success})
		}
		// Refused by both engines with the same message.
		for _, c := range []struct {
			sql  string
			code pb.RewriteCode
			msg  string
		}{
			{"SELECT * FROM bigquery('p', 'd', 't')", pb.RewriteCode_UnsupportedStatement, "table function bigquery is not recognised"},
			{"SELECT * FROM filesystem('/etc')", pb.RewriteCode_UnsupportedStatement, "table function filesystem is not recognised"},
			{"SELECT * FROM eval('SELECT 1')", pb.RewriteCode_UnsupportedStatement, "table function eval is not recognised"},
			{"SELECT * FROM timeSeriesSamples(db1, ts)", pb.RewriteCode_UnsupportedStatement, "table function timeSeriesSamples is not recognised"},
			{"CREATE TABLE db1.n (a UInt8) ENGINE = QueryRunner", pb.RewriteCode_UnsupportedStatement, "table engine QueryRunner is not accepted"},
			{"CREATE TABLE db1.n (a UInt8) ENGINE = BigQuery('p', 'd', 't')", pb.RewriteCode_UnsupportedStatement, "table engine BigQuery is not accepted"},
			{"CREATE TABLE db1.n (a UInt8) ENGINE = PaimonLocal('/x')", pb.RewriteCode_UnsupportedStatement, "table engine PaimonLocal is not accepted"},
			{"ALTER TABLE db1.o MODIFY CONSTRAINT c CHECK a IN (SELECT a FROM db2.x)", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"ALTER TABLE db1.o ADD PROJECTION p (SELECT a WHERE a IN (SELECT a FROM db2.x))", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"ALTER TABLE db1.o MODIFY PROJECTION p (SELECT a WHERE a IN (SELECT a FROM db2.x))", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"CREATE TABLE db1.n (a UInt8, PROJECTION p (SELECT a WHERE a IN (SELECT 1 FROM db2.x))) ENGINE = MergeTree ORDER BY a", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"SELECT a FROM db1.o SETTINGS max_threads = {t:UInt64}", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"SELECT a FROM db1.o SETTINGS enable_json_ast_dialect = 1", pb.RewriteCode_UnsupportedStatement, "table setting enable_json_ast_dialect is not accepted"},
			{"SELECT a FROM system.user_query_log", pb.RewriteCode_UnsupportedStatement, "system table system.user_query_log is not accessible"},
			{"SELECT * FROM system.handlers", pb.RewriteCode_UnsupportedStatement, "system table system.handlers is not accessible"},
			{"CREATE MATERIALIZED VIEW db1.mv TO db1.p POPULATE AS SELECT a FROM db2.x", pb.RewriteCode_InvalidRewriteRequest, unmappedDB2},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: c.code, wantMsg: c.msg})
		}
	}
	runTablerefCases(t, cases)
	// Refused by both engines, whatever the message.
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"FROM db2.x |> SELECT a",
			"SELECT a FROM db1.o |> WHERE a IN (SELECT a FROM db2.x)",
			"SELECT a FROM db1.o WHERE a IN (FROM db2.x |> SELECT a)",
			"SELECT * FROM (VALUES (1, 'a'), (2, 'b')) AS v(id, val)",
			"CREATE HANDLER h URL '/x' AS SELECT 1",
			"CREATE HANDLER h URL '/x' AS SELECT a FROM db2.x",
			"DROP HANDLER IF EXISTS h",
			"CREATE HYPOTHETICAL INDEX i ON db1.o (a) TYPE minmax",
			"EXPLAIN WHATIF SELECT a FROM db1.o",
			"EXPLAIN ANALYZE SELECT a FROM db2.x",
			"CREATE USER u VALID FOR INTERVAL 1 DAY",
			"ALTER USER u SET max_threads = 1",
			"SYSTEM PAUSE VIEW db1.v",
			"SYSTEM UNLOAD DICTIONARY db1.d",
			"SYSTEM STOP db1.o",
			"SYSTEM START LISTEN TCP",
			"SET max_threads = {t:UInt64}",
			"CREATE DATABASE d2 ENGINE = URL('https://x/')",
			"CREATE TABLE db1.n ENGINE = Remote('h', db1.o)",
			"CREATE TABLE db1.n ENGINE = RemoteSecure('h', db1.o)",
			"SELECT * FROM remote('h', db1.o, SETTINGS skip_unavailable_shards = 1)",
			"SELECT * FROM postgresql('h:5432', query('SELECT 1'), 'u', 'p')",
			"SELECT * FROM mysql('h:3306', (SELECT 1), 'u', 'p')",
			"SELECT * FROM mergeTreeCodecBlockCounts(db1, o)",
			"SET TIME ZONE 'UTC'",
		} {
			resp, err := doRewrite(e, sql, tablerefOpts(si))
			if err == nil && resp.GetCode() == pb.RewriteCode_Success {
				t.Errorf("si=%v %s: Success, want a refusal", si, sql)
			}
		}
	}
}

// TestDeploymentIdentity_ClickHouse268FixRoundOneRoutes mirrors rewriter-grpc
// MetadataAccess.ClickHouse268FixRoundOneRoutesAreRefused (Step 5 fix round
// 1): a bare refused name in the texts Polyglot leaves opaque (an INSERT
// with a column list and SETTINGS, ALTER … UPDATE, a column DEFAULT in
// ALTER), measured on 26.8.37.168 (`INSERT INTO t (s) SETTINGS max_threads =
// 1 SELECT arrayMap(currentSchemas, [true])` stores the physical database),
// the pre-existing bare currentUser / currentDatabase in the same positions,
// an APPLY column transformer, and an Identifier query parameter in an expression (`statement is
// not supported`).
func TestDeploymentIdentity_ClickHouse268FixRoundOneRoutes(t *testing.T) {
	refused := []struct{ sql, name string }{
		{"SELECT * APPLY(aiGenerate) FROM db1.o", "aiGenerate"},
		{"SELECT COLUMNS('s') APPLY aiEmbed FROM db1.o", "aiEmbed"},
		{"SELECT COLUMNS('a') APPLY(pgGetUserById) FROM db1.o", "pgGetUserById"},
		{"SELECT COLUMNS('a') APPLY currentSchemas FROM db1.o", "currentSchemas"},
		{"SELECT * APPLY(currentDatabase) FROM db1.o", "currentDatabase"},
		{"INSERT INTO db1.o (s) SETTINGS max_threads = 1 SELECT arrayMap(currentSchemas, [true])", "currentSchemas"},
		{"INSERT INTO db1.o (s) SETTINGS max_threads = 1 SELECT arrayMap(aiGenerate, ['x'])", "aiGenerate"},
		{"ALTER TABLE db1.o UPDATE s = toString(arrayMap(aiGenerate, ['x'])) WHERE 1", "aiGenerate"},
		{"ALTER TABLE db1.o UPDATE s = SESSION_USER WHERE 1", "SESSION_USER"},
		{"ALTER TABLE db1.o MODIFY COLUMN s String DEFAULT SESSION_USER", "SESSION_USER"},
		{"INSERT INTO db1.o (s) SETTINGS max_threads = 1 VALUES (SESSION_USER)", "SESSION_USER"},
		{"INSERT INTO db1.o (s) VALUES (SESSION_USER)", "SESSION_USER"},
		{"INSERT INTO db1.o (s) VALUES ('a'), (`session_user`)", "session_user"},
		{"INSERT INTO db1.o (s) SETTINGS max_threads = 1 SELECT currentDatabase", "currentDatabase"},
		{"ALTER TABLE db1.o UPDATE s = currentUser WHERE 1", "currentUser"},
		{"ALTER TABLE db1.o MODIFY COLUMN s String DEFAULT currentDatabase", "currentDatabase"},
		{"INSERT INTO db1.o (s) SETTINGS max_threads = 1 VALUES (currentUser)", "currentUser"},
		{"INSERT INTO db1.o (s) VALUES (current_user)", "current_user"},
	}
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range refused {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement,
				wantMsg:  "function " + c.name + " is not accessible", wantSQL: c.sql})
		}
		for _, sql := range []string{
			"SELECT arrayMap({f:Identifier}, ['x'])",
			"SELECT {f:Identifier}",
			"SELECT {f:Identifier} FROM db1.o",
			"SELECT a FROM db1.o WHERE s = {f:Identifier}",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported", wantSQL: sql})
		}
		for _, sql := range []string{
			"SELECT * APPLY(toString) FROM db1.o", "SELECT {v:String}",
			"SELECT t.hostName FROM db1.t AS t", "SELECT user, database FROM db1.o",
			"INSERT INTO db1.o (s) VALUES ('hostName')",
			"CREATE TABLE IF NOT EXISTS hostName (a UInt64) ENGINE = MergeTree ORDER BY a",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: pb.RewriteCode_Success})
		}
	}
	runTablerefCases(t, cases)
}

// TestDeploymentIdentity_BareNamesOfNonCallableFunctionsAreOrdinaryColumns
// mirrors rewriter-grpc
// MetadataAccess.BareNamesOfNonCallableFunctionsAreOrdinaryColumns (Step 5
// fix round 1b): functions measured NOT callable by a bare name on
// 26.8.37.168 (hostName, FQDN, serverUUID, getMacro, the lookups, …) are
// ordinary column names when bare, in the AST and in opaque text; their call
// form stays refused.
func TestDeploymentIdentity_BareNamesOfNonCallableFunctionsAreOrdinaryColumns(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"SELECT hostname FROM db1.o WHERE hostname = 'h' ORDER BY hostname",
			"SELECT fqdn, serverUUID, getMacro, dictGet, naiveBayesClassifier FROM db1.o",
			"SELECT arrayMap(hostName, ['x']) FROM db1.o",
			"SELECT arrayMap(dictGet, ['d'], ['a'], [1]) FROM db1.o",
			"ALTER TABLE db1.o UPDATE hostname = 'x' WHERE hostname = 'y'",
			"ALTER TABLE db1.o MODIFY COLUMN hostname String DEFAULT 'h'",
			"INSERT INTO db1.o (hostname) VALUES ('x')",
			"INSERT INTO db1.o (hostname) SETTINGS max_threads = 1 VALUES ('x')",
			"INSERT INTO db1.o (hostname) SELECT hostname FROM db1.p",
		} {
			resp, err := doRewrite(e, sql, tablerefOpts(si))
			if err != nil {
				t.Fatalf("si=%v %s: %v", si, sql, err)
			}
			if resp.GetCode() != pb.RewriteCode_Success {
				t.Errorf("si=%v %s: code=%v message=%q, want Success", si, sql, resp.GetCode(), resp.GetMessage())
				continue
			}
			if got := resp.GetSqlAfterRewrite(); !strings.Contains(got, `phys."db1.o"`) && !strings.Contains(got, "phys.`db1.o`") {
				t.Errorf("si=%v %s: not rewritten: %s", si, sql, got)
			}
		}
		for _, sql := range []string{"SELECT hostName()", "SELECT getMacro('shard')", "SELECT FQDN() FROM db1.o"} {
			resp, err := doRewrite(e, sql, tablerefOpts(si))
			if err != nil || resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
				t.Errorf("si=%v %s: code=%v err=%v, want UnsupportedStatement", si, sql, resp.GetCode(), err)
			}
		}
		for _, sql := range []string{
			"CREATE TABLE IF NOT EXISTS session_user (a UInt64) ENGINE = MergeTree ORDER BY a",
			"INSERT INTO current_user (a) VALUES (1)",
		} {
			resp, err := doRewrite(e, sql, tablerefOpts(si))
			if err != nil || resp.GetCode() != pb.RewriteCode_Success {
				t.Errorf("si=%v %s: code=%v message=%q err=%v, want Success", si, sql, resp.GetCode(), resp.GetMessage(), err)
			}
		}
	}
}
