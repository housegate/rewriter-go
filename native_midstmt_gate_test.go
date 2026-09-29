package rewriter

import (
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// TestMidStatementDropGate pins the refusal of a statement Polyglot parsed in
// full but regenerates as a different statement (spec 2026-09-26 §1). Every
// refused input passes the whole-statement parse gate; rewriter-grpc parses
// each with ClickHouse's own parser and keeps the clause, so none of them can
// live in the shared corpus.
func TestMidStatementDropGate(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			// The parse-gate final review's list.
			"DELETE FROM db1.o IN PARTITION 1 WHERE a = 1",
			"DELETE FROM db1.o IN PARTITION '2024-01' WHERE a = 1",
			"SELECT a FROM db1.o ORDER BY a LIMIT 1 WITH TIES FORMAT JSON",
			"SELECT a FROM db1.o ORDER BY a LIMIT 1 WITH TIES SETTINGS max_threads = 1",
			"SELECT * FROM (SELECT a FROM db1.o ORDER BY a LIMIT 1 WITH TIES) AS s",
			"DROP TABLE db1.o ON CLUSTER c",
			"DROP VIEW IF EXISTS db1.v ON CLUSTER 'c'",
			"SELECT a FROM db1.o LIMIT 2 BY a LIMIT 10",
			"CREATE TABLE db1.n (a Int32, e Int32 EPHEMERAL) ENGINE = Memory",
			"ALTER TABLE db1.o ADD COLUMN c Int32 EPHEMERAL",
			"INSERT INTO db1.o (* EXCEPT (b)) VALUES (1)",
			// Found by the measurement.
			"DROP TABLE IF EMPTY db1.o",
			"DROP TEMPORARY TABLE n",
			"TRUNCATE TABLE db1.o SETTINGS max_threads = 1",
			"CREATE TABLE db1.n (a Int32 STATISTICS(tdigest)) ENGINE = MergeTree ORDER BY a",
			"CREATE VIEW db1.v AS SELECT a FROM db1.o LIMIT 2 BY a LIMIT 3",
			"SELECT a::String FROM db1.o",
			"SELECT CHAR_LENGTH(s) FROM db1.o",
			"SELECT instr(s, 'x') FROM db1.o",
			"SELECT toStartOfDay(t) FROM db1.o",
			"SELECT group_concat(s, '-') FROM db1.o",
			"SELECT startsWith(s, 'x') FROM db1.o",
			// An SI table keeps the T7 text.
			"SELECT * FROM db1.t ORDER BY a LIMIT 1 WITH TIES FORMAT JSON",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: sql})
		}
		// Cosmetic respellings pass.
		for _, sql := range []string{
			"SELECT a FROM db1.o ORDER BY a DESC NULLS LAST",
			"SELECT TOP 5 a FROM db1.o",
			"SELECT a FROM db1.o LIMIT 5, 10",
			"SELECT * FROM db1.o, db1.p",
			"SELECT a FROM db1.o FORMAT JSON SETTINGS max_threads = 1",
			"SELECT pow(a, 2), substr(s, 1) FROM db1.o",
			"CREATE TABLE db1.n (a INT, b VARCHAR(255), c BOOLEAN) ENGINE = Memory",
			"INSERT INTO TABLE db1.o (a) VALUES (1)",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: pb.RewriteCode_Success})
		}
		if !si {
			// The active surface refuses a structured DELETE for its own reasons.
			cases = append(cases, tablerefCase{name: "delete", sql: "DELETE FROM db1.o WHERE a = 1", wantCode: pb.RewriteCode_Success})
		}
	}
	runTablerefCases(t, cases)
}

// The gate refuses in every mode, through the response, never the Go error.
func TestMidStatementDropGateEveryMode(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{
		"DELETE FROM db1.o IN PARTITION 1 WHERE a = 1",
		"SELECT a FROM db1.o ORDER BY a LIMIT 1 WITH TIES FORMAT JSON",
	} {
		for name, opts := range map[string][]*pb.RewriteOption{
			"no rewrite": nil,
			"static":     {tableRewriteStatic()},
			"dynamic":    tablerefOpts(false),
		} {
			t.Run(name+"/"+sql, func(t *testing.T) {
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
}

// TestCommandRerenderGate pins the handlers that re-render a command from
// parsed fields: a clause they do not model is refused instead of dropped.
func TestCommandRerenderGate(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"EXISTS TABLE db1.o FORMAT JSON",
			"EXISTS TABLE db1.o XYZ",
			"EXISTS TEMPORARY TABLE n",
			"SHOW CREATE TABLE db1.o SETTINGS max_threads = 1",
			"SHOW CREATE TABLE db1.o XYZ phys.x",
			"DESCRIBE TABLE t FORMAT JSON",
			"USE db1 XYZ",
			"SHOW TABLES FROM db1 LIKE 'x%'",
			"SHOW TABLES FROM db1 LIMIT 10",
			"SHOW FULL TABLES FROM db1",
			"SHOW TEMPORARY TABLES",
			"SHOW DATABASES LIMIT 1",
			"SHOW DATABASES FORMAT JSON",
			"EXISTS TABLE db1.t FORMAT JSON",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: sql})
		}
		// DESCRIBE re-renders an SI table as the metadata SELECT; an ordinary
		// qualified target is echoed verbatim and drops nothing.
		describeSI := tablerefCase{name: "describe si", sql: "DESCRIBE TABLE db1.t FORMAT JSON", si: si, wantCode: pb.RewriteCode_Success}
		if si {
			describeSI.wantCode, describeSI.wantMsg, describeSI.wantSQL = pb.RewriteCode_UnsupportedStatement, unsupported, "DESCRIBE TABLE db1.t FORMAT JSON"
		}
		cases = append(cases, describeSI)
		for _, sql := range []string{
			"EXISTS TABLE db1.o",
			"SHOW CREATE TABLE db1.o;",
			"DESCRIBE TABLE t",
			"USE db1",
			"SHOW TABLES FROM db1",
			"SHOW DATABASES LIKE 'd%'",
			"SHOW DATABASES NOT ILIKE 'd%'",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: pb.RewriteCode_Success})
		}
	}
	runTablerefCases(t, cases)
}

// EXISTS re-renders its target in every mode, not only under dynamic args.
func TestCommandRerenderGateEveryMode(t *testing.T) {
	e := newEngine(t)
	const sql = "EXISTS TABLE db1.o FORMAT JSON"
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
				resp.GetSqlAfterRewrite() != sql {
				t.Fatalf("resp = %+v", resp)
			}
		})
	}
}
