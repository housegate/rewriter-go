package rewriter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
	"google.golang.org/protobuf/encoding/protojson"
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
			// Column modifiers reordered (final review Minor 1).
			"CREATE TABLE db1.n (a Int32 CODEC(ZSTD) COMMENT 'x') ENGINE = Memory",
			"ALTER TABLE db1.o ADD COLUMN a Int32 CODEC(ZSTD) COMMENT 'x'",
			// An SI table keeps the T7 text.
			"SELECT * FROM db1.t ORDER BY a LIMIT 1 WITH TIES FORMAT JSON",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: sql})
		}
		// A streamed-VALUES INSERT is a command node that lost its table; the
		// active SI surface refuses it as an unmodelled class first.
		for _, sql := range []string{
			"INSERT INTO db1.o (a, b) VALUES",
			"INSERT INTO db1.o VALUES",
			"INSERT INTO db1.o SETTINGS async_insert = 1 VALUES",
		} {
			msg := unsupported
			if si {
				msg = StorageIntegrityUnmodelledMessage
			}
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg, wantSQL: sql})
		}
		// Cosmetic respellings pass.
		for _, sql := range []string{
			"CREATE TABLE db1.n ENGINE = MergeTree ORDER BY a COMMENT 'h' AS SELECT a, ttl FROM db1.o",
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
		// Polyglot keeps a streamed-VALUES INSERT as a command node whose
		// text is INSERT INTO VALUES (final review I1 / M3). Dynamic mode
		// refuses it at the T7 fallthrough before any Success is built, the
		// other modes at the gate; the answer must be the same.
		"INSERT INTO db1.o (a, b) VALUES",
		"INSERT INTO db1.o VALUES",
		"INSERT INTO db1.o SETTINGS async_insert = 1 VALUES",
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

// TestMidStatementDropGateResponseShape pins every field of a gate answer
// (task 2 review M3): the CREATE TABLE … EMPTY path, which checks the text
// without EMPTY and splices it back; a GRANT, whose command text is compared
// and whose privileges_deltas survive; and a refusal the gate converted from
// Success. A converted refusal echoes the input and clears statement_type,
// but keeps existence_clause, the contract version and the handler's
// original_accessed_tables / table_rewrites, including an SI table's
// physical name (review M1, parked: HouseGate turns every non-Success into a
// RejectedError and forwards none of these fields).
func TestMidStatementDropGateResponseShape(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		name string
		opts []*pb.RewriteOption
		sql  string
		want string // the protojson response
	}{
		{"empty/dynamic", tablerefOpts(false),
			"CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT a FROM db1.o",
			`{"message":"success","sqlAfterRewrite":"CREATE TABLE phys.\"db1.n\" ENGINE=Memory EMPTY AS (SELECT a FROM phys.\"db1.o\" \"db1.o\")",
			  "statementType":"STATEMENT_TYPE_CREATE_TABLE","tableRewrites":{"db1.n":"phys.db1.n","db1.o":"phys.db1.o"},
			  "originalAccessedTables":[{"logicalDatabase":"db1","originalDatabase":"db1","originalTable":"n","physicalDatabase":"phys"},
			    {"logicalDatabase":"db1","originalDatabase":"db1","originalTable":"o","physicalDatabase":"phys"}]}`},
		{"empty/no rewrite", nil,
			"CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT a FROM db1.o",
			`{"message":"success","sqlAfterRewrite":"CREATE TABLE db1.n ENGINE=Memory EMPTY AS (SELECT a FROM db1.o)",
			  "statementType":"STATEMENT_TYPE_CREATE_TABLE",
			  "originalAccessedTables":[{"originalDatabase":"db1","originalTable":"n"},{"originalDatabase":"db1","originalTable":"o"}]}`},
		{"empty refused/dynamic", tablerefOpts(false),
			"CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT a FROM db1.o LIMIT 2 BY a LIMIT 3",
			`{"code":"UnsupportedStatement","message":"statement is not supported",
			  "sqlAfterRewrite":"CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT a FROM db1.o LIMIT 2 BY a LIMIT 3",
			  "tableRewrites":{"db1.n":"phys.db1.n","db1.o":"phys.db1.o"},
			  "originalAccessedTables":[{"logicalDatabase":"db1","originalDatabase":"db1","originalTable":"n","physicalDatabase":"phys"},
			    {"logicalDatabase":"db1","originalDatabase":"db1","originalTable":"o","physicalDatabase":"phys"}]}`},
		{"grant/dynamic", tablerefOpts(false), "GRANT SELECT ON db1.o TO u1",
			`{"message":"success","sqlAfterRewrite":"SELECT 'GRANT SELECT ON db1.o TO u1' AS gstmt","statementType":"STATEMENT_TYPE_GRANT",
			  "privilegesDeltas":[{"action":"ACTION_GRANT","grantees":[{"name":"u1"}],"logicalDatabase":"db1","originalDatabase":"db1",
			    "originalTable":"o","physicalDatabase":"phys","physicalTable":"db1.o","privileges":["SELECT"],"scope":"SCOPE_TABLE"}]}`},
		{"grant/si", tablerefOpts(true), "GRANT SELECT ON db1.o TO u1",
			`{"message":"success","sqlAfterRewrite":"SELECT 'GRANT SELECT ON db1.o TO u1' AS gstmt","statementType":"STATEMENT_TYPE_GRANT",
			  "storageIntegrityContractVersion":"STORAGE_INTEGRITY_CONTRACT_V2",
			  "privilegesDeltas":[{"action":"ACTION_GRANT","grantees":[{"name":"u1"}],"logicalDatabase":"db1","originalDatabase":"db1",
			    "originalTable":"o","physicalDatabase":"phys","physicalTable":"db1.o","privileges":["SELECT"],"scope":"SCOPE_TABLE"}]}`},
		{"refused si table", tablerefOpts(true), "SELECT * FROM db1.t ORDER BY a LIMIT 1 WITH TIES FORMAT JSON",
			`{"code":"UnsupportedStatement","message":"statement is not supported",
			  "sqlAfterRewrite":"SELECT * FROM db1.t ORDER BY a LIMIT 1 WITH TIES FORMAT JSON",
			  "storageIntegrityContractVersion":"STORAGE_INTEGRITY_CONTRACT_V2","tableRewrites":{"db1.t":"hg_safe.db1__t"},
			  "originalAccessedTables":[{"isStorageIntegrity":true,"logicalDatabase":"db1","originalDatabase":"db1","originalTable":"t","physicalDatabase":"phys"}]}`},
		{"refused keeps existence clause", tablerefOpts(false), "CREATE VIEW IF NOT EXISTS db1.v AS SELECT a FROM db1.o LIMIT 2 BY a LIMIT 3",
			`{"code":"UnsupportedStatement","message":"statement is not supported","existenceClause":"EXISTENCE_CLAUSE_IF_NOT_EXISTS",
			  "sqlAfterRewrite":"CREATE VIEW IF NOT EXISTS db1.v AS SELECT a FROM db1.o LIMIT 2 BY a LIMIT 3",
			  "tableRewrites":{"db1.o":"phys.db1.o","db1.v":"phys.db1.v"},
			  "originalAccessedTables":[{"logicalDatabase":"db1","originalDatabase":"db1","originalTable":"v","physicalDatabase":"phys"},
			    {"logicalDatabase":"db1","originalDatabase":"db1","originalTable":"o","physicalDatabase":"phys"}]}`},
		{"refused streamed values/no rewrite", nil, "INSERT INTO db1.o (a, b) VALUES",
			`{"code":"UnsupportedStatement","message":"statement is not supported","sqlAfterRewrite":"INSERT INTO db1.o (a, b) VALUES"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := doRewrite(e, tc.sql, tc.opts)
			if err != nil {
				t.Fatalf("doRewrite: %v", err)
			}
			b, err := protojson.Marshal(resp)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatalf("want: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("resp = %s\nwant %s", b, tc.want)
			}
		})
	}
}

// TestRawNodeQuotedIdentifierGate pins that a raw access-entity statement
// whose text lost the quotes of an identifier spelled like a literal is
// refused in the modes that would otherwise answer Success (re-review N1):
// CREATE ROW POLICY … USING `null` = 1 regenerated as USING null = 1 filters
// on the NULL literal, not on the column. Dynamic requests refuse these
// statements before the gate.
func TestRawNodeQuotedIdentifierGate(t *testing.T) {
	e := newEngine(t)
	for _, w := range []string{"null", "true", "false", "inf", "nan"} {
		for _, sql := range []string{
			"CREATE ROW POLICY p1 ON db1.o FOR SELECT USING `" + w + "` = 1 TO u1",
			"CREATE SETTINGS PROFILE `" + w + "`",
			"CREATE ROLE `" + w + "`",
			"CREATE QUOTA `" + w + "`",
		} {
			for name, opts := range map[string][]*pb.RewriteOption{
				"no rewrite": nil,
				"static":     {tableRewriteStatic()},
			} {
				t.Run(name+"/"+sql, func(t *testing.T) {
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
	}
	// A bare name has no quotes to lose.
	for _, sql := range []string{"CREATE ROLE r1", "CREATE QUOTA q1"} {
		resp, err := doRewrite(e, sql, nil)
		if err != nil || resp.GetCode() != pb.RewriteCode_Success {
			t.Fatalf("%s: resp = %+v, err = %v; want Success", sql, resp, err)
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

// modes is the four request shapes a command statement can arrive in: no
// rewrite option, static table rewrite, dynamic args, and dynamic args with an
// active storage-integrity surface.
func commandModes() map[string][]*pb.RewriteOption {
	return map[string][]*pb.RewriteOption{
		"none":    nil,
		"static":  {tableRewriteStatic()},
		"dynamic": tablerefOpts(false),
		"si":      tablerefOpts(true),
	}
}

// TestCommandTargetGate pins EXISTS / SHOW CREATE targets the parser cannot
// reduce to a [db.]name (a keyword-lexed first token, no target) and the bare
// access-entity SHOW CREATE forms, which ClickHouse dispatches before it tries
// a table name: every mode refuses them instead of re-rendering an empty or
// re-typed target (mid-statement drop gate, review I1 / I2).
func TestCommandTargetGate(t *testing.T) {
	e := newEngine(t)
	refused := []string{
		// I1: target not recognised or empty.
		"EXISTS TABLE system.one",
		"EXISTS TABLE system.one FORMAT JSON",
		"EXISTS TABLE default.o",
		"EXISTS",
		"EXISTS TABLE",
		"SHOW CREATE TABLE system.one",
		"SHOW CREATE TABLE default.o",
		"SHOW CREATE TABLE",
		"SHOW CREATE ROW POLICIES",
		"SHOW CREATE SETTINGS PROFILES",
		"SHOW CREATE ROW POLICY p ON db1.o",
		"SHOW CREATE SETTINGS PROFILE p",
		// I2: bare access-entity forms are never a table.
		"SHOW CREATE USER",
		"SHOW CREATE USER u1",
		"SHOW CREATE USERS",
		"SHOW CREATE QUOTA",
		"SHOW CREATE QUOTA q1",
		"SHOW CREATE QUOTAS",
		"SHOW CREATE ROLE",
		"SHOW CREATE ROLE r1",
		"SHOW CREATE ROLES",
		"SHOW CREATE PROFILE",
		"SHOW CREATE PROFILES",
		"SHOW CREATE POLICY p ON db1.o",
		"SHOW CREATE POLICIES",
		"SHOW CREATE POLICIES ON db1.o",
		"SHOW CREATE MASKING POLICY p ON db1.o",
		"show create user",
	}
	for mode, opts := range commandModes() {
		for _, sql := range refused {
			t.Run(mode+"/"+sql, func(t *testing.T) {
				resp, err := doRewrite(e, sql, opts)
				if err != nil {
					t.Fatalf("doRewrite: %v", err)
				}
				if resp.GetCode() == pb.RewriteCode_Success {
					t.Fatalf("code = Success, sql = %q; want a refusal", resp.GetSqlAfterRewrite())
				}
				if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
					t.Fatalf("code = %s (%s), want UnsupportedStatement", resp.GetCode(), resp.GetMessage())
				}
			})
		}
	}
	// A quoted word is an identifier, and an explicit TABLE keyword makes the
	// word a table name: neither is an access entity.
	for mode, opts := range commandModes() {
		for _, sql := range []string{"SHOW CREATE TABLE db1.user", "SHOW CREATE `user`", "EXISTS TABLE db1.user", "SHOW CREATE TABLE db1.o"} {
			t.Run(mode+"/ok/"+sql, func(t *testing.T) {
				resp, err := doRewrite(e, sql, opts)
				if err != nil {
					t.Fatalf("doRewrite: %v", err)
				}
				if mode == "si" && resp.GetCode() != pb.RewriteCode_Success {
					return // an SI surface may refuse SHOW CREATE by its own rule
				}
				if resp.GetCode() != pb.RewriteCode_Success {
					t.Fatalf("code = %s (%s), want Success", resp.GetCode(), resp.GetMessage())
				}
			})
		}
	}
}

// TestShowDatabasesGate pins SHOW DATABASES: a FROM / IN clause is invalid in
// ClickHouse and would be dropped, and the LIKE pattern is carried as its raw
// lexeme, so an escape such as \_ or \% keeps its meaning (review I3 / M1).
func TestShowDatabasesGate(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"SHOW DATABASES FROM db1",
			"SHOW DATABASES IN db1",
			"SHOW DATABASES FROM db1 LIKE 'd%'",
			"SHOW DATABASES WHERE name = 'x'",
			"SHOW DATABASES INTO OUTFILE '/tmp/x'",
			"SHOW DATABASES SETTINGS max_threads = 1",
			"SHOW DATABASES PARALLEL WITH SHOW DATABASES",
			"SHOW EXTENDED DATABASES",
			"SHOW DATABASES LIKE \"d%\"",
			"SHOW DATABASES LIKE $$d%$$",
			"SHOW DATABASES LIKE 'd%' LIMIT 1",
			"SHOW DATABASES LIKE 'd%' FORMAT JSON",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported})
		}
		for _, c := range []struct{ in, like string }{
			{`SHOW DATABASES LIKE 'd\_%'`, `LIKE 'd\_%'`},
			{`SHOW DATABASES LIKE 'd\%'`, `LIKE 'd\%'`},
			{`SHOW DATABASES NOT ILIKE 'D\_X'`, `NOT ILIKE 'D\_X'`},
			{`SHOW DATABASES LIKE 'd\x5f%'`, `LIKE 'd\x5f%'`},
			{`SHOW DATABASES LIKE 'O''Brien%'`, `LIKE 'O''Brien%'`},
			{`SHOW DATABASES LIKE 'a\\b'`, `LIKE 'a\\b'`},
			{`SHOW DATABASES LIKE 'd%'`, `LIKE 'd%'`},
		} {
			cases = append(cases, tablerefCase{name: c.in, sql: c.in, si: si, wantCode: pb.RewriteCode_Success,
				wantSQL: "SELECT name FROM (SELECT 'db1' AS name) WHERE name " + c.like + " ORDER BY name"})
		}
	}
	runTablerefCases(t, cases)
}

// TestCommandRerenderGateClauses pins every clause the T7 gate refuses on the
// handlers that re-render a command: none of them may be dropped.
func TestCommandRerenderGateClauses(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"EXISTS TABLE db1.o INTO OUTFILE '/tmp/x'",
			"EXISTS TABLE db1.o SETTINGS max_threads = 1",
			"EXISTS TABLE db1.o PARALLEL WITH EXISTS TABLE db1.o",
			"EXISTS TABLE db1.o WHERE 1",
			"SHOW CREATE TABLE db1.o INTO OUTFILE '/tmp/x'",
			"SHOW CREATE TABLE db1.o FORMAT JSON",
			"SHOW CREATE TABLE db1.o PARALLEL WITH SHOW CREATE TABLE db1.o",
			"DESCRIBE TABLE t SETTINGS max_threads = 1",
			"DESCRIBE TABLE t INTO OUTFILE '/tmp/x'",
			"USE db1 PARALLEL WITH USE db1",
			"USE db1 FORMAT JSON",
			"USE db1 SETTINGS max_threads = 1",
			"SHOW TABLES FROM db1 WHERE name = 'o'",
			"SHOW TABLES FROM db1 INTO OUTFILE '/tmp/x'",
			"SHOW TABLES FROM db1 SETTINGS max_threads = 1",
			"SHOW TABLES FROM db1 FORMAT JSON",
			"SHOW TABLES FROM db1 ILIKE 'x%'",
			"SHOW TABLES FROM db1 NOT LIKE 'x%'",
			"SHOW TABLES FROM db1 NOT ILIKE 'x%'",
			"SHOW EXTENDED TABLES FROM db1",
			"SHOW TABLES FROM db1 PARALLEL WITH SHOW TABLES FROM db1",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported})
		}
	}
	// DESCRIBE re-renders an SI table as the metadata SELECT; only there is
	// a trailing clause dropped, so only there does DESCRIBE refuse it.
	for _, sql := range []string{"DESCRIBE TABLE db1.t SETTINGS max_threads = 1", "DESCRIBE TABLE db1.t INTO OUTFILE '/tmp/x'"} {
		cases = append(cases,
			tablerefCase{name: "si/" + sql, sql: sql, si: true, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: sql},
			tablerefCase{name: "ordinary/" + sql, sql: sql, si: false, wantCode: pb.RewriteCode_Success})
	}
	runTablerefCases(t, cases)
}

// TestCommandPrefixKeepsPolicyRefusal pins that FULL / TEMPORARY / EXTENDED
// never turn a policy refusal into another answer: the handler's own refusal
// (protected or reserved database, unknown logical database) wins over the
// T7 gate that refuses the prefix.
func TestCommandPrefixKeepsPolicyRefusal(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, base := range []string{
			"SHOW TABLES FROM phys",
			"SHOW TABLES FROM hg_safe",
			"SHOW TABLES FROM hg_unsafe",
			"SHOW TABLES FROM nope",
		} {
			for _, prefix := range []string{"SHOW FULL TABLES", "SHOW TEMPORARY TABLES", "SHOW EXTENDED TABLES", "SHOW FULL TEMPORARY TABLES"} {
				prefixed := prefix + strings.TrimPrefix(base, "SHOW TABLES")
				name := base + " / " + prefixed
				t.Run(name, func(t *testing.T) {
					want, err := doRewrite(e, base, tablerefOpts(si))
					if err != nil {
						t.Fatalf("doRewrite base: %v", err)
					}
					got, err := doRewrite(e, prefixed, tablerefOpts(si))
					if err != nil {
						t.Fatalf("doRewrite prefixed: %v", err)
					}
					if want.GetCode() == pb.RewriteCode_Success {
						t.Fatalf("base %q answered Success; the row must start from a refusal", base)
					}
					if got.GetCode() != want.GetCode() || got.GetMessage() != want.GetMessage() {
						t.Fatalf("prefixed = %s (%s), base = %s (%s)", got.GetCode(), got.GetMessage(), want.GetCode(), want.GetMessage())
					}
				})
			}
		}
	}
}

// MaterializeSQL must not sign a statement the generator changed: before the
// gate it returned the regenerated SQL, WITH TIES dropped, as Success.
func TestMaterializeRefusesStatementNotRegeneratedFaithfully(t *testing.T) {
	e := newEngine(t)
	now := int64(1_700_000_000_000_000_000)
	for _, sql := range []string{
		"INSERT INTO db1.o SELECT now(), a FROM db1.p ORDER BY a LIMIT 1 WITH TIES FORMAT JSON",
		"INSERT INTO db1.o SELECT now(), a::String FROM db1.p",
		// startsWith is regenerated as STARTS_WITH, which is not a ClickHouse
		// function: a known conservative refusal for now() beside it.
		"INSERT INTO db1.o SELECT now(), startsWith(s, 'x') FROM db1.p",
	} {
		t.Run(sql, func(t *testing.T) {
			resp, err := doMaterializeSQL(e, &pb.MaterializeSQLRequest{Sql: sql,
				Inputs: &pb.MaterializationInputs{NowUnixNs: &now}})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetCode() != pb.MaterializeCode_MaterializeUnsupportedStatement ||
				!strings.Contains(resp.GetMessage(), "the regenerated statement differs from the input") ||
				resp.GetSqlAfterMaterialization() != sql || len(resp.GetReplacements()) != 0 {
				t.Fatalf("resp = %+v", resp)
			}
		})
	}
	resp, err := doMaterializeSQL(e, &pb.MaterializeSQLRequest{Sql: "INSERT INTO db1.o SELECT now(), a FROM db1.p ORDER BY a DESC NULLS LAST",
		Inputs: &pb.MaterializationInputs{NowUnixNs: &now}})
	if err != nil || resp.GetCode() != pb.MaterializeCode_MaterializeSuccess || len(resp.GetReplacements()) != 1 {
		t.Fatalf("a faithful statement still materializes: resp = %+v, err = %v", resp, err)
	}
}

// TestMidStatementDropGateUnquotedAlias pins the gate's refusal of a
// SQL-injection shape through an alias.
//
// DEFECT (Polyglot, measured on v0.12.1 through the pinned build): Polyglot
// drops the quotes of a quoted alias on a bare parenthesised tuple, both in a
// WITH item and in a projection, so the alias text is printed as SQL. On a
// build without this gate
//
//	WITH (1, 2) AS "x SELECT a FROM phys.`other.secret` --" SELECT a FROM db1.o
//
// was rewritten to
//
//	WITH (1, 2) AS x SELECT a FROM phys.`other.secret` -- SELECT a FROM phys."db1.o" "db1.o"
//
// and answered Success: a cross-tenant read. Nothing else in the rewriter
// looks at the regenerated text, so CheckRegenerated is the only barrier; a
// change that weakens its comparison of quoted identifiers reopens this hole.
//
// Measured with engine.Generate against the input (a quoted alias `x y`, in
// WITH and in a projection): the quotes are lost after a tuple of any arity
// ((1, 2), (1, 2, 3), (a, b), (1, 'x'), (1, [2])), after -tuple, NOT tuple and
// a tuple in a comparison or arithmetic (as a WITH item the negated and NOT
// forms are a Polyglot SyntaxError instead); they are kept after an array literal
// ([1, 2], [], [a], [(1, 2)]), tuple(…), array(…), map(…), a one-element
// parenthesised expression, a nested ((1, 2)), a (SELECT …) scalar subquery,
// a tuple cast to a type, a tuple element access, an IN predicate and a scalar
// literal, column, function call or CASE. A backtick alias loses its quotes
// the same way.
func TestMidStatementDropGateUnquotedAlias(t *testing.T) {
	const unsupported = "statement is not supported"
	e := newEngine(t)

	// Every alias below is one quoted identifier in the input; unquoted it
	// would run as SQL.
	injections := []string{
		"x SELECT a FROM phys.`other.secret` --",
		"x FROM phys.`other.secret` --",
		"x SELECT a FROM hg_unsafe.db1__t --",
		"x\nSELECT a FROM phys.`other.secret`",
		"x y",
		"x\ty",
		"x--",
		"x/*",
		"x;",
		"x)",
		"x.y",
	}
	shapes := []struct{ name, expr string }{
		{"pair", "(1, 2)"},
		{"triple", "(1, 2, 3)"},
		{"columns", "(a, b)"},
		{"mixed", "(1, 'x')"},
		{"tuple with array", "(1, [2])"},
		{"tuple comparison", "(1, 2) = (3, 4)"},
		{"tuple sum", "(1, 2) + (3, 4)"},
	}
	// The forms an aliased expression can sit in. WITH and the projection are
	// the ones the defect was found in; the rest measured the same.
	forms := []struct{ name, tmpl string }{
		{"with", "WITH %s AS %s SELECT a FROM db1.o"},
		{"projection", "SELECT %s AS %s FROM db1.o"},
		{"subquery projection", "SELECT * FROM (SELECT %s AS %s FROM db1.o)"},
		{"cte body", "WITH c AS (SELECT %s AS %s) SELECT a FROM db1.o"},
		{"where", "SELECT a FROM db1.o WHERE %s AS %s"},
		{"group by", "SELECT a FROM db1.o GROUP BY %s AS %s"},
		{"function argument", "SELECT f(%s AS %s) FROM db1.o"},
		{"create view", "CREATE VIEW db1.v AS SELECT %s AS %s FROM db1.o"},
		{"insert select", "INSERT INTO db1.o SELECT %s AS %s FROM db1.p"},
	}
	type row struct{ name, sql string }
	var refused []row
	add := func(name, sql string) { refused = append(refused, row{name, sql}) }
	for _, form := range forms {
		for _, shape := range shapes {
			// The full injection, in both quote styles.
			add(form.name+"/"+shape.name+"/double/injection", fillAliasForm(form.tmpl, shape.expr, `"`+injections[0]+`"`))
			// A backtick alias cannot contain a backtick, so it uses a plain
			// injection.
			add(form.name+"/"+shape.name+"/backtick/injection", fillAliasForm(form.tmpl, shape.expr, "`x SELECT a FROM hg_unsafe.db1__t --`"))
		}
	}
	// The verbatim statements from the report, and each injection alias on the
	// two forms the defect was found in.
	add("report/with", "WITH (1, 2) AS \"x SELECT a FROM phys.`other.secret` --\" SELECT a FROM db1.o")
	add("report/projection", "SELECT (1, 2) AS \"x FROM phys.`other.secret` --\" FROM db1.o")
	add("report/hg_unsafe", "WITH (1, 2) AS \"x SELECT a FROM hg_unsafe.db1__t --\" SELECT a FROM db1.o")
	// A single-token alias unquoted to a word with a # in it: ClickHouse reads
	// `# ` and `#!` as a comment to the end of the line, which drops the rest
	// of the regenerated statement, and any other # is an unrecognised token.
	for _, alias := range []string{"x#", "x#x", "a#b", "#x", "x#!", "#"} {
		add("hash alias/with/"+alias, "WITH (1, 2) AS \""+alias+"\" SELECT a FROM db1.o")
		add("hash alias/projection/"+alias, "SELECT (1, 2) AS \""+alias+"\" FROM db1.o")
		add("hash alias/backtick/"+alias, "SELECT (1, 2, 3) AS `"+alias+"` FROM db1.o")
	}
	// Polyglot cannot parse a negated or NOT tuple as a WITH item (a SyntaxError
	// before the gate); in a projection it loses the quotes like a bare tuple.
	add("projection/negated tuple", "SELECT -(1, 2) AS \"x FROM phys.`other.secret` --\" FROM db1.o")
	add("projection/not tuple", "SELECT NOT (1, 2) AS \"x FROM phys.`other.secret` --\" FROM db1.o")
	for _, inj := range injections {
		for _, form := range forms[:2] {
			for _, shape := range shapes[:3] {
				add("alias/"+form.name+"/"+shape.name+"/"+strings.NewReplacer("\n", `\n`, "\t", `\t`).Replace(inj),
					fillAliasForm(form.tmpl, shape.expr, `"`+inj+`"`))
			}
		}
	}

	for _, r := range refused {
		for mode, opts := range commandModes() {
			t.Run(mode+"/"+r.name, func(t *testing.T) {
				resp, err := doRewrite(e, r.sql, opts)
				if err != nil {
					t.Fatalf("doRewrite: %v", err)
				}
				if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
					t.Fatalf("code = %s (%s), want UnsupportedStatement; sql_after_rewrite = %q", resp.GetCode(), resp.GetMessage(), resp.GetSqlAfterRewrite())
				}
				if resp.GetMessage() != unsupported {
					t.Fatalf("message = %q, want %q", resp.GetMessage(), unsupported)
				}
				if resp.GetSqlAfterRewrite() != r.sql {
					t.Fatalf("sql_after_rewrite = %q, want the input echoed", resp.GetSqlAfterRewrite())
				}
			})
		}
	}

	// Shapes that keep their quotes are not refused: the alias stays one
	// quoted identifier, whatever it contains, and the regenerated statement
	// is the one the client wrote. The rewritten text must still carry the
	// alias inside its quotes.
	for _, r := range []row{
		{"array in with", "WITH [1, 2] AS \"x SELECT a FROM phys.`other.secret` --\" SELECT a FROM db1.o"},
		{"array in projection", "SELECT [1, 2] AS \"x FROM phys.`other.secret` --\" FROM db1.o"},
		{"empty array", "WITH [] AS \"x SELECT a FROM hg_unsafe.db1__t --\" SELECT a FROM db1.o"},
		{"array of tuples", "SELECT [(1, 2)] AS \"x y\" FROM db1.o"},
		{"array function", "SELECT array(1, 2) AS \"x y\" FROM db1.o"},
		{"tuple function", "SELECT tuple(1, 2) AS \"x y\" FROM db1.o"},
		{"map function", "SELECT map('k', 1) AS \"x y\" FROM db1.o"},
		{"tuple element access", "SELECT (1, 2).1 AS \"x y\" FROM db1.o"},
		{"scalar subquery", "SELECT (SELECT 1, 2) AS \"x y\" FROM db1.o"},
		{"column", "SELECT a AS \"x SELECT 1 --\" FROM db1.o"},
	} {
		for mode, opts := range commandModes() {
			t.Run(mode+"/keeps quotes/"+r.name, func(t *testing.T) {
				resp, err := doRewrite(e, r.sql, opts)
				if err != nil {
					t.Fatalf("doRewrite: %v", err)
				}
				if resp.GetCode() != pb.RewriteCode_Success {
					t.Fatalf("code = %s (%s), want Success", resp.GetCode(), resp.GetMessage())
				}
				alias := r.sql[strings.Index(r.sql, `AS "`)+len(`AS `):]
				alias = alias[:strings.Index(alias[1:], `"`)+2]
				if !strings.Contains(resp.GetSqlAfterRewrite(), " AS "+alias) {
					t.Fatalf("sql_after_rewrite = %q lost the quotes of %s", resp.GetSqlAfterRewrite(), alias)
				}
			})
		}
	}

	// A tuple alias with no special characters loses its quotes but stays the
	// same identifier, and the gate accepts it: the raw token spelling is
	// unchanged (measured: `xy`, `x`, and a keyword such as `from` all print
	// unquoted, and ClickHouse 26.8 reads each as the alias).
	for _, sql := range []string{
		`SELECT (1, 2) AS "xy" FROM db1.o`,
		`WITH (1, 2) AS "xy" SELECT a FROM db1.o`,
		`SELECT (1, 2) AS xy FROM db1.o`,
	} {
		for mode, opts := range commandModes() {
			t.Run(mode+"/plain alias/"+sql, func(t *testing.T) {
				resp, err := doRewrite(e, sql, opts)
				if err != nil {
					t.Fatalf("doRewrite: %v", err)
				}
				if resp.GetCode() != pb.RewriteCode_Success {
					t.Fatalf("code = %s (%s), want Success", resp.GetCode(), resp.GetMessage())
				}
			})
		}
	}
}

// fillAliasForm substitutes the expression and the (already quoted) alias into
// a form template's two %s verbs without interpreting % in either.
func fillAliasForm(tmpl, expr, alias string) string {
	return strings.Replace(strings.Replace(tmpl, "%s", expr, 1), "%s", alias, 1)
}
