package rewriter

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// tablerefDynamic is the request housegate sends after Plan C: db1 maps to
// phys, phys is known-physical AND protected, hg_* are protected; si adds the
// V2 surface with db1.t Active.
func tablerefDynamic(si bool) *pb.RewriteTableDynamicArgs {
	dyn := &pb.RewriteTableDynamicArgs{
		DatabaseMap:                      map[string]string{"db1": "phys"},
		KnownPhysicalDatabases:           []string{"phys"},
		UpstreamLogicalDatabaseInContext: "db1",
		Delim:                            "_",
		ProtectedDatabases:               []string{"phys", "hg_safe", "hg_unsafe", "hg_promote"},
	}
	if si {
		dyn.StorageIntegrity = &pb.StorageIntegrityArgs{
			Tables: map[string]*pb.StorageIntegrityArgs_Table{
				"db1.t": {SafeTable: "hg_safe.db1__t", UnsafeTable: "hg_unsafe.db1__t"},
			},
			ReadMode:            pb.StorageIntegrityArgs_READ_MODE_SAFE,
			ReservedRowIdColumn: "_hg_row_id",
			ContractVersion:     pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2,
			ReservedDatabases:   []string{"hg_safe", "hg_unsafe", "hg_promote"},
		}
	}
	return dyn
}

func tablerefOpts(si bool) []*pb.RewriteOption {
	return []*pb.RewriteOption{tableRewriteDynamic(tablerefDynamic(si))}
}

type tablerefCase struct {
	name     string
	sql      string
	si       bool
	wantCode pb.RewriteCode
	wantMsg  string   // substring; "" = don't check
	wantSQL  string   // exact; "" = don't check
	wantAcc  []string // "db.table" in response order; nil = don't check
}

func runTablerefCases(t *testing.T, cases []tablerefCase) {
	t.Helper()
	e := newEngine(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := doRewrite(e, c.sql, tablerefOpts(c.si))
			if err != nil {
				t.Fatalf("doRewrite: %v", err)
			}
			if resp.GetCode() != c.wantCode {
				t.Fatalf("code = %s (%s), want %s", resp.GetCode(), resp.GetMessage(), c.wantCode)
			}
			if c.wantMsg != "" && !strings.Contains(resp.GetMessage(), c.wantMsg) {
				t.Fatalf("message = %q, want substring %q", resp.GetMessage(), c.wantMsg)
			}
			if c.wantSQL != "" && resp.GetSqlAfterRewrite() != c.wantSQL {
				t.Fatalf("sql = %q, want %q", resp.GetSqlAfterRewrite(), c.wantSQL)
			}
			if c.wantAcc != nil {
				var got []string
				for _, a := range resp.GetOriginalAccessedTables() {
					got = append(got, a.GetOriginalDatabase()+"."+a.GetOriginalTable())
				}
				if strings.Join(got, ",") != strings.Join(c.wantAcc, ",") {
					t.Fatalf("accessed = %v, want %v", got, c.wantAcc)
				}
			}
		})
	}
}

func TestTableRef_ParametersInTablePositionsAreRefused(t *testing.T) {
	const msg = "query parameters are not supported in a database or table position"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"SELECT * FROM {p:Identifier}",
			"SELECT * FROM db1.{p:Identifier}",
			"SELECT * FROM {d:Identifier}.t",
			"SELECT * FROM db1.o AS a JOIN {p:Identifier} AS b USING (a)",
			"SELECT * FROM (SELECT * FROM {p:Identifier})",
			"SELECT * FROM db1.o WHERE a IN {p:Identifier}",
			"SELECT * FROM db1.o WHERE a IN db1.{p:Identifier}",
			"SELECT * FROM db1.o WHERE in(a, {p:Identifier})",
			"INSERT INTO db1.o SELECT * FROM {p:Identifier}",
			"CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM {p:Identifier}",
			"CREATE MATERIALIZED VIEW db1.mv TO db1.{p:Identifier} AS SELECT * FROM db1.o",
			"CREATE MATERIALIZED VIEW {p:Identifier} ENGINE = Memory AS SELECT * FROM db1.o",
			"DROP TABLE {p:Identifier}",
			"DROP TABLE db1.{p:Identifier}",
			"INSERT INTO db1.{p:Identifier} VALUES (1)",
			"CREATE TABLE {p:Identifier} (a UInt64) ENGINE = Memory",
			"EXISTS TABLE db1.{p:Identifier}",
			"SHOW CREATE TABLE db1.{p:Identifier}",
			"DESCRIBE TABLE db1.{p:Identifier}",
			"RENAME TABLE db1.{p:Identifier} TO db1.z",
			"SHOW TABLES FROM {d:Identifier}",
			"USE {d:Identifier}",
			"ALTER TABLE db1.{p:Identifier} UPDATE a = 1 WHERE 1",
			"CREATE DATABASE {d:Identifier}",
			"DROP DATABASE {d:Identifier}",
			"SHOW COLUMNS FROM {p:Identifier}",
			"SHOW INDEX FROM db1.{p:Identifier}",
			// T2 precedes T3 (spec 2026-09-26 §5): an SI physical table
			// beside an identifier parameter keeps the T2 message.
			"SELECT * FROM {p:Identifier} JOIN hg_safe.db1__t USING (a)",
			"CREATE TABLE db1.n (a UInt64 DEFAULT (SELECT max(a) FROM {p:Identifier} JOIN hg_safe.db1__t USING (a))) ENGINE = Memory",
			"CHECK TABLE hg_safe.db1__t PARTITION {p:Identifier}",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: msg, wantSQL: sql})
		}
	}
	runTablerefCases(t, cases)
}

// TestTableRef_CommentBypassAttemptsAreRefused pins the exact concrete bypass
// SQLs from the controller review of this task: a prior hand-rolled command-
// text scanner recognized "--" and "/* */" but not "#", "#!" or "//" as
// comment openers, so it treated the "'" inside "it's" as an opening quote and
// swallowed the genuine parameter that followed as if it were unterminated
// string content. The tokenizer-based scan in IdentifierParameterInText
// recognizes all five ClickHouse comment openers regardless of what they
// contain, so each of these must still be refused.
func TestTableRef_CommentBypassAttemptsAreRefused(t *testing.T) {
	const msg = "query parameters are not supported in a database or table position"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases,
			tablerefCase{name: "exists_line_comment_slash_slash", sql: "EXISTS TABLE // it's\n db1.{p:Identifier}", si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: msg},
			tablerefCase{name: "exists_line_comment_hash", sql: "EXISTS TABLE # it's\n db1.{p:Identifier}", si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: msg},
			tablerefCase{name: "describe_line_comment_slash_slash", sql: "DESCRIBE TABLE // it's\n db1.{p:Identifier}", si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: msg},
			tablerefCase{name: "show_create_line_comment_hash_bang", sql: "SHOW CREATE TABLE #! it's\n db1.{p:Identifier}", si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: msg},
		)
	}
	runTablerefCases(t, cases)
}

func TestTableRef_ValueAndColumnParametersStayAllowed(t *testing.T) {
	runTablerefCases(t, []tablerefCase{
		{name: "value", sql: "SELECT * FROM db1.o WHERE a = {v:UInt64}", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a = {v: UInt64}`},
		{name: "column", sql: "SELECT {c:Identifier} FROM db1.o", wantCode: pb.RewriteCode_Success},
		// Spec 2026-09-26 R2/R8: an opaque ALTER … UPDATE tail cannot be
		// proven column-only, so an Identifier parameter anywhere in a command
		// node's text is refused with the T2 message (this row used to pin the
		// assignment position as allowed).
		{name: "alter_update_assignment", sql: "ALTER TABLE db1.o UPDATE a = {c:Identifier} WHERE 1",
			wantCode: pb.RewriteCode_InvalidRewriteRequest,
			wantMsg:  "query parameters are not supported in a database or table position"},
	})
}

func TestTableRef_ProtectedDatabasesAreRefusedEverywhere(t *testing.T) {
	var cases []tablerefCase
	for _, db := range []string{"phys", "hg_safe", "hg_unsafe", "hg_promote"} {
		msg := "protected database " + db + " is not addressable"
		for _, shape := range []string{
			"SELECT * FROM %s.`db2.x`",
			"SELECT * FROM db1.o AS a JOIN %s.`db2.x` AS b USING (a)",
			"SELECT * FROM (SELECT * FROM %s.`db2.x`)",
			"WITH c AS (SELECT * FROM %s.`db2.x`) SELECT * FROM c",
			"SELECT * FROM db1.o WHERE a IN %s.`db2.x`",
			"SELECT * FROM db1.o WHERE a IN (%s.`db2.x`)",
			"SELECT * FROM db1.o WHERE (a, b) IN %s.`db2.x`",
			"SELECT * FROM db1.o WHERE in(a, %s.`db2.x`)",
			"SELECT * FROM db1.o WHERE a GLOBAL IN %s.`db2.x`",
			"INSERT INTO db1.o SELECT * FROM %s.`db2.x`",
			"CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM %s.`db2.x`",
			"CREATE VIEW db1.v AS SELECT * FROM %s.`db2.x`",
			"CREATE MATERIALIZED VIEW db1.mv TO %s.`db2.x` AS SELECT * FROM db1.o",
			"INSERT INTO %s.`db2.x` VALUES (1)",
			"DROP TABLE %s.`db2.x`",
			"CREATE TABLE %s.`db2.x` (a UInt64) ENGINE = Memory",
			"CREATE TABLE db1.n AS %s.`db2.x`",
			"RENAME TABLE %s.`db2.x` TO db1.z",
			"EXISTS TABLE %s.`db2.x`",
			"SHOW CREATE TABLE %s.`db2.x`",
			"DESCRIBE TABLE %s.`db2.x`",
			"SHOW TABLES FROM %s",
			"USE %s",
			"CREATE DATABASE %s",
			"DROP DATABASE %s",
			"SELECT * FROM merge('%s', 'db2')",
			"SELECT * FROM remote('127.0.0.1:9000', '%s', 'db2.x')",
			"CREATE TABLE db1.n (a UInt64) ENGINE = Merge('%s', '^db2')",
			"SELECT joinGet('%s.`db2.x`', 'v', 1)",
			// Review round 1 findings 1 & 2: the identifier form of a
			// lookup's table argument (no surrounding string literal), and
			// hasColumnInTable's optional leading hostname[, username] form,
			// which shifts the database to the third argument from the end.
			"SELECT joinGet(%s.`db2.x`, 'v', 1)",
			"SELECT dictGet(%s.d, 'v', 1)",
			"SELECT hasColumnInTable('%s', 't', 'c')",
			"SELECT hasColumnInTable('localhost', '%s', 't', 'c')",
			"SELECT hasColumnInTable('localhost', 'user', '%s', 't', 'c')",
		} {
			sql := strings.ReplaceAll(shape, "%s", db)
			// Under the active SI surface the hg_* names keep their existing
			// SI messages; the code is still a rejection.
			//
			// Two deviations from the brief's starting assumption, found by
			// running this test (see task-4 report "corpus cases that
			// changed" / self-review for the full writeup):
			//
			//  1. siHandlerBlindShapes: the joinGet shape's database
			//     qualifier is a string-lookup argument -- no existing SI
			//     handler classifies that position as a table reference (a
			//     parenthesized IN operand is decoded for the SI handlers by
			//     decodeInOperand and gets their message, spec T3), so
			//     nothing downstream would otherwise reject it.
			//     PreflightTableReferences now rejects a protected hit there
			//     unconditionally too (engine.CollectDatabaseReferenceSets' blind set),
			//     with the preflight's own generic message/code rather than an
			//     SI handler's. Ruling 3 anticipated adjusting the *code* per
			//     observed handler behaviour for an SI-owned row; this is the
			//     same kind of adjustment for a position no handler covers at
			//     all.
			//  2. writeSideShapes: every non-plain-SELECT-read shape (INSERT/
			//     CREATE/DROP/RENAME/EXISTS/SHOW/USE/CREATE-DROP-DATABASE/the
			//     ENGINE=Merge(...) table-engine form) answers
			//     UnsupportedStatement under active SI, not RewriteError --
			//     pre-existing, corpus-pinned SI dispatch this task does not
			//     touch. A plain SELECT read (including CREATE VIEW's body)
			//     keeps rejectCodeFor's RewriteError default.
			isSIHandlerBlind := strings.Contains(shape, "joinGet") || strings.Contains(shape, "dictGet") ||
				strings.Contains(shape, "hasColumnInTable")
			isWriteSide := siWriteSideShapes[shape]
			for _, si := range []bool{false, true} {
				want := msg
				wantCode := rejectCodeFor(db, si)
				if si && db != "phys" && !isSIHandlerBlind {
					want = "storage-integrity"
					if isWriteSide {
						wantCode = pb.RewriteCode_UnsupportedStatement
					}
				}
				if isSIHandlerBlind {
					wantCode = pb.RewriteCode_InvalidRewriteRequest
				}
				cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantMsg: want,
					wantCode: wantCode})
			}
		}
	}
	runTablerefCases(t, cases)
}

// rejectCodeFor: the protected rule answers InvalidRewriteRequest; an SI-owned
// hg_* rejection keeps whatever code the SI handler uses today, so only the
// non-Success property is asserted for those by comparing against the
// engine's own answer at the first green run. Start strict and relax per case.
func rejectCodeFor(db string, si bool) pb.RewriteCode {
	if si && db != "phys" {
		return pb.RewriteCode_RewriteError // most SI SELECT-side messages; write-side ones use UnsupportedStatement
	}
	return pb.RewriteCode_InvalidRewriteRequest
}

// siWriteSideShapes: observed at the first green run (see rejectCodeFor's
// comment) -- every one of these non-plain-SELECT-read shapes answers
// UnsupportedStatement under active SI for a protected hg_* database, not
// RewriteError. Pre-existing, corpus-pinned SI dispatch; this task does not
// touch it, only records which shapes hit it.
//
// The INSERT ... SELECT and CREATE TABLE ... AS SELECT embedded-source shapes
// ARE listed here (controller ruling, cross-engine parity, spec 2026-09-26
// T4 second half): although the embedded body is classified by the SELECT
// pipeline (rewriteEmbeddedBody -> rewriteSelectCore), the outer statement is
// still a write (INSERT/CREATE TABLE), and the corpus convention pins
// UnsupportedStatement for the write-statement family (RewriteError is
// SELECT-family only, matching the C++ engine's embedded-body path, which
// passes UnsupportedStatement explicitly). Only the CODE is forced; the
// SELECT pipeline's message text is kept verbatim (see rewriteEmbeddedBody).
// CREATE VIEW's body is genuinely different: dispatchView does NOT force the
// code (it keeps bodyResp.Code as-is), so CREATE VIEW's own shape stays out
// of this map.
var siWriteSideShapes = map[string]bool{
	"INSERT INTO db1.o SELECT * FROM %s.`db2.x`":                           true,
	"CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM %s.`db2.x`":       true,
	"CREATE MATERIALIZED VIEW db1.mv TO %s.`db2.x` AS SELECT * FROM db1.o": true,
	"INSERT INTO %s.`db2.x` VALUES (1)":                                    true,
	"DROP TABLE %s.`db2.x`":                                                true,
	"CREATE TABLE %s.`db2.x` (a UInt64) ENGINE = Memory":                   true,
	"CREATE TABLE db1.n AS %s.`db2.x`":                                     true,
	"RENAME TABLE %s.`db2.x` TO db1.z":                                     true,
	"EXISTS TABLE %s.`db2.x`":                                              true,
	"SHOW CREATE TABLE %s.`db2.x`":                                         true,
	"DESCRIBE TABLE %s.`db2.x`":                                            true,
	"SHOW TABLES FROM %s":                                                  true,
	"USE %s":                                                               true,
	"CREATE DATABASE %s":                                                   true,
	"DROP DATABASE %s":                                                     true,
	"CREATE TABLE db1.n (a UInt64) ENGINE = Merge('%s', '^db2')":           true,
}

func TestTableRef_ProtectedNameAsColumnOrAliasIsAllowed(t *testing.T) {
	runTablerefCases(t, []tablerefCase{
		{name: "column named phys", sql: "SELECT phys FROM db1.o", wantCode: pb.RewriteCode_Success},
		{name: "alias named hg_safe", sql: "SELECT a AS hg_safe FROM db1.o", wantCode: pb.RewriteCode_Success},
	})
}

func TestTableRef_InOperandsAreRewrittenAndReported(t *testing.T) {
	runTablerefCases(t, []tablerefCase{
		{name: "infix", sql: "SELECT * FROM db1.o WHERE a IN db1.p", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.p"`, wantAcc: []string{"db1.o", "db1.p"}},
		{name: "paren", sql: "SELECT * FROM db1.o WHERE a IN (db1.p)", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (phys."db1.p")`, wantAcc: []string{"db1.o", "db1.p"}},
		{name: "tuple", sql: "SELECT * FROM db1.o WHERE (a, b) IN db1.p", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE (a, b) IN phys."db1.p"`, wantAcc: []string{"db1.o", "db1.p"}},
		{name: "callable", sql: "SELECT * FROM db1.o WHERE in(a, db1.p)", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE in(a, phys."db1.p")`, wantAcc: []string{"db1.o", "db1.p"}},
		{name: "global", sql: "SELECT * FROM db1.o WHERE a GLOBAL IN db1.p", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a GLOBAL IN phys."db1.p"`, wantAcc: []string{"db1.o", "db1.p"}},
		// wantAcc's second entry is ".p", not "db1.p": an unqualified IN operand
		// decodes to TableTarget{DB: "", Table: "p"} just like an unqualified
		// FROM table does (confirmed by direct comparison against `SELECT *
		// FROM p` under the same dynamic args — its OriginalDatabase is also
		// ""), and OriginalAccessedTables reports the field verbatim
		// (buildAccessed's `OriginalDatabase: tt.DB`); only LogicalDatabase
		// resolves the implicit "db1" context, which this helper does not
		// surface. wantSQL still confirms the rewrite itself correctly
		// resolves the unqualified operand to phys."db1.p".
		{name: "unqualified", sql: "SELECT * FROM db1.o WHERE a IN p", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.p"`, wantAcc: []string{"db1.o", ".p"}},
		{name: "own table, SI active", sql: "SELECT * FROM db1.o WHERE a IN db1.o", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.o"`, wantAcc: []string{"db1.o"}},
		{name: "active table derived read", sql: "SELECT * FROM db1.o WHERE a IN db1.t", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t)`, wantAcc: []string{"db1.o", "db1.t"}},
		{name: "callable active table derived read", sql: "SELECT * FROM db1.o WHERE in(a, db1.t)", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE in(a, (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t))`,
			wantAcc: []string{"db1.o", "db1.t"}},
		// Unqualified spellings of the same Active table (controller review
		// round 2): the operand resolves via UpstreamLogicalDatabaseInContext
		// ("db1") to the same db1.t, so it must read through the derived safe
		// surface exactly like the qualified form above — not get blanket-
		// rejected the way an unqualified table-function/table-engine operand
		// still does. wantAcc's second entry is ".t", not "db1.t": like the
		// "unqualified" (non-SI) case above, TableTarget{DB:"",Table:"t"}
		// reports its raw, unresolved OriginalDatabase; the accessed entry is
		// still correctly flagged IsStorageIntegrity (measured, not asserted by
		// this helper, which only compares db+table).
		{name: "unqualified active table derived read (infix)", sql: "SELECT * FROM db1.o WHERE a IN t", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t)`, wantAcc: []string{"db1.o", ".t"}},
		{name: "unqualified active table derived read (paren)", sql: "SELECT * FROM db1.o WHERE a IN (t)", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t)`, wantAcc: []string{"db1.o", ".t"}},
		{name: "unqualified active table derived read (callable)", sql: "SELECT * FROM db1.o WHERE in(a, t)", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE in(a, (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t))`, wantAcc: []string{"db1.o", ".t"}},
		{name: "cte alias untouched", sql: "WITH c AS (SELECT 1 AS a) SELECT * FROM db1.o WHERE a IN c", wantCode: pb.RewriteCode_Success,
			wantSQL: `WITH c AS (SELECT 1 AS a) SELECT * FROM phys."db1.o" "db1.o" WHERE a IN c`, wantAcc: []string{"db1.o"}},
		{name: "system stays", sql: "SELECT * FROM db1.o WHERE a IN system.tables", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN system.tables`, wantAcc: []string{"db1.o", "system.tables"}},
		// A single-element *literal* value list is never a table operand
		// (controller review round 1, finding 2): decodeInNamespaceRefDetail
		// requires is_field or a structurally-provable column/dot shape, and a
		// string literal is neither, even when its text happens to spell a
		// protected or SI-active name. It stays an ordinary value comparison —
		// untouched, unreported, and never checked against SI/protected-database
		// policy — exactly as a bare `a = 'hg_safe.db1__t'` would.
		{name: "literal value list stays a value, not a table (SI)", sql: "SELECT * FROM db1.o WHERE a IN ('hg_safe.db1__t')", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN ('hg_safe.db1__t')`, wantAcc: []string{"db1.o"}},
		{name: "literal value list stays a value, not a table (non-SI)", sql: "SELECT * FROM db1.o WHERE a IN ('phys.x')", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN ('phys.x')`, wantAcc: []string{"db1.o"}},
	})
}

// TestTableRef_InOperandRemoteMappedLogicalRendersRemoteCall pins controller
// review round 1, finding 3: a logical database mapped through
// LogicalDatabaseToRemoteUpstreamIndex/RemoteUpstreams (decideTable's
// StatusRemote) renders as a bare remote(addr, db, table, user, password) call
// when it appears as an IN operand, exactly mirroring applyDecision's
// ActionRemote branch for a FROM table (see remoteFunc). Before this fix the
// operand was left untouched (still reading the logical name in the generated
// SQL) even though decideTable had already recorded a table_rewrites entry
// and an accessed table for it — a real rewrite that never actually happened
// in the SQL.
func TestTableRef_InOperandRemoteMappedLogicalRendersRemoteCall(t *testing.T) {
	e := newEngine(t)
	dyn := tablerefDynamic(false)
	dyn.DatabaseMap["tenant1"] = "testnet"
	dyn.KnownPhysicalDatabases = append(dyn.KnownPhysicalDatabases, "testnet")
	dyn.LogicalDatabaseToRemoteUpstreamIndex = map[string]string{"tenant1": "peer"}
	dyn.RemoteUpstreams = map[string]*pb.RewriteTableDynamicArgs_RemoteUpstream{
		"peer": {Addr: "h:9000", User: "u", Password: "p"},
	}
	sql := "SELECT * FROM db1.o WHERE a IN tenant1.x"
	resp, err := doRewrite(e, sql, []*pb.RewriteOption{tableRewriteDynamic(dyn)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code = %s (%s), want Success", resp.GetCode(), resp.GetMessage())
	}
	wantSQL := `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN remote('h:9000', 'testnet', 'tenant1.x', 'u', 'p')`
	if resp.GetSqlAfterRewrite() != wantSQL {
		t.Fatalf("sql = %q, want %q", resp.GetSqlAfterRewrite(), wantSQL)
	}
	wantRewrites := map[string]string{"db1.o": "phys.db1.o", "tenant1.x": "testnet.tenant1.x"}
	if !reflect.DeepEqual(resp.GetTableRewrites(), wantRewrites) {
		t.Fatalf("table_rewrites = %v, want %v", resp.GetTableRewrites(), wantRewrites)
	}
}

func TestTableRef_ProtectedLogicalContextIsRefused(t *testing.T) {
	e := newEngine(t)
	dyn := tablerefDynamic(false)
	dyn.UpstreamLogicalDatabaseInContext = "phys"
	resp, err := doRewrite(e, "SELECT * FROM o", []*pb.RewriteOption{tableRewriteDynamic(dyn)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest || resp.GetMessage() != "protected database phys is not addressable" {
		t.Fatalf("resp = %s %q", resp.GetCode(), resp.GetMessage())
	}
}

// TestTableRef_EmbeddedSourcesAreRewrittenAndReported pins spec T4 (second
// half): an INSERT ... SELECT or CREATE TABLE ... AS SELECT body is routed
// through the same SELECT pipeline as a view body, so its FROM/IN sources are
// rewritten to physical names and reported in original_accessed_tables, and an
// SI source becomes the derived safe/unsafe read the FROM path already emits.
func TestTableRef_EmbeddedSourcesAreRewrittenAndReported(t *testing.T) {
	runTablerefCases(t, []tablerefCase{
		{name: "insert select own", sql: "INSERT INTO db1.o SELECT * FROM db1.p", wantCode: pb.RewriteCode_Success,
			wantSQL: `INSERT INTO phys."db1.o" SELECT * FROM phys."db1.p" "db1.p"`, wantAcc: []string{"db1.o", "db1.p"}},
		// DEVIATION FROM BRIEF (reported DONE_WITH_CONCERNS): the brief's wantSQL
		// spells the back-alias quoted (`"p"`), but applyDecision's back-alias is
		// ident(originName(tt)) — for an unqualified operand tt.DB=="" so
		// originName is the bare "p", which the generator prints unquoted like
		// any other plain identifier with no dot (see TestTableRef_InOperandsAreRewrittenAndReported's
		// "unqualified" case, which documents the same TableTarget{DB:"",Table:"p"}
		// decode). Pinning the engine's actual, measured output.
		// wantAcc's second entry is ".p", not "db1.p", for the same documented
		// reason as TestTableRef_InOperandsAreRewrittenAndReported's "unqualified"
		// case: OriginalAccessedTables reports TableTarget{DB:"",Table:"p"} verbatim.
		{name: "insert select unqualified", sql: "INSERT INTO db1.o SELECT * FROM p", wantCode: pb.RewriteCode_Success,
			wantSQL: `INSERT INTO phys."db1.o" SELECT * FROM phys."db1.p" p`, wantAcc: []string{"db1.o", ".p"}},
		{name: "insert select nested in", sql: "INSERT INTO db1.o SELECT * FROM (SELECT * FROM db1.p WHERE a IN db1.q)", wantCode: pb.RewriteCode_Success,
			wantAcc: []string{"db1.o", "db1.p", "db1.q"}},
		{name: "insert select active source", sql: "INSERT INTO db1.o SELECT * FROM db1.t", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: `INSERT INTO phys."db1.o" SELECT * FROM (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t) AS "db1.t"`, wantAcc: []string{"db1.o", "db1.t"}},
		{name: "insert into active target keeps signed-lane marking", sql: "INSERT INTO db1.t SELECT * FROM db1.o", si: true, wantCode: pb.RewriteCode_Success,
			wantAcc: []string{"db1.t", "db1.o"}},
		{name: "ctas own", sql: "CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM db1.p", wantCode: pb.RewriteCode_Success,
			wantSQL: `CREATE TABLE phys."db1.n" ENGINE=Memory AS (SELECT * FROM phys."db1.p" "db1.p")`, wantAcc: []string{"db1.n", "db1.p"}},
		{name: "ctas empty keeps and rewrites the body", sql: "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM db1.p", wantCode: pb.RewriteCode_Success,
			wantSQL: `CREATE TABLE phys."db1.n" ENGINE=Memory EMPTY AS (SELECT * FROM phys."db1.p" "db1.p")`, wantAcc: []string{"db1.n", "db1.p"}},
		{name: "ctas empty table named empty", sql: "CREATE TABLE db1.empty ENGINE = Memory EMPTY AS SELECT * FROM db1.p", wantCode: pb.RewriteCode_Success,
			wantSQL: `CREATE TABLE phys."db1.empty" ENGINE=Memory EMPTY AS (SELECT * FROM phys."db1.p" "db1.p")`, wantAcc: []string{"db1.empty", "db1.p"}},
		{name: "ctas empty comment before empty", sql: "CREATE TABLE db1.n ENGINE = Memory COMMENT 'c' EMPTY AS SELECT * FROM db1.p", wantCode: pb.RewriteCode_Success,
			wantSQL: `CREATE TABLE phys."db1.n" ENGINE=Memory EMPTY AS (SELECT * FROM phys."db1.p" "db1.p") COMMENT 'c'`, wantAcc: []string{"db1.n", "db1.p"}},
		{name: "ctas empty comment after body", sql: "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM db1.p COMMENT 'c'", wantCode: pb.RewriteCode_Success,
			wantSQL: `CREATE TABLE phys."db1.n" ENGINE=Memory EMPTY AS (SELECT * FROM phys."db1.p" "db1.p") COMMENT 'c'`, wantAcc: []string{"db1.n", "db1.p"}},
		{name: "ctas empty protected source refused", sql: "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM phys.`db2.x`",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable",
			wantSQL: "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM phys.`db2.x`"},
		{name: "ctas empty active source", sql: "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM db1.t", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: `CREATE TABLE phys."db1.n" ENGINE=Memory EMPTY AS (SELECT * FROM (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t) AS "db1.t")`, wantAcc: []string{"db1.n", "db1.t"}},
		// The EMPTY-stripped form parses and is rewritten, but InsertCreateTableEmpty
		// cannot locate the body in the generated SQL (FINAL SAMPLE), so
		// sealCreateTableEmpty refuses instead of forwarding a CTAS without EMPTY
		// (a data copy).
		{name: "ctas empty unreparseable is sealed", sql: "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM db1.p FINAL SAMPLE 0.1",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported",
			wantSQL: "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM db1.p FINAL SAMPLE 0.1"},
		{name: "ctas empty unreparseable is sealed with the SI catch-all", sql: "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM db1.p FINAL SAMPLE 0.1", si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: StorageIntegrityUnmodelledMessage,
			wantSQL: "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM db1.p FINAL SAMPLE 0.1"},
		{name: "ctas active source", sql: "CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM db1.t", si: true, wantCode: pb.RewriteCode_Success,
			wantAcc: []string{"db1.n", "db1.t"}},
		{name: "ctas into active target still refused", sql: "CREATE TABLE db1.t ENGINE = Memory AS SELECT * FROM db1.o", si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "accepts writes only through the signed statement lane"},
		// Fix round 1: a PARENTHESIZED embedded body (`AS (SELECT …)` /
		// `INSERT INTO t (SELECT …)`) is polyglot's {"subquery":{"this":…, …}}
		// wrapper around the same read body, not a bare {"select":…} —
		// isReadBody saw only "subquery" and rejected it, so
		// ExtractInsertBody/ExtractCreateSelectBody returned has=false and
		// rewriteEmbeddedBody passed the statement through UNCHANGED: an SI
		// source that must be refused was silently forwarded as Success
		// instead. These rows cover both statements, SI and non-SI, in the
		// paren form (subqueryShells in internal/engine/writes.go peels the
		// wrapper; see the engine-level ..._paren tests in writes_test.go).
		{name: "insert select own paren", sql: "INSERT INTO db1.o (SELECT * FROM db1.p)", wantCode: pb.RewriteCode_Success,
			wantSQL: `INSERT INTO phys."db1.o" (SELECT * FROM phys."db1.p" "db1.p")`, wantAcc: []string{"db1.o", "db1.p"}},
		{name: "insert select active source paren", sql: "INSERT INTO db1.o (SELECT * FROM db1.t)", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: `INSERT INTO phys."db1.o" (SELECT * FROM (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t) AS "db1.t")`, wantAcc: []string{"db1.o", "db1.t"}},
		// CTAS's generator unconditionally wraps as_select in an extra paren
		// layer on every Generate call (see internal/engine/writes_test.go's
		// TestCreateSelectBody_extractRewriteSet note), so a singly-
		// parenthesized original renders with TWO layers here — matching a
		// plain parse+Generate round trip of the same SQL with no rewriting
		// at all (verified via probe).
		{name: "ctas own paren with comment", sql: "CREATE TABLE db1.n ENGINE = Memory AS (SELECT * FROM db1.p) COMMENT 'x'", wantCode: pb.RewriteCode_Success,
			wantSQL: `CREATE TABLE phys."db1.n" ENGINE=Memory AS ((SELECT * FROM phys."db1.p" "db1.p")) COMMENT 'x'`, wantAcc: []string{"db1.n", "db1.p"}},
		{name: "ctas active source paren", sql: "CREATE TABLE db1.n ENGINE = Memory AS (SELECT * FROM db1.t)", si: true, wantCode: pb.RewriteCode_Success,
			wantSQL: `CREATE TABLE phys."db1.n" ENGINE=Memory AS ((SELECT * FROM (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t) AS "db1.t"))`, wantAcc: []string{"db1.n", "db1.t"}},
	})

	// The signed lane's contract (spec §2): "INSERT remains an ordinary
	// successful physical rewrite marked is_storage_integrity". Confirm the
	// INSERT target itself (not just its embedded source) still carries the
	// SI marker after this task routes the body through the SELECT pipeline.
	e := newEngine(t)
	resp, err := doRewrite(e, "INSERT INTO db1.t SELECT * FROM db1.o", tablerefOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code = %s (%s), want Success", resp.GetCode(), resp.GetMessage())
	}
	acc := resp.GetOriginalAccessedTables()
	if len(acc) == 0 || !acc[0].GetIsStorageIntegrity() {
		t.Fatalf("accessed[0] = %+v, want IsStorageIntegrity=true", acc)
	}
}

// TestTableRef_TableFunctionsAndEnginesAreAllowlisted pins spec 2026-09-26 T5
// (Task 7): every source-role table function and CREATE TABLE engine/setting
// must be on a closed allowlist, and an unrecognized table function name is
// refused as "not recognised" rather than silently forwarded.
//
// Controller ruling 1 restructured the brief's original single si-independent
// table: the refused-function and refused-engine rows below all run with
// si:false (the default, unchanged from the brief) because with the
// storage-integrity surface active, PreflightTableReferences defers this
// check entirely to rewriteSelectCore / preflightStorageIntegrityWrite,
// AFTER their own SI namespace policy — so an SI-owned message (e.g.
// merge('hg_safe', …) or merge('db1', …) under contract V2) is never
// pre-empted. The three si:true rows appended at the end exercise exactly
// that ordering: the existing SI message for a table function whose target IS
// SI-owned, the NEW T5 refusal for one whose target is NOT, and a plain
// success for an allowed function.
func TestTableRef_TableFunctionsAndEnginesAreAllowlisted(t *testing.T) {
	var cases []tablerefCase
	for _, fn := range []string{
		"merge('db1', 'o')", "remote('h', 'db1', 'o')", "remoteSecure('h', 'db1', 'o')", "cluster('c', db1.o)",
		"clusterAllReplicas('c', db1.o)", "loop('db1', 'o')", "dictionary(db1.d)", "mergeTreeIndex('db1', 'o')",
		"mergeTreeProjection('db1', 'o', 'p')", "timeSeriesData('db1', 'o')", "prometheusQuery('db1', 'o', 'up')",
		"clickhouse('db1.o')", "mysql('h', 'db1', 'o', 'u', 'p')", "postgresql('h', 'db1', 'o', 'u', 'p')",
		"mongodb('h', 'db1', 'o', 'u', 'p', 'a UInt8')", "jdbc('ds', 'db1', 'o')", "odbc('ds', 'db1', 'o')",
		"executable('x.sh', 'TSV', 'a UInt8')", "fuzzQuery('SELECT 1')",
	} {
		name := fn[:strings.IndexByte(fn, '(')]
		cases = append(cases, tablerefCase{name: fn, sql: "SELECT * FROM " + fn,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table function " + name + " is not accepted"})
	}
	cases = append(cases,
		tablerefCase{name: "unknown", sql: "SELECT * FROM frobnicate('x')", wantCode: pb.RewriteCode_UnsupportedStatement,
			wantMsg: "table function frobnicate is not recognised"},
		tablerefCase{name: "numbers", sql: "SELECT * FROM numbers(10)", wantCode: pb.RewriteCode_Success, wantAcc: []string{}},
		tablerefCase{name: "view body", sql: "SELECT * FROM view(SELECT * FROM db1.o)", wantCode: pb.RewriteCode_Success, wantAcc: []string{"db1.o"}},
		tablerefCase{name: "input", sql: "INSERT INTO db1.o SELECT * FROM input('a UInt8')", wantCode: pb.RewriteCode_Success},
		tablerefCase{name: "url unchanged (non-goal)", sql: "SELECT * FROM url('http://127.0.0.1/x', CSV)", wantCode: pb.RewriteCode_Success, wantAcc: []string{}},
		tablerefCase{name: "insert function", sql: "INSERT INTO FUNCTION remote('h', 'db1', 'o') VALUES (1)", wantCode: pb.RewriteCode_UnsupportedStatement},
	)
	for _, eng := range []string{
		"Merge('db1', '^o')", "Buffer(db1.o, 16, 10, 100, 10000, 1000000, 10000000, 100000000)",
		"Distributed(default, db1.o)", "URL('http://127.0.0.1/x', CSV)", "Dictionary(db1.d)", "KeeperMap('/x')",
		"EmbeddedRocksDB", "Kafka", "S3('http://127.0.0.1/x', CSV)", "File(CSV)",
		"ReplicatedMergeTree('/clickhouse/tables/x', 'r1')",
	} {
		name := eng
		if i := strings.IndexByte(eng, '('); i >= 0 {
			name = eng[:i]
		}
		cases = append(cases, tablerefCase{name: eng, sql: "CREATE TABLE db1.n (a UInt64) ENGINE = " + eng + " ORDER BY a",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table engine " + name + " is not accepted"})
	}
	cases = append(cases,
		tablerefCase{name: "MergeTree", sql: "CREATE TABLE db1.n (a UInt64) ENGINE = MergeTree ORDER BY a", wantCode: pb.RewriteCode_Success},
		tablerefCase{name: "ReplicatedMergeTree bare", sql: "CREATE TABLE db1.n (a UInt64) ENGINE = ReplicatedMergeTree ORDER BY a", wantCode: pb.RewriteCode_Success},
		tablerefCase{name: "Memory", sql: "CREATE TABLE db1.n (a UInt64) ENGINE = Memory", wantCode: pb.RewriteCode_Success},
		tablerefCase{name: "storage_policy", sql: "CREATE TABLE db1.n (a UInt64) ENGINE = MergeTree ORDER BY a SETTINGS storage_policy = 's3'",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table setting storage_policy is not accepted"},
		tablerefCase{name: "disk", sql: "CREATE TABLE db1.n (a UInt64) ENGINE = MergeTree ORDER BY a SETTINGS disk = 'd'",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table setting disk is not accepted"},
		tablerefCase{name: "alter modify setting disk", sql: "ALTER TABLE db1.o MODIFY SETTING disk = 'd'",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table setting disk is not accepted"},
		tablerefCase{name: "unknown engine", sql: "CREATE TABLE db1.n (a UInt64) ENGINE = Frob", wantCode: pb.RewriteCode_UnsupportedStatement,
			wantMsg: "table engine Frob is not accepted"},
		// Controller ruling 1's first and third si:true rows: the precedence
		// guarantee that an SI-owned message never moves, and that a plainly
		// allowed function stays allowed. The second si:true row (an
		// ordinary local-catalog function reaching the NEW T5 check itself)
		// needs a database with no Active table of its own -- "db1" cannot be
		// reused here, since db1.t Active reserves the WHOLE "db1" database
		// for every indirect namespace surface (see the row directly below);
		// it is TestTableRef_TableFunctionAllowlistAppliesUnderActiveSI.
		tablerefCase{name: "si merge active logical db unmoved", sql: "SELECT * FROM merge('db1', 'o')", si: true,
			wantCode: pb.RewriteCode_RewriteError, wantMsg: "not directly addressable through merge table function"},
		tablerefCase{name: "si numbers stays allowed", sql: "SELECT * FROM numbers(10)", si: true, wantCode: pb.RewriteCode_Success},
	)
	runTablerefCases(t, cases)
}

// TestTableRef_TableFunctionAllowlistAppliesUnderActiveSI pins controller
// ruling 1's second si:true row directly: a table function whose database
// hosts no Active storage-integrity table of its own (unlike "db1", which
// owns db1.t and so is entirely reserved by rejectStorageIntegrityNamespaces's
// IsStorageIntegrityLogicalDatabase branch — see
// TestTableRef_ProtectedDatabasesAreRefusedEverywhere and the "si merge
// active logical db unmoved" row above) still reaches the NEW T5 allowlist
// from rewriteSelectCore, once the SI namespace policy finds nothing to
// reject for it.
func TestTableRef_TableFunctionAllowlistAppliesUnderActiveSI(t *testing.T) {
	e := newEngine(t)
	dyn := tablerefDynamic(true)
	dyn.DatabaseMap["other"] = "phys"
	resp, err := doRewrite(e, "SELECT * FROM mergeTreeIndex('other', 'u')", []*pb.RewriteOption{tableRewriteDynamic(dyn)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement || !strings.Contains(resp.GetMessage(), "table function mergeTreeIndex is not accepted") {
		t.Fatalf("resp = %s %q, want UnsupportedStatement \"table function mergeTreeIndex is not accepted\"",
			resp.GetCode(), resp.GetMessage())
	}
}

// TestTableRef_StringLookupsAreResolvedOrRefused pins spec 2026-09-26 T6
// (Task 7): joinGet/dictGet-family calls are always refused (measured against
// ClickHouse 25.8 directly by the controller — none of the string/identifier
// forms those functions accept actually resolves a dotted logical table name,
// so there is no working rewrite for them), while hasColumnInTable's
// database/table pair is resolved and rewritten exactly like a FROM
// reference.
func TestTableRef_StringLookupsAreResolvedOrRefused(t *testing.T) {
	runTablerefCases(t, []tablerefCase{
		{name: "joinGet mapped literal", sql: "SELECT joinGet('db1.j', 'v', 1)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `joinGet target "db1.j" does not resolve`},
		{name: "joinGet identifier form", sql: "SELECT joinGet(db1.j, 'v', 1)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `joinGet target "db1.j" does not resolve`},
		{name: "dictGet mapped", sql: "SELECT dictGet('db1.d', 'v', 1)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `dictGet target "db1.d" does not resolve`},
		// Task 4's preflight protected-database step fires first: "phys" is
		// protected, so this never reaches the T6 refusal above.
		{name: "joinGet protected preflight fires first", sql: "SELECT joinGet('phys.`db2.x`', 'v', 1)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable"},
		{name: "hasColumnInTable mapped", sql: "SELECT hasColumnInTable('db1', 'j', 'v')", wantCode: pb.RewriteCode_Success,
			wantSQL: "SELECT hasColumnInTable('phys', 'db1.j', 'v')", wantAcc: []string{"db1.j"}},
		{name: "hasColumnInTable with host prefix", sql: "SELECT hasColumnInTable('localhost', 'db1', 'j', 'v')", wantCode: pb.RewriteCode_Success,
			wantSQL: "SELECT hasColumnInTable('localhost', 'phys', 'db1.j', 'v')", wantAcc: []string{"db1.j"}},
		{name: "hasColumnInTable active table refused", sql: "SELECT hasColumnInTable('db1', 't', 'v')", si: true,
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "db1.t" does not resolve`},
		{name: "hasColumnInTable non-literal db refused", sql: "SELECT hasColumnInTable(concat('db', '1'), 'j', 'v')",
			wantCode: pb.RewriteCode_InvalidRewriteRequest},
		// Task 7 fix round 1 finding 2: the joinGet/dictGet-family refusal and
		// hasColumnInTable's "outside a SELECT body" refusal both run
		// statement-wide in PreflightTableReferences, not just inside
		// rewriteSelectCore's SELECT-body handling. Measured directly: real
		// ClickHouse executes `ALTER TABLE t UPDATE a = joinGet(...)` reading
		// a Join table from another database, so these four opaque/column
		// positions must not pass through unrefused.
		{name: "joinGet in ALTER UPDATE assignment (opaque mutation)",
			sql:      "ALTER TABLE db1.o UPDATE a = joinGet('default.j','v',1) WHERE 1",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `joinGet target "default.j" does not resolve`},
		{name: "dictHas in ALTER DELETE predicate (opaque mutation)",
			sql:      "ALTER TABLE db1.o DELETE WHERE dictHas('db1.d',1)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `dictHas target "db1.d" does not resolve`},
		{name: "hasColumnInTable in ALTER UPDATE assignment refused, not rewritten",
			sql:      "ALTER TABLE db1.o UPDATE a = hasColumnInTable('default','x','v') WHERE 1",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		{name: "joinGet in CREATE TABLE column DEFAULT",
			sql:      "CREATE TABLE db1.n (a String DEFAULT joinGet('default.j','v',1)) ENGINE = Memory",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `joinGet target "default.j" does not resolve`},
		// Task 7 fix round 2 finding 2: mechanism (b) (the raw tokenizer scan
		// over every command/Raw-action text span) closes what round 1's
		// probe-reparse collector missed — a MULTI-command ALTER tail (which
		// fails a single-action reparse) and a Raw action that is not a
		// mutation at all (MODIFY COLUMN … DEFAULT …).
		{name: "dictHas in multi-command ALTER tail (UPDATE, DELETE)",
			sql:      "ALTER TABLE db1.o UPDATE a = 1 WHERE 1, DELETE WHERE dictHas('db1.d',1)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `dictHas target "db1.d" does not resolve`},
		{name: "joinGet in multi-command ALTER tail (DELETE, UPDATE)",
			sql:      "ALTER TABLE db1.o DELETE WHERE b = 1, UPDATE a = joinGet('default.j','v',1) WHERE 1",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `joinGet target "default.j" does not resolve`},
		{name: "joinGet in ALTER MODIFY COLUMN DEFAULT (Raw action, not a mutation)",
			sql:      "ALTER TABLE db1.o MODIFY COLUMN a String DEFAULT joinGet('default.j','v',1)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `joinGet target "default.j" does not resolve`},
		// hasColumnInTable outside every SELECT-body position: mechanism (a)'s
		// generic structured walk finds each of these via an ordinary
		// "function" node (no per-position collector needed, unlike round
		// 1's now-deleted CREATE-column/single-ALTER-mutation collectors),
		// and none of them is InSelectBody, so all are refused rather than
		// rewritten.
		{name: "hasColumnInTable in DELETE predicate",
			sql:      "DELETE FROM db1.o WHERE hasColumnInTable('default','x','v')",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		{name: "hasColumnInTable in UPDATE assignment",
			sql:      "UPDATE db1.o SET a = hasColumnInTable('default','x','v') WHERE 1",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		{name: "hasColumnInTable in INSERT VALUES",
			sql:      "INSERT INTO db1.o VALUES (hasColumnInTable('default','x','v'))",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		{name: "hasColumnInTable in ALTER ADD COLUMN DEFAULT",
			sql:      "ALTER TABLE db1.o ADD COLUMN b UInt8 DEFAULT hasColumnInTable('default','x','v')",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		{name: "hasColumnInTable in CREATE TABLE PARTITION BY",
			sql:      "CREATE TABLE db1.n (a UInt8) ENGINE = MergeTree ORDER BY a PARTITION BY hasColumnInTable('default','x','v')",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		{name: "hasColumnInTable in CREATE TABLE TTL",
			sql:      "CREATE TABLE db1.n (a DateTime) ENGINE = MergeTree ORDER BY a TTL a + hasColumnInTable('default','x','v')",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		{name: "hasColumnInTable in CREATE TABLE CONSTRAINT CHECK",
			sql:      "CREATE TABLE db1.n (a UInt8, CONSTRAINT c CHECK hasColumnInTable('default','x','v')) ENGINE = MergeTree ORDER BY a",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		// Keep: an ordinary multi-value IN predicate, and an ordinary
		// non-lookup DEFAULT expression, must stay untouched.
		{name: "ALTER UPDATE with ordinary IN predicate stays Success",
			sql:      "ALTER TABLE db1.o UPDATE a = 1 WHERE b IN (1, 2)",
			wantCode: pb.RewriteCode_Success},
		{name: "CREATE TABLE with ordinary DEFAULT now() stays Success",
			sql:      "CREATE TABLE db1.n (a DateTime DEFAULT now()) ENGINE = Memory",
			wantCode: pb.RewriteCode_Success},
		// Task 7 fix round 3 new breakage 1: a backquoted or double-quoted
		// lookup-family function name defeats mechanism (b)'s raw-text scan
		// (lookupCallsInRawTokens unconditionally skipped every
		// QUOTED_IDENTIFIER token), so these opaque-command/Raw-action
		// positions ran the quoted call as a real lookup instead of refusing
		// it. Measured: ClickHouse accepts a quoted identifier as a function
		// name regardless of spelling.
		{name: "quoted dictGet (backtick) in ALTER UPDATE assignment",
			sql:      "ALTER TABLE db1.o UPDATE a = `dictGet`('db1.a','v',1) WHERE 1",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `dictGet target "db1.a" does not resolve`},
		{name: "quoted joinGet (backtick) in ALTER UPDATE assignment",
			sql:      "ALTER TABLE db1.o UPDATE a = `joinGet`('default.j','v',1) WHERE 1",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `joinGet target "default.j" does not resolve`},
		{name: `quoted joinGet (double-quote) in ALTER UPDATE assignment`,
			sql:      `ALTER TABLE db1.o UPDATE a = "joinGet"('default.j','v',1) WHERE 1`,
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `joinGet target "default.j" does not resolve`},
		{name: "quoted joinGet (backtick) in ALTER MODIFY COLUMN DEFAULT (Raw action)",
			sql:      "ALTER TABLE db1.o MODIFY COLUMN a String DEFAULT `joinGet`('default.j','v',1)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `joinGet target "default.j" does not resolve`},
		{name: "quoted hasColumnInTable (backtick) in ALTER UPDATE assignment",
			sql:      "ALTER TABLE db1.o UPDATE a = `hasColumnInTable`('default','x','v') WHERE 1",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		// Task 7 fix round 3 new breakage 2: InSelectBody was true inside a
		// SELECT subtree reached via a structured UPDATE SET / DELETE WHERE
		// IN / INSERT VALUES / CREATE TABLE column DEFAULT scalar or IN
		// subquery — a position no rewrite pipeline ever reaches — so the
		// call was neither refused nor rewritten (an unmapped logical name
		// silently reached ClickHouse). Only the genuine SELECT-body-rewrite
		// roots (top-level SELECT-family, CTAS as_select, view body, INSERT
		// query) may elevate InSelectBody now; every other embedded SELECT
		// stays refused.
		{name: "hasColumnInTable in UPDATE SET scalar subquery, mapped-looking name still refused",
			sql:      "UPDATE db1.o SET a = (SELECT hasColumnInTable('db1','j','v')) WHERE 1",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "db1.j" does not resolve`},
		{name: "hasColumnInTable in UPDATE SET scalar subquery",
			sql:      "UPDATE db1.o SET a = (SELECT hasColumnInTable('default','x','v')) WHERE 1",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		{name: "hasColumnInTable in DELETE WHERE IN subquery",
			sql:      "DELETE FROM db1.o WHERE b IN (SELECT hasColumnInTable('default','x','v'))",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		{name: "hasColumnInTable in INSERT VALUES scalar subquery",
			sql:      "INSERT INTO db1.o VALUES ((SELECT hasColumnInTable('default','x','v')))",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		{name: "hasColumnInTable in CREATE TABLE column DEFAULT scalar subquery",
			sql:      "CREATE TABLE db1.n (a UInt8 DEFAULT (SELECT hasColumnInTable('default','x','v'))) ENGINE = Memory",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `hasColumnInTable target "default.x" does not resolve`},
		// Keep green: a genuine SELECT-body-rewrite root — INSERT … SELECT,
		// CREATE TABLE … AS SELECT, CREATE VIEW … AS SELECT, and an
		// IN-subquery nested inside a top-level SELECT — still resolves and
		// rewrites hasColumnInTable exactly like a bare SELECT does (Task 7
		// fix round 3 ruling: these roots must stay green after the breakage
		// 2 fix narrows InSelectBody elevation).
		{name: "hasColumnInTable in INSERT ... SELECT body stays rewritten",
			sql:      "INSERT INTO db1.o SELECT hasColumnInTable('db1','j','v') FROM db1.p",
			wantCode: pb.RewriteCode_Success,
			wantSQL:  `INSERT INTO phys."db1.o" SELECT hasColumnInTable('phys', 'db1.j', 'v') FROM phys."db1.p" "db1.p"`,
			wantAcc:  []string{"db1.o", "db1.p", "db1.j"}},
		{name: "hasColumnInTable in CREATE TABLE ... AS SELECT body stays rewritten",
			sql:      "CREATE TABLE db1.n ENGINE = Memory AS SELECT hasColumnInTable('db1','j','v') FROM db1.p",
			wantCode: pb.RewriteCode_Success,
			wantSQL:  `CREATE TABLE phys."db1.n" ENGINE=Memory AS (SELECT hasColumnInTable('phys', 'db1.j', 'v') FROM phys."db1.p" "db1.p")`,
			wantAcc:  []string{"db1.n", "db1.p", "db1.j"}},
		{name: "hasColumnInTable in CREATE VIEW ... AS SELECT body stays rewritten",
			sql:      "CREATE VIEW db1.v AS SELECT hasColumnInTable('db1','j','v') FROM db1.p",
			wantCode: pb.RewriteCode_Success,
			wantSQL:  `CREATE VIEW phys."db1.v" AS SELECT hasColumnInTable('phys', 'db1.j', 'v') FROM phys."db1.p" "db1.p"`,
			wantAcc:  []string{"db1.v", "db1.p", "db1.j"}},
		{name: "hasColumnInTable in IN-subquery nested in a top-level SELECT stays rewritten",
			sql:      "SELECT * FROM db1.o WHERE x IN (SELECT hasColumnInTable('db1','j','v') FROM db1.p)",
			wantCode: pb.RewriteCode_Success,
			wantSQL:  `SELECT * FROM phys."db1.o" "db1.o" WHERE x IN (SELECT hasColumnInTable('phys', 'db1.j', 'v') FROM phys."db1.p" "db1.p")`,
			wantAcc:  []string{"db1.o", "db1.p", "db1.j"}},
	})
}

// TestTableRef_StringLookupOrderIsDeterministic pins Task 7 fix round 1
// finding 1 (measured over 200 runs of the original map-range-order
// implementation: up to 24 distinct accessed orders for one statement, and
// up to 3 distinct first-refusal messages for another), its fix round 2
// refinement (document order, not just a stable order — the reviewer's own
// probes below), and fix round 3 minor 3 (dropping the span-based secondary
// sort entirely, since it wasn't a strict weak ordering: the comparator
// returned false whenever either call lacked a span, so incomparability
// wasn't transitive). collectStringLookupOccurrences (references.go) now
// orders calls purely by the structural walk order — clause rank
// (stringLookupClauseOrder), then sorted key, then array index — which
// already reproduces every probe below, including the reviewer's own
// WHERE/GROUP BY/HAVING/ORDER BY/LIMIT and JOIN ON cases. Run with
// `-count=20` to prove stability, not just `-count=1`.
func TestTableRef_StringLookupOrderIsDeterministic(t *testing.T) {
	runTablerefCases(t, []tablerefCase{
		{name: "four hasColumnInTable calls, stable accessed order",
			sql:      "SELECT hasColumnInTable('db1','a','v'), hasColumnInTable('db1','b','v'), hasColumnInTable('db1','c','v'), hasColumnInTable('db1','d','v')",
			wantCode: pb.RewriteCode_Success,
			wantSQL: "SELECT hasColumnInTable('phys', 'db1.a', 'v'), hasColumnInTable('phys', 'db1.b', 'v'), " +
				"hasColumnInTable('phys', 'db1.c', 'v'), hasColumnInTable('phys', 'db1.d', 'v')",
			wantAcc: []string{"db1.a", "db1.b", "db1.c", "db1.d"}},
		{name: "mixed joinGet/dictHas/dictGet, stable first refusal",
			sql:      "SELECT joinGet('db1.j','v',1), dictHas('db1.d',1), dictGet('db1.g','v',1)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `joinGet target "db1.j" does not resolve`},
		// Fix round 2 finding 1's own probe: WHERE's hasColumnInTable ('a')
		// must be reported before ORDER BY's ('z') — the FROM target ('o')
		// always leads, recorded earlier in rewriteSelectCore before any
		// string-lookup handling runs at all.
		{name: "WHERE hasColumnInTable precedes ORDER BY hasColumnInTable",
			sql:      "SELECT x FROM db1.o WHERE hasColumnInTable('db1','a','v') ORDER BY hasColumnInTable('db1','z','v')",
			wantCode: pb.RewriteCode_Success, wantAcc: []string{"db1.o", "db1.a", "db1.z"}},
		// Fix round 2 finding 1's second probe: the CTE body's dictHas is the
		// first call in document order (the "with" clause precedes the main
		// SELECT's own "expressions"), so it must be the one named in the
		// refusal message, not the main body's dictGet.
		{name: "CTE body's dictHas precedes main body's dictGet",
			sql:      "WITH (SELECT dictHas('db1.a',1)) AS c SELECT dictGet('db1.j','v',1), c",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `dictHas target "db1.a" does not resolve`},
		// Task 7 fix round 3 minor 3's own probes: with the span-based
		// secondary sort dropped entirely, the walk order (clause rank, then
		// sorted key, then array index) alone must still reproduce the
		// clause-grammar-ordered accessed list.
		{name: "WHERE/GROUP BY/HAVING/ORDER BY/LIMIT accessed order",
			sql: "SELECT hasColumnInTable('db1','a','v') FROM db1.o WHERE hasColumnInTable('db1','b','v') " +
				"GROUP BY 1 HAVING hasColumnInTable('db1','c','v') ORDER BY hasColumnInTable('db1','z','v') LIMIT 10",
			wantCode: pb.RewriteCode_Success,
			wantAcc:  []string{"db1.o", "db1.a", "db1.b", "db1.c", "db1.z"}},
		{name: "JOIN ON accessed order",
			sql:      "SELECT * FROM db1.o JOIN db1.p ON hasColumnInTable('db1','a','v') AND hasColumnInTable('db1','b','v')",
			wantCode: pb.RewriteCode_Success,
			wantAcc:  []string{"db1.o", "db1.p", "db1.a", "db1.b"}},
	})
}

// TestTableRef_DescribeFunctionTargetIsClassified pins Task 7 fix round 1
// finding 3: ParseObjectTarget's tokenizer-based name-run extraction stopped
// at the name token and silently dropped a following "(...)", so
// `DESCRIBE TABLE mysql('h', 'default', 'u', 'x', 'y')` reported
// Table="mysql" and RewriteDescribe passed the whole statement through
// unchanged as Success — neither the T5 table-function allowlist nor the
// protected-database check ever saw it. ParseObjectTargetFunctionCall now
// detects this shape and PreflightTableReferences classifies it
// unconditionally (both SI states — "merge" stays refused even under
// contract V2, since no other handler defers this check for an active SI
// surface the way SELECT/write dispatch does).
func TestTableRef_DescribeFunctionTargetIsClassified(t *testing.T) {
	runTablerefCases(t, []tablerefCase{
		{name: "DESCRIBE mysql refused", sql: "DESCRIBE TABLE mysql('h','default','u','x','y')",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table function mysql is not accepted"},
		{name: "DESCRIBE remote refused", sql: "DESCRIBE TABLE remote('localhost','default','secret')",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table function remote is not accepted"},
		{name: "DESCRIBE unknown function refused", sql: "DESCRIBE TABLE frobnicate('x')",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table function frobnicate is not recognised"},
		{name: "DESCRIBE merge refused even under V2", sql: "DESCRIBE TABLE merge('hg_safe','db1__t')", si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table function merge is not accepted"},
		{name: "DESCRIBE numbers stays allowed", sql: "DESCRIBE TABLE numbers(10)", wantCode: pb.RewriteCode_Success},
		// Task 7 fix round 2 finding 3: a leading comment defeated the verb
		// gate, which matched EXISTS/SHOW/DESCRIBE against the raw SQL TEXT
		// (comment included) before ever tokenizing. The gate now reads the
		// tokenizer's first token directly — comments are stripped from the
		// token stream entirely (attached to the FOLLOWING token as
		// metadata, never emitted as their own token), so it is comment-
		// agnostic by construction.
		{name: "leading block comment does not defeat DESCRIBE merge, even under V2",
			sql: "/* c */ DESCRIBE TABLE merge('hg_safe','db1__t')", si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table function merge is not accepted"},
		{name: "leading block comment does not defeat DESCRIBE mysql",
			sql:      "/* c */ DESCRIBE TABLE mysql('h','default','u','x','y')",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table function mysql is not accepted"},
		{name: "leading line comment does not defeat DESCRIBE remote",
			sql:      "-- c\nDESCRIBE TABLE remote('localhost','default','secret')",
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table function remote is not accepted"},
	})
}

// TestTableRef_UnmodelledClassesAreRefusedWithoutSI pins spec 2026-09-26 T7
// (Task 7): with the storage-integrity surface inactive, a statement class no
// handler models is now refused (it used to pass through as Success) — except
// a session SET, which names no table and which clients send routinely.
func TestTableRef_UnmodelledClassesAreRefusedWithoutSI(t *testing.T) {
	runTablerefCases(t, []tablerefCase{
		{name: "system", sql: "SYSTEM RELOAD CONFIG", wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"},
		{name: "explain", sql: "EXPLAIN SELECT * FROM db1.o", wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"},
		{name: "check", sql: "CHECK TABLE db1.o", wantCode: pb.RewriteCode_UnsupportedStatement},
		{name: "create user", sql: "CREATE USER u1", wantCode: pb.RewriteCode_UnsupportedStatement},
		{name: "create function", sql: "CREATE FUNCTION f AS x -> x + 1", wantCode: pb.RewriteCode_UnsupportedStatement},
		{name: "set passes when inactive", sql: "SET max_threads = 1", wantCode: pb.RewriteCode_Success, wantMsg: "success", wantSQL: "SET max_threads = 1"},
		{name: "set refused under V2", sql: "SET max_threads = 1", si: true, wantCode: pb.RewriteCode_UnsupportedStatement},
		{name: "select 1", sql: "SELECT 1", wantCode: pb.RewriteCode_Success},
		// Task 7 fix round 1 finding 4: the SET carve-out must admit only a
		// settings assignment, not every statement starting with the SET
		// keyword. SET ROLE / SET DEFAULT ROLE are access-management
		// statements this repo does not model.
		{name: "set role refused", sql: "SET ROLE r1", wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"},
		{name: "set default role refused", sql: "SET DEFAULT ROLE r1 TO u1", wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"},
		{name: "set tab-separated setting still passes", sql: "SET\tmax_threads = 1", wantCode: pb.RewriteCode_Success, wantSQL: "SET\tmax_threads = 1"},
	})
}

// TestTableRef_InOperandsDecodeOnce pins spec 2026-09-26 R1 (final review
// Critical 1 and 6): every IN consumer decodes its operand through one shared
// decoder, which unwraps parentheses to any depth, treats a parameter operand
// as a T2 hit, and decodes an identifier operand structurally, so a bare
// quoted `db2.x` is an unqualified table named "db2.x" in the session's
// logical database, exactly like FROM.
func TestTableRef_InOperandsDecodeOnce(t *testing.T) {
	const paramMsg = "query parameters are not supported in a database or table position"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, db string }{
			{"SELECT * FROM db1.o WHERE a IN ((phys.`db2.x`))", "phys"},
			{"SELECT * FROM db1.o WHERE a IN (((phys.`db2.x`)))", "phys"},
			{"SELECT * FROM db1.o WHERE a IN ((hg_safe.db1__t))", "hg_safe"},
			{"SELECT * FROM db1.o WHERE in(a, ((phys.`db2.x`)))", "phys"},
		} {
			code, msg := pb.RewriteCode_InvalidRewriteRequest, "protected database "+c.db+" is not addressable"
			if si && c.db == "hg_safe" {
				// A parenthesized IN operand is an ordinary SI-handler position:
				// the SI message keeps precedence (spec 2026-09-26 T3).
				code, msg = pb.RewriteCode_RewriteError, "storage-integrity physical table hg_safe.db1__t is not directly addressable"
			}
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: code, wantMsg: msg, wantSQL: c.sql})
		}
		for _, sql := range []string{
			"SELECT * FROM db1.o WHERE a IN ({p:Identifier})",
			"SELECT * FROM db1.o WHERE a IN (({p:Identifier}))",
			"SELECT * FROM db1.o WHERE in(a, (({p:Identifier})))",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: paramMsg, wantSQL: sql})
		}
		const scan = `SELECT * FROM phys."db1.o" "db1.o" WHERE `
		for _, c := range []struct{ sql, want string }{
			{"SELECT * FROM db1.o WHERE a IN `db2.x`", scan + `a IN phys."db1.db2.x"`},
			{"SELECT * FROM db1.o WHERE a IN (`db2.x`)", scan + `a IN (phys."db1.db2.x")`},
			{"SELECT * FROM db1.o WHERE a IN ((`db2.x`))", scan + `a IN (phys."db1.db2.x")`},
			{"SELECT * FROM db1.o WHERE in(a, `db2.x`)", scan + `in(a, phys."db1.db2.x")`},
			{"SELECT * FROM db1.o WHERE a NOT IN `db2.x`", scan + `a NOT IN phys."db1.db2.x"`},
			{"SELECT * FROM db1.o WHERE a GLOBAL IN `db2.x`", scan + `a GLOBAL IN phys."db1.db2.x"`},
			{"SELECT * FROM db1.o WHERE nullIn(a, `db2.x`)", scan + `nullIn(a, phys."db1.db2.x")`},
			{"SELECT * FROM db1.o WHERE nullIn(a, ((`db2.x`)))", scan + `nullIn(a, phys."db1.db2.x")`},
			{"SELECT * FROM db1.o WHERE (a, b) IN `db2.x`", scan + `(a, b) IN phys."db1.db2.x"`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: c.want, wantAcc: []string{"db1.o", ".db2.x"}})
		}
		// A literal operand stays a value at any paren depth.
		cases = append(cases, tablerefCase{name: "literal_nested_paren", si: si,
			sql:      "SELECT * FROM db1.o WHERE a IN ((1))",
			wantCode: pb.RewriteCode_Success, wantSQL: scan + "a IN ((1))", wantAcc: []string{"db1.o"}})
	}
	// Under the SI surface a nested-paren Active table is still the derived read.
	cases = append(cases, tablerefCase{name: "active_nested_paren", si: true,
		sql:      "SELECT * FROM db1.o WHERE a IN ((db1.t))",
		wantCode: pb.RewriteCode_Success,
		wantSQL:  `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t)`})
	runTablerefCases(t, cases)
}

// TestTableRef_GoErrorsFailClosed pins spec 2026-09-26 R6 (final review
// Critical 7): in dynamic mode a handler, walk or generate error — including a
// polyglot recursion-limit error — is a coded UnsupportedStatement rejection
// in both SI states, never a Go error a caller could treat as fail-open.
func TestTableRef_GoErrorsFailClosed(t *testing.T) {
	nestedIN := "SELECT a FROM db1.o"
	for i := 0; i < 60; i++ {
		nestedIN = "SELECT a FROM db1.o WHERE a IN (" + nestedIN + ")"
	}
	union := strings.TrimSuffix(strings.Repeat("SELECT a FROM db1.o UNION ALL ", 500), " UNION ALL ")
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		msg := "statement is not supported"
		if si {
			msg = StorageIntegrityUnmodelledMessage
		}
		for _, c := range []struct{ name, sql string }{
			{"lambda_body_subquery", "SELECT arrayMap(x -> x IN (SELECT a FROM phys.`db2.x`), [1]) FROM db1.o"},
			{"any_subquery", "SELECT * FROM db1.o WHERE a = ANY (SELECT a FROM phys.`db2.x`)"},
			{"values_scalar_subquery", "SELECT * FROM values('a UInt64', (SELECT max(a) FROM phys.`db2.x`))"},
			{"nested_in_60", nestedIN},
			{"union_500", union},
		} {
			cases = append(cases, tablerefCase{name: c.name, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg, wantSQL: c.sql})
		}
	}
	runTablerefCases(t, cases)
}

// TestTableRef_MutationAndColumnExpressionReads pins spec 2026-09-26 R2
// (final review Critical 2, Important 3 and 6): structured UPDATE / DELETE
// predicates and assignments, INSERT VALUES, column / constraint / storage-
// property expressions and structured ALTER actions are visited by the walker
// (so T2 / T3 apply), and opaque ALTER text is scanned by the tokenizer. A
// read the rewriter cannot rewrite and report there is refused with
// "statement is not supported" — protected and parameter messages win.
func TestTableRef_MutationAndColumnExpressionReads(t *testing.T) {
	const (
		unsupported = "statement is not supported"
		paramMsg    = "query parameters are not supported in a database or table position"
	)
	protected := func(db string) string { return "protected database " + db + " is not addressable" }
	type row struct {
		sql     string
		code    pb.RewriteCode
		msgOff  string // SI surface inactive
		msgOn   string // SI surface active; "" = same as msgOff
		codeOn  pb.RewriteCode
		setCode bool
	}
	rows := []row{
		{sql: "ALTER TABLE db1.o UPDATE b = (SELECT count() FROM remote('127.0.0.1','phys','db2.x')) WHERE 1", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o UPDATE b = (SELECT max(a) FROM {p:Identifier}) WHERE 1", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: paramMsg},
		{sql: "ALTER TABLE db1.o UPDATE b = (SELECT count() FROM merge(currentDatabase(),'^db2')) WHERE 1", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o UPDATE b = (SELECT max(a) FROM `db2.x`) WHERE 1", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o DELETE WHERE a IN phys.`db2.x`", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("phys")},
		{sql: "ALTER TABLE db1.o DELETE WHERE a IN (SELECT a FROM phys.`db2.x`)", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("phys")},
		{sql: "ALTER TABLE db1.o DELETE WHERE a IN {p:Identifier}", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: paramMsg},
		{sql: "ALTER TABLE db1.o DELETE WHERE a IN (SELECT a FROM merge(currentDatabase(),'^db2'))", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o DELETE WHERE a IN `db2.x`", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o DELETE WHERE a IN hg_safe.db1__t", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("hg_safe"),
			setCode: true, codeOn: pb.RewriteCode_UnsupportedStatement, msgOn: "storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		{sql: "ALTER TABLE db1.o UPDATE b = (SELECT count() FROM hg_promote.x) WHERE 1", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("hg_promote"),
			setCode: true, codeOn: pb.RewriteCode_UnsupportedStatement, msgOn: "storage-integrity physical table hg_promote.x is not directly addressable"},
		{sql: "ALTER TABLE db1.o UPDATE b = 1 WHERE 1, DELETE WHERE a IN phys.`db2.x`", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("phys")},
		{sql: "ALTER TABLE db1.o UPDATE b = 1 WHERE 1, DELETE WHERE a IN `db2.x`", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o UPDATE b = 1 WHERE in(a, db1.p)", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o UPDATE b = 1 WHERE a IN ((db1.p))", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "DELETE FROM db1.o WHERE a IN (SELECT a FROM `db2.x`)", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "DELETE FROM db1.o WHERE a IN {p:Identifier}", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: paramMsg},
		{sql: "UPDATE db1.o SET b = 1 WHERE a IN `db2.x`", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "UPDATE db1.o SET b = (SELECT max(a) FROM db1.p) WHERE 1", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "INSERT INTO db1.o VALUES ((SELECT max(a) FROM db1.p))", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (a UInt64 DEFAULT (SELECT max(a) FROM hg_promote.x)) ENGINE = Memory", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("hg_promote"),
			setCode: true, codeOn: pb.RewriteCode_UnsupportedStatement, msgOn: "storage-integrity physical table hg_promote.x is not directly addressable"},
		{sql: "CREATE TABLE db1.n (a UInt64 DEFAULT (SELECT max(a) FROM hg_safe.db1__t)) ENGINE = Memory", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("hg_safe"),
			setCode: true, codeOn: pb.RewriteCode_UnsupportedStatement, msgOn: "storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		{sql: "CREATE TABLE db1.n (a UInt64 DEFAULT (SELECT max(a) FROM phys.`db2.x`)) ENGINE = Memory", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("phys")},
		{sql: "CREATE TABLE db1.n (a UInt64 DEFAULT (SELECT max(a) FROM {p:Identifier})) ENGINE = Memory", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: paramMsg},
		{sql: "CREATE TABLE db1.n (a UInt64 MATERIALIZED a IN `db2.x`) ENGINE = Memory", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (a UInt64, CONSTRAINT c CHECK a IN (SELECT 1 FROM db1.z)) ENGINE = MergeTree ORDER BY a", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "CREATE TABLE db1.n (a UInt64) ENGINE = MergeTree PARTITION BY a IN phys.x ORDER BY a", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("phys")},
		{sql: "CREATE TABLE db1.n (a UInt64) ENGINE = MergeTree ORDER BY a TTL d + 1 WHERE a IN `db2.x`", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		// Residual round 3: a reserved qualifier in opaque ALTER text gets the
		// SI physical-name message while the SI surface is active.
		{sql: "ALTER TABLE db1.o MODIFY COLUMN b UInt64 DEFAULT (SELECT max(a) FROM hg_unsafe.db1__t)", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("hg_unsafe"),
			setCode: true, codeOn: pb.RewriteCode_UnsupportedStatement, msgOn: "storage-integrity physical table hg_unsafe.db1__t is not directly addressable"},
		{sql: "ALTER TABLE db1.o MODIFY COLUMN b UInt64 DEFAULT (SELECT max(a) FROM `db2.x`)", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o MODIFY TTL d + INTERVAL 1 DAY DELETE WHERE a IN `db2.x`", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o ADD COLUMN c UInt8 DEFAULT a IN `db2.x`", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o ADD INDEX i a IN db1.q TYPE minmax", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o ADD PROJECTION p (SELECT a FROM db1.x)", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o REPLACE PARTITION tuple() FROM phys.`db2.x`", code: pb.RewriteCode_InvalidRewriteRequest, msgOff: protected("phys")},
		{sql: "ALTER TABLE db1.o REPLACE PARTITION tuple() FROM db1.p", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o ATTACH PARTITION tuple() FROM db1.p", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o MOVE PARTITION tuple() TO TABLE db1.p", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o FETCH PARTITION tuple() FROM '/clickhouse/tables/x'", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
		{sql: "ALTER TABLE db1.o FETCH PART 'p' FROM '/clickhouse/tables/x'", code: pb.RewriteCode_UnsupportedStatement, msgOff: unsupported},
	}
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, r := range rows {
			code, msg := r.code, r.msgOff
			if si && r.setCode {
				code, msg = r.codeOn, r.msgOn
			}
			cases = append(cases, tablerefCase{name: r.sql, sql: r.sql, si: si, wantCode: code, wantMsg: msg, wantSQL: r.sql})
		}
		// Reads-free mutations and column expressions keep working.
		for _, c := range []struct{ sql, want string }{
			{"ALTER TABLE db1.o UPDATE b = 1 WHERE a IN (1, 2)", "ALTER TABLE phys.`db1.o` UPDATE b = 1 WHERE a IN (1, 2)"},
			{"ALTER TABLE db1.o UPDATE b = 1 WHERE a IN tuple(1, 2)", "ALTER TABLE phys.`db1.o` UPDATE b = 1 WHERE a IN tuple(1, 2)"},
			{"ALTER TABLE db1.o DELETE WHERE a = 1", `ALTER TABLE phys."db1.o" DELETE WHERE a=1`},
			{"DELETE FROM db1.o WHERE a IN (1, 2)", `DELETE FROM phys."db1.o" WHERE a IN (1, 2)`},
			{"UPDATE db1.o SET b = 1 WHERE a = 1", `UPDATE phys."db1.o" SET b = 1 WHERE a = 1`},
			{"ALTER TABLE db1.o ADD PROJECTION p (SELECT a ORDER BY b)", `ALTER TABLE phys."db1.o" ADD PROJECTION p(SELECT a ORDER BY b)`},
			{"ALTER TABLE db1.o ADD COLUMN c UInt8 DEFAULT a + 1", `ALTER TABLE phys."db1.o" ADD COLUMN c UInt8 DEFAULT a + 1`},
			{"ALTER TABLE db1.o MODIFY TTL d + INTERVAL 1 DAY", `ALTER TABLE phys."db1.o" MODIFY TTL d + INTERVAL 1 DAY`},
			{"ALTER TABLE db1.o FREEZE WITH NAME 'x'", `ALTER TABLE phys."db1.o" FREEZE WITH NAME 'x'`},
			{"ALTER TABLE db1.o MOVE PARTITION tuple() TO DISK 'd'", `ALTER TABLE phys."db1.o" MOVE PARTITION tuple() TO DISK 'd'`},
			{"ALTER TABLE db1.o ATTACH PART 'x'", `ALTER TABLE phys."db1.o" ATTACH PART 'x'`},
			{"CREATE TABLE db1.n (a UInt64 DEFAULT 1, b UInt64 MATERIALIZED a * 2) ENGINE = MergeTree PARTITION BY a % 2 ORDER BY a",
				`CREATE TABLE phys."db1.n" (a UInt64 DEFAULT 1, b UInt64 MATERIALIZED a * 2) ENGINE=MergeTree PARTITION BY a % 2 ORDER BY a`},
		} {
			cases = append(cases, tablerefCase{name: "allowed/" + c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
	}
	runTablerefCases(t, cases)
}

// TestTableRef_ModifyQueryIsRefused pins spec 2026-09-26 R3 (final review
// Critical 3). Option taken: refuse. ALTER TABLE … MODIFY QUERY is a Raw ALTER
// action whose body polyglot does not structure; rather than splice a
// rewritten materialized-view body back into that text, dynamic mode refuses
// every MODIFY QUERY with "statement is not supported" after T2 (parameter)
// and T3 (protected database) have run on its text.
func TestTableRef_ModifyQueryIsRefused(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		hgSafe := tablerefCase{name: "hg_safe", sql: "ALTER TABLE db1.mv MODIFY QUERY SELECT * FROM hg_safe.db1__t", si: si,
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database hg_safe is not addressable"}
		if si {
			// Residual round 3: the reserved qualifier in the opaque MODIFY
			// QUERY text gets the SI physical-name message.
			hgSafe.wantCode, hgSafe.wantMsg = pb.RewriteCode_UnsupportedStatement, "storage-integrity physical table hg_safe.db1__t is not directly addressable"
		}
		cases = append(cases, hgSafe,
			tablerefCase{name: "phys", sql: "ALTER TABLE db1.mv MODIFY QUERY SELECT * FROM phys.`db2.x`", si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable"},
			tablerefCase{name: "parameter", sql: "ALTER TABLE db1.mv MODIFY QUERY SELECT * FROM {p:Identifier}", si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "query parameters are not supported in a database or table position"},
			tablerefCase{name: "dotted_unqualified", sql: "ALTER TABLE db1.mv MODIFY QUERY SELECT * FROM `db2.x`", si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"},
			tablerefCase{name: "own_table", sql: "ALTER TABLE db1.mv MODIFY QUERY SELECT a FROM db1.o", si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"},
		)
	}
	runTablerefCases(t, cases)
}

// TestTableRef_MaterializedViewStorageAllowlists pins spec 2026-09-26 R4
// (final review Critical 4): a materialized view's own ENGINE / SETTINGS go
// through the same T5 allowlist and engine-argument protected-database walk as
// a CREATE TABLE's, in both SI states.
func TestTableRef_MaterializedViewStorageAllowlists(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, msg string }{
			{"CREATE MATERIALIZED VIEW db1.mv ENGINE = Merge(currentDatabase(),'^db2') AS SELECT * FROM db1.o", "table engine Merge is not accepted"},
			{"CREATE MATERIALIZED VIEW db1.mv ENGINE = Buffer(currentDatabase(), `db2.x`, 1, 10, 100, 10000, 1000000, 10000000, 100000000) AS SELECT * FROM db1.o", "table engine Buffer is not accepted"},
			{"CREATE MATERIALIZED VIEW db1.mv ENGINE = Distributed('c', currentDatabase(), `db2.x`) AS SELECT * FROM db1.o", "table engine Distributed is not accepted"},
			{"CREATE MATERIALIZED VIEW db1.mv ENGINE = Kafka('h:9092', 't', 'g', 'JSONEachRow') AS SELECT * FROM db1.o", "table engine Kafka is not accepted"},
			{"CREATE MATERIALIZED VIEW db1.mv ENGINE = MergeTree ORDER BY a SETTINGS disk = disk(type=local, path='/') AS SELECT * FROM db1.o", "table setting disk is not accepted"},
			{"CREATE MATERIALIZED VIEW db1.mv ENGINE = MergeTree ORDER BY a SETTINGS storage_policy = 'p' AS SELECT * FROM db1.o", "table setting storage_policy is not accepted"},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: c.msg, wantSQL: c.sql})
		}
		cases = append(cases,
			tablerefCase{name: "merge_protected_literal", si: si,
				sql:      "CREATE MATERIALIZED VIEW db1.mv ENGINE = Merge('phys', '^db2') AS SELECT * FROM db1.o",
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable"},
			tablerefCase{name: "mergetree_allowed", si: si,
				sql:      "CREATE MATERIALIZED VIEW db1.mv ENGINE = MergeTree ORDER BY a AS SELECT * FROM db1.o",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `CREATE MATERIALIZED VIEW phys."db1.mv" ENGINE=MergeTree ORDER BY a AS SELECT * FROM phys."db1.o" "db1.o"`},
		)
	}
	regexp := tablerefCase{name: "merge_regexp_inactive", sql: "CREATE MATERIALIZED VIEW db1.mv ENGINE = Merge(REGEXP('hg_.*'),'.*') AS SELECT * FROM db1.o",
		wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table engine Merge is not accepted"}
	cases = append(cases, regexp)
	runTablerefCases(t, cases)
}

// TestTableRef_SQLBearingSettings pins spec 2026-09-26 R5 (final review
// Critical 5, Minor SET subquery): a SQL-bearing setting is refused with the
// table-setting message wherever it appears, and a setting value must be a
// numeric literal, a string literal or a bare identifier / keyword.
func TestTableRef_SQLBearingSettings(t *testing.T) {
	const unsupported = "statement is not supported"
	setting := func(name string) string { return "table setting " + name + " is not accepted" }
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, msg string }{
			{"SELECT * FROM db1.o SETTINGS additional_table_filters = {'db1.o': 'a IN (SELECT a FROM phys.`db2.x`)'}", setting("additional_table_filters")},
			{"SELECT * FROM db1.o SETTINGS additional_result_filter = 'a = (SELECT max(a) FROM `db2.x`)'", setting("additional_result_filter")},
			{"SELECT * FROM db1.o SETTINGS parallel_replicas_custom_key = 'a'", setting("parallel_replicas_custom_key")},
			{"SELECT * FROM db1.o SETTINGS max_threads = 1, additional_result_filter = 'a > 1'", setting("additional_result_filter")},
			{"SELECT a FROM db1.o UNION ALL SELECT a FROM db1.p SETTINGS additional_result_filter = 'a > 1'", setting("additional_result_filter")},
			{"INSERT INTO db1.o SELECT * FROM db1.p SETTINGS additional_table_filters = {'db1.p': 'a > 1'}", setting("additional_table_filters")},
			{"INSERT INTO db1.o SETTINGS additional_result_filter = 'a > 1' VALUES (1)", setting("additional_result_filter")},
			{"ALTER TABLE db1.o UPDATE b = 1 WHERE 1 SETTINGS additional_table_filters = '{}'", setting("additional_table_filters")},
			{"SELECT * FROM db1.o SETTINGS max_threads = (SELECT 1)", unsupported},
			{"SELECT * FROM db1.o SETTINGS max_threads = [1]", unsupported},
		} {
			msg := c.msg
			if si && strings.HasPrefix(c.sql, "ALTER") {
				// The SI mutation-surface probe cannot model a SETTINGS
				// tail and refuses first (SI precedence).
				msg = unsupported
			}
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg, wantSQL: c.sql})
		}
		for _, c := range []struct{ sql, want string }{
			{"SELECT * FROM db1.o SETTINGS max_threads = 1", `SELECT * FROM phys."db1.o" "db1.o" SETTINGS max_threads = 1`},
			{"SELECT * FROM db1.o SETTINGS max_threads = 1, join_algorithm = 'hash', load_balancing = random",
				`SELECT * FROM phys."db1.o" "db1.o" SETTINGS max_threads = 1, join_algorithm = 'hash', load_balancing = random`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
	}
	// The session SET carve-out exists only while the SI surface is inactive.
	for _, c := range []struct{ sql, msg string }{
		{"SET additional_table_filters = {'db1.o': 'a IN (SELECT a FROM phys.`db2.x`)'}", setting("additional_table_filters")},
		{"SET max_threads = 1, additional_result_filter = 'a > 1'", setting("additional_result_filter")},
		{"SET max_threads = (SELECT count() FROM phys.`db2.x`)", unsupported},
		{"SET max_threads = [1]", unsupported},
		{"SET max_threads = 1 + 1", unsupported},
		{"SET max_threads = {p:UInt64}", unsupported},
	} {
		cases = append(cases, tablerefCase{name: c.sql, sql: c.sql,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: c.msg, wantSQL: c.sql})
	}
	for _, sql := range []string{
		"SET max_threads = 1",
		"SET max_threads = 1, max_block_size = 'a', load_balancing = random",
		"SET max_threads = -1, enable_optimize_predicate_expression = true, x = NULL",
	} {
		cases = append(cases, tablerefCase{name: sql, sql: sql, wantCode: pb.RewriteCode_Success, wantSQL: sql})
		cases = append(cases, tablerefCase{name: "si/" + sql, sql: sql, si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: StorageIntegrityUnmodelledMessage})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_DescribeAndShowTargets pins spec 2026-09-26 R7 (final review
// Critical 8, Minor empty EXISTS / SHOW CREATE): DESCRIBE (SELECT …) is
// refused, an unqualified dotted quoted target of DESCRIBE / EXISTS / SHOW
// CREATE / SHOW COLUMNS resolves exactly like FROM, and EXISTS / SHOW CREATE
// with no target are refused.
func TestTableRef_DescribeAndShowTargets(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		// DESCRIBE (SELECT …) is an unmodelled shape refused before T3; with
		// the SI surface active it answers with the SI catch-all, upgraded to
		// the SI object its body names (spec 2026-09-26 §5 precedence).
		for _, c := range []struct{ sql, siMsg string }{
			{"DESCRIBE (SELECT * FROM phys.`db2.x`)", StorageIntegrityUnmodelledMessage},
			{"DESCRIBE (SELECT * FROM hg_safe.db1__t)", "storage-integrity physical table hg_safe.db1__t is not directly addressable"},
			{"DESCRIBE TABLE (SELECT * FROM phys.`db2.x`)", StorageIntegrityUnmodelledMessage},
			{"DESC (SELECT a FROM db1.o)", StorageIntegrityUnmodelledMessage},
			{"EXISTS", unsupported},
			{"SHOW CREATE", unsupported},
		} {
			msg := unsupported
			if si {
				msg = c.siMsg
			}
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg, wantSQL: c.sql})
		}
		for _, c := range []struct{ sql, want string }{
			{"DESCRIBE TABLE `db2.x`", "DESCRIBE TABLE phys.`db1.db2.x`"},
			{"DESCRIBE `db2.x`", "DESCRIBE TABLE phys.`db1.db2.x`"},
			{"EXISTS TABLE `db2.x`", "EXISTS TABLE phys.`db1.db2.x`"},
			{"SHOW CREATE TABLE `db2.x`", "SHOW CREATE TABLE phys.`db1.db2.x`"},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: c.want, wantAcc: []string{".db2.x"}})
		}
	}
	// SHOW COLUMNS under an active SI surface is refused by the SI handler
	// first (db1 owns an SI table), so its FROM-like resolution is pinned
	// with the surface inactive.
	cases = append(cases,
		tablerefCase{name: "show_columns_dotted", sql: "SHOW COLUMNS FROM `db2.x`",
			wantCode: pb.RewriteCode_Success, wantSQL: "SHOW COLUMNS FROM phys.`db1.db2.x`", wantAcc: []string{".db2.x"}},
		tablerefCase{name: "show_columns_like", sql: "SHOW COLUMNS FROM o LIKE 'a%'",
			wantCode: pb.RewriteCode_Success, wantSQL: "SHOW COLUMNS FROM phys.`db1.o` LIKE 'a%'", wantAcc: []string{".o"}},
		tablerefCase{name: "show_columns_si", sql: "SHOW COLUMNS FROM `db2.x`", si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "storage-integrity logical database db1 is not directly addressable"},
	)
	runTablerefCases(t, cases)
}

// TestTableRef_CommandPrecedence pins spec 2026-09-26 R8 (final review
// Important 1): for a command node the T2 scan of the whole text runs first,
// then the unmodelled-class refusal, then the protected-database check. With
// the SI surface active every unmodelled class answers with the SI catch-all.
func TestTableRef_CommandPrecedence(t *testing.T) {
	const paramMsg = "query parameters are not supported in a database or table position"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		unmodelled := "statement is not supported"
		if si {
			unmodelled = StorageIntegrityUnmodelledMessage
		}
		for _, sql := range []string{
			"EXPLAIN SELECT * FROM {p:Identifier}",
			"KILL QUERY WHERE query_id = {p:Identifier}",
			"SYSTEM SYNC REPLICA {p:Identifier}",
			"DETACH TABLE {p:Identifier}",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: paramMsg, wantSQL: sql})
		}
		for _, c := range []struct {
			sql string
			acc []string
		}{
			{"DETACH TABLE phys.x", []string{"phys.x"}},
			{"OPTIMIZE TABLE phys.x", []string{"phys.x"}},
			{"ATTACH TABLE db1.x", []string{"db1.x"}},
			{"KILL QUERY WHERE query_id = 'x'", nil},
			{"EXPLAIN SELECT * FROM db1.o", nil},
			{"CHECK TABLE phys.x", nil},
		} {
			acc := c.acc
			if acc == nil {
				acc = []string{}
			}
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unmodelled, wantSQL: c.sql, wantAcc: acc})
		}
		// A modelled command class still gets the protected-database check.
		cases = append(cases, tablerefCase{name: "rename_protected", si: si,
			sql:      "RENAME TABLE phys.x TO db1.z",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable"})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_BareIdentifierInOperand pins spec 2026-09-26 R9: a bare
// identifier IN operand is rewritten as a table in the session's logical
// database (fail-safe; see AGENTS.md for the deviation from ClickHouse's
// column-first resolution).
func TestTableRef_BareIdentifierInOperand(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases,
			tablerefCase{name: "paren", sql: "SELECT * FROM db1.o WHERE a IN (b)", si: si, wantCode: pb.RewriteCode_Success,
				wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN (phys."db1.b")`, wantAcc: []string{".b", "db1.o"}},
			tablerefCase{name: "bare", sql: "SELECT * FROM db1.o WHERE a IN b", si: si, wantCode: pb.RewriteCode_Success,
				wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.b"`, wantAcc: []string{".b", "db1.o"}},
			tablerefCase{name: "tuple_is_a_value", sql: "SELECT * FROM db1.o WHERE a IN tuple(b)", si: si, wantCode: pb.RewriteCode_Success,
				wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN tuple(b)`, wantAcc: []string{"db1.o"}},
		)
	}
	runTablerefCases(t, cases)
}

// TestTableRef_LookupNonLiteralTarget pins spec 2026-09-26 R10 (final review
// Important 5): a non-literal first argument reports target "" on every path,
// the structured walk and the command-text / Raw-action scan alike.
func TestTableRef_LookupNonLiteralTarget(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, msg string }{
			{"SELECT dictGet(currentDatabase() || '.d', 'v', 1)", `dictGet target "" does not resolve`},
			{"ALTER TABLE db1.o UPDATE b = dictGet(currentDatabase() || '.d', 'v', 1) WHERE 1", `dictGet target "" does not resolve`},
			{"ALTER TABLE db1.o DELETE WHERE dictGet(concat('db1', '.d'), 'v', a) = 1", `dictGet target "" does not resolve`},
			{"ALTER TABLE db1.o UPDATE b = joinGet(db1.j, 'v', 1) WHERE 1", `joinGet target "db1.j" does not resolve`},
			{"ALTER TABLE db1.o UPDATE b = hasColumnInTable(currentDatabase(), 'j', 'v') WHERE 1", `hasColumnInTable target "" does not resolve`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: c.msg, wantSQL: c.sql})
		}
	}
	runTablerefCases(t, cases)
}

// TestTableRef_MinorRulings pins spec 2026-09-26 R12: engine names are
// case-sensitive (a list name in the wrong case is not recognised), and the
// statements the generator does not round-trip — a refreshable view and
// INSERT … FROM INFILE — are refused in dynamic mode.
func TestTableRef_MinorRulings(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, msg string }{
			{"CREATE TABLE db1.n (a UInt64) ENGINE = memory", "table engine memory is not recognised"},
			{"CREATE TABLE db1.n (a UInt64) ENGINE = mergetree ORDER BY a", "table engine mergetree is not recognised"},
			{"CREATE MATERIALIZED VIEW db1.mv REFRESH EVERY 1 HOUR TO db1.p AS SELECT * FROM db1.o", unsupported},
			{"CREATE MATERIALIZED VIEW db1.mv REFRESH EVERY 1 HOUR ENGINE = Memory AS SELECT * FROM db1.o", unsupported},
			{"INSERT INTO db1.o FROM INFILE 'x.csv' FORMAT CSV", unsupported},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: c.msg, wantSQL: c.sql})
		}
		cases = append(cases, tablerefCase{name: "memory_allowed", si: si,
			sql:      "CREATE TABLE db1.n (a UInt64) ENGINE = Memory",
			wantCode: pb.RewriteCode_Success, wantSQL: `CREATE TABLE phys."db1.n" (a UInt64) ENGINE=Memory`})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_HousekeepingRows pins the parked-review rows the final review
// asked for: nested-paren INSERT … SELECT / CTAS bodies, a statement with
// several lookups, mixed ALTER MODIFY actions, the SI-active Merge engine, and
// one accessed entry per table named by several hasColumnInTable calls.
func TestTableRef_HousekeepingRows(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases,
			tablerefCase{name: "insert_nested_paren_source", sql: "INSERT INTO db1.o SELECT * FROM ((SELECT * FROM db1.p))", si: si,
				wantCode: pb.RewriteCode_Success, wantAcc: []string{"db1.o", "db1.p"},
				wantSQL: `INSERT INTO phys."db1.o" SELECT * FROM ((SELECT * FROM phys."db1.p" "db1.p"))`},
			tablerefCase{name: "ctas_nested_paren_body", sql: "CREATE TABLE db1.n ENGINE = Memory AS ((SELECT * FROM db1.p))", si: si,
				wantCode: pb.RewriteCode_Success, wantAcc: []string{"db1.n", "db1.p"},
				wantSQL: `CREATE TABLE phys."db1.n" ENGINE=Memory AS (((SELECT * FROM phys."db1.p" "db1.p")))`},
			tablerefCase{name: "multi_lookup_first_wins", si: si,
				sql:      "SELECT dictGet('db1.d', 'v', 1), joinGet('db1.j', 'v', 1)",
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `dictGet target "db1.d" does not resolve`},
			tablerefCase{name: "multi_lookup_hascolumnintable_then_joinget", si: si,
				sql:      "SELECT hasColumnInTable('db1', 'o', 'a'), joinGet('db1.j', 'v', 1)",
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `joinGet target "db1.j" does not resolve`},
			tablerefCase{name: "hascolumnintable_accessed_once", si: si,
				sql:      "SELECT hasColumnInTable('db1', 'o', 'a'), hasColumnInTable('db1', 'o', 'b') FROM db1.o",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `SELECT hasColumnInTable('phys', 'db1.o', 'a'), hasColumnInTable('phys', 'db1.o', 'b') FROM phys."db1.o" "db1.o"`,
				wantAcc:  []string{"db1.o"}},
			tablerefCase{name: "alter_modify_setting_disk_mixed", si: si,
				sql:      "ALTER TABLE db1.o MODIFY COLUMN b UInt8 DEFAULT 2, MODIFY SETTING disk = 'd'",
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "table setting disk is not accepted"},
			tablerefCase{name: "alter_modify_column_lookup", si: si,
				sql:      "ALTER TABLE db1.o MODIFY COLUMN b UInt8 DEFAULT dictGet('db1.d', 'v', a)",
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `dictGet target "db1.d" does not resolve`},
		)
	}
	cases = append(cases, tablerefCase{name: "merge_engine_si_active", si: true,
		sql:      "CREATE TABLE db1.n (a UInt64) ENGINE = Merge('db1', '^o')",
		wantCode: pb.RewriteCode_UnsupportedStatement})
	runTablerefCases(t, cases)
}

// TestTableRef_ResidualQuotedCallableIn pins residual 1 of the final
// re-review: a quoted callable-IN name in opaque ALTER text is the same
// function as its unquoted spelling, so the R2 scanner refuses it.
func TestTableRef_ResidualQuotedCallableIn(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"ALTER TABLE db1.o DELETE WHERE `in`((a, 0), `db2.x`)",
			"ALTER TABLE db1.o DELETE WHERE \"notIn\"(a, `db2.x`)",
			"ALTER TABLE db1.o DELETE WHERE `globalNotIn`(a, `db2.x`)",
			"ALTER TABLE db1.o DELETE WHERE `nullIn`(a, (`db2.x`))",
			"ALTER TABLE db1.o DELETE WHERE `\\Nin`(a, db1.p)",
			"ALTER TABLE db1.o UPDATE b = 1 WHERE `in`(a, `db2.x`)",
			"ALTER TABLE db1.o MODIFY TTL d + INTERVAL 1 DAY DELETE WHERE `in`(a, `db2.x`)",
			"ALTER TABLE db1.o MODIFY COLUMN b UInt64 DEFAULT `in`(a, `db2.x`)",
			"ALTER TABLE db1.o MODIFY COLUMN b UInt64 MATERIALIZED \"notIn\"(a, `db2.x`)",
			"ALTER TABLE db1.o ADD PROJECTION pr (SELECT a), MODIFY COLUMN b UInt64 DEFAULT `in`(a, `db2.x`)",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported", wantSQL: sql})
		}
	}
	runTablerefCases(t, cases)
}

// TestTableRef_OpaqueNameDecode pins the cross-tenant-read fix (2026-09-30):
// every opaque-text and structured matcher finishes ClickHouse's quoted-name
// escape decode before it compares an IN-family or string-lookup name, so a
// spelling ClickHouse resolves to a real read (`\Nin`, `i\Nn`, `in\N`,
// `not\x49n`, `glob\x61lIn`, `\NjoinGet`) is refused exactly like its plain
// spelling on the paths that forward the text verbatim (ALTER DELETE / UPDATE,
// INSERT VALUES). Spellings ClickHouse treats as an unknown function that reads
// nothing (a wrong-case `IN` / `NOTIN`, an unknown escape `\in` / `\\in` that
// keeps its backslash) are not over-refused by the IN rule. Measured against
// ClickHouse 26.2.
func TestTableRef_OpaqueNameDecode(t *testing.T) {
	var cases []tablerefCase
	// Refused on both SI states: an escaped IN whose operand reads another table.
	inRefuse := []string{
		"ALTER TABLE db1.o DELETE WHERE `\\Nin`(a, `db2.x`)",
		"ALTER TABLE db1.o DELETE WHERE `i\\Nn`(a, `db2.x`)",
		"ALTER TABLE db1.o DELETE WHERE `in\\N`(a, `db2.x`)",
		"ALTER TABLE db1.o DELETE WHERE \"i\\Nn\"(a, `db2.x`)",
		"ALTER TABLE db1.o DELETE WHERE `not\\x49n`(a, `db2.x`)",
		"ALTER TABLE db1.o DELETE WHERE `glob\\x61lIn`(a, `db2.x`)",
		"ALTER TABLE db1.o UPDATE b = 1 WHERE `\\Nin`(a, `db2.x`)",
		"ALTER TABLE db1.o UPDATE b = 1 WHERE `in\\N`(a, `db2.x`)",
		"INSERT INTO db1.o VALUES (`\\Nin`(1, `db2.x`), 0, today())",
		"INSERT INTO db1.o VALUES (`i\\Nn`(1, `db2.x`), 0, today())",
		"INSERT INTO db1.o VALUES (`in\\N`(1, `db2.x`), 0, today())",
	}
	// Refused on both SI states: an escaped string-lookup whose literal target
	// resolves through no caller database (InvalidRewriteRequest, like `joinGet`).
	lookupRefuse := []string{
		"ALTER TABLE db1.o DELETE WHERE `\\NjoinGet`('db2.x', 'v', 1) = 1",
		"INSERT INTO db1.o VALUES (`\\NjoinGet`('db2.x', 'v', 1), 0, today())",
	}
	for _, si := range []bool{false, true} {
		for _, sql := range inRefuse {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported", wantSQL: sql})
		}
		for _, sql := range lookupRefuse {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: `does not resolve through the caller's databases`, wantSQL: sql})
		}
		// An escaped SQL-bearing / dialect setting name resolves to the real
		// setting and is refused wherever it appears.
		q := "SELECT * FROM db1.o SETTINGS `\\Ndialect` = 1"
		cases = append(cases, tablerefCase{name: q, sql: q, si: si,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantSQL: q})
		// An escaped carrier name resolves to the real table function; phys is a
		// protected database, so it is refused (like the plain `remote`).
		r := "SELECT count() FROM `\\Nremote`('127.0.0.1', 'phys', 'db2.x')"
		cases = append(cases, tablerefCase{name: r, sql: r, si: si,
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable", wantSQL: r})
	}
	// Unknown functions ClickHouse reads nothing from: not over-refused by the
	// IN rule. A wrong-case or unknown-escape spelling in a SELECT is forwarded
	// (the operand is rewritten into the caller's own namespace, never another
	// tenant's), and in an INSERT VALUES row it is a plain unknown call.
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"SELECT * FROM db1.o WHERE `IN`(a, `db2.x`)",
			"SELECT * FROM db1.o WHERE `NOTIN`(a, `db2.x`)",
			"INSERT INTO db1.o VALUES (`IN`(1, db1.p), 0, today())",
		} {
			cases = append(cases, tablerefCase{name: "ok/" + sql, sql: sql, si: si, wantCode: pb.RewriteCode_Success})
		}
		// An unknown-escape spelling in a SELECT is refused by the
		// mid-statement drop gate (#50), as on main: Polyglot regenerates the
		// function name unquoted (\in(a, …)), which is not a token.
		sql := "SELECT * FROM db1.o WHERE `\\in`(a, `db2.x`)"
		cases = append(cases, tablerefCase{name: "gate/" + sql, sql: sql, si: si,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"})
	}
	// On the verbatim-forwarding ALTER DELETE path, an unknown spelling is a
	// Success (no read) with storage integrity inactive; with it active the SI
	// surface refuses the mutation for its own reasons, so this pin is SI-off.
	for _, sql := range []string{
		"ALTER TABLE db1.o DELETE WHERE `IN`(a, db1.p)",
		"ALTER TABLE db1.o DELETE WHERE `NOTIN`(a, `db2.x`)",
		"ALTER TABLE db1.o DELETE WHERE `\\in`(a, `db2.x`)",
		"ALTER TABLE db1.o DELETE WHERE `\\\\in`(a, `db2.x`)",
	} {
		cases = append(cases, tablerefCase{name: "ok-alter/" + sql, sql: sql, wantCode: pb.RewriteCode_Success})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_ResidualShowColumnsBody pins residual 2: the WHERE / LIKE
// body of a SHOW COLUMNS / INDEX family statement is scanned; a subquery, a
// call, a table-operand IN or a quoted dotted name refuses the statement,
// after the parameter and protected-database checks.
func TestTableRef_ResidualShowColumnsBody(t *testing.T) {
	const unsupported = "statement is not supported"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct {
			sql  string
			code pb.RewriteCode
			msg  string
		}{
			{"SHOW COLUMNS FROM o WHERE (SELECT count() FROM phys.`db2.x`) = 2", pb.RewriteCode_InvalidRewriteRequest, "protected database phys is not addressable"},
			{"SHOW COLUMNS FROM o WHERE (SELECT count() FROM `db2.x`) = 2", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"SHOW COLUMNS FROM o WHERE (SELECT count() FROM remote('127.0.0.1','phys','db2.x')) = 2", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"SHOW COLUMNS FROM o WHERE (SELECT count() FROM merge('phys','^db2')) = 2", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"SHOW INDEX FROM o WHERE (SELECT count() FROM `db2.x`) = 2", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"SHOW EXTENDED FULL COLUMNS FROM o WHERE name IN `db2.x`", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"SHOW COLUMNS FROM o LIKE (SELECT max(name) FROM `db2.x`)", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"SHOW COLUMNS FROM o WHERE name = `db2.x`", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"SHOW KEYS FROM o WHERE lower(name) = 'a'", pb.RewriteCode_UnsupportedStatement, unsupported},
			{"SHOW COLUMNS FROM o WHERE name = {p:Identifier}", pb.RewriteCode_InvalidRewriteRequest, "query parameters are not supported"},
		} {
			code, msg := c.code, c.msg
			if si && code == pb.RewriteCode_UnsupportedStatement {
				// db1 owns an SI table: the SI handler refuses the target first.
				msg = "storage-integrity logical database db1 is not directly addressable"
			}
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: code, wantMsg: msg, wantSQL: c.sql})
		}
		hg := tablerefCase{name: "hg_safe_body", sql: "SHOW COLUMNS FROM o WHERE (SELECT count() FROM hg_safe.db1__t) = 2", si: si,
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database hg_safe is not addressable"}
		if si {
			hg.wantCode, hg.wantMsg = pb.RewriteCode_UnsupportedStatement, ""
		}
		cases = append(cases, hg)
	}
	cases = append(cases, tablerefCase{name: "plain_like_body", sql: "SHOW COLUMNS FROM o WHERE name LIKE 'a%'",
		wantCode: pb.RewriteCode_Success, wantSQL: "SHOW COLUMNS FROM phys.`db1.o` WHERE name LIKE 'a%'"})
	runTablerefCases(t, cases)
}

// TestTableRef_ResidualDialectSettings pins residual 3: a dialect switch
// changes how ClickHouse parses later SQL, so every dialect setting is refused
// in the SET carve-out and in query-level SETTINGS.
func TestTableRef_ResidualDialectSettings(t *testing.T) {
	names := []string{"dialect", "polyglot_dialect", "allow_experimental_polyglot_dialect",
		"allow_experimental_prql_dialect", "allow_experimental_kusto_dialect", "Dialect"}
	var cases []tablerefCase
	for _, n := range names {
		msg := "table setting " + n + " is not accepted"
		set := "SET " + n + " = 1"
		cases = append(cases, tablerefCase{name: set, sql: set, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg, wantSQL: set},
			tablerefCase{name: "si/" + set, sql: set, si: true, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: StorageIntegrityUnmodelledMessage})
		for _, si := range []bool{false, true} {
			q := "SELECT * FROM db1.o SETTINGS " + n + " = 1"
			cases = append(cases, tablerefCase{name: q, sql: q, si: si, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg, wantSQL: q})
		}
	}
	quoted := "SET `polyglot_dialect` = 'sqlite'"
	cases = append(cases, tablerefCase{name: quoted, sql: quoted, wantCode: pb.RewriteCode_UnsupportedStatement,
		wantMsg: "table setting polyglot_dialect is not accepted"})
	runTablerefCases(t, cases)
}

// TestTableRef_ResidualMinors pins the residual-round minors: REFRESH is
// matched only as the refresh clause, and under V2 an unmodelled command class
// answers with the SI catch-all before its SETTINGS clause is examined.
func TestTableRef_ResidualMinors(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases,
			tablerefCase{name: "mv_named_refresh", si: si, sql: "CREATE MATERIALIZED VIEW db1.refresh TO db1.t2 AS SELECT * FROM db1.o",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `CREATE MATERIALIZED VIEW phys."db1.refresh" TO phys."db1.t2" AS SELECT * FROM phys."db1.o" "db1.o"`},
			tablerefCase{name: "mv_to_refresh", si: si, sql: "CREATE MATERIALIZED VIEW db1.mv TO db1.refresh AS SELECT * FROM db1.o",
				wantCode: pb.RewriteCode_Success,
				wantSQL:  `CREATE MATERIALIZED VIEW phys."db1.mv" TO phys."db1.refresh" AS SELECT * FROM phys."db1.o" "db1.o"`},
			tablerefCase{name: "mv_refresh_after", si: si, sql: "CREATE MATERIALIZED VIEW db1.mv REFRESH AFTER 1 HOUR TO db1.p AS SELECT * FROM db1.o",
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"},
		)
	}
	cases = append(cases, tablerefCase{name: "explain_settings_v2", si: true,
		sql:      "EXPLAIN SELECT * FROM db1.o SETTINGS additional_result_filter = 'a > 1'",
		wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: StorageIntegrityUnmodelledMessage})
	runTablerefCases(t, cases)
}

// TestTableRef_Residual2ShowBodies pins residual round 2, open 1: every SHOW
// family forwarded verbatim has its trailing clauses (WHERE / LIKE / ILIKE /
// LIMIT / …) scanned, so a subquery or call there refuses the statement.
func TestTableRef_Residual2ShowBodies(t *testing.T) {
	const unsupported = "statement is not supported"
	const siDB1 = "storage-integrity logical database db1 is not directly addressable"
	sub := "(SELECT count() FROM `db2.x`)"
	var cases []tablerefCase
	for _, c := range []struct {
		sql     string
		siDBOne bool // with the SI surface active, db1 (an SI owner) is refused first
	}{
		{"SHOW COLUMNS FROM o LIMIT " + sub, true},
		{"SHOW FIELDS FROM o LIMIT " + sub, true},
		{"SHOW EXTENDED FULL COLUMNS IN o LIMIT " + sub, true},
		{"SHOW COLUMNS FROM o LIMIT 1 + " + sub, true},
		{"SHOW COLUMNS FROM o FROM db1 LIMIT " + sub, true},
		{"SHOW COLUMNS FROM db3.o LIMIT " + sub, false},
		{"SHOW COLUMNS FROM o LIMIT throwIf(1)", true},
		{"SHOW DICTIONARIES WHERE " + sub + " = 2", true},
		{"SHOW DICTIONARIES FROM default WHERE " + sub + " = 2", false},
		{"SHOW DICTIONARIES LIMIT " + sub, true},
		{"SHOW FULL DICTIONARIES LIMIT " + sub, true},
		{"SHOW CLUSTERS LIMIT " + sub, false},
		{"SHOW CLUSTERS LIKE 'x' LIMIT " + sub, false},
		{"SHOW MERGES LIMIT " + sub, false},
		{"SHOW MERGES LIKE 'x' LIMIT " + sub, false},
	} {
		cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: unsupported, wantSQL: c.sql})
		msg := unsupported
		if c.siDBOne {
			msg = siDB1
		}
		cases = append(cases, tablerefCase{name: "si/" + c.sql, sql: c.sql, si: true, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg, wantSQL: c.sql})
	}
	cases = append(cases,
		tablerefCase{name: "columns_limit_5", sql: "SHOW COLUMNS FROM o LIMIT 5", wantCode: pb.RewriteCode_Success, wantSQL: "SHOW COLUMNS FROM phys.`db1.o` LIMIT 5"},
		tablerefCase{name: "dictionaries_like", sql: "SHOW DICTIONARIES LIKE 'a%'", wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "system table system.dictionaries is not accessible"},
		tablerefCase{name: "clusters_like_limit", sql: "SHOW CLUSTERS LIKE 'x' LIMIT 3", wantCode: pb.RewriteCode_Success},
		tablerefCase{name: "si/clusters_like_limit", sql: "SHOW CLUSTERS LIKE 'x' LIMIT 3", si: true, wantCode: pb.RewriteCode_Success},
		tablerefCase{name: "keyword_if_call_allowed", sql: "SHOW COLUMNS FROM o WHERE if(1, 1, 0) = 1", wantCode: pb.RewriteCode_Success},
	)
	runTablerefCases(t, cases)
}

// TestTableRef_Residual2InsertHeaderSettings pins residual round 2, open 2:
// the SETTINGS list polyglot leaves as opaque query text after an INSERT
// column list is examined like any other, a token backstop refuses a
// denylisted setting name after any SETTINGS keyword, and a read in that
// opaque query text is refused.
func TestTableRef_Residual2InsertHeaderSettings(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, msg string }{
			{"INSERT INTO db1.o (a) SETTINGS additional_table_filters = {'system.one': '(SELECT throwIf(count() = 2) FROM `db2.x`) = 0'} SELECT dummy FROM system.one", "table setting additional_table_filters is not accepted"},
			{"INSERT INTO db1.o (a) SETTINGS additional_result_filter = 'a > 1' SELECT 1", "table setting additional_result_filter is not accepted"},
			{"INSERT INTO db1.o (a) SETTINGS additional_result_filter = 'a > 1' VALUES (1)", "table setting additional_result_filter is not accepted"},
			{"INSERT INTO db1.o (a) SETTINGS dialect = 'prql' SELECT 1", "table setting dialect is not accepted"},
			{"INSERT INTO db1.o (a) SETTINGS polyglot_dialect = 'sqlite' SELECT 1", "table setting polyglot_dialect is not accepted"},
			{"INSERT INTO db1.o (a) SETTINGS `POLYGLOT_DIALECT` = 'sqlite' SELECT 1", "table setting POLYGLOT_DIALECT is not accepted"},
			{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT a FROM `db2.x`", "statement is not supported"},
			{"INSERT INTO db1.o (a) SETTINGS max_threads = (SELECT 1) SELECT 1", "statement is not supported"},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: c.msg, wantSQL: c.sql})
		}
		cases = append(cases,
			tablerefCase{name: "protected_in_opaque_query", sql: "INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT a FROM phys.x", si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable"},
			tablerefCase{name: "plain_select", sql: "INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT 1", si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" (a) SETTINGS max_threads = 1 SELECT 1`},
			tablerefCase{name: "plain_values", sql: "INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES (1)", si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.o" (a) SETTINGS max_threads = 1 VALUES (1)`},
		)
	}
	runTablerefCases(t, cases)
}

// TestTableRef_Residual2RefreshPositions pins residual round 2, open 3:
// REFRESH in a CREATE VIEW header is allowed only as the view's own name or
// the TO target's name; anywhere else the statement is refused.
func TestTableRef_Residual2RefreshPositions(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"CREATE MATERIALIZED VIEW db1.mv REFRESH TO db1.t2 AS SELECT * FROM db1.o",
			"CREATE MATERIALIZED VIEW db1.mv REFRESH EVERY 1 HOUR TO db1.p AS SELECT * FROM db1.o",
			"CREATE MATERIALIZED VIEW db1.mv REFRESH AFTER 1 HOUR TO db1.p AS SELECT * FROM db1.o",
			"CREATE MATERIALIZED VIEW db1.`refresh` REFRESH EVERY 1 HOUR TO db1.p AS SELECT * FROM db1.o",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported", wantSQL: sql})
		}
		for _, c := range []struct{ sql, want string }{
			{"CREATE MATERIALIZED VIEW db1.refresh TO db1.t2 AS SELECT * FROM db1.o", `CREATE MATERIALIZED VIEW phys."db1.refresh" TO phys."db1.t2" AS SELECT * FROM phys."db1.o" "db1.o"`},
			{"CREATE MATERIALIZED VIEW db1.mv TO db1.refresh AS SELECT * FROM db1.o", `CREATE MATERIALIZED VIEW phys."db1.mv" TO phys."db1.refresh" AS SELECT * FROM phys."db1.o" "db1.o"`},
			{"CREATE VIEW db1.refresh AS SELECT 1", `CREATE VIEW phys."db1.refresh" AS SELECT 1`},
			{"CREATE VIEW db1.v AS SELECT refresh FROM db1.o", `CREATE VIEW phys."db1.v" AS SELECT refresh FROM phys."db1.o" "db1.o"`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
	}
	runTablerefCases(t, cases)
}

// TestTableRef_Residual3KeywordCallableIn pins residual round 3, open A: a
// keyword-tokenized callable `in(` / `IN(` is the callable IN form; its SECOND
// argument is the table operand.
func TestTableRef_Residual3KeywordCallableIn(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range []string{
			"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT in(42, `db2.x`)",
			"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT IN(42, `db2.x`)",
			"INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES (in(42, `db2.x`))",
			"INSERT INTO db1.o (a) SETTINGS max_threads = 1 FORMAT Values (in(42, `db2.x`))",
			"ALTER TABLE db1.o UPDATE a = in(42, `db2.x`) WHERE 1",
			"ALTER TABLE db1.o DELETE WHERE in(42, `db2.x`)",
			"ALTER TABLE db1.o DELETE WHERE In /* c */ (42, ((`db2.x`)))",
			"ALTER TABLE db1.o MODIFY TTL d + INTERVAL 1 DAY DELETE WHERE in(42, `db2.x`)",
			"ALTER TABLE db1.o MODIFY COLUMN b UInt64 DEFAULT in(42, `db2.x`)",
			"SHOW DICTIONARIES FROM default WHERE in(7, `db2.x`)",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported", wantSQL: sql})
		}
		cases = append(cases, tablerefCase{name: "literal_list_passes", si: si,
			sql:      "ALTER TABLE db1.o DELETE WHERE in(42, (1, 2))",
			wantCode: pb.RewriteCode_Success})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_Residual3OpaqueReservedQualifiers pins residual round 3, open
// B: a reserved hg_* qualifier found by an opaque-text scan is refused with the
// SI physical-name message when the SI surface is active, and with the
// protected-database message when it is not.
func TestTableRef_Residual3OpaqueReservedQualifiers(t *testing.T) {
	var cases []tablerefCase
	for _, c := range []struct{ sql, db, table string }{
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT in(7, hg_safe.db1__t) + 100", "hg_safe", "db1__t"},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT in(7, hg_unsafe.db1__t) + 100", "hg_unsafe", "db1__t"},
		{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT in(7, hg_promote.x)", "hg_promote", "x"},
		{"SHOW DICTIONARIES FROM default WHERE in(7, hg_safe.db1__t)", "hg_safe", "db1__t"},
		{"SHOW FULL DICTIONARIES FROM default WHERE in(7, hg_unsafe.db1__t)", "hg_unsafe", "db1__t"},
		{"ALTER TABLE db1.o MODIFY COLUMN b UInt64 DEFAULT in(7, hg_promote.x)", "hg_promote", "x"},
	} {
		cases = append(cases,
			tablerefCase{name: c.sql, sql: c.sql, wantCode: pb.RewriteCode_InvalidRewriteRequest,
				wantMsg: "protected database " + c.db + " is not addressable", wantSQL: c.sql},
			tablerefCase{name: "si/" + c.sql, sql: c.sql, si: true, wantCode: pb.RewriteCode_UnsupportedStatement,
				wantMsg: "storage-integrity physical table " + c.db + "." + c.table + " is not directly addressable", wantSQL: c.sql})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_Residual3SettingsBackstopNeedsAssignment pins the residual
// round 3 minor: the backstop needs `name =` after SETTINGS, so column aliases
// named settings / dialect pass.
func TestTableRef_Residual3SettingsBackstopNeedsAssignment(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases, tablerefCase{name: "aliases", si: si, sql: "SELECT 1 AS settings, 2 AS dialect",
			wantCode: pb.RewriteCode_Success, wantSQL: "SELECT 1 AS settings, 2 AS dialect"})
	}
	runTablerefCases(t, cases)
}

// residual4DefectSQL is every ClickHouse-26.8-confirmed cross-tenant read that
// residual round 4 closes: a keyword-lexed left operand (or the END of a CASE)
// before IN, and the callable in( after a NOT / GLOBAL prefix.
var residual4DefectSQL = []string{
	"ALTER TABLE db1.o UPDATE a = 1 WHERE date IN (`db2.x`)",
	"ALTER TABLE db1.o UPDATE a = 1, b = 2 WHERE key IN (`db2.x`)",
	"ALTER TABLE db1.o UPDATE a = date IN (`db2.x`) WHERE 1",
	"ALTER TABLE db1.o DELETE WHERE date IN (`db2.x`)",
	"ALTER TABLE db1.o DELETE WHERE key IN (`db2.x`)",
	"ALTER TABLE db1.o DELETE WHERE timestamp IN (`db2.x`)",
	"ALTER TABLE db1.o DELETE WHERE CASE WHEN a THEN 1 END IN (`db2.x`)",
	"ALTER TABLE db1.o DELETE WHERE 1, UPDATE a = 1 WHERE key IN (`db2.x`)",
	"ALTER TABLE db1.o MODIFY TTL d + INTERVAL 1 DAY DELETE WHERE date IN (`db2.x`)",
	"ALTER TABLE db1.o MODIFY COLUMN b UInt8 DEFAULT date IN (`db2.x`)",
	"ALTER TABLE db1.o ADD PROJECTION p (SELECT a WHERE date IN (`db2.x`))",
	"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT CASE WHEN 1 THEN 42 END IN (`db2.x`)",
	"INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES (CASE WHEN 1 THEN 42 END IN (`db2.x`))",
	"ALTER TABLE db1.o DELETE WHERE not in(42, `db2.x`)",
	"ALTER TABLE db1.o DELETE WHERE NOT in(42, `db2.x`)",
	"ALTER TABLE db1.o DELETE WHERE (NOT in(42, `db2.x`))",
	"ALTER TABLE db1.o DELETE WHERE GLOBAL in(42, `db2.x`)",
	"ALTER TABLE db1.o UPDATE a = 1 WHERE not in(1, `db2.x`)",
	"ALTER TABLE db1.o MODIFY TTL d + INTERVAL 1 DAY DELETE WHERE not in(1, `db2.x`)",
	"ALTER TABLE db1.o MODIFY COLUMN b UInt8 MATERIALIZED not in(1, `db2.x`)",
	"ALTER TABLE db1.o ADD PROJECTION p (SELECT a WHERE not in(1, `db2.x`))",
	"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT not in(1, `db2.x`)",
	"INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES (not in(1, `db2.x`))",
	"INSERT INTO db1.o (a) SETTINGS max_threads = 1 FORMAT Values (not in(1, `db2.x`))",
}

// TestTableRef_Residual4OperandRegion pins residual round 4: every IN-family
// occurrence in opaque text is refused unless its operand region (the bracket
// group after it, or the single next token) is literal-only, whatever token
// precedes it. The accepted cost (a column inside the region) is refused; a
// literal region passes.
func TestTableRef_Residual4OperandRegion(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range residual4DefectSQL {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported", wantSQL: sql})
		}
		for _, sql := range []string{
			"ALTER TABLE db1.o DELETE WHERE in(a, (1, 2))",
			"ALTER TABLE db1.o DELETE WHERE a IN (1, b)",
			"ALTER TABLE db1.o DELETE WHERE a IN (b, 1)",
		} {
			cases = append(cases, tablerefCase{name: "cost/" + sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported", wantSQL: sql})
		}
		for _, c := range []struct{ sql, want string }{
			{"ALTER TABLE db1.o DELETE WHERE a IN (1, 2)", `ALTER TABLE phys."db1.o" DELETE WHERE a IN(1, 2)`},
			{"ALTER TABLE db1.o DELETE WHERE in(42, [1, 2])", ""},
			{"ALTER TABLE db1.o DELETE WHERE a NOT IN ('x', 'y')", ""},
			{"ALTER TABLE db1.o DELETE WHERE date IN (1, 2)", ""},
			{"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT not in(1, (1, 2))", ""},
		} {
			cases = append(cases, tablerefCase{name: "literal/" + c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: c.want, wantAcc: []string{"db1.o"}})
		}
	}
	runTablerefCases(t, cases)
}

// residual5SignSQL is every ClickHouse-confirmed read through a unary sign
// before an IN operand (ClickHouse drops a unary plus): residual round 5,
// open 1.
var residual5SignSQL = []string{
	"ALTER TABLE db1.o DELETE WHERE a IN +`db2.x`",
	"ALTER TABLE db1.o DELETE WHERE a IN + (`db2.x`)",
	"ALTER TABLE db1.o DELETE WHERE a IN + + `db2.x`",
	"ALTER TABLE db1.o DELETE WHERE a NOT IN +`db2.x`",
	"ALTER TABLE db1.o DELETE WHERE a IN -`db2.x`",
	"ALTER TABLE db1.o UPDATE b = 1 WHERE key IN +`db2.x`",
	"ALTER TABLE db1.o UPDATE b = a IN +`db2.x` WHERE 1",
	"ALTER TABLE db1.o MODIFY TTL d + INTERVAL 1 DAY DELETE WHERE a IN + \"db2.y\"",
	"ALTER TABLE db1.o MODIFY COLUMN b UInt8 DEFAULT a IN +`db2.x`",
	"ALTER TABLE db1.o MODIFY COLUMN b UInt8 MATERIALIZED a IN +`db2.x`",
	"ALTER TABLE db1.o ADD PROJECTION p (SELECT a WHERE a IN +`db2.x`)",
	"INSERT INTO db1.o (a) SETTINGS max_threads = 1 SELECT 3 IN +`db2.y`",
	"INSERT INTO db1.o (a) SETTINGS max_threads = 1 VALUES (3 IN +`db2.y`)",
	"INSERT INTO db1.o (a) SETTINGS max_threads = 1 FORMAT Values (3 IN +`db2.y`)",
	"ALTER TABLE db1.o DELETE WHERE a IN [1, 2], UPDATE b = 1 WHERE key IN (`db2.x`)",
}

// TestTableRef_Residual5SignsAndSplitBrackets pins residual round 5: a sign
// is never an IN operand region by itself (open 1), and a Raw ALTER action
// polyglot split at a comma inside a bracket group is scanned whole again
// (open 2) while every action keeps its own refusal.
func TestTableRef_Residual5SignsAndSplitBrackets(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, sql := range residual5SignSQL {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported", wantSQL: sql})
		}
		cases = append(cases,
			tablerefCase{name: "phys/" + "+phys", si: si, sql: "ALTER TABLE db1.o DELETE WHERE a IN +phys.`db2.x`",
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable"},
			tablerefCase{name: "param/+{p:Identifier}", si: si, sql: "ALTER TABLE db1.o DELETE WHERE a IN +{p:Identifier}",
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "query parameters are not supported"})
		literals := []string{"ALTER TABLE db1.o DELETE WHERE a IN -1", "ALTER TABLE db1.o DELETE WHERE a IN (-1, -2)"}
		if !si {
			// With the SI surface active a unary plus is refused anywhere in a
			// Raw DELETE (`a = +1` too) — pre-existing, not this rule.
			literals = append(literals, "ALTER TABLE db1.o DELETE WHERE a IN +1", "ALTER TABLE db1.o DELETE WHERE a IN (+1, -2)")
		}
		for _, sql := range literals {
			cases = append(cases, tablerefCase{name: "literal/" + sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_Success, wantAcc: []string{"db1.o"}})
		}
		for _, list := range []string{"[1, 2]", "[1, 2, 3]"} {
			for _, sql := range []string{
				"ALTER TABLE db1.o DELETE WHERE a IN " + list,
				"ALTER TABLE db1.o MODIFY TTL d + INTERVAL 1 DAY DELETE WHERE a IN " + list,
				"ALTER TABLE db1.o MODIFY COLUMN b UInt8 DEFAULT a IN " + list,
				"ALTER TABLE db1.o MODIFY COLUMN b UInt8 MATERIALIZED a IN " + list,
				"ALTER TABLE db1.o ADD COLUMN c UInt8 DEFAULT a IN " + list,
				"ALTER TABLE db1.o ADD PROJECTION p (SELECT a WHERE a IN " + list + ")",
			} {
				cases = append(cases, tablerefCase{name: "bracket/" + sql, sql: sql, si: si,
					wantCode: pb.RewriteCode_Success, wantAcc: []string{"db1.o"}})
			}
		}
	}
	runTablerefCases(t, cases)
}

// TestTableRef_ParenthesizedInOperandUnderActiveSurface pins that dropping
// the parenthesized IN operand from the SI-handler-blind set opens nothing:
// with the SI surface active, every position that can hold `x IN ((hg_*.t))`
// is still refused, now with the SI handlers' own message (spec 2026-09-26 T3).
func TestTableRef_ParenthesizedInOperandUnderActiveSurface(t *testing.T) {
	const safe = "storage-integrity physical table hg_safe.db1__t is not directly addressable"
	var cases []tablerefCase
	for _, c := range []struct {
		sql  string
		code pb.RewriteCode
		msg  string
	}{
		{"SELECT * FROM db1.o WHERE a IN (hg_safe.db1__t)", pb.RewriteCode_RewriteError, safe},
		{"SELECT * FROM db1.o WHERE in(a, (hg_safe.db1__t))", pb.RewriteCode_RewriteError, safe},
		{"SELECT * FROM db1.o WHERE a GLOBAL IN ((hg_unsafe.db1__t))", pb.RewriteCode_RewriteError,
			"storage-integrity physical table hg_unsafe.db1__t is not directly addressable"},
		{"SELECT * FROM db1.o WHERE a NOT IN ((hg_promote.x))", pb.RewriteCode_RewriteError,
			"storage-integrity physical table hg_promote.x is not directly addressable"},
		{"ALTER TABLE db1.o DELETE WHERE a IN ((hg_safe.db1__t))", pb.RewriteCode_UnsupportedStatement, safe},
		{"ALTER TABLE db1.o UPDATE b = 1 WHERE a IN ((hg_safe.db1__t))", pb.RewriteCode_UnsupportedStatement, safe},
		{"DELETE FROM db1.o WHERE a IN ((hg_safe.db1__t))", pb.RewriteCode_UnsupportedStatement, safe},
		{"UPDATE db1.o SET b = 1 WHERE a IN ((hg_safe.db1__t))", pb.RewriteCode_UnsupportedStatement, safe},
		{"INSERT INTO db1.o SELECT * FROM db1.p WHERE a IN ((hg_safe.db1__t))", pb.RewriteCode_UnsupportedStatement, safe},
		{"CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM db1.p WHERE a IN ((hg_safe.db1__t))", pb.RewriteCode_UnsupportedStatement, safe},
		{"CREATE VIEW db1.v AS SELECT * FROM db1.p WHERE a IN ((hg_safe.db1__t))", pb.RewriteCode_UnsupportedStatement, safe},
		{"CREATE MATERIALIZED VIEW db1.mv TO db1.o AS SELECT * FROM db1.p WHERE a IN ((hg_safe.db1__t))", pb.RewriteCode_UnsupportedStatement, safe},
		{"CREATE TABLE db1.n (a UInt64, b UInt8 DEFAULT a IN ((hg_safe.db1__t))) ENGINE = Memory", pb.RewriteCode_UnsupportedStatement, safe},
		{"ALTER TABLE db1.o MODIFY COLUMN b UInt8 DEFAULT a IN ((hg_safe.db1__t))", pb.RewriteCode_UnsupportedStatement, safe},
		{"ALTER TABLE db1.o MODIFY TTL d + INTERVAL 1 DAY DELETE WHERE a IN ((hg_safe.db1__t))", pb.RewriteCode_UnsupportedStatement, safe},
	} {
		cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: true, wantCode: c.code, wantMsg: c.msg, wantSQL: c.sql})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_EngineLocalShapes pins native-engine behaviour the shared
// storage-integrity corpus deliberately does not: shapes the native engine
// does not model and refuses (spec 2026-09-26 §5: "where the engine models
// the position"), which rewriter-grpc parses into an ordinary AST.
func TestTableRef_EngineLocalShapes(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		msg := "statement is not supported"
		if si {
			msg = StorageIntegrityUnmodelledMessage
		}
		for _, sql := range []string{
			"(SELECT * FROM db1.o)",
			"((SELECT * FROM db1.o))",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg, wantSQL: sql})
		}
	}
	runTablerefCases(t, cases)
}

// TestTableRef_AliasedInOperandIsATableOperand pins that an aliased IN
// operand, `x IN (db.t AS z)`, is the same table operand as `x IN (db.t)`:
// ClickHouse 26.2 executes both as a table-valued IN that reads db.t, and the
// alias can be referenced nowhere else (UNKNOWN_IDENTIFIER). Polyglot emits
// the operand as an `alias` node, which the IN-operand decoder used to leave
// to the value path, so a protected or storage-integrity physical table was
// forwarded unchecked, unreported and unrewritten (spec 2026-09-26 T2-T4).
// Every expectation is the unaliased equivalent's answer; a refusal echoes the
// caller's aliased SQL, and a rewrite drops the alias exactly as the unaliased
// operand is rewritten.
func TestTableRef_AliasedInOperandIsATableOperand(t *testing.T) {
	const (
		protPhys   = "protected database phys is not addressable"
		protSafe   = "protected database hg_safe is not addressable"
		siSafe     = "storage-integrity physical table hg_safe.db1__t is not directly addressable"
		paramMsg   = "query parameters are not supported in a database or table position"
		plainScan  = `SELECT * FROM phys."db1.t" "db1.t" WHERE `
		siScan     = `SELECT * FROM (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t) AS "db1.t" WHERE `
		safeDerive = `(SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t)`
	)
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		scan := plainScan
		if si {
			scan = siScan
		}
		// Protected phys: refused with T3 in both SI states, like `IN (phys.x)`.
		for _, sql := range []string{
			"SELECT * FROM db1.t WHERE a IN (phys.x AS z)",
			"SELECT * FROM db1.t WHERE a IN ((phys.x AS z))",
			"SELECT * FROM db1.t WHERE a IN ((phys.x) AS z)",
			"SELECT * FROM db1.t WHERE a NOT IN (phys.x AS z)",
			"SELECT * FROM db1.t WHERE a GLOBAL IN (phys.x AS z)",
			"SELECT * FROM db1.t WHERE (a, b) IN (phys.x AS z)",
			"SELECT * FROM db1.t WHERE in(a, phys.x AS z)",
			"SELECT * FROM db1.t WHERE nullIn(a, phys.x AS z)",
			"SELECT * FROM db1.t WHERE a IN (SELECT 1 FROM db1.o WHERE b IN (phys.x AS z))",
			"SELECT * FROM (SELECT * FROM db1.t WHERE a IN (phys.x AS z))",
			"ALTER TABLE db1.t DELETE WHERE a IN (phys.x AS z)",
			"INSERT INTO db1.t SELECT * FROM db1.o WHERE a IN (phys.x AS z)",
			"SELECT * FROM db1.t WHERE a IN (phys.x AS z) SETTINGS max_threads = 1",
			// Nesting depth and interleaving: the decoder peels paren and alias
			// wrappers to any depth, so a double alias is the same operand.
			"SELECT * FROM db1.t WHERE a IN (((phys.x) AS z))",
			"SELECT * FROM db1.t WHERE a IN ((phys.x AS y) AS z)",
			// The whole callable IN family goes through the same decoder.
			"SELECT * FROM db1.t WHERE notIn(a, phys.x AS z)",
			"SELECT * FROM db1.t WHERE globalIn(a, phys.x AS z)",
			"SELECT * FROM db1.t WHERE globalNotIn(a, phys.x AS z)",
			"SELECT * FROM db1.t WHERE inIgnoreSet(a, phys.x AS z)",
			// Mutation and column-expression positions, where the operand is
			// forwarded verbatim and only this check stands between the caller
			// and the physical table.
			"DELETE FROM db1.o WHERE a IN (phys.x AS z)",
			"ALTER TABLE db1.o UPDATE b = 1 WHERE a IN (phys.x AS z)",
			"CREATE TABLE db1.n (a UInt64, b UInt8 DEFAULT a IN (phys.x AS z)) ENGINE = Memory",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: protPhys, wantSQL: sql, wantAcc: []string{"phys."}})
		}
		// hg_safe: T3 with the surface inactive, the SI handlers' message with
		// it active, like `IN (hg_safe.db1__t)`.
		for _, c := range []struct {
			sql    string
			siCode pb.RewriteCode
			siAcc  []string
		}{
			{"SELECT * FROM db1.t WHERE a IN (hg_safe.db1__t AS z)", pb.RewriteCode_RewriteError, []string{"db1.t", "hg_safe.db1__t"}},
			{"SELECT * FROM db1.t WHERE in(a, hg_safe.db1__t AS z)", pb.RewriteCode_RewriteError, []string{"db1.t", "hg_safe.db1__t"}},
			{"INSERT INTO db1.t SELECT * FROM db1.o WHERE a IN (hg_safe.db1__t AS z)", pb.RewriteCode_UnsupportedStatement, []string{"hg_safe.db1__t"}},
			{"DELETE FROM db1.o WHERE a IN (hg_safe.db1__t AS z)", pb.RewriteCode_UnsupportedStatement, []string{"hg_safe.db1__t"}},
			{"ALTER TABLE db1.o UPDATE b = 1 WHERE a IN (hg_safe.db1__t AS z)", pb.RewriteCode_UnsupportedStatement, []string{"hg_safe.db1__t"}},
			{"CREATE TABLE db1.n (a UInt64, b UInt8 DEFAULT a IN (hg_safe.db1__t AS z)) ENGINE = Memory", pb.RewriteCode_UnsupportedStatement, []string{"hg_safe.db1__t"}},
		} {
			tc := tablerefCase{name: c.sql, sql: c.sql, si: si, wantSQL: c.sql,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: protSafe, wantAcc: []string{"hg_safe."}}
			if si {
				tc.wantCode, tc.wantMsg, tc.wantAcc = c.siCode, siSafe, c.siAcc
			}
			cases = append(cases, tc)
		}
		// T2: an Identifier parameter under an alias, like `IN ({p:Identifier})`.
		cases = append(cases, tablerefCase{name: "parameter", si: si,
			sql:      "SELECT * FROM db1.t WHERE a IN ({p:Identifier} AS z)",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: paramMsg,
			wantSQL: "SELECT * FROM db1.t WHERE a IN ({p:Identifier} AS z)", wantAcc: []string{}})
		// A logical source is resolved, reported and rewritten like `IN (db1.o)`;
		// the alias is dropped with the operand it named.
		selfRead := `(phys."db1.t")`
		if si {
			selfRead = safeDerive
		}
		for _, c := range []struct {
			sql, want string
			acc       []string
		}{
			{"SELECT * FROM db1.t WHERE a IN (db1.o AS z)", scan + `a IN (phys."db1.o")`, []string{"db1.o", "db1.t"}},
			{"SELECT * FROM db1.t WHERE a IN ((db1.o) AS z)", scan + `a IN (phys."db1.o")`, []string{"db1.o", "db1.t"}},
			{"SELECT * FROM db1.t WHERE in(a, db1.o AS z)", scan + `in(a, phys."db1.o")`, []string{"db1.o", "db1.t"}},
			{"SELECT * FROM db1.t WHERE a IN (o AS z)", scan + `a IN (phys."db1.o")`, []string{"db1.t", ".o"}},
			{"SELECT * FROM db1.t WHERE a IN (db1.t AS z)", scan + `a IN ` + selfRead, []string{"db1.t"}},
			{"SELECT * FROM db1.t WHERE a GLOBAL IN (db1.t AS z)", scan + `a GLOBAL IN ` + selfRead, []string{"db1.t"}},
			{"SELECT * FROM db1.t WHERE notIn(a, db1.o AS z)", scan + `notIn(a, phys."db1.o")`, []string{"db1.o", "db1.t"}},
			{"SELECT * FROM db1.t WHERE globalIn(a, db1.o AS z)", scan + `globalIn(a, phys."db1.o")`, []string{"db1.o", "db1.t"}},
			{"SELECT * FROM db1.t WHERE globalNotIn(a, db1.o AS z)", scan + `globalNotIn(a, phys."db1.o")`, []string{"db1.o", "db1.t"}},
			{"SELECT * FROM db1.t WHERE inIgnoreSet(a, db1.o AS z)", scan + `inIgnoreSet(a, phys."db1.o")`, []string{"db1.o", "db1.t"}},
			// A quoted dotted name is one identifier: an unqualified table named
			// `db2.x` in the session's logical database, never tenant db2.
			{"SELECT * FROM db1.t WHERE a IN (`db2.x` AS z)", scan + `a IN (phys."db1.db2.x")`, []string{"db1.t", ".db2.x"}},
			// A bare identifier under an alias is read as a table, like `a IN (b)`
			// (the safe reading; see AGENTS.md "Known deviation"). An
			// alias-qualified column is an unmapped logical table that the host's
			// permission observer refuses; it is forwarded unchanged.
			{"SELECT * FROM db1.t WHERE a IN (b AS z)", scan + `a IN (phys."db1.b")`, []string{".b", "db1.t"}},
			{"SELECT * FROM db1.t WHERE a IN (b)", scan + `a IN (phys."db1.b")`, []string{".b", "db1.t"}},
			{"SELECT * FROM db1.t WHERE a IN (tt.b AS z)", scan + `a IN (tt.b AS z)`, []string{"db1.t", "tt.b"}},
			{"SELECT * FROM db1.t WHERE a IN (tt.b)", scan + `a IN (tt.b)`, []string{"db1.t", "tt.b"}},
			// A CTE name is left untouched and unreported, aliased or not.
			{"WITH o2 AS (SELECT 1) SELECT * FROM db1.t WHERE a IN (o2 AS z)", `WITH o2 AS (SELECT 1) ` + scan + `a IN (o2 AS z)`, []string{"db1.t"}},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: c.want, wantAcc: c.acc})
		}
		// Values stay values: an aliased subquery or literal is unchanged.
		for _, v := range []string{"((SELECT 1) AS z)", "(1 AS z)"} {
			sql := "SELECT * FROM db1.t WHERE a IN " + v
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: scan + "a IN " + v, wantAcc: []string{"db1.t"}})
		}
		// Tuple literals and table functions stay as the unaliased forms are
		// (ClickHouse rejects a table function here, so forwarding it reads
		// nothing); Polyglot spaces the tuple.
		for _, c := range []struct{ sql, want string }{
			{"SELECT * FROM db1.t WHERE a IN ((1,2) AS z)", scan + "a IN ((1, 2) AS z)"},
			{"SELECT * FROM db1.t WHERE (a, b) IN ((1,2) AS z)", scan + "(a, b) IN ((1, 2) AS z)"},
			{"SELECT * FROM db1.t WHERE a IN (numbers(3) AS z)", scan + "a IN (numbers(3) AS z)"},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_Success, wantSQL: c.want, wantAcc: []string{"db1.t"}})
		}
		// R2: an aliased table operand in a position no rewrite reaches is
		// refused like `IN (db1.p)` there (T7 with the surface inactive, the SI
		// IN-table message with it active), not forwarded unrewritten.
		r2Code, r2Msg, r2Acc := pb.RewriteCode_UnsupportedStatement, "statement is not supported", []string{}
		if si {
			r2Msg, r2Acc = "storage-integrity logical database db1 is not directly addressable through IN table target", []string{"db1."}
		}
		for _, sql := range []string{
			"DELETE FROM db1.o WHERE a IN (db1.p AS z)",
			"UPDATE db1.o SET b = 1 WHERE a IN (db1.p AS z)",
			"CREATE TABLE db1.n (a UInt64, b UInt8 DEFAULT a IN (db1.p AS z)) ENGINE = Memory",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: r2Code, wantMsg: r2Msg, wantSQL: sql, wantAcc: r2Acc})
		}
		// A bare identifier under an alias is a table operand, so in a position
		// no rewrite reaches it is refused (T7 in both SI states: `b` is neither
		// a protected nor an SI name, so the SI IN-table message does not apply).
		bareSQL := "DELETE FROM db1.o WHERE a IN (b AS z)"
		cases = append(cases, tablerefCase{name: bareSQL, sql: bareSQL, si: si,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported", wantSQL: bareSQL, wantAcc: []string{}})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_InnerStorageEngineForms pins spec 2026-09-26 T3 / T5 on every
// position a CREATE statement can name a storage engine, not only the one
// ENGINE clause of a CREATE TABLE or a materialized view's own storage:
//
//   - `CREATE MATERIALIZED VIEW … TO INNER [UUID '…'] ENGINE = …` (ClickHouse
//     26.2 creates `.inner_id.<uuid>` with that engine, so
//     `ENGINE = Merge('phys','^x')` reads the raw physical tables);
//   - a window view's `INNER ENGINE`;
//   - a TimeSeries table's `DATA` / `TAGS` / `METRICS [INNER UUID] ENGINE`;
//   - a refreshable view's `REFRESH … [APPEND] TO INNER UUID … ENGINE`;
//   - a second ENGINE clause (T5 used to check only the last one).
//
// A shape the engine models gets T3 then T5 exactly like a CREATE TABLE
// engine; a shape it cannot inspect (Polyglot has no `INNER UUID` /
// `INNER ENGINE` grammar and stops before the engine) is refused, never
// forwarded.
func TestTableRef_InnerStorageEngineForms(t *testing.T) {
	const uuid = "'3bd68e3e-0000-4000-8000-000000000001'"
	const t7 = "statement is not supported"
	const body = " AS SELECT 1 AS a"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		add := func(sql string, code pb.RewriteCode, msg string) {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: code, wantMsg: msg, wantSQL: sql})
		}
		// Every TO INNER UUID form: Polyglot stops at UUID, so the engine is
		// not inspectable and the statement is refused whatever its engine.
		for _, eng := range []string{
			"Merge('phys','^x')", "Merge('hg_safe','^x')", "Merge('db1','^x')",
			"Buffer(phys, x, 1, 10, 100, 10000, 1000000, 10000000, 100000000)",
			"Distributed('c', 'phys', 'x')", "Dictionary(phys.d)",
			"URL('http://127.0.0.1/x', CSV)", "MySQL('h:3306', 'db', 't', 'u', 'p')",
			"MergeTree ORDER BY a", "Memory", "MergeTree ORDER BY a SETTINGS disk = 'd'",
		} {
			add("CREATE MATERIALIZED VIEW db1.mv TO INNER UUID "+uuid+" ENGINE = "+eng+body, pb.RewriteCode_UnsupportedStatement, t7)
		}
		for _, sql := range []string{
			"CREATE MATERIALIZED VIEW db1.mv TO INNER UUID " + uuid + body,
			"create materialized view db1.mv to inner uuid " + uuid + " engine = Merge('phys','^x') as select 1 as a",
			"CREATE MATERIALIZED VIEW db1.mv UUID " + uuid + " TO INNER UUID " + uuid + " ENGINE = Merge('phys','^x')" + body,
			"CREATE MATERIALIZED VIEW IF NOT EXISTS db1.mv ON CLUSTER c TO INNER UUID " + uuid + " ENGINE = Merge('phys','^x')" + body,
			"CREATE OR REPLACE MATERIALIZED VIEW db1.mv TO INNER UUID " + uuid + " ENGINE = Merge('phys','^x')" + body,
			"CREATE MATERIALIZED VIEW db1.mv (a UInt8) TO INNER UUID " + uuid + " ENGINE = Merge('phys','^x')" + body,
			"CREATE MATERIALIZED VIEW db1.mv REFRESH EVERY 1 HOUR APPEND TO INNER UUID " + uuid + " ENGINE = Merge('phys','^x')" + body,
			"CREATE MATERIALIZED VIEW db1.mv REFRESH EVERY 1 HOUR APPEND TO INNER UUID " + uuid + " ENGINE = MergeTree ORDER BY a" + body,
			"CREATE MATERIALIZED VIEW db1.mv REFRESH EVERY 1 HOUR TO INNER UUID " + uuid + " ENGINE = MergeTree ORDER BY a" + body,
			// TimeSeries inner targets: Polyglot stops at DATA / TAGS / METRICS.
			"CREATE TABLE db1.ts ENGINE = TimeSeries DATA ENGINE = Merge('phys','^x')",
			"CREATE TABLE db1.ts ENGINE = TimeSeries DATA ENGINE = MergeTree ORDER BY a TAGS ENGINE = Merge('phys','^x') METRICS ENGINE = Memory",
			"CREATE TABLE db1.ts ENGINE = TimeSeries METRICS ENGINE = Merge('phys','^x')",
			"CREATE TABLE db1.ts ENGINE = TimeSeries TAGS ENGINE = URL('http://127.0.0.1/x', CSV)",
			"CREATE TABLE db1.ts ENGINE = TimeSeries DATA INNER UUID " + uuid + " TAGS ENGINE = Merge('phys','^x')",
			"CREATE TABLE db1.ts (a UInt64) ENGINE = TimeSeries DATA phys.x",
			// An unquoted TO INNER followed by ENGINE: ClickHouse reads
			// `TO INNER` as the table INNER and then refuses TO with ENGINE,
			// so the shape is refused rather than forwarded as that table.
			"CREATE MATERIALIZED VIEW db1.mv TO INNER ENGINE = Memory" + body,
			"CREATE MATERIALIZED VIEW db1.mv TO INNER ENGINE = MergeTree ORDER BY a" + body,
		} {
			add(sql, pb.RewriteCode_UnsupportedStatement, t7)
		}
		// The window-view family is refused as a class.
		for _, sql := range []string{
			"CREATE WINDOW VIEW db1.wv INNER ENGINE = Merge('phys','^x') ENGINE = Memory AS SELECT count(a) FROM db1.o GROUP BY tumble(now(), INTERVAL '1' SECOND)",
			"CREATE WINDOW VIEW db1.wv TO db1.t2 INNER ENGINE = Merge('phys','^x') AS SELECT count(a) FROM db1.o GROUP BY tumble(now(), INTERVAL '1' SECOND)",
			"CREATE WINDOW VIEW db1.wv TO INNER UUID " + uuid + " INNER ENGINE = Merge('phys','^x') AS SELECT count(a) FROM db1.o GROUP BY tumble(now(), INTERVAL '1' SECOND)",
			"CREATE OR REPLACE WINDOW VIEW db1.wv INNER ENGINE URL('http://127.0.0.1/x', CSV) AS SELECT 1",
		} {
			add(sql, pb.RewriteCode_UnsupportedStatement, "CREATE LIVE VIEW / WINDOW VIEW is not supported")
		}
		// Modelled positions: T3 before T5, SI messages first while the
		// surface is active.
		siSafe := func(inactive pb.RewriteCode, inactiveMsg, activeMsg string) (pb.RewriteCode, string) {
			if si {
				return pb.RewriteCode_UnsupportedStatement, activeMsg
			}
			return inactive, inactiveMsg
		}
		for _, tgt := range []string{"TO INNER ", "", "REFRESH EVERY 1 HOUR ", "REFRESH EVERY 1 HOUR TO INNER UUID " + uuid + " "} {
			add("CREATE MATERIALIZED VIEW db1.mv "+tgt+"ENGINE = Merge('phys','^x')"+body,
				pb.RewriteCode_InvalidRewriteRequest, "protected database phys is not addressable")
			code, msg := siSafe(pb.RewriteCode_InvalidRewriteRequest, "protected database hg_safe is not addressable",
				"storage-integrity physical table hg_safe.x is not directly addressable")
			add("CREATE MATERIALIZED VIEW db1.mv "+tgt+"ENGINE = Buffer(hg_safe, x, 1, 10, 100, 10000, 1000000, 10000000, 100000000)"+body, code, msg)
			code, msg = siSafe(pb.RewriteCode_UnsupportedStatement, "table engine Distributed is not accepted",
				"storage-integrity logical database db1 is not directly addressable through Distributed table engine")
			add("CREATE MATERIALIZED VIEW db1.mv "+tgt+"ENGINE = Distributed('c', 'db1', 'x')"+body, code, msg)
			for _, eng := range []string{"URL('http://127.0.0.1/x', CSV)", "MySQL('h:3306', 'db', 't', 'u', 'p')", "Dictionary(db1.d)", "Kafka"} {
				add("CREATE MATERIALIZED VIEW db1.mv "+tgt+"ENGINE = "+eng+body,
					pb.RewriteCode_UnsupportedStatement, "table engine "+eng[:strings.IndexAny(eng+"(", "(")]+" is not accepted")
			}
		}
		// A second ENGINE clause: every clause is checked, not the last one.
		for _, c := range []struct{ sql, eng string }{
			{"CREATE TABLE db1.n (a UInt64) ENGINE = URL('http://127.0.0.1/x', CSV) ENGINE = Memory", "URL"},
			{"CREATE TABLE db1.n (a UInt64) ENGINE = Memory ENGINE = URL('http://127.0.0.1/x', CSV)", "URL"},
			{"CREATE MATERIALIZED VIEW db1.mv ENGINE = URL('http://127.0.0.1/x', CSV) ENGINE = Memory" + body, "URL"},
			{"CREATE MATERIALIZED VIEW db1.mv ENGINE = MySQL('h:3306', 'db', 't', 'u', 'p') ENGINE = MergeTree ORDER BY a" + body, "MySQL"},
			{"CREATE MATERIALIZED VIEW db1.mv TO INNER ENGINE = URL('http://127.0.0.1/x', CSV) ENGINE = Memory" + body, "URL"},
		} {
			add(c.sql, pb.RewriteCode_UnsupportedStatement, "table engine "+c.eng+" is not accepted")
		}
		code, msg := siSafe(pb.RewriteCode_UnsupportedStatement, "table engine Merge is not accepted",
			"storage-integrity logical database db1 is not directly addressable through Merge table engine")
		add("CREATE TABLE db1.n (a UInt64) ENGINE = Merge('db1','^x') ENGINE = Memory", code, msg)
		add("CREATE TABLE db1.n (a UInt64) ENGINE = Merge('phys','^x') ENGINE = Memory",
			pb.RewriteCode_InvalidRewriteRequest, "protected database phys is not addressable")
		// Still accepted: an allowed engine of the view's own storage, and a
		// TO target that is a table named INNER.
		cases = append(cases,
			tablerefCase{name: "own storage allowed", si: si, sql: "CREATE MATERIALIZED VIEW db1.mv ENGINE = Memory" + body,
				wantCode: pb.RewriteCode_Success, wantSQL: `CREATE MATERIALIZED VIEW phys."db1.mv" ENGINE=Memory AS SELECT 1 AS a`},
			tablerefCase{name: "TO table named INNER", si: si, sql: "CREATE MATERIALIZED VIEW db1.mv TO INNER" + body,
				wantCode: pb.RewriteCode_Success, wantSQL: `CREATE MATERIALIZED VIEW phys."db1.mv" TO phys."db1.INNER" AS SELECT 1 AS a`},
			tablerefCase{name: "TO table named quoted INNER", si: si, sql: "CREATE MATERIALIZED VIEW db1.mv TO `INNER`" + body,
				wantCode: pb.RewriteCode_Success, wantSQL: `CREATE MATERIALIZED VIEW phys."db1.mv" TO phys."db1.INNER" AS SELECT 1 AS a`},
			tablerefCase{name: "TO db1.INNER", si: si, sql: "CREATE MATERIALIZED VIEW db1.mv TO db1.INNER" + body,
				wantCode: pb.RewriteCode_Success, wantSQL: `CREATE MATERIALIZED VIEW phys."db1.mv" TO phys."db1.INNER" AS SELECT 1 AS a`},
		)
	}
	runTablerefCases(t, cases)
}

// twinAccessed is one expected AccessedTable of the quoted-twin tests: the
// original database and table as the caller wrote them, the physical
// database, and the storage-integrity flag.
type twinAccessed struct {
	db, table, phys string
	si              bool
}

// runTwinCase asserts a Success rewrite whose accessed tables match want
// exactly, field by field and in order. The generic tablerefCase helper joins
// database and table with "." and so cannot tell `db1.t` from db1.t.
func runTwinCase(t *testing.T, e engine.Engine, sql string, si bool, wantSQL string, want []twinAccessed) {
	t.Helper()
	resp, err := doRewrite(e, sql, tablerefOpts(si))
	if err != nil {
		t.Fatalf("doRewrite: %v", err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("code = %s (%s), want Success", resp.GetCode(), resp.GetMessage())
	}
	if wantSQL != "" && resp.GetSqlAfterRewrite() != wantSQL {
		t.Fatalf("sql = %q, want %q", resp.GetSqlAfterRewrite(), wantSQL)
	}
	var got []twinAccessed
	for _, a := range resp.GetOriginalAccessedTables() {
		got = append(got, twinAccessed{a.GetOriginalDatabase(), a.GetOriginalTable(), a.GetPhysicalDatabase(), a.GetIsStorageIntegrity()})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("accessed = %+v, want %+v", got, want)
	}
}

// TestTableRef_QuotedTwinIsItsOwnTable pins that a qualified table and its
// quoted single-identifier twin are two tables. `db1.t` is a table literally
// named "db1.t" in the session's logical database (ClickHouse 26.2 reads
// db1.`db1.t` for it), so it must be reported and rewritten on its own; keying
// accessed tables by the written name "db1.t" used to drop one of the two,
// which hid an Active storage-integrity table from the SI checks (FINAL was
// silently discarded) and from HouseGate's permission and table-state gates.
func TestTableRef_QuotedTwinIsItsOwnTable(t *testing.T) {
	e := newEngine(t)
	const safeT = `(SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t) AS "db1.t"`
	for _, si := range []bool{false, true} {
		qualifiedT := `phys."db1.t" "db1.t"`
		if si {
			qualifiedT = safeT
		}
		cases := []struct {
			sql, wantSQL string
			want         []twinAccessed
		}{
			{"SELECT a IN `db1.t` FROM db1.t",
				`SELECT a IN phys."db1.db1.t" FROM ` + qualifiedT,
				[]twinAccessed{{"", "db1.t", "phys", false}, {"db1", "t", "phys", si}}},
			{"SELECT a FROM `db1.t` WHERE a IN db1.t", "",
				[]twinAccessed{{"", "db1.t", "phys", false}, {"db1", "t", "phys", si}}},
			{"SELECT a IN `default.x` FROM default.x",
				`SELECT a IN phys."db1.default.x" FROM default.x`,
				[]twinAccessed{{"", "default.x", "phys", false}, {"default", "x", "", false}}},
			{"SELECT a FROM default.x WHERE a IN `default.x`",
				`SELECT a FROM default.x WHERE a IN phys."db1.default.x"`,
				[]twinAccessed{{"", "default.x", "phys", false}, {"default", "x", "", false}}},
			{"SELECT a FROM db1.o WHERE a IN `db2.x` OR a IN db2.x",
				`SELECT a FROM phys."db1.o" "db1.o" WHERE a IN phys."db1.db2.x" OR a IN db2.x`,
				[]twinAccessed{{"db1", "o", "phys", false}, {"", "db2.x", "phys", false}, {"db2", "x", "", false}}},
			{"SELECT a FROM `db1.o` UNION ALL SELECT a FROM db1.o",
				`SELECT a FROM phys."db1.db1.o" "db1.o" UNION ALL SELECT a FROM phys."db1.o" "db1.o"`,
				[]twinAccessed{{"", "db1.o", "phys", false}, {"db1", "o", "phys", false}}},
			{"WITH c AS (SELECT a FROM db1.t) SELECT a FROM c WHERE a IN (SELECT a FROM `db1.t`)", "",
				[]twinAccessed{{"", "db1.t", "phys", false}, {"db1", "t", "phys", si}}},
			{"SELECT (SELECT max(a) FROM `system.one`) FROM system.one", "",
				[]twinAccessed{{"", "system.one", "phys", false}, {"system", "one", "", false}}},
			// Two qualified names that share the written key "db1.t.x".
			{"SELECT a IN `db1.t`.x FROM db1.`t.x`",
				`SELECT a IN "db1.t".x FROM phys."db1.t.x" "db1.t.x"`,
				[]twinAccessed{{"db1", "t.x", "phys", false}, {"db1.t", "x", "", false}}},
		}
		for _, c := range cases {
			t.Run(fmt.Sprintf("si=%v/%s", si, c.sql), func(t *testing.T) {
				runTwinCase(t, e, c.sql, si, c.wantSQL, c.want)
			})
		}
	}
}

// TestTableRef_QuotedTwinPositionMatrix places a qualified name in one
// read position and its quoted twin in another (FROM, JOIN, IN operand,
// scalar subquery, CTE body, UNION arm), in both orders and both SI states,
// and requires both tables to be reported and the twin to be rewritten to its
// own physical name.
func TestTableRef_QuotedTwinPositionMatrix(t *testing.T) {
	e := newEngine(t)
	positions := []string{"FROM", "JOIN", "IN", "SUBQ", "CTE", "UNION"}
	build := func(slots [][2]string) string {
		var withs, joins, where, unions []string
		sel := []string{"a"}
		from := ""
		for i, s := range slots {
			pos, ref := s[0], s[1]
			switch pos {
			case "FROM":
				if from == "" {
					from = fmt.Sprintf("%s AS f%d", ref, i)
				} else {
					joins = append(joins, fmt.Sprintf("CROSS JOIN %s AS f%d", ref, i))
				}
			case "JOIN":
				joins = append(joins, fmt.Sprintf("JOIN %s AS j%d USING (a)", ref, i))
			case "IN":
				where = append(where, "a IN "+ref)
			case "SUBQ":
				sel = append(sel, fmt.Sprintf("(SELECT max(a) FROM %s) AS s%d", ref, i))
			case "CTE":
				withs = append(withs, fmt.Sprintf("c%d AS (SELECT a FROM %s)", i, ref))
				joins = append(joins, fmt.Sprintf("JOIN c%d USING (a)", i))
			case "UNION":
				unions = append(unions, "SELECT a"+strings.Repeat(", 0", len(sel)-1)+" FROM "+ref)
			}
		}
		if from == "" {
			from = "db1.o AS base"
		}
		sql := ""
		if len(withs) > 0 {
			sql = "WITH " + strings.Join(withs, ", ") + " "
		}
		sql += "SELECT " + strings.Join(sel, ", ") + " FROM " + from
		if len(joins) > 0 {
			sql += " " + strings.Join(joins, " ")
		}
		if len(where) > 0 {
			sql += " WHERE " + strings.Join(where, " AND ")
		}
		for _, u := range unions {
			sql += " UNION ALL " + u
		}
		return sql
	}
	names := [][2]string{{"db1", "t"}, {"db1", "o"}, {"db2", "x"}, {"default", "x"}, {"system", "one"}}
	for _, si := range []bool{false, true} {
		for _, n := range names {
			qualified := n[0] + "." + n[1]
			twin := "`" + qualified + "`"
			twinPhys := `phys."db1.` + qualified + `"`
			for _, a := range positions {
				for _, b := range positions {
					for _, swap := range []bool{false, true} {
						slots := [][2]string{{a, qualified}, {b, twin}}
						if swap {
							slots = [][2]string{{a, twin}, {b, qualified}}
						}
						sql := build(slots)
						t.Run(fmt.Sprintf("si=%v/%s", si, sql), func(t *testing.T) {
							resp, err := doRewrite(e, sql, tablerefOpts(si))
							if err != nil {
								t.Fatalf("doRewrite: %v", err)
							}
							if resp.GetCode() != pb.RewriteCode_Success {
								t.Fatalf("code = %s (%s), want Success", resp.GetCode(), resp.GetMessage())
							}
							var sawQualified, sawTwin int
							for _, acc := range resp.GetOriginalAccessedTables() {
								switch {
								case acc.GetOriginalDatabase() == n[0] && acc.GetOriginalTable() == n[1]:
									sawQualified++
								case acc.GetOriginalDatabase() == "" && acc.GetOriginalTable() == qualified:
									sawTwin++
								}
							}
							if sawQualified != 1 || sawTwin != 1 {
								t.Fatalf("accessed reports %s %d time(s) and %s %d time(s), want once each: %v",
									qualified, sawQualified, twin, sawTwin, resp.GetOriginalAccessedTables())
							}
							if !strings.Contains(resp.GetSqlAfterRewrite(), twinPhys) {
								t.Fatalf("sql = %q, want the twin rewritten to %s", resp.GetSqlAfterRewrite(), twinPhys)
							}
						})
					}
				}
			}
		}
	}
}

// TestTableRef_QuotedTwinRawSplice pins the RENAME / EXCHANGE byte-span
// splice: each side is rewritten to its own physical name, where a map keyed
// by the written name used to rewrite both to one of them.
func TestTableRef_QuotedTwinRawSplice(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases,
			tablerefCase{name: "rename_twin_first", sql: "RENAME TABLE `db1.o` TO db1.a, db1.o TO db1.b", si: si,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "RENAME TABLE phys.`db1.db1.o` TO phys.`db1.a`, phys.`db1.o` TO phys.`db1.b`",
				wantAcc:  []string{".db1.o", "db1.a", "db1.o", "db1.b"}},
			tablerefCase{name: "rename_qualified_first", sql: "RENAME TABLE db1.o TO db1.a, `db1.o` TO db1.b", si: si,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "RENAME TABLE phys.`db1.o` TO phys.`db1.a`, phys.`db1.db1.o` TO phys.`db1.b`",
				wantAcc:  []string{"db1.o", "db1.a", ".db1.o", "db1.b"}},
			tablerefCase{name: "exchange_twin_first", sql: "EXCHANGE TABLES `db1.o` AND db1.o", si: si,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "EXCHANGE TABLES phys.`db1.db1.o` AND phys.`db1.o`",
				wantAcc:  []string{".db1.o", "db1.o"}},
			tablerefCase{name: "exchange_qualified_first", sql: "EXCHANGE TABLES db1.o AND `db1.o`", si: si,
				wantCode: pb.RewriteCode_Success,
				wantSQL:  "EXCHANGE TABLES phys.`db1.o` AND phys.`db1.db1.o`",
				wantAcc:  []string{"db1.o", ".db1.o"}},
		)
	}
	runTablerefCases(t, cases)
}

// TestTableRef_QuotedReservedTwinIsRefused pins that a quoted twin of a
// reserved physical table (`hg_safe.db1__t`, an ordinary table name in db1)
// never hides the qualified reserved name beside it: the statement is refused
// in either order, with the same message for both orders.
func TestTableRef_QuotedReservedTwinIsRefused(t *testing.T) {
	const protected = "protected database hg_safe is not addressable"
	const safe = "storage-integrity physical table hg_safe.db1__t is not directly addressable"
	var cases []tablerefCase
	for _, sql := range []string{
		"SELECT a IN `hg_safe.db1__t` FROM hg_safe.db1__t",
		"SELECT a IN hg_safe.db1__t FROM `hg_safe.db1__t`",
		"SELECT a FROM db1.o WHERE a IN `hg_safe.db1__t` OR a IN hg_safe.db1__t",
		"SELECT a FROM db1.o WHERE a IN hg_safe.db1__t OR a IN `hg_safe.db1__t`",
		"SELECT a FROM `hg_safe.db1__t` UNION ALL SELECT a FROM hg_safe.db1__t",
		"WITH c AS (SELECT a FROM `hg_safe.db1__t`) SELECT a FROM c JOIN hg_safe.db1__t USING (a)",
	} {
		cases = append(cases,
			tablerefCase{name: sql, sql: sql, wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: protected, wantSQL: sql},
			tablerefCase{name: sql, sql: sql, si: true, wantCode: pb.RewriteCode_RewriteError, wantMsg: safe, wantSQL: sql},
		)
	}
	for _, sql := range []string{
		"DROP TABLE `hg_safe.db1__t`, hg_safe.db1__t",
		"DROP TABLE hg_safe.db1__t, `hg_safe.db1__t`",
		"INSERT INTO `hg_safe.db1__t` SELECT * FROM hg_safe.db1__t",
	} {
		cases = append(cases,
			tablerefCase{name: sql, sql: sql, wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: protected, wantSQL: sql},
			tablerefCase{name: sql, sql: sql, si: true, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: safe, wantSQL: sql},
		)
	}
	runTablerefCases(t, cases)
}

// TestTableRef_QuotedTwinKeepsStorageIntegrityChecks pins the security
// consequence of the quoted-twin fix: with the storage-integrity surface
// active, an Active table that carries FINAL / SAMPLE / PREWHERE or addresses
// the reserved row-id column is refused even when its quoted twin `db1.t` (an
// ordinary table in db1) sits in the same statement, whichever comes first.
// Before the fix the twin hid the Active table and these answered Success.
func TestTableRef_QuotedTwinKeepsStorageIntegrityChecks(t *testing.T) {
	const modifiers = "FINAL/SAMPLE/PREWHERE/WITH OFFSET/column aliases on storage-integrity tables are not supported"
	const reserved = "reserved column _hg_row_id is not addressable"
	var cases []tablerefCase
	for _, c := range []struct{ sql, msg string }{
		{"SELECT a IN `db1.t` FROM db1.t FINAL", modifiers},
		{"SELECT a FROM db1.t FINAL WHERE a IN `db1.t`", modifiers},
		{"SELECT a FROM `db1.t` WHERE a IN (SELECT a FROM db1.t FINAL)", modifiers},
		{"SELECT a FROM db1.t FINAL JOIN `db1.t` AS w USING (a)", modifiers},
		{"SELECT a IN `db1.t` FROM db1.t SAMPLE 0.5", modifiers},
		{"SELECT a FROM db1.t SAMPLE 0.5 WHERE a IN `db1.t`", modifiers},
		{"SELECT a FROM `db1.t` WHERE a IN (SELECT a FROM db1.t SAMPLE 0.5)", modifiers},
		{"SELECT a IN `db1.t` FROM db1.t PREWHERE a > 1", modifiers},
		{"SELECT a FROM db1.t PREWHERE a > 1 WHERE a IN `db1.t`", modifiers},
		{"SELECT a FROM `db1.t` WHERE a IN (SELECT a FROM db1.t PREWHERE a > 1)", modifiers},
		{"SELECT _hg_row_id, a IN `db1.t` FROM db1.t", reserved},
		{"SELECT _hg_row_id FROM db1.t WHERE a IN `db1.t`", reserved},
		{"SELECT a FROM `db1.t` WHERE a IN (SELECT _hg_row_id FROM db1.t)", reserved},
		{"SELECT a FROM db1.t WHERE a IN (SELECT _hg_row_id FROM db1.t) AND a IN `db1.t`", reserved},
	} {
		cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: true,
			wantCode: pb.RewriteCode_RewriteError, wantMsg: c.msg, wantSQL: c.sql,
			wantAcc: []string{".db1.t", "db1.t"}})
	}
	runTablerefCases(t, cases)
}

// grantDelta is one expected PrivilegeDelta of the GRANT identity tests.
type grantDelta struct {
	action, scope, priv, origDB, origTable, logical, phys, physTable string
}

// TestTableRef_GrantTargetIsKeyedByIdentity pins that a GRANT / REVOKE ON
// target is decoded structurally, never flattened to "db.table" and re-split:
// `db1.t` is the table named "db1.t" in the session database (ClickHouse
// 26.2's SHOW GRANTS reports db1.`db1.t` for it), and `db2.x` is a table in
// db1, not table x of db2. A target a privilege delta cannot represent (a
// quoted name containing '*', which a wildcard reading would widen) is
// refused.
func TestTableRef_GrantTargetIsKeyedByIdentity(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, c := range []struct {
			sql, marker string
			want        []grantDelta
		}{
			{"GRANT SELECT ON `db1.t` TO u", "SELECT 'GRANT SELECT ON `db1.t` TO u' AS gstmt",
				[]grantDelta{{"ACTION_GRANT", "SCOPE_TABLE", "SELECT", "", "db1.t", "db1", "phys", "db1.db1.t"}}},
			{"REVOKE SELECT ON `db1.t` FROM u", "SELECT 'REVOKE SELECT ON `db1.t` FROM u' AS rstmt",
				[]grantDelta{{"ACTION_REVOKE", "SCOPE_TABLE", "SELECT", "", "db1.t", "db1", "phys", "db1.db1.t"}}},
			{"GRANT SELECT ON `db2.x` TO u", "SELECT 'GRANT SELECT ON `db2.x` TO u' AS gstmt",
				[]grantDelta{{"ACTION_GRANT", "SCOPE_TABLE", "SELECT", "", "db2.x", "db1", "phys", "db1.db2.x"}}},
			{"REVOKE INSERT ON `db2.x` FROM u", "SELECT 'REVOKE INSERT ON `db2.x` FROM u' AS rstmt",
				[]grantDelta{{"ACTION_REVOKE", "SCOPE_TABLE", "INSERT", "", "db2.x", "db1", "phys", "db1.db2.x"}}},
			{"GRANT SELECT, INSERT ON `db1.t` TO u", "SELECT 'GRANT SELECT, INSERT ON `db1.t` TO u' AS gstmt",
				[]grantDelta{
					{"ACTION_GRANT", "SCOPE_TABLE", "SELECT", "", "db1.t", "db1", "phys", "db1.db1.t"},
					{"ACTION_GRANT", "SCOPE_TABLE", "INSERT", "", "db1.t", "db1", "phys", "db1.db1.t"},
				}},
			{"GRANT SELECT ON db1.`t.x` TO u", "SELECT 'GRANT SELECT ON db1.`t.x` TO u' AS gstmt",
				[]grantDelta{{"ACTION_GRANT", "SCOPE_TABLE", "SELECT", "db1", "t.x", "db1", "phys", "db1.t.x"}}},
			{"GRANT SELECT ON `db1`.`o` TO u", "SELECT 'GRANT SELECT ON db1.o TO u' AS gstmt",
				[]grantDelta{{"ACTION_GRANT", "SCOPE_TABLE", "SELECT", "db1", "o", "db1", "phys", "db1.o"}}},
			{"GRANT SELECT ON `hg_safe.db1__t` TO u", "SELECT 'GRANT SELECT ON `hg_safe.db1__t` TO u' AS gstmt",
				[]grantDelta{{"ACTION_GRANT", "SCOPE_TABLE", "SELECT", "", "hg_safe.db1__t", "db1", "phys", "db1.hg_safe.db1__t"}}},
		} {
			t.Run(fmt.Sprintf("si=%v/%s", si, c.sql), func(t *testing.T) {
				resp, err := doRewrite(e, c.sql, tablerefOpts(si))
				if err != nil {
					t.Fatalf("doRewrite: %v", err)
				}
				if resp.GetCode() != pb.RewriteCode_Success {
					t.Fatalf("code = %s (%s), want Success", resp.GetCode(), resp.GetMessage())
				}
				if resp.GetSqlAfterRewrite() != c.marker {
					t.Fatalf("sql = %q, want %q", resp.GetSqlAfterRewrite(), c.marker)
				}
				var got []grantDelta
				for _, d := range resp.GetPrivilegesDeltas() {
					got = append(got, grantDelta{d.GetAction().String(), d.GetScope().String(), strings.Join(d.GetPrivileges(), ","),
						d.GetOriginalDatabase(), d.GetOriginalTable(), d.GetLogicalDatabase(), d.GetPhysicalDatabase(), d.GetPhysicalTable()})
				}
				if !reflect.DeepEqual(got, c.want) {
					t.Fatalf("deltas = %+v, want %+v", got, c.want)
				}
			})
		}
	}

	const unrepresentable = "target names a database or table whose name contains '*', which a privilege delta cannot represent"
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct {
			sql  string
			code pb.RewriteCode
			msg  string
		}{
			{"GRANT SELECT ON db1.`*` TO u", pb.RewriteCode_UnsupportedStatement, "GRANT " + unrepresentable},
			{"GRANT SELECT ON `db1.*` TO u", pb.RewriteCode_UnsupportedStatement, "GRANT " + unrepresentable},
			{"REVOKE SELECT ON `db1.*` FROM u", pb.RewriteCode_UnsupportedStatement, "REVOKE " + unrepresentable},
			{"GRANT SELECT ON `*`.`*` TO u", pb.RewriteCode_UnsupportedStatement, "GRANT " + unrepresentable},
			{"GRANT SELECT ON `*`.t TO u", pb.RewriteCode_UnsupportedStatement, "GRANT " + unrepresentable},
			{"GRANT SELECT ON `db1.x`.t TO u", pb.RewriteCode_InvalidRewriteRequest,
				"GRANT target references logical database 'db1.x' which is not in database_map"},
			{"GRANT SELECT ON `db1.o`, INSERT ON db1.p TO u", pb.RewriteCode_UnsupportedStatement, "GRANT form is not supported"},
			{"GRANT SELECT ON db1.p, INSERT ON `db2.x` TO u", pb.RewriteCode_UnsupportedStatement, "GRANT form is not supported"},
			{"REVOKE SELECT ON db1.o, INSERT ON `db1.p` FROM u", pb.RewriteCode_UnsupportedStatement, "REVOKE form is not supported"},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: c.code, wantMsg: c.msg, wantSQL: c.sql})
		}
	}
	// The qualified Active table and reserved names keep their refusals.
	cases = append(cases,
		tablerefCase{name: "active_qualified", sql: "GRANT SELECT ON db1.t TO u", si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement,
			wantMsg:  "storage-integrity table db1.t accepts writes only through the signed statement lane"},
		tablerefCase{name: "reserved_qualified", sql: "GRANT SELECT ON hg_safe.db1__t TO u", si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement,
			wantMsg:  "storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		tablerefCase{name: "reserved_qualified", sql: "GRANT SELECT ON hg_safe.db1__t TO u",
			wantCode: pb.RewriteCode_InvalidRewriteRequest,
			wantMsg:  "GRANT target references logical database 'hg_safe' which is not in database_map"},
	)
	runTablerefCases(t, cases)
}

// TestTableRef_HasColumnInTableArgumentsAreNotResplit pins that
// hasColumnInTable's database and table literals are resolved as the two
// separate names ClickHouse reads, never joined with "." and re-split: a
// database or table literal that contains a '.' or is empty cannot be
// resolved faithfully and is refused. hasColumnInTable('db1.t', 'x', 'a')
// used to answer Success as hasColumnInTable('phys', 'db1.t.x', 'a'),
// reported as db1 / t.x, while ClickHouse reads database "db1.t", table "x".
func TestTableRef_HasColumnInTableArgumentsAreNotResplit(t *testing.T) {
	const unresolved = `" does not resolve through the caller's databases`
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, target string }{
			{"SELECT hasColumnInTable('db1.t', 'x', 'a')", "db1.t.x"},
			{"SELECT hasColumnInTable('db1', 't.x', 'a')", "db1.t.x"},
			{"SELECT hasColumnInTable('db1', 'db1.o', 'a')", "db1.db1.o"},
			{"SELECT hasColumnInTable('', 'db1.o', 'a')", ".db1.o"},
			{"SELECT hasColumnInTable('', 'o', 'a')", ".o"},
			{"SELECT hasColumnInTable('localhost:9000', 'db1.t', 'x', 'a')", "db1.t.x"},
			{"SELECT a FROM db1.o WHERE hasColumnInTable('db1.o', 'y', 'a')", "db1.o.y"},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest,
				wantMsg:  `hasColumnInTable target "` + c.target + unresolved, wantSQL: c.sql})
		}
		cases = append(cases, tablerefCase{name: "plain_pair", sql: "SELECT hasColumnInTable('db1', 'o', 'a')", si: si,
			wantCode: pb.RewriteCode_Success, wantSQL: "SELECT hasColumnInTable('phys', 'db1.o', 'a')", wantAcc: []string{"db1.o"}})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_OuterCTEDoesNotShadowViewBody pins that a CTE declared outside
// a view(SELECT …) table-function body never binds a name inside it. Measured
// on ClickHouse 26.2 (analyzer on, the default): `WITH t AS (…) SELECT * FROM
// view(SELECT * FROM t)` reads the table t of the current database, not the
// CTE, at the SELECT root and in INSERT … SELECT / CREATE TABLE … AS SELECT
// bodies; an ordinary subquery `FROM (SELECT * FROM t)` does resolve the CTE.
// The engine used to treat the name as the CTE and forward it verbatim, so
// `db2.x` read another tenant's physical table and `db1.t` read the ordinary
// physical table behind the Active db1.t, bypassing hg_safe. A view() body
// name now resolves as a table — rewritten, reported and SI-checked — exactly
// as it does without the outer CTE. In CREATE VIEW / MATERIALIZED VIEW bodies
// and under the legacy analyzer ClickHouse resolves the CTE instead; there
// the rewrite is fail-safe (it can only reach the caller's own governed
// table).
func TestTableRef_OuterCTEDoesNotShadowViewBody(t *testing.T) {
	const siWrite = "storage-integrity table db1.t accepts writes only through the signed statement lane"
	type shadow struct {
		label      string
		in, out    string // the name as written, and as the generator prints it
		ref, refSI string // the rewritten body reference without / with the SI surface
		acc        string // its OriginalAccessedTables entry
		active     bool   // the reference is the Active db1.t under the SI surface
	}
	shadows := []shadow{
		{"other tenant", "`db2.x`", `"db2.x"`, `phys."db1.db2.x" "db2.x"`, `phys."db1.db2.x" "db2.x"`, ".db2.x", false},
		{"quoted twin", "`db1.t`", `"db1.t"`, `phys."db1.db1.t" "db1.t"`, `phys."db1.db1.t" "db1.t"`, ".db1.t", false},
		{"plain", "t", "t", `phys."db1.t" t`, `(SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t) AS t`, ".t", true},
	}
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, s := range shadows {
			ref := s.ref
			if si {
				ref = s.refSI
			}
			with := "WITH " + s.in + " AS (SELECT 1) "
			withOut := "WITH " + s.out + " AS (SELECT 1) "
			add := func(shape, sql, wantSQL string, wantAcc []string) {
				cases = append(cases, tablerefCase{name: shape + "/" + s.label, sql: sql, si: si,
					wantCode: pb.RewriteCode_Success, wantSQL: wantSQL, wantAcc: wantAcc})
			}
			refuse := func(shape, sql string, wantAcc []string) {
				cases = append(cases, tablerefCase{name: shape + "/" + s.label, sql: sql, si: si,
					wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: siWrite, wantSQL: sql, wantAcc: wantAcc})
			}
			add("root", with+"SELECT * FROM view(SELECT * FROM "+s.in+")",
				withOut+"SELECT * FROM view(SELECT * FROM "+ref+")", []string{s.acc})
			add("root nested subquery", with+"SELECT * FROM view(SELECT * FROM (SELECT * FROM "+s.in+"))",
				withOut+"SELECT * FROM view(SELECT * FROM (SELECT * FROM "+ref+"))", []string{s.acc})
			add("root nested view", with+"SELECT * FROM view(SELECT * FROM view(SELECT * FROM "+s.in+"))",
				withOut+"SELECT * FROM view(SELECT * FROM view(SELECT * FROM "+ref+"))", []string{s.acc})
			add("root upper-case VIEW", with+"SELECT * FROM VIEW(SELECT * FROM "+s.in+")", "", []string{s.acc})
			add("root inner CTE reads the outer name", with+"SELECT * FROM view(WITH u AS (SELECT * FROM "+s.in+") SELECT * FROM u)",
				"", []string{s.acc})
			add("insert select", "INSERT INTO db1.o "+with+"SELECT * FROM view(SELECT * FROM "+s.in+")",
				`INSERT INTO phys."db1.o" `+withOut+"SELECT * FROM view(SELECT * FROM "+ref+")", []string{"db1.o", s.acc})
			add("ctas", "CREATE TABLE db1.n ENGINE = Memory AS "+with+"SELECT * FROM view(SELECT * FROM "+s.in+")",
				`CREATE TABLE phys."db1.n" ENGINE=Memory AS (`+withOut+"SELECT * FROM view(SELECT * FROM "+ref+"))", []string{"db1.n", s.acc})
			viewSQL := "CREATE VIEW db1.v AS " + with + "SELECT * FROM view(SELECT * FROM " + s.in + ")"
			mvSQL := "CREATE MATERIALIZED VIEW db1.mv TO db1.o AS " + with + "SELECT * FROM db1.p, view(SELECT * FROM " + s.in + ") AS u"
			if si && s.active {
				// A view / MV body reading an Active table is refused, as it is
				// without the outer CTE.
				refuse("view", viewSQL, []string{"db1.v", s.acc})
				refuse("materialized view", mvSQL, []string{"db1.mv", "db1.o", "db1.p", s.acc})
			} else {
				add("view", viewSQL, `CREATE VIEW phys."db1.v" AS `+withOut+"SELECT * FROM view(SELECT * FROM "+ref+")",
					[]string{"db1.v", s.acc})
				add("materialized view", mvSQL,
					`CREATE MATERIALIZED VIEW phys."db1.mv" TO phys."db1.o" AS `+withOut+`SELECT * FROM phys."db1.p" "db1.p" CROSS JOIN view(SELECT * FROM `+ref+") AS u",
					[]string{"db1.mv", "db1.o", "db1.p", s.acc})
			}

			// Controls: the CTE keeps binding outside a view() body.
			for _, sql := range []string{
				with + "SELECT * FROM " + s.in,
				with + "SELECT * FROM (SELECT * FROM " + s.in + ")",
				with + "SELECT * FROM view(SELECT 1) AS v, " + s.in,
				"SELECT * FROM view(" + with + "SELECT * FROM " + s.in + ")",
			} {
				cases = append(cases, tablerefCase{name: "control/" + sql, sql: sql, si: si,
					wantCode: pb.RewriteCode_Success, wantAcc: []string{}})
			}
		}

		// An IN operand inside the view() body is a table too.
		inRef := `phys."db1.t"`
		if si {
			inRef = "(SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t)"
		}
		cases = append(cases, tablerefCase{name: "root IN operand", si: si,
			sql:      "WITH t AS (SELECT 1) SELECT * FROM view(SELECT * FROM db1.o WHERE a IN t)",
			wantCode: pb.RewriteCode_Success,
			wantSQL:  `WITH t AS (SELECT 1) SELECT * FROM view(SELECT * FROM phys."db1.o" "db1.o" WHERE a IN ` + inRef + ")",
			wantAcc:  []string{"db1.o", ".t"}})
		// A scalar argument of a data-only table function is not a view()
		// body: ClickHouse 26.2 resolves the CTE there.
		cases = append(cases, tablerefCase{name: "control/numbers scalar argument", si: si,
			sql:      "WITH t AS (SELECT 1) SELECT * FROM numbers((SELECT count() FROM t))",
			wantCode: pb.RewriteCode_Success, wantSQL: "WITH t AS (SELECT 1) SELECT * FROM numbers((SELECT count() FROM t))",
			wantAcc: []string{}})
	}
	runTablerefCases(t, cases)
}

// TestTableRef_OuterCTEDoesNotHideReservedColumnInViewBody pins that the
// reserved-row-id check sees a view() body through an outer CTE of the same
// name: the body reads the Active table, so `_hg_row_id` there is refused
// exactly as it is without the CTE.
func TestTableRef_OuterCTEDoesNotHideReservedColumnInViewBody(t *testing.T) {
	e := newEngine(t)
	want, err := doRewrite(e, "SELECT * FROM view(SELECT _hg_row_id FROM t)", tablerefOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	if want.GetCode() == pb.RewriteCode_Success {
		t.Fatalf("baseline unexpectedly succeeded: %s", want.GetSqlAfterRewrite())
	}
	got, err := doRewrite(e, "WITH t AS (SELECT 1 AS _hg_row_id) SELECT * FROM view(SELECT _hg_row_id FROM t)", tablerefOpts(true))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetCode() != want.GetCode() || got.GetMessage() != want.GetMessage() {
		t.Fatalf("got %s %q, want %s %q", got.GetCode(), got.GetMessage(), want.GetCode(), want.GetMessage())
	}
}

// TestTableRef_ActiveToActiveInsertIsRefused pins spec 2026-09-26 §7
// ("Sources"): an INSERT … SELECT into an Active table whose body reads an
// Active table is refused — the signed lane admits only a client payload, so
// the source can never become the derived read there — with the write
// message naming the source, the target reported first. An ordinary source
// keeps the signed-lane marking, and without the SI surface nothing changes.
func TestTableRef_ActiveToActiveInsertIsRefused(t *testing.T) {
	const siWrite = "storage-integrity table db1.t accepts writes only through the signed statement lane"
	var cases []tablerefCase
	for _, c := range []struct {
		sql string
		acc []string
	}{
		{"INSERT INTO db1.t SELECT * FROM db1.t", []string{"db1.t", "db1.t"}},
		{"INSERT INTO db1.t SELECT * FROM t", []string{"db1.t", ".t"}},
		{"INSERT INTO t SELECT * FROM db1.t", []string{".t", "db1.t"}},
		{"INSERT INTO db1.t SELECT * FROM (SELECT * FROM db1.t)", []string{"db1.t", "db1.t"}},
		{"INSERT INTO db1.t SELECT * FROM view(SELECT * FROM db1.t)", []string{"db1.t", "db1.t"}},
		{"INSERT INTO db1.t SELECT * FROM db1.o UNION ALL SELECT * FROM db1.t", nil},
		{"INSERT INTO db1.t WITH c AS (SELECT * FROM db1.t) SELECT * FROM c", []string{"db1.t", "db1.t"}},
		{"INSERT INTO db1.t SELECT a, (SELECT max(a) FROM db1.t) FROM db1.o", nil},
		{"INSERT INTO db1.t WITH t AS (SELECT 1) SELECT * FROM view(SELECT * FROM t)", []string{"db1.t", ".t"}},
	} {
		cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: true,
			wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: siWrite, wantSQL: c.sql, wantAcc: c.acc})
		cases = append(cases, tablerefCase{name: "no SI surface/" + c.sql, sql: c.sql, si: false,
			wantCode: pb.RewriteCode_Success})
	}
	cases = append(cases,
		tablerefCase{name: "ordinary source keeps the signed-lane marking", si: true,
			sql:      "INSERT INTO db1.t SELECT * FROM db1.o",
			wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.t" SELECT * FROM phys."db1.o" "db1.o"`,
			wantAcc: []string{"db1.t", "db1.o"}},
		tablerefCase{name: "quoted twin source is not the Active table", si: true,
			sql:      "INSERT INTO db1.t SELECT * FROM `db1.t`",
			wantCode: pb.RewriteCode_Success, wantSQL: `INSERT INTO phys."db1.t" SELECT * FROM phys."db1.db1.t" "db1.t"`,
			wantAcc: []string{"db1.t", ".db1.t"}},
	)
	runTablerefCases(t, cases)
}

// TestTableRef_UnresolvedUnqualifiedNameIsRefused pins the refusal of an
// unqualified table name — including the one-part dotted quoted form
// `db1.t` — that does not resolve through the session's logical database in
// dynamic mode. ClickHouse resolves such a name in the session's current
// database, which housegate makes the physical database, so forwarding it
// verbatim would read phys.t or phys.`db1.t` (the ordinary physical table of
// an Active storage-integrity table, outside hg_safe). The permission observer
// in housegate refuses it for tenants, but driver and auth-disabled sessions
// skip that observer. The logical context is empty or unmapped (db9); both
// SI states.
func TestTableRef_UnresolvedUnqualifiedNameIsRefused(t *testing.T) {
	e := newEngine(t)
	msg := func(table string) string {
		return `unqualified table "` + table + `" does not resolve through the session's logical database`
	}
	type pos struct {
		name, sql, table string
		code             pb.RewriteCode
	}
	invalid, unsupported := pb.RewriteCode_InvalidRewriteRequest, pb.RewriteCode_UnsupportedStatement
	positions := []pos{
		{"from", "SELECT * FROM t", "t", invalid},
		{"from_dotted_quoted", "SELECT * FROM `db1.t`", "db1.t", invalid},
		{"from_dotted_double_quoted", `SELECT * FROM "other.x"`, "other.x", invalid},
		{"join", "SELECT * FROM db1.o AS a JOIN t AS b USING (a)", "t", invalid},
		{"subquery", "SELECT * FROM (SELECT * FROM t)", "t", invalid},
		{"scalar_subquery", "SELECT (SELECT max(a) FROM t)", "t", invalid},
		{"cte_body", "WITH c AS (SELECT * FROM t) SELECT * FROM c", "t", invalid},
		{"union_arm", "SELECT a FROM db1.o UNION ALL SELECT a FROM t", "t", invalid},
		{"in_subquery", "SELECT * FROM db1.o WHERE a IN (SELECT a FROM t)", "t", invalid},
		{"view_function", "SELECT * FROM view(SELECT * FROM t)", "t", invalid},
		{"view_body", "CREATE VIEW db1.v AS SELECT * FROM t", "t", invalid},
		{"mv_body", "CREATE MATERIALIZED VIEW db1.mv TO db1.x AS SELECT * FROM t", "t", invalid},
		// INSERT … SELECT / CTAS bodies: a body rejection is UnsupportedStatement
		// on a write statement (rewriteEmbeddedBody), message kept verbatim.
		{"insert_select_source", "INSERT INTO db1.o SELECT * FROM t", "t", unsupported},
		{"insert_select_dotted_source", "INSERT INTO db1.o SELECT * FROM `db1.t`", "db1.t", unsupported},
		{"ctas_source", "CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM t", "t", unsupported},
	}
	// IN table operands: these requests send no physical context, so with the
	// SI surface active the SI namespace policy cannot resolve an execution
	// database and refuses them first with its own message; without the
	// surface the new refusal fires. With a physical context the new refusal
	// fires under V2 too (TestTableRef_UnresolvedUnqualifiedPrecedence).
	inOperands := []pos{
		{"in_bare", "SELECT * FROM db1.o WHERE a IN t", "t", invalid},
		{"in_paren", "SELECT * FROM db1.o WHERE a IN (t)", "t", invalid},
		{"in_dotted_quoted", "SELECT * FROM db1.o WHERE a IN `db1.t`", "db1.t", invalid},
		{"in_call", "SELECT * FROM db1.o WHERE in(a, t)", "t", invalid},
	}
	for _, si := range []bool{false, true} {
		for _, ctx := range []string{"", "db9"} {
			dyn := tablerefDynamic(si)
			dyn.UpstreamLogicalDatabaseInContext = ctx
			opts := []*pb.RewriteOption{tableRewriteDynamic(dyn)}
			all := positions
			if !si {
				all = append(append([]pos{}, positions...), inOperands...)
			}
			for _, p := range all {
				t.Run(fmt.Sprintf("si=%v/ctx=%q/%s", si, ctx, p.name), func(t *testing.T) {
					resp, err := doRewrite(e, p.sql, opts)
					if err != nil {
						t.Fatalf("doRewrite: %v", err)
					}
					if resp.GetCode() != p.code || resp.GetMessage() != msg(p.table) {
						t.Fatalf("got %s %q, want %s %q", resp.GetCode(), resp.GetMessage(), p.code, msg(p.table))
					}
					if resp.GetSqlAfterRewrite() != p.sql {
						t.Fatalf("sql = %q, want the input echoed", resp.GetSqlAfterRewrite())
					}
				})
			}
			if si {
				for _, p := range inOperands {
					t.Run(fmt.Sprintf("si=%v/ctx=%q/%s_si_message_first", si, ctx, p.name), func(t *testing.T) {
						resp, err := doRewrite(e, p.sql, opts)
						if err != nil {
							t.Fatalf("doRewrite: %v", err)
						}
						if resp.GetCode() != pb.RewriteCode_RewriteError ||
							resp.GetMessage() != "storage-integrity IN table target namespace is not statically resolvable" {
							t.Fatalf("got %s %q, want the SI IN-namespace refusal", resp.GetCode(), resp.GetMessage())
						}
					})
				}
			}
		}
	}
}

// TestTableRef_UnresolvedUnqualifiedRuleLeavesOthersAlone pins what the
// refusal above must not touch: CTE names and table aliases (not tables),
// qualified unmapped names (lenient; spec §5 and the permission observer own
// them), a mapped logical context, and the none / static modes.
func TestTableRef_UnresolvedUnqualifiedRuleLeavesOthersAlone(t *testing.T) {
	e := newEngine(t)
	type c struct {
		name, sql, wantSQL string
	}
	for _, si := range []bool{false, true} {
		for _, ctx := range []string{"", "db9"} {
			dyn := tablerefDynamic(si)
			dyn.UpstreamLogicalDatabaseInContext = ctx
			opts := []*pb.RewriteOption{tableRewriteDynamic(dyn)}
			for _, k := range []c{
				{"cte_name", "WITH c AS (SELECT 1 AS a) SELECT * FROM c", "WITH c AS (SELECT 1 AS a) SELECT * FROM c"},
				{"cte_name_in_operand", "WITH c AS (SELECT 1 AS a) SELECT * FROM db1.o WHERE a IN c",
					`WITH c AS (SELECT 1 AS a) SELECT * FROM phys."db1.o" "db1.o" WHERE a IN c`},
				{"table_alias_column", "SELECT b.a FROM db1.o AS b", `SELECT b.a FROM phys."db1.o" AS b`},
				{"qualified_unmapped", "SELECT * FROM db2.x", "SELECT * FROM db2.x"},
				{"qualified_unmapped_in", "SELECT * FROM db1.o WHERE a IN db2.x",
					`SELECT * FROM phys."db1.o" "db1.o" WHERE a IN db2.x`},
				{"no_table", "SELECT 1", "SELECT 1"},
			} {
				t.Run(fmt.Sprintf("si=%v/ctx=%q/%s", si, ctx, k.name), func(t *testing.T) {
					resp, err := doRewrite(e, k.sql, opts)
					if err != nil {
						t.Fatalf("doRewrite: %v", err)
					}
					if resp.GetCode() != pb.RewriteCode_Success || resp.GetSqlAfterRewrite() != k.wantSQL {
						t.Fatalf("got %s %q %q, want Success %q", resp.GetCode(), resp.GetMessage(), resp.GetSqlAfterRewrite(), k.wantSQL)
					}
				})
			}
		}
	}
	// A mapped context still rewrites the unqualified name.
	for _, si := range []bool{false, true} {
		resp, err := doRewrite(e, "SELECT * FROM `other.x`", tablerefOpts(si))
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetCode() != pb.RewriteCode_Success || resp.GetSqlAfterRewrite() != `SELECT * FROM phys."db1.other.x" "other.x"` {
			t.Fatalf("si=%v mapped context: got %s %q %q", si, resp.GetCode(), resp.GetMessage(), resp.GetSqlAfterRewrite())
		}
	}
	// None and static modes forward an unqualified name unchanged.
	for name, opts := range map[string][]*pb.RewriteOption{
		"none":   nil,
		"static": {{Op: pb.RewriteOp_TableNameRewrite, Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{StaticArgs: &pb.RewriteTableStaticArgs{TableMap: map[string]string{"db.x": "y"}}}}}},
	} {
		for _, sql := range []string{"SELECT * FROM t", "SELECT * FROM `db1.t`", "INSERT INTO db1.o SELECT * FROM t"} {
			resp, err := doRewrite(e, sql, opts)
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetCode() != pb.RewriteCode_Success {
				t.Fatalf("%s %q: got %s %q", name, sql, resp.GetCode(), resp.GetMessage())
			}
		}
	}
}

// TestTableRef_UnresolvedUnqualifiedRejectCarriesNoTableRewrites pins that
// the unresolved-unqualified refusal returns an empty table_rewrites map in
// every statement family: no partial map of the tables the walk rewrote
// before the refusal (or of a write statement's own targets) leaks.
func TestTableRef_UnresolvedUnqualifiedRejectCarriesNoTableRewrites(t *testing.T) {
	e := newEngine(t)
	phys := "phys"
	for _, si := range []bool{false, true} {
		for _, ctx := range []string{"", "db9"} {
			dyn := tablerefDynamic(si)
			dyn.UpstreamLogicalDatabaseInContext = ctx
			dyn.UpstreamPhysicalDatabaseInContext = &phys
			opts := []*pb.RewriteOption{tableRewriteDynamic(dyn)}
			for _, sql := range []string{
				"SELECT * FROM db1.o AS a JOIN t AS b USING (a)",
				"SELECT * FROM db1.t JOIN t2 USING (a)",
				"SELECT * FROM db1.o WHERE a IN t",
				"INSERT INTO db1.o SELECT * FROM t",
				"CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM db1.o JOIN t USING (a)",
				"CREATE VIEW db1.v AS SELECT * FROM db1.o JOIN t USING (a)",
				"CREATE MATERIALIZED VIEW db1.mv TO db1.x AS SELECT * FROM t",
			} {
				t.Run(fmt.Sprintf("si=%v/ctx=%q/%s", si, ctx, sql), func(t *testing.T) {
					resp, err := doRewrite(e, sql, opts)
					if err != nil {
						t.Fatalf("doRewrite: %v", err)
					}
					if !strings.HasPrefix(resp.GetMessage(), "unqualified table ") {
						t.Fatalf("got %s %q, want the unresolved-unqualified refusal", resp.GetCode(), resp.GetMessage())
					}
					if len(resp.GetTableRewrites()) != 0 {
						t.Fatalf("table_rewrites = %v, want empty", resp.GetTableRewrites())
					}
				})
			}
		}
	}
}

// TestTableRef_UnresolvedUnqualifiedPrecedence pins where the refusal sits:
// when several names are unresolved, the first in document order is named;
// a storage-integrity read-surface refusal outranks it; and an unqualified IN
// operand under V2 gets the SI namespace refusal first only when the
// session's execution database (physical context, then logical) cannot be
// resolved. The table name is inserted without escaping, like T6.
func TestTableRef_UnresolvedUnqualifiedPrecedence(t *testing.T) {
	e := newEngine(t)
	msg := func(table string) string {
		return `unqualified table "` + table + `" does not resolve through the session's logical database`
	}
	phys := "phys"
	type c struct {
		name, sql string
		si, phys  bool
		code      pb.RewriteCode
		message   string
	}
	invalid := pb.RewriteCode_InvalidRewriteRequest
	cases := []c{
		{"first_of_two_from", "SELECT * FROM b JOIN a USING (x)", false, true, invalid, msg("b")},
		{"subquery_before_join", "SELECT * FROM (SELECT * FROM inner_t) AS q JOIN outer_t USING (a)", false, true, invalid, msg("inner_t")},
		{"select_list_before_from", "SELECT (SELECT max(a) FROM s) FROM f", false, true, invalid, msg("s")},
		{"cte_body_before_main", "WITH c AS (SELECT * FROM x) SELECT * FROM y JOIN c USING (a)", false, true, invalid, msg("x")},
		{"from_before_in", "SELECT * FROM f WHERE a IN (SELECT a FROM s)", false, true, invalid, msg("f")},
		{"union_first_arm", "SELECT a FROM u1 UNION ALL SELECT a FROM u2", false, true, invalid, msg("u1")},
		{"si_final_outranks", "SELECT * FROM t2 JOIN db1.t FINAL USING (a)", true, true, pb.RewriteCode_RewriteError,
			"FINAL/SAMPLE/PREWHERE/WITH OFFSET/column aliases on storage-integrity tables are not supported"},
		{"v2_in_without_physical_context", "SELECT * FROM db1.o WHERE a IN t", true, false, pb.RewriteCode_RewriteError,
			"storage-integrity IN table target namespace is not statically resolvable"},
		{"v2_in_with_physical_context", "SELECT * FROM db1.o WHERE a IN t", true, true, invalid, msg("t")},
		{"quote_not_escaped", "SELECT * FROM `a\"b`", false, true, invalid, msg(`a"b`)},
	}
	for _, k := range cases {
		for _, ctx := range []string{"", "db9"} {
			t.Run(fmt.Sprintf("%s/ctx=%q", k.name, ctx), func(t *testing.T) {
				dyn := tablerefDynamic(k.si)
				dyn.UpstreamLogicalDatabaseInContext = ctx
				if k.phys {
					dyn.UpstreamPhysicalDatabaseInContext = &phys
				}
				resp, err := doRewrite(e, k.sql, []*pb.RewriteOption{tableRewriteDynamic(dyn)})
				if err != nil {
					t.Fatalf("doRewrite: %v", err)
				}
				if resp.GetCode() != k.code || resp.GetMessage() != k.message {
					t.Fatalf("got %s %q, want %s %q", resp.GetCode(), resp.GetMessage(), k.code, k.message)
				}
			})
		}
	}
}

// TestTableRef_UnresolvedNameDoesNotStripSIWriteRefusalMap pins that the
// empty-table_rewrites rule applies only when the final rejection is the
// unresolved-name refusal: a view or MV body that reads an authorized SI table
// is refused by the SI write refusal, and its table_rewrites must be the same
// whether or not the body also names an unresolved table.
func TestTableRef_UnresolvedNameDoesNotStripSIWriteRefusalMap(t *testing.T) {
	e := newEngine(t)
	phys := "phys"
	const siWrite = "storage-integrity table db1.t accepts writes only through the signed statement lane"
	for _, ctx := range []string{"", "db9"} {
		dyn := tablerefDynamic(true)
		dyn.UpstreamLogicalDatabaseInContext = ctx
		dyn.UpstreamPhysicalDatabaseInContext = &phys
		opts := []*pb.RewriteOption{tableRewriteDynamic(dyn)}
		for _, pair := range [][2]string{
			{"CREATE VIEW db1.v AS SELECT * FROM db1.t", "CREATE VIEW db1.v AS SELECT * FROM db1.t JOIN t2 USING (a)"},
			{"CREATE MATERIALIZED VIEW db1.mv TO db1.x AS SELECT * FROM db1.t",
				"CREATE MATERIALIZED VIEW db1.mv TO db1.x AS SELECT * FROM db1.t JOIN t2 USING (a)"},
		} {
			t.Run(fmt.Sprintf("ctx=%q/%s", ctx, pair[1]), func(t *testing.T) {
				var maps [2]map[string]string
				for i, sql := range pair {
					resp, err := doRewrite(e, sql, opts)
					if err != nil {
						t.Fatalf("doRewrite(%q): %v", sql, err)
					}
					if resp.GetCode() != pb.RewriteCode_UnsupportedStatement || resp.GetMessage() != siWrite {
						t.Fatalf("%q: got %s %q, want the SI write refusal", sql, resp.GetCode(), resp.GetMessage())
					}
					maps[i] = resp.GetTableRewrites()
				}
				if maps[0]["db1.t"] != "hg_safe.db1__t" {
					t.Fatalf("baseline table_rewrites = %v, want db1.t → hg_safe.db1__t", maps[0])
				}
				if !reflect.DeepEqual(maps[0], maps[1]) {
					t.Fatalf("table_rewrites = %v, want %v (same as without the unresolved name)", maps[1], maps[0])
				}
			})
		}
	}
}

// TestTableRef_ViewColumnListIsWalkedLikeATable pins that a view's column
// list is governed by the same rules as a CREATE TABLE column list (spec
// 2026-09-26 R2 / T2 / T3 / T5). Polyglot keeps a view's column definitions
// under create_view.schema (a CREATE TABLE keeps them under columns /
// constraints), and an INDEX / PROJECTION / PRIMARY KEY item as an opaque raw
// node, so the walker used to skip the whole list: `CREATE MATERIALIZED VIEW
// db1.mv (a UInt8 DEFAULT a IN phys.x) ENGINE = Memory AS …` answered Success.
// Measured on ClickHouse 26.2, a view or materialized view accepts an
// IN-table operand in a column DEFAULT / MATERIALIZED / ALIAS / EPHEMERAL /
// TTL expression, an INDEX expression and a PROJECTION select list: the table
// is resolved at CREATE time (an unknown table fails the CREATE), and an ALIAS
// column reads it on every SELECT. Every row's answer is the one the same
// column list gets in a CREATE TABLE, which the test pins alongside.
func TestTableRef_ViewColumnListIsWalkedLikeATable(t *testing.T) {
	const (
		t7    = "statement is not supported"
		t2    = "query parameters are not supported in a database or table position"
		siPhy = "storage-integrity physical table hg_safe.db1__t is not directly addressable"
	)
	t3 := func(db string) string { return "protected database " + db + " is not addressable" }
	type answer struct {
		code pb.RewriteCode
		msg  string
	}
	inv := func(msg string) answer { return answer{pb.RewriteCode_InvalidRewriteRequest, msg} }
	uns := func(msg string) answer { return answer{pb.RewriteCode_UnsupportedStatement, msg} }
	rows := []struct {
		col     string
		off, on answer // SI surface inactive / active
	}{
		{"a UInt8 DEFAULT (SELECT count() FROM phys.x)", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8 DEFAULT (SELECT count() FROM hg_safe.db1__t)", inv(t3("hg_safe")), uns(siPhy)},
		{"a UInt8 DEFAULT (SELECT count() FROM db1.p)", uns(t7), uns(t7)},
		{"a UInt8 DEFAULT (SELECT count() FROM db1.t)", uns(t7), uns(t7)},
		{"a UInt8 DEFAULT (SELECT count() FROM `db2.x`)", uns(t7), uns(t7)},
		{"a UInt8 DEFAULT (SELECT count() FROM {p:Identifier})", inv(t2), inv(t2)},
		{"a UInt8 DEFAULT (SELECT count() FROM remote('127.0.0.1','phys','x'))", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8 DEFAULT (SELECT count() FROM merge('phys','^x'))", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8 DEFAULT (SELECT count() FROM remote('127.0.0.1','db1','x'))", uns("table function remote is not accepted"),
			uns("storage-integrity logical database db1 is not directly addressable through remote table function")},
		{"a UInt8 DEFAULT (SELECT count() FROM numbers(10))", uns(t7), uns(t7)},
		{"a UInt8 DEFAULT 1 IN phys.x", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8 DEFAULT 1 IN `db2.x`", uns(t7), uns(t7)},
		{"a UInt8 DEFAULT 1 IN (SELECT a FROM db1.p)", uns(t7), uns(t7)},
		{"a UInt8 DEFAULT 1 IN {p:Identifier}", inv(t2), inv(t2)},
		{"a UInt8 DEFAULT 1 IN hg_safe.db1__t", inv(t3("hg_safe")), uns(siPhy)},
		{"a UInt8 DEFAULT 1 IN db1.t", uns(t7), uns("storage-integrity table db1.t is not directly addressable through IN table target")},
		{"a UInt8 DEFAULT 1 IN `db1.t`", uns(t7), uns(t7)},
		{"a UInt8 DEFAULT in(1, phys.x)", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8 MATERIALIZED (SELECT count() FROM phys.x)", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8 MATERIALIZED (SELECT count() FROM {p:Identifier})", inv(t2), inv(t2)},
		{"a UInt8 MATERIALIZED (SELECT count() FROM db1.p)", uns(t7), uns(t7)},
		{"a UInt8 MATERIALIZED 1 IN db1.p", uns(t7), uns("storage-integrity logical database db1 is not directly addressable through IN table target")},
		{"a UInt8 ALIAS (SELECT count() FROM phys.x)", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8 ALIAS (SELECT count() FROM hg_safe.db1__t)", inv(t3("hg_safe")), uns(siPhy)},
		{"a UInt8 ALIAS (SELECT count() FROM remote('127.0.0.1','phys','x'))", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8 ALIAS (SELECT count() FROM db1.p)", uns(t7), uns(t7)},
		{"a UInt8 ALIAS 1 IN phys.x", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8 EPHEMERAL (SELECT count() FROM phys.x)", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8, d DateTime TTL d + INTERVAL (SELECT count() FROM phys.x) DAY", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8, b UInt8 DEFAULT (SELECT count() FROM phys.x)", inv(t3("phys")), inv(t3("phys"))},
		// Opaque INDEX / PROJECTION items: Polyglot keeps them as raw text.
		{"a UInt8, INDEX i a IN phys.x TYPE minmax", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8, INDEX i a IN (SELECT a FROM phys.x) TYPE minmax", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8, INDEX i (a IN phys.x) TYPE set(0)", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8, INDEX i a IN {p:Identifier} TYPE minmax", inv(t2), inv(t2)},
		{"a UInt8, INDEX i a IN `db2.x` TYPE minmax", uns(t7), uns(t7)},
		{"a UInt8, PROJECTION p (SELECT a IN phys.x ORDER BY a)", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8, PROJECTION p (SELECT a FROM phys.x)", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8, PROJECTION p (SELECT a IN {p:Identifier} ORDER BY a)", inv(t2), inv(t2)},
		{"a UInt8, PROJECTION p (SELECT a IN hg_safe.db1__t ORDER BY a)", inv(t3("hg_safe")), uns(siPhy)},
		// Already refused before this fix (string lookups are found anywhere).
		{"a UInt8 DEFAULT joinGet('phys.x', 'a', 1)", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8 DEFAULT hasColumnInTable('phys', 'x', 'a')", inv(t3("phys")), inv(t3("phys"))},
		{"a UInt8 DEFAULT dictGet('db1.d', 'a', toUInt64(1))", inv(`dictGet target "db1.d" does not resolve through the caller's databases`),
			inv(`dictGet target "db1.d" does not resolve through the caller's databases`)},
	}
	views := []string{
		"CREATE VIEW db1.v (%s) AS SELECT 1 AS a",
		"CREATE OR REPLACE VIEW db1.v (%s) AS SELECT 1 AS a",
		"CREATE MATERIALIZED VIEW db1.mv (%s) ENGINE = Memory AS SELECT 1 AS a",
		"CREATE MATERIALIZED VIEW db1.mv (%s) ENGINE = MergeTree ORDER BY a AS SELECT 1 AS a",
		"CREATE MATERIALIZED VIEW db1.mv TO db1.o (%s) AS SELECT 1 AS a",
		"CREATE MATERIALIZED VIEW db1.mv (%s) ENGINE = Memory POPULATE AS SELECT 1 AS a",
		"CREATE MATERIALIZED VIEW IF NOT EXISTS db1.mv ON CLUSTER c (%s) ENGINE = Memory AS SELECT 1 AS a",
	}
	e := newEngine(t)
	check := func(t *testing.T, sql string, si bool, want answer) {
		t.Helper()
		resp, err := doRewrite(e, sql, tablerefOpts(si))
		if err != nil {
			t.Fatalf("doRewrite: %v", err)
		}
		if resp.GetCode() != want.code || resp.GetMessage() != want.msg {
			t.Fatalf("got %s %q, want %s %q", resp.GetCode(), resp.GetMessage(), want.code, want.msg)
		}
		if resp.GetSqlAfterRewrite() != sql {
			t.Fatalf("sql = %q, want the input echoed", resp.GetSqlAfterRewrite())
		}
	}
	for _, si := range []bool{false, true} {
		for _, r := range rows {
			want := r.off
			if si {
				want = r.on
			}
			table := fmt.Sprintf("CREATE TABLE db1.n (%s) ENGINE = Memory", r.col)
			t.Run(fmt.Sprintf("si=%v/%s", si, table), func(t *testing.T) { check(t, table, si, want) })
			for _, tmpl := range views {
				sql := fmt.Sprintf(tmpl, r.col)
				t.Run(fmt.Sprintf("si=%v/%s", si, sql), func(t *testing.T) { check(t, sql, si, want) })
			}
		}
	}
}

// TestTableRef_ViewColumnListReadFreeShapesStayAccepted pins that a view
// column list without a read keeps the same CREATE TABLE column list's
// answer when Polyglot regenerates it faithfully: Success, generated SQL
// pinned. A read-free column whose DEFAULT / MATERIALIZED / ALIAS / CODEC /
// COMMENT the generator drops is refused instead, by the mid-statement drop
// gate (see AGENTS.md), because forwarding the regenerated statement would
// silently change what ClickHouse creates.
func TestTableRef_ViewColumnListReadFreeShapesStayAccepted(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, want string }{
			{"CREATE VIEW db1.v (a, b) AS SELECT 1, 2",
				`CREATE VIEW phys."db1.v" (a, b) AS SELECT 1, 2`},
			{"CREATE MATERIALIZED VIEW db1.mv (a UInt8, INDEX i a TYPE minmax) ENGINE = MergeTree ORDER BY a AS SELECT 1 AS a",
				`CREATE MATERIALIZED VIEW phys."db1.mv" (a UInt8, INDEX i a TYPE minmax) ENGINE=MergeTree ORDER BY a AS SELECT 1 AS a`},
			{"CREATE MATERIALIZED VIEW db1.mv (a UInt8, b UInt8, PROJECTION p (SELECT a, b ORDER BY a)) ENGINE = MergeTree ORDER BY a AS SELECT 1 AS a, 2 AS b",
				`CREATE MATERIALIZED VIEW phys."db1.mv" (a UInt8, b UInt8, PROJECTION p (SELECT a, b ORDER BY a)) ENGINE=MergeTree ORDER BY a AS SELECT 1 AS a, 2 AS b`},
			{"CREATE MATERIALIZED VIEW db1.mv (a UInt8, PRIMARY KEY a) ENGINE = MergeTree AS SELECT 1 AS a",
				`CREATE MATERIALIZED VIEW phys."db1.mv" (a UInt8, PRIMARY KEY a) ENGINE=MergeTree AS SELECT 1 AS a`},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
		for _, sql := range []string{
			"CREATE MATERIALIZED VIEW db1.mv (a UInt8 DEFAULT 1) ENGINE = Memory AS SELECT 1 AS a",
			"CREATE MATERIALIZED VIEW db1.mv (a UInt8 DEFAULT (SELECT 1)) ENGINE = Memory AS SELECT 1 AS a",
			"CREATE MATERIALIZED VIEW db1.mv (a UInt8 MATERIALIZED 1 IN (1, 2)) ENGINE = Memory AS SELECT 1 AS a",
			"CREATE MATERIALIZED VIEW db1.mv TO db1.o (a UInt8 ALIAS 1 IN tuple(1, 2), b UInt8 CODEC(ZSTD(1))) AS SELECT 1 AS a",
			"CREATE VIEW db1.v (a UInt8 COMMENT 'x', b String) AS SELECT 1 AS a, 'b' AS b",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"})
		}
	}
	runTablerefCases(t, cases)
}

// TestTableRef_ViewColumnListResiduals pins the view column-list answers that
// differ from the same CREATE TABLE column list, and the view kinds refused
// as a class whatever their column list holds.
//
//   - An INDEX / PROJECTION item is opaque text to the view pipeline, so a
//     logical table it names reaches the R2 text refusal rather than the SI
//     IN-target message a CREATE TABLE gets while the SI surface is active.
//     Both refuse the statement.
//   - Polyglot cannot parse a CONSTRAINT in a view's column list at all.
//   - CREATE LIVE VIEW / WINDOW VIEW and CREATE DICTIONARY are refused as a
//     class (a dictionary's attribute list is a CREATE TABLE column list to
//     Polyglot, so T2 / T3 / R2 name it first).
func TestTableRef_ViewColumnListResiduals(t *testing.T) {
	const (
		t7         = "statement is not supported"
		liveWindow = "CREATE LIVE VIEW / WINDOW VIEW is not supported"
	)
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		add := func(sql string, code pb.RewriteCode, msg string) {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si, wantCode: code, wantMsg: msg, wantSQL: sql})
		}
		add("CREATE MATERIALIZED VIEW db1.mv (a UInt8, PROJECTION p (SELECT a IN db1.p ORDER BY a)) ENGINE = MergeTree ORDER BY a AS SELECT 1 AS a",
			pb.RewriteCode_UnsupportedStatement, t7)
		add("CREATE MATERIALIZED VIEW db1.mv (a UInt8, INDEX i a IN db1.t TYPE minmax) ENGINE = MergeTree ORDER BY a AS SELECT 1 AS a",
			pb.RewriteCode_UnsupportedStatement, t7)
		cases = append(cases, tablerefCase{name: "view CONSTRAINT", si: si,
			sql:      "CREATE MATERIALIZED VIEW db1.mv (a UInt8, CONSTRAINT c CHECK a IN phys.x) ENGINE = Memory AS SELECT 1 AS a",
			wantCode: pb.RewriteCode_SyntaxError})
		live := liveWindow
		if si {
			live = "storage-integrity is configured; statement class is not modelled by the rewriter and cannot be forwarded"
		}
		add("CREATE LIVE VIEW db1.lv (a UInt8, b UInt8 ALIAS a IN phys.x) AS SELECT 1 AS a", pb.RewriteCode_UnsupportedStatement, live)
		add("CREATE WINDOW VIEW db1.wv (a UInt8, b UInt8 ALIAS a IN phys.x) ENGINE = Memory AS SELECT 1 AS a", pb.RewriteCode_UnsupportedStatement, liveWindow)
		add("CREATE DICTIONARY db1.d (a UInt64, b UInt8 DEFAULT 0 EXPRESSION a IN phys.x) PRIMARY KEY a SOURCE(NULL()) LAYOUT(FLAT()) LIFETIME(0)",
			pb.RewriteCode_InvalidRewriteRequest, "protected database phys is not addressable")
		add("CREATE DICTIONARY db1.d (a UInt64, b UInt8 DEFAULT 0) PRIMARY KEY a SOURCE(NULL()) LAYOUT(FLAT()) LIFETIME(0)",
			pb.RewriteCode_UnsupportedStatement, "CREATE DICTIONARY is not supported")
	}
	runTablerefCases(t, cases)
}
