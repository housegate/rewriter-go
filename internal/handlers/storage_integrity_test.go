package handlers

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

func siDyn(mode pb.StorageIntegrityArgs_ReadMode, excluded ...string) *pb.RewriteTableDynamicArgs {
	return &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"db1": "phys", "other": "phys"},
		KnownPhysicalDatabases: []string{"phys"},
		Delim:                  "_",
		StorageIntegrity: &pb.StorageIntegrityArgs{
			Tables: map[string]*pb.StorageIntegrityArgs_Table{
				"db1.t": {SafeTable: "hg_safe.db1__t", UnsafeTable: "hg_unsafe.db1__t", ExcludedUnsafeParts: excluded},
			},
			ReadMode: mode,
		},
	}
}

func TestStorageIntegritySurfaceSQL(t *testing.T) {
	tbl := &pb.StorageIntegrityArgs_Table{SafeTable: "hg_safe.db1__t", UnsafeTable: "hg_unsafe.db1__t"}
	safe := storageIntegritySurfaceSQL(tbl, &pb.StorageIntegrityArgs{ReadMode: pb.StorageIntegrityArgs_READ_MODE_SAFE})
	if safe != "SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t" {
		t.Fatalf("safe = %q", safe)
	}
	unspecified := storageIntegritySurfaceSQL(tbl, &pb.StorageIntegrityArgs{})
	if unspecified != safe {
		t.Fatalf("UNSPECIFIED must behave as SAFE: %q", unspecified)
	}
	tbl.ExcludedUnsafeParts = []string{"all_1_1_0", "it's"}
	unsafe := storageIntegritySurfaceSQL(tbl, &pb.StorageIntegrityArgs{ReadMode: pb.StorageIntegrityArgs_READ_MODE_UNSAFE_LATEST, ReservedRowIdColumn: "_rid"})
	want := "SELECT * EXCEPT (`_rid`) FROM hg_safe.db1__t UNION ALL SELECT * EXCEPT (`_rid`) FROM hg_unsafe.db1__t WHERE _part NOT IN ('all_1_1_0', 'it''s')"
	if unsafe != want {
		t.Fatalf("unsafe_latest = %q\nwant %q", unsafe, want)
	}
	tbl.ExcludedUnsafeParts = nil
	if got := storageIntegritySurfaceSQL(tbl, &pb.StorageIntegrityArgs{ReadMode: pb.StorageIntegrityArgs_READ_MODE_UNSAFE_LATEST}); strings.Contains(got, "WHERE") {
		t.Fatalf("empty exclusion list must omit WHERE: %q", got)
	}
}

func TestStorageIntegrityReservedKeywordIsQuoted(t *testing.T) {
	e := newEngine(t)
	dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
	dyn.StorageIntegrity.ReservedRowIdColumn = "from"
	ast, _ := e.ParseOne("SELECT a FROM db1.t")
	resp, err := RewriteSelect(e, ast, dynOpt(dyn))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || !strings.Contains(resp.GetSqlAfterRewrite(), `EXCEPT ("from")`) {
		t.Fatalf("code=%v sql=%q msg=%q", resp.GetCode(), resp.GetSqlAfterRewrite(), resp.GetMessage())
	}
}

func TestRewriteSelect_storageIntegritySafe(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT a FROM db1.t AS x JOIN other.u AS y ON x.id = y.id")
	resp, err := RewriteSelect(e, ast, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Code != pb.RewriteCode_Success {
		t.Fatalf("code = %v (%s)", resp.Code, resp.Message)
	}
	want := `SELECT a FROM (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t) AS x JOIN phys."other.u" AS y ON x.id = y.id`
	if resp.SqlAfterRewrite != want {
		t.Fatalf("sql = %q\nwant %q", resp.SqlAfterRewrite, want)
	}
	if !mapEq(resp.TableRewrites, map[string]string{"db1.t": "hg_safe.db1__t", "other.u": "phys.other.u"}) {
		t.Fatalf("table_rewrites = %v", resp.TableRewrites)
	}
	var si, plain bool
	for _, a := range resp.OriginalAccessedTables {
		switch a.OriginalTable {
		case "t":
			si = a.IsStorageIntegrity && a.LogicalDatabase == "db1" && a.PhysicalDatabase == "phys"
		case "u":
			plain = !a.IsStorageIntegrity
		}
	}
	if !si || !plain {
		t.Fatalf("accessed = %+v", resp.OriginalAccessedTables)
	}
}

func TestRewriteSelect_storageIntegrityUnsafeLatest(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT a FROM db1.t")
	resp, err := RewriteSelect(e, ast, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_UNSAFE_LATEST, "all_1_1_0", "all_2_2_0")))
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT a FROM (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t UNION ALL SELECT * EXCEPT (_hg_row_id) FROM hg_unsafe.db1__t WHERE _part NOT IN ('all_1_1_0', 'all_2_2_0')) AS "db1.t"`
	if resp.SqlAfterRewrite != want {
		t.Fatalf("sql = %q\nwant %q", resp.SqlAfterRewrite, want)
	}
}

func TestRewriteSelect_reservedColumnRejected(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{
		"SELECT _hg_row_id FROM db1.t",
		"SELECT db1.t._hg_row_id FROM db1.t",
		"SELECT * REPLACE (1 AS _hg_row_id) FROM db1.t",
		"SELECT * RENAME (a AS _hg_row_id) FROM db1.t",
		"SELECT * FROM db1.t AS a JOIN other.u AS b USING (_hg_row_id)",
		"SELECT a FROM db1.t WHERE _hg_row_id = 'x'",
		"SELECT a FROM other.u WHERE a IN (SELECT _hg_row_id FROM db1.t)",
	} {
		ast, _ := e.ParseOne(sql)
		resp, err := RewriteSelect(e, ast, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Code != pb.RewriteCode_RewriteError || resp.Message != "reserved column _hg_row_id is not addressable" {
			t.Fatalf("%q: code=%v msg=%q", sql, resp.Code, resp.Message)
		}
		if resp.SqlAfterRewrite != "" {
			t.Fatalf("%q: reject must leave sql_after_rewrite empty for finalize to echo the input, got %q", sql, resp.SqlAfterRewrite)
		}
		if len(resp.OriginalAccessedTables) == 0 {
			t.Fatalf("%q: accessed must be recorded before the reject", sql)
		}
	}
}

func TestRewriteSelect_reservedColumnOnNonSITableAllowed(t *testing.T) {
	e := newEngine(t)
	ast, _ := e.ParseOne("SELECT _hg_row_id FROM other.u")
	resp, err := RewriteSelect(e, ast, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Code != pb.RewriteCode_Success {
		t.Fatalf("statement touching no SI table must not be guarded: %v %s", resp.Code, resp.Message)
	}
}

func TestRewriteSelect_storageIntegrityModifiersRejected(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{
		"SELECT a FROM db1.t FINAL",
		"SELECT a FROM db1.t SAMPLE 0.1",
		"SELECT * FROM db1.t WITH OFFSET AS off",
		"SELECT * FROM db1.t WITH\nOFFSET AS off",
		"SELECT * FROM db1.t WITH\tOFFSET AS off",
		"SELECT * FROM db1.t AS x(a)",
	} {
		t.Run(sql, func(t *testing.T) {
			ast, err := e.ParseOne(sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := RewriteSelect(e, ast, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)), sql)
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetCode() != pb.RewriteCode_RewriteError ||
				resp.GetMessage() != "FINAL/SAMPLE/WITH OFFSET/column aliases on storage-integrity tables are not supported" {
				t.Fatalf("code=%v msg=%q ast=%s", resp.GetCode(), resp.GetMessage(), ast)
			}
			if len(resp.GetOriginalAccessedTables()) != 1 ||
				!resp.GetOriginalAccessedTables()[0].GetIsStorageIntegrity() {
				t.Fatalf("accessed=%+v, want one SI-flagged target", resp.GetOriginalAccessedTables())
			}
		})
	}
}

func TestRewriteSelect_storageIntegrityWithOffsetLiteralAllowed(t *testing.T) {
	e := newEngine(t)
	sql := "SELECT 'WITH OFFSET' FROM db1.t"
	ast, err := e.ParseOne(sql)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := RewriteSelect(e, ast, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)), sql)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || !strings.Contains(resp.GetSqlAfterRewrite(), "hg_safe.db1__t") {
		t.Fatalf("code=%v msg=%q sql=%q", resp.GetCode(), resp.GetMessage(), resp.GetSqlAfterRewrite())
	}
}

func TestStorageIntegrityReadFastPathsRequireAuthorizedLogicalDatabase(t *testing.T) {
	e := newEngine(t)
	dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
	dyn.DatabaseMap = map[string]string{}
	opts := dynOpt(dyn)

	selectAST, _ := e.ParseOne("SELECT a FROM db1.t")
	selectResp, err := RewriteSelect(e, selectAST, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertUnauthorizedSI := func(name string, resp *pb.RewriteSQLResponse) {
		t.Helper()
		if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest ||
			resp.GetMessage() != "storage-integrity logical database db1 is not authorized by database_map" {
			t.Fatalf("%s: code=%v msg=%q", name, resp.GetCode(), resp.GetMessage())
		}
		if len(resp.GetOriginalAccessedTables()) != 1 ||
			!resp.GetOriginalAccessedTables()[0].GetIsStorageIntegrity() {
			t.Fatalf("%s: accessed=%+v, want one SI-flagged target", name, resp.GetOriginalAccessedTables())
		}
	}
	assertUnauthorizedSI("SELECT", selectResp)

	for _, sql := range []string{"EXISTS TABLE db1.t", "DESCRIBE TABLE db1.t"} {
		ast, _ := e.ParseOne(sql)
		var resp *pb.RewriteSQLResponse
		var handled bool
		if strings.HasPrefix(sql, "EXISTS") {
			resp, handled, err = RewriteExistsShowCreate(e, ast, sql, opts)
		} else {
			resp, handled, err = RewriteDescribe(e, ast, sql, opts)
		}
		if err != nil || !handled {
			t.Fatalf("%s: handled=%v err=%v", sql, handled, err)
		}
		assertUnauthorizedSI(sql, resp)
	}
}

func TestDescribeStorageIntegrityOnlyAcceptsTableObject(t *testing.T) {
	e := newEngine(t)
	opts := dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE))
	for _, sql := range []string{
		"DESCRIBE DATABASE db1.t",
		"DESCRIBE VIEW db1.t",
		"DESCRIBE DICTIONARY db1.t",
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatal(err)
		}
		resp, handled, err := RewriteDescribe(e, ast, sql, opts)
		if err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", sql, handled, err)
		}
		if resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
			!strings.Contains(resp.GetMessage(), "only DESCRIBE TABLE is allowed") {
			t.Fatalf("%q: code=%v msg=%q", sql, resp.GetCode(), resp.GetMessage())
		}
		if len(resp.GetOriginalAccessedTables()) != 0 {
			t.Fatalf("%q: non-table object must not be recorded as SI: %+v", sql, resp.GetOriginalAccessedTables())
		}
	}
}

func TestStorageIntegrityPhysicalNamespaceRejected(t *testing.T) {
	e := newEngine(t)
	opts := dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE))
	for _, sql := range []string{
		"SELECT * FROM hg_safe.db1__t",
		"SELECT * FROM hg_unsafe.db1__t",
		"SELECT _hg_row_id FROM hg_safe.db1__t",
	} {
		ast, _ := e.ParseOne(sql)
		resp, err := RewriteSelect(e, ast, opts)
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetCode() != pb.RewriteCode_RewriteError ||
			!strings.Contains(resp.GetMessage(), "storage-integrity physical table") {
			t.Fatalf("%q: code=%v msg=%q", sql, resp.GetCode(), resp.GetMessage())
		}
		if len(resp.GetOriginalAccessedTables()) != 1 ||
			!resp.GetOriginalAccessedTables()[0].GetIsStorageIntegrity() {
			t.Fatalf("%q: accessed=%+v, want one SI-flagged target", sql, resp.GetOriginalAccessedTables())
		}
	}

	for _, sql := range []string{
		"DROP TABLE hg_safe.db1__t",
		"INSERT INTO hg_unsafe.db1__t VALUES (1)",
	} {
		ast, _ := e.ParseOne(sql)
		resp, handled, err := RewriteWrite(e, ast, sql, opts)
		if err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", sql, handled, err)
		}
		if resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
			!strings.Contains(resp.GetMessage(), "storage-integrity physical table") {
			t.Fatalf("%q: code=%v msg=%q", sql, resp.GetCode(), resp.GetMessage())
		}
		if len(resp.GetOriginalAccessedTables()) != 1 ||
			!resp.GetOriginalAccessedTables()[0].GetIsStorageIntegrity() {
			t.Fatalf("%q: accessed=%+v, want one SI-flagged target", sql, resp.GetOriginalAccessedTables())
		}
	}

	for _, sql := range []string{
		"EXISTS TABLE hg_safe.db1__t",
		"SHOW CREATE TABLE hg_unsafe.db1__t",
		"DESCRIBE TABLE hg_safe.db1__t",
	} {
		ast, _ := e.ParseOne(sql)
		var resp *pb.RewriteSQLResponse
		var handled bool
		var err error
		if strings.HasPrefix(sql, "DESCRIBE") {
			resp, handled, err = RewriteDescribe(e, ast, sql, opts)
		} else {
			resp, handled, err = RewriteExistsShowCreate(e, ast, sql, opts)
		}
		if err != nil || !handled || resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
			len(resp.GetOriginalAccessedTables()) != 1 || !resp.GetOriginalAccessedTables()[0].GetIsStorageIntegrity() {
			t.Fatalf("%q: handled=%v err=%v code=%v msg=%q accessed=%+v", sql, handled, err, resp.GetCode(), resp.GetMessage(), resp.GetOriginalAccessedTables())
		}
	}
}

func TestWrites_storageIntegrityRejectsNonLane(t *testing.T) {
	e := newEngine(t)
	opts := dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE))
	want := "storage-integrity table db1.t accepts writes only through the signed statement lane"
	for _, sql := range []string{
		"ALTER TABLE db1.t DELETE WHERE a = 1",
		"DROP TABLE db1.t",
		"TRUNCATE TABLE db1.t",
		"CREATE TABLE db1.t (a UInt32) ENGINE = MergeTree ORDER BY a",
		"CREATE TABLE db1.t2 AS db1.t",
		"RENAME TABLE db1.t TO db1.t2",
		"EXCHANGE TABLES db1.t AND db1.t2",
		"ALTER TABLE db1.t UPDATE a = 1 WHERE a = 2",
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		resp, handled, err := RewriteWrite(e, ast, sql, opts)
		if err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", sql, handled, err)
		}
		if resp.Code != pb.RewriteCode_UnsupportedStatement || resp.Message != want {
			t.Fatalf("%q: code=%v msg=%q", sql, resp.Code, resp.Message)
		}
		var sawSI bool
		for _, a := range resp.OriginalAccessedTables {
			sawSI = sawSI || a.GetIsStorageIntegrity()
		}
		if !sawSI {
			t.Fatalf("%q: SI access must be recorded before the reject: %+v", sql, resp.OriginalAccessedTables)
		}
	}
}

func TestWrites_storageIntegrityInsertRewritesLikeToday(t *testing.T) {
	e := newEngine(t)
	sql := "INSERT INTO db1.t (a) VALUES (1)"
	ast, _ := e.ParseOne(sql)
	resp, handled, err := RewriteWrite(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.Code != pb.RewriteCode_Success {
		t.Fatalf("INSERT must stay on the ordinary path (D-1): %v %s", resp.Code, resp.Message)
	}
	if resp.SqlAfterRewrite != `INSERT INTO phys."db1.t" (a) VALUES (1)` {
		t.Fatalf("sql = %q", resp.SqlAfterRewrite)
	}
	if !resp.OriginalAccessedTables[0].IsStorageIntegrity {
		t.Fatalf("accessed must carry the SI flag: %+v", resp.OriginalAccessedTables)
	}
}

func TestExists_storageIntegrityMapsToSafe(t *testing.T) {
	e := newEngine(t)
	sql := "EXISTS TABLE db1.t"
	ast, _ := e.ParseOne(sql)
	resp, handled, err := RewriteExistsShowCreate(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if resp.SqlAfterRewrite != "EXISTS TABLE hg_safe.db1__t" || resp.TableRewrites["db1.t"] != "hg_safe.db1__t" {
		t.Fatalf("sql=%q rewrites=%v", resp.SqlAfterRewrite, resp.TableRewrites)
	}
	if !resp.OriginalAccessedTables[0].IsStorageIntegrity {
		t.Fatalf("accessed = %+v", resp.OriginalAccessedTables)
	}
}

func TestShowCreate_storageIntegrityRejected(t *testing.T) {
	e := newEngine(t)
	sql := "SHOW CREATE TABLE db1.t"
	ast, _ := e.ParseOne(sql)
	resp, _, err := RewriteExistsShowCreate(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Code != pb.RewriteCode_UnsupportedStatement || resp.Message != "SHOW CREATE TABLE on storage-integrity table db1.t is not supported" {
		t.Fatalf("code=%v msg=%q", resp.Code, resp.Message)
	}
}

func TestGrant_storageIntegrityRejected(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{
		"GRANT SELECT ON db1.t TO u1",
		"REVOKE SELECT ON db1.t FROM u1",
		"GRANT SELECT(a) ON db1.t TO u1",
		"GRANT SELECT ON db1.t TO u1 WITH REPLACE OPTION",
	} {
		ast, _ := e.ParseOne(sql)
		resp, handled, err := RewriteGrant(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
		if err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", sql, handled, err)
		}
		if resp.Code != pb.RewriteCode_UnsupportedStatement || !strings.Contains(resp.Message, "storage-integrity table db1.t accepts writes only") {
			t.Fatalf("%q: code=%v msg=%q", sql, resp.Code, resp.Message)
		}
		if len(resp.OriginalAccessedTables) != 1 ||
			!resp.OriginalAccessedTables[0].GetIsStorageIntegrity() {
			t.Fatalf("%q: accessed=%+v, want one SI-flagged target", sql, resp.OriginalAccessedTables)
		}
	}
	// Database-scoped grants are not table-targeting and stay allowed.
	sql := "GRANT SELECT ON db1.* TO u1"
	ast, _ := e.ParseOne(sql)
	resp, _, _ := RewriteGrant(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
	if resp.Code != pb.RewriteCode_Success {
		t.Fatalf("db-scoped grant: code=%v msg=%q", resp.Code, resp.Message)
	}

	sql = "GRANT SELECT ON hg_safe.db1__t TO u1"
	ast, _ = e.ParseOne(sql)
	resp, _, _ = RewriteGrant(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
		!strings.Contains(resp.GetMessage(), "physical table hg_safe.db1__t is not directly addressable") ||
		len(resp.GetOriginalAccessedTables()) != 1 || !resp.GetOriginalAccessedTables()[0].GetIsStorageIntegrity() {
		t.Fatalf("direct physical grant: code=%v msg=%q accessed=%+v", resp.GetCode(), resp.GetMessage(), resp.GetOriginalAccessedTables())
	}
}

func TestWrite_storageIntegrityBareRejectsRecordTarget(t *testing.T) {
	e := newEngine(t)
	opts := dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE))
	for _, sql := range []string{
		"OPTIMIZE TABLE db1.t FINAL",
		"DETACH TABLE db1.t",
		"ATTACH TABLE db1.t",
	} {
		t.Run(sql, func(t *testing.T) {
			ast, err := e.ParseOne(sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, handled, err := RewriteWrite(e, ast, sql, opts)
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if resp.GetCode() != pb.RewriteCode_UnsupportedStatement {
				t.Fatalf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
			}
			if len(resp.GetOriginalAccessedTables()) != 1 {
				t.Fatalf("accessed=%+v, want one target", resp.GetOriginalAccessedTables())
			}
			got := resp.GetOriginalAccessedTables()[0]
			if got.GetOriginalDatabase() != "db1" || got.GetOriginalTable() != "t" ||
				got.GetLogicalDatabase() != "db1" || got.GetPhysicalDatabase() != "phys" ||
				!got.GetIsStorageIntegrity() {
				t.Fatalf("accessed=%+v, want db1.t resolved and SI-flagged", got)
			}
		})
	}
}

func TestWrite_storageIntegrityEarlyRejectsRecordTarget(t *testing.T) {
	e := newEngine(t)
	opts := dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE))
	for _, sql := range []string{
		"ALTER TABLE db1.t ATTACH PARTITION 1 FROM other.src",
		"ALTER TABLE other.u ATTACH PARTITION 1 FROM db1.t",
		"DROP TABLE db1.t, other.u",
		"DROP TABLE other.u, db1.t",
		"CREATE TABLE db1.t AS remote('h', d, x)",
	} {
		t.Run(sql, func(t *testing.T) {
			ast, err := e.ParseOne(sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, handled, err := RewriteWrite(e, ast, sql, opts)
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
				!strings.Contains(resp.GetMessage(), "storage-integrity table db1.t accepts writes only") {
				t.Fatalf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
			}
			if len(resp.GetOriginalAccessedTables()) != 1 ||
				!resp.GetOriginalAccessedTables()[0].GetIsStorageIntegrity() {
				t.Fatalf("accessed=%+v, want one SI-flagged target", resp.GetOriginalAccessedTables())
			}
		})
	}
}

func TestWrite_storageIntegrityViewBodiesRejected(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		sql  string
		mode pb.StorageIntegrityArgs_ReadMode
	}{
		{"CREATE VIEW other.v AS SELECT * FROM db1.t", pb.StorageIntegrityArgs_READ_MODE_SAFE},
		{"CREATE MATERIALIZED VIEW other.mv TO other.dst AS SELECT * FROM db1.t", pb.StorageIntegrityArgs_READ_MODE_SAFE},
		{"CREATE VIEW other.v2 AS SELECT * FROM db1.t", pb.StorageIntegrityArgs_READ_MODE_UNSAFE_LATEST},
		{"CREATE VIEW other.v3 AS SELECT _hg_row_id FROM db1.t", pb.StorageIntegrityArgs_READ_MODE_SAFE},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			dyn := siDyn(tc.mode, "all_1_1_0")
			resp, handled, err := RewriteWrite(e, ast, tc.sql, dynOpt(dyn))
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
				!strings.Contains(resp.GetMessage(), "storage-integrity table db1.t accepts writes only") {
				t.Fatalf("code=%v msg=%q", resp.GetCode(), resp.GetMessage())
			}
			sawSI := false
			for _, a := range resp.GetOriginalAccessedTables() {
				sawSI = sawSI || a.GetIsStorageIntegrity()
			}
			if !sawSI {
				t.Fatalf("accessed=%+v, want an SI-flagged body target", resp.GetOriginalAccessedTables())
			}
		})
	}
}

func TestGrant_storageIntegrityEffectiveTableRewriteIsLastWins(t *testing.T) {
	e := newEngine(t)
	sql := "GRANT SELECT ON db1.t TO u1"
	ast, _ := e.ParseOne(sql)
	si := dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE))[0]
	static := statOpt(&pb.RewriteTableStaticArgs{})[0]

	for _, tc := range []struct {
		name       string
		opts       []*pb.RewriteOption
		wantReject bool
	}{
		{name: "shadowed SI", opts: []*pb.RewriteOption{si, static}},
		{name: "active SI", opts: []*pb.RewriteOption{static, si}, wantReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, handled, err := RewriteGrant(e, ast, sql, tc.opts)
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if got := resp.Code == pb.RewriteCode_UnsupportedStatement; got != tc.wantReject {
				t.Fatalf("code=%v msg=%q wantReject=%v", resp.Code, resp.Message, tc.wantReject)
			}
		})
	}
}

func assertStorageIntegrityReject(t *testing.T, resp *pb.RewriteSQLResponse, message string) {
	t.Helper()
	if resp.GetCode() == pb.RewriteCode_Success || !strings.Contains(resp.GetMessage(), message) {
		t.Fatalf("code=%v msg=%q, want reject containing %q", resp.GetCode(), resp.GetMessage(), message)
	}
	for _, accessed := range resp.GetOriginalAccessedTables() {
		if accessed.GetIsStorageIntegrity() {
			return
		}
	}
	t.Fatalf("accessed=%+v, want storage-integrity classification", resp.GetOriginalAccessedTables())
}

func TestRewriteSelect_storageIntegrityPhysicalTableFunctionsRejected(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{
		`SELECT * FROM merge('hg_safe', 'db1__t')`,
		`SELECT _hg_row_id FROM remote('127.0.0.1', 'hg_unsafe', 'db1__t')`,
		`SELECT * FROM cluster('c', 'hg_safe.db1__t')`,
		`SELECT * FROM remote('127.0.0.1', hg_safe, other_raw)`,
	} {
		t.Run(sql, func(t *testing.T) {
			ast, err := e.ParseOne(sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := RewriteSelect(e, ast, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)), sql)
			if err != nil {
				t.Fatal(err)
			}
			assertStorageIntegrityReject(t, resp, "storage-integrity physical")
		})
	}
}

func TestRewriteSelect_storageIntegrityPhysicalContextAndDatabaseWide(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		context, sql string
	}{
		{"hg_safe", "SELECT * FROM db1__t"},
		{"hg_unsafe", "SELECT _hg_row_id FROM db1__t"},
		{"", "SELECT * FROM hg_safe.other_raw"},
		{"", "SELECT * FROM hg_unsafe.other_raw"},
	} {
		t.Run(tc.context+tc.sql, func(t *testing.T) {
			dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
			dyn.UpstreamLogicalDatabaseInContext = tc.context
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := RewriteSelect(e, ast, dynOpt(dyn), tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			assertStorageIntegrityReject(t, resp, "storage-integrity physical")
		})
	}
}

func TestRewriteSelect_mixedOrdinaryWrappersDoNotRejectStorageIntegrity(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{
		`SELECT * FROM db1.t AS s JOIN other.u FINAL ON 1`,
		`SELECT * FROM db1.t AS s JOIN other.u SAMPLE 0.1 ON 1`,
		`SELECT * FROM db1.t AS s JOIN other.u AS o(id) ON s.id = o.id`,
		`SELECT * FROM db1.t AS s JOIN other.u WITH OFFSET AS off ON 1`,
	} {
		t.Run(sql, func(t *testing.T) {
			ast, err := e.ParseOne(sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := RewriteSelect(e, ast, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)), sql)
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetCode() != pb.RewriteCode_Success || !strings.Contains(resp.GetSqlAfterRewrite(), "hg_safe.db1__t") {
				t.Fatalf("code=%v msg=%q sql=%q", resp.GetCode(), resp.GetMessage(), resp.GetSqlAfterRewrite())
			}
		})
	}
}

func TestRewriteSelect_storageIntegrityMergeOneArgUsesCurrentDatabase(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		context string
		sql     string
	}{
		{"hg_safe", `SELECT * FROM merge('db1__t')`},
		{"hg_unsafe", `SELECT _hg_row_id FROM merge('db1__t')`},
	} {
		t.Run(tc.context, func(t *testing.T) {
			dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
			dyn.UpstreamLogicalDatabaseInContext = tc.context
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := RewriteSelect(e, ast, dynOpt(dyn), tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			assertStorageIntegrityReject(t, resp, "storage-integrity physical table "+tc.context+".db1__t")
		})
	}
}

func TestWrite_storageIntegrityEmbeddedMergeOneArgUsesCurrentDatabase(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		context string
		sql     string
	}{
		{"hg_safe", `CREATE TABLE other.x AS SELECT * FROM merge('db1__t')`},
		{"hg_unsafe", `INSERT INTO other.u SELECT * FROM merge('db1__t')`},
	} {
		t.Run(tc.context, func(t *testing.T) {
			dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
			dyn.UpstreamLogicalDatabaseInContext = tc.context
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, handled, err := RewriteWrite(e, ast, tc.sql, dynOpt(dyn))
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			assertStorageIntegrityReject(t, resp, "storage-integrity physical table "+tc.context+".db1__t")
		})
	}
}

func TestStorageIntegrityMergeOneArgUsesPhysicalExecutionContext(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"select", `SELECT * FROM merge('db1__t')`},
		{"ctas", `CREATE TABLE other.x AS SELECT * FROM merge('db1__t')`},
		{"insert_select", `INSERT INTO other.u SELECT * FROM merge('db1__t')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
			dyn.UpstreamLogicalDatabaseInContext = "db1"
			dyn.DatabaseMap["db1"] = "hg_safe"
			physical := "hg_safe"
			dyn.UpstreamPhysicalDatabaseInContext = &physical
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "select" {
				resp, err := RewriteSelect(e, ast, dynOpt(dyn), tc.sql)
				if err != nil {
					t.Fatal(err)
				}
				assertStorageIntegrityReject(t, resp, "storage-integrity physical table hg_safe.db1__t")
				return
			}
			resp, handled, err := RewriteWrite(e, ast, tc.sql, dynOpt(dyn))
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			assertStorageIntegrityReject(t, resp, "storage-integrity physical table hg_safe.db1__t")
		})
	}
}

func TestRewriteSelect_storageIntegrityCommaWithOffsetBindsActualTable(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		sql      string
		wantCode pb.RewriteCode
	}{
		{`SELECT * FROM other.u, db1.t WITH OFFSET AS off`, pb.RewriteCode_RewriteError},
		{`SELECT * FROM db1.t, other.u WITH OFFSET AS off`, pb.RewriteCode_Success},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := RewriteSelect(e, ast, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)), tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetCode() != tc.wantCode {
				t.Fatalf("code=%v want=%v msg=%q sql=%q", resp.GetCode(), tc.wantCode, resp.GetMessage(), resp.GetSqlAfterRewrite())
			}
		})
	}
}

func TestWrite_storageIntegrityEmbeddedSelectSourcesRejected(t *testing.T) {
	e := newEngine(t)
	opts := dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE))
	for _, sql := range []string{
		`CREATE TABLE other.x AS SELECT * FROM db1.t`,
		`CREATE TABLE other.x AS SELECT * FROM hg_safe.db1__t`,
		`INSERT INTO other.u SELECT * FROM db1.t`,
		`INSERT INTO other.u SELECT * FROM hg_unsafe.db1__t`,
	} {
		t.Run(sql, func(t *testing.T) {
			ast, err := e.ParseOne(sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, handled, err := RewriteWrite(e, ast, sql, opts)
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			assertStorageIntegrityReject(t, resp, "storage-integrity")
		})
	}
}

func TestStorageIntegrityUnresolvedTableFunctionNamespacesRejected(t *testing.T) {
	e := newEngine(t)
	opts := dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE))
	for _, sql := range []string{
		`SELECT * FROM remote('h', 'hg_safe', concat('db1', '__t'))`,
		`SELECT * FROM merge('hg_unsafe', concat('db1', '__t'))`,
		`SELECT * FROM cluster('c', concat('hg_', 'safe'), 'db1__t')`,
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := RewriteSelect(e, ast, opts, sql)
		if err != nil {
			t.Fatal(err)
		}
		assertStorageIntegrityReject(t, resp, "storage-integrity table function namespace")
	}

	for _, sql := range []string{
		`INSERT INTO FUNCTION remote('h', 'hg_safe', concat('db1', '__t')) SELECT 1`,
		`CREATE TABLE other.x AS merge('hg_unsafe', concat('db1', '__t'))`,
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatal(err)
		}
		resp, handled, err := RewriteWrite(e, ast, sql, opts)
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		assertStorageIntegrityReject(t, resp, "storage-integrity table function namespace")
	}
}

func TestUnresolvedTableFunctionNamespaceWithoutStorageIntegrityPassesThrough(t *testing.T) {
	e := newEngine(t)
	sql := `SELECT * FROM remote('h', concat('ordinary_', 'db'), 't')`
	ast, err := e.ParseOne(sql)
	if err != nil {
		t.Fatal(err)
	}
	dyn := &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"db1": "phys"},
		KnownPhysicalDatabases: []string{"phys"},
		Delim:                  "_",
	}
	resp, err := RewriteSelect(e, ast, dynOpt(dyn), sql)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCode() != pb.RewriteCode_Success {
		t.Fatalf("non-SI dynamic rewrite changed: code=%v msg=%q", resp.GetCode(), resp.GetMessage())
	}
}

func TestGrant_storageIntegrityUnqualifiedDatabaseScopeUsesContext(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		sql, context string
	}{
		{`GRANT SELECT ON * TO u1`, "hg_safe"},
		{`REVOKE SELECT ON * FROM u1`, "hg_unsafe"},
	} {
		dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
		dyn.UpstreamLogicalDatabaseInContext = tc.context
		ast, err := e.ParseOne(tc.sql)
		if err != nil {
			t.Fatal(err)
		}
		resp, handled, err := RewriteGrant(e, ast, tc.sql, dynOpt(dyn))
		if err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", tc.sql, handled, err)
		}
		assertStorageIntegrityReject(t, resp, "storage-integrity physical database "+tc.context)
	}
}

func TestMetadataNonTableObjectResolvesPhysicalContextBeforeKindReject(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		sql, context string
	}{
		{`EXISTS VIEW db1__t`, "hg_safe"},
		{`DESCRIBE DICTIONARY db1__t`, "hg_unsafe"},
	} {
		dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
		dyn.UpstreamLogicalDatabaseInContext = tc.context
		ast, err := e.ParseOne(tc.sql)
		if err != nil {
			t.Fatal(err)
		}
		var resp *pb.RewriteSQLResponse
		var handled bool
		if strings.HasPrefix(tc.sql, "EXISTS") {
			resp, handled, err = RewriteExistsShowCreate(e, ast, tc.sql, dynOpt(dyn))
		} else {
			resp, handled, err = RewriteDescribe(e, ast, tc.sql, dynOpt(dyn))
		}
		if err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", tc.sql, handled, err)
		}
		assertStorageIntegrityReject(t, resp, "storage-integrity physical table")
	}
}

func TestRewriteSelect_reservedAliasRejected(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{
		`SELECT 1 AS _hg_row_id FROM db1.t`,
		`WITH 1 AS _hg_row_id SELECT a FROM db1.t`,
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := RewriteSelect(e, ast, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)), sql)
		if err != nil {
			t.Fatal(err)
		}
		assertStorageIntegrityReject(t, resp, "reserved column _hg_row_id is not addressable")
	}
}

func TestWrite_storageIntegrityTableFunctionsAndCrossAlterRejectedWithMetadata(t *testing.T) {
	e := newEngine(t)
	opts := dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE))
	for _, sql := range []string{
		`INSERT INTO FUNCTION remote('h', 'hg_unsafe', 'db1__t') SELECT 1`,
		`INSERT INTO FUNCTION cluster('c', 'hg_safe.db1__t') SELECT 1`,
		`CREATE TABLE other.x AS merge('hg_safe', 'db1__t')`,
		`ALTER TABLE other.u ATTACH PARTITION 'FROM' FROM hg_safe.db1__t`,
		`ALTER TABLE other.u ATTACH PARTITION 1 /* FROM decoy */ FROM db1.t`,
	} {
		t.Run(sql, func(t *testing.T) {
			ast, err := e.ParseOne(sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, handled, err := RewriteWrite(e, ast, sql, opts)
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			assertStorageIntegrityReject(t, resp, "storage-integrity")
		})
	}
}

func TestWrite_storageIntegrityInsertRequiresLogicalAuthorization(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{
		`INSERT INTO db1.t VALUES (1)`,
		`INSERT INTO db1.t (a) SELECT 1`,
		`INSERT INTO db1.t (a) VALUES (1)`,
	} {
		dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
		dyn.DatabaseMap = map[string]string{}
		dyn.KnownPhysicalDatabases = []string{"db1"}
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatal(err)
		}
		resp, handled, err := RewriteWrite(e, ast, sql, dynOpt(dyn))
		if err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", sql, handled, err)
		}
		if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest {
			t.Fatalf("%q: code=%v msg=%q", sql, resp.GetCode(), resp.GetMessage())
		}
		assertStorageIntegrityReject(t, resp, "not authorized by database_map")
	}

	authorized := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
	for _, sql := range []string{`INSERT INTO db1.t (a) VALUES (1)`, `INSERT INTO db1.t (a) SELECT 1`} {
		ast, _ := e.ParseOne(sql)
		resp, handled, err := RewriteWrite(e, ast, sql, dynOpt(authorized))
		if err != nil || !handled || resp.GetCode() != pb.RewriteCode_Success {
			t.Fatalf("authorized %q: handled=%v err=%v resp=%+v", sql, handled, err, resp)
		}
		if len(resp.GetOriginalAccessedTables()) != 1 || !resp.GetOriginalAccessedTables()[0].GetIsStorageIntegrity() {
			t.Fatalf("authorized %q accessed=%+v", sql, resp.GetOriginalAccessedTables())
		}
	}
}

func TestPhysicalDatabaseNamespaceRejectedAcrossDBLevelAndDDL(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		sql   string
		known bool
	}{
		{"USE hg_safe", true},
		{"USE hg_unsafe", false},
		{"SHOW TABLES FROM hg_safe", true},
		{"CREATE DATABASE hg_safe", false},
		{"DROP DATABASE hg_unsafe", true},
		{"RENAME DATABASE hg_safe TO other", false},
		{"RENAME DATABASE other TO hg_unsafe", true},
		{"ATTACH DATABASE hg_safe FROM '/var/lib/clickhouse/data/'", false},
		{"DETACH DATABASE hg_unsafe", true},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
			if tc.known {
				dyn.KnownPhysicalDatabases = append(dyn.KnownPhysicalDatabases, "hg_safe", "hg_unsafe")
			}
			opts := dynOpt(dyn)
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			resp, handled, err := RewriteWrite(e, ast, tc.sql, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !handled {
				resp, handled, err = RewriteDBLevel(e, ast, tc.sql, opts)
			}
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			assertStorageIntegrityReject(t, resp, "storage-integrity physical database")
		})
	}

	dyn := siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)
	dyn.UpstreamLogicalDatabaseInContext = "hg_unsafe"
	ast, _ := e.ParseOne("SHOW TABLES")
	resp, handled, err := RewriteDBLevel(e, ast, "SHOW TABLES", dynOpt(dyn))
	if err != nil || !handled {
		t.Fatalf("SHOW TABLES context handled=%v err=%v", handled, err)
	}
	assertStorageIntegrityReject(t, resp, "storage-integrity physical database")
}

func TestGrant_storageIntegrityPhysicalDatabaseScopeAndAttachGrant(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{
		`GRANT SELECT ON hg_safe.* TO u1`,
		`REVOKE SELECT ON hg_unsafe.* FROM u1`,
		`ATTACH GRANT SELECT ON hg_safe.db1__t TO u1`,
		`ATTACH GRANT SELECT ON db1.t TO u1`,
	} {
		t.Run(sql, func(t *testing.T) {
			ast, err := e.ParseOne(sql)
			if err != nil {
				t.Fatal(err)
			}
			if _, handled, err := RewriteWrite(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE))); err != nil || handled {
				t.Fatalf("write dispatch swallowed GRANT: handled=%v err=%v", handled, err)
			}
			resp, handled, err := RewriteGrant(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
			if err != nil || !handled {
				t.Fatalf("grant handled=%v err=%v", handled, err)
			}
			assertStorageIntegrityReject(t, resp, "storage-integrity")
		})
	}
}
