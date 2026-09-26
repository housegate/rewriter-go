package rewriter

import (
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
		{name: "cte alias untouched", sql: "WITH c AS (SELECT 1 AS a) SELECT * FROM db1.o WHERE a IN c", wantCode: pb.RewriteCode_Success,
			wantSQL: `WITH c AS (SELECT 1 AS a) SELECT * FROM phys."db1.o" "db1.o" WHERE a IN c`, wantAcc: []string{"db1.o"}},
		{name: "system stays", sql: "SELECT * FROM db1.o WHERE a IN system.tables", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a IN system.tables`, wantAcc: []string{"db1.o", "system.tables"}},
	})
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
