package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// TestWholeStatementParseGate pins the refusal of a statement Polyglot did not
// consume in full. Every input here is one ClickHouse refuses or one whose
// meaning the pinned Polyglot changes, so none of them can live in the shared
// corpus: rewriter-grpc answers them with ClickHouse's own parser.
func TestWholeStatementParseGate(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		refused := func(sql string) tablerefCase {
			return tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: sql}
		}
		for _, sql := range []string{
			// The PR #43 review findings.
			"CREATE TABLE db1.n ENGINE = Memory XYZ AS SELECT * FROM phys.x",
			"CREATE TABLE db1.n (a Int32) ENGINE = MergeTree ORDER BY a XYZ SETTINGS storage_policy = 'x'",
			"CREATE TABLE db1.n AS db1.src ENGINE = Memory EMPTY AS SELECT * FROM phys.x",
			// Every statement family truncates the same way.
			"CREATE VIEW db1.v AS SELECT * FROM db1.o AS a XYZ",
			"CREATE MATERIALIZED VIEW db1.mv TO db1.o XYZ AS SELECT * FROM db1.p",
			"ALTER TABLE db1.o ADD COLUMN c Int32 DEFAULT 1 XYZ, DROP COLUMN d",
			"INSERT INTO db1.o SELECT * FROM db1.p SELECT * FROM phys.x",
			"INSERT INTO db1.o VALUES (1) XYZ",
			"SELECT * FROM db1.o SELECT * FROM phys.x",
			"SELECT * FROM db1.o LIMIT 1 WITH TIES",
			"DROP TABLE db1.o SETTINGS max_threads = 1",
			"DELETE FROM db1.o WHERE a = 1 SETTINGS lightweight_deletes_sync = 2",
			"CREATE DATABASE db2 SETTINGS max_threads = 1",
			"DELETE FROM db1.o WHERE a = 1 XYZ",
			// Polyglot closes a bracket the input left open.
			"SELECT * FROM db1.o WHERE a IN (1, 2",
			// Left the shared corpus with this change (spec 2026-09-26 §1:
			// ClickHouse cannot parse WITH OFFSET; Polyglot dropped it, and
			// with it a JOIN's ON clause).
			"SELECT * FROM db1.t WITH OFFSET AS off",
			"SELECT * FROM db1.t WITH\nOFFSET AS off",
			"SELECT * FROM db1.t WITH\tOFFSET AS off",
			"SELECT * FROM db1.t AS s JOIN db1.o WITH OFFSET AS off ON 1",
			"SELECT * FROM db1.o, db1.t WITH OFFSET AS off",
			"SELECT * FROM db1.t, db1.o WITH OFFSET AS off",
			// The gate precedes T2 and T3: no policy check sees a statement
			// the engine did not parse in full.
			"SELECT * FROM {p:Identifier} AS a XYZ",
			"SELECT * FROM db1.o AS a XYZ JOIN phys.x ON 1",
		} {
			cases = append(cases, refused(sql))
		}
		// With the surface active the final annotation still names an SI
		// object the statement proves.
		hgUnsafe := tablerefCase{name: "truncate all tables", sql: "TRUNCATE ALL TABLES FROM hg_unsafe", si: si,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: "TRUNCATE ALL TABLES FROM hg_unsafe"}
		hgSafe := refused("SELECT * FROM hg_safe.db1__t AS a XYZ")
		if si {
			hgUnsafe.wantMsg = "storage-integrity physical database hg_unsafe is not directly addressable"
			hgSafe.wantMsg = "storage-integrity physical table hg_safe.db1__t is not directly addressable"
		}
		cases = append(cases, hgUnsafe, hgSafe)
		// CREATE TABLE … EMPTY AS SELECT is parsed without its EMPTY keyword,
		// so it passes the gate.
		cases = append(cases, tablerefCase{name: "empty form", sql: "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM db1.p", si: si,
			wantCode: pb.RewriteCode_Success,
			wantSQL:  `CREATE TABLE phys."db1.n" ENGINE=Memory EMPTY AS (SELECT * FROM phys."db1.p" "db1.p")`})
	}
	runTablerefCases(t, cases)
}

// The gate refuses in every mode, through the response, never the Go error.
func TestWholeStatementParseGateEveryMode(t *testing.T) {
	e := newEngine(t)
	const sql = "CREATE TABLE db1.n ENGINE = Memory XYZ AS SELECT * FROM phys.x"
	for name, opts := range map[string][]*pb.RewriteOption{
		"no rewrite": nil,
		"static":     {tableRewriteStatic()},
		"dynamic":    tablerefOpts(false),
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := doRewrite(e, sql, opts)
			if err != nil {
				t.Fatalf("doRewrite: %v", err)
			}
			if resp.GetCode() != pb.RewriteCode_UnsupportedStatement || resp.GetMessage() != "statement is not supported" ||
				resp.GetSqlAfterRewrite() != sql || resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_UNSPECIFIED {
				t.Fatalf("resp = %+v", resp)
			}
		})
	}
}

// MaterializeSQL must not sign a statement it did not parse in full: before
// the gate it returned the materialized prefix as Success.
func TestMaterializeRefusesStatementNotParsedInFull(t *testing.T) {
	e := newEngine(t)
	now := int64(1_700_000_000_000_000_000)
	for _, sql := range []string{
		"INSERT INTO db1.o SELECT now() FROM db1.p AS a XYZ",
		"INSERT INTO db1.o SELECT now() FROM db1.p SELECT * FROM phys.x",
	} {
		t.Run(sql, func(t *testing.T) {
			resp, err := doMaterializeSQL(e, &pb.MaterializeSQLRequest{Sql: sql,
				Inputs: &pb.MaterializationInputs{NowUnixNs: &now}})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetCode() != pb.MaterializeCode_MaterializeSyntaxError ||
				!strings.Contains(resp.GetMessage(), "statement was not parsed in full") ||
				resp.GetSqlAfterMaterialization() != sql || len(resp.GetReplacements()) != 0 {
				t.Fatalf("resp = %+v", resp)
			}
		})
	}
}
