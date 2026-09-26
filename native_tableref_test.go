package rewriter

import (
	"reflect"
	"strings"
	"testing"

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
	runTablerefCases(t, []tablerefCase{
		{name: "exists_line_comment_slash_slash", sql: "EXISTS TABLE // it's\n db1.{p:Identifier}",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: msg},
		{name: "exists_line_comment_hash", sql: "EXISTS TABLE # it's\n db1.{p:Identifier}",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: msg},
		{name: "describe_line_comment_slash_slash", sql: "DESCRIBE TABLE // it's\n db1.{p:Identifier}",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: msg},
		{name: "show_create_line_comment_hash_bang", sql: "SHOW CREATE TABLE #! it's\n db1.{p:Identifier}",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: msg},
	})
}

func TestTableRef_ValueAndColumnParametersStayAllowed(t *testing.T) {
	runTablerefCases(t, []tablerefCase{
		{name: "value", sql: "SELECT * FROM db1.o WHERE a = {v:UInt64}", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a = {v: UInt64}`},
		{name: "column", sql: "SELECT {c:Identifier} FROM db1.o", wantCode: pb.RewriteCode_Success},
		// Controller review (task-3 fix round), ruling 2: an ALTER ... UPDATE
		// statement's assignment/predicate tail is a column/value position, not
		// a table position -- only the target (between ALTER TABLE and UPDATE)
		// is refused. The target itself still rewrites normally.
		{name: "alter_update_assignment", sql: "ALTER TABLE db1.o UPDATE a = {c:Identifier} WHERE 1", wantCode: pb.RewriteCode_Success,
			wantSQL: "ALTER TABLE phys.`db1.o` UPDATE a = {c:Identifier} WHERE 1"},
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
			//     qualifier is a string-lookup argument, and the parenthesized
			//     IN shape is not is_field-tagged -- no existing SI handler in
			//     this repo classifies either position as a table reference,
			//     so nothing downstream would otherwise reject them.
			//     PreflightTableReferences now rejects a protected hit there
			//     unconditionally too (engine.CollectSIHandlerBlindDatabaseReferences),
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
				strings.Contains(shape, "hasColumnInTable") || strings.Contains(shape, "IN (%s.")
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
		{name: "ctas empty drops the body", sql: "CREATE TABLE db1.n ENGINE = Memory EMPTY AS SELECT * FROM db1.p", wantCode: pb.RewriteCode_Success,
			wantSQL: `CREATE TABLE phys."db1.n" ENGINE=Memory`, wantAcc: []string{"db1.n"}},
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
		{name: "set passes when inactive", sql: "SET max_threads = 1", wantCode: pb.RewriteCode_Success, wantSQL: "SET max_threads = 1"},
		{name: "set refused under V2", sql: "SET max_threads = 1", si: true, wantCode: pb.RewriteCode_UnsupportedStatement},
		{name: "select 1", sql: "SELECT 1", wantCode: pb.RewriteCode_Success},
	})
}
