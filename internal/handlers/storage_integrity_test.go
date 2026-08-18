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
	want := "SELECT * EXCEPT (_rid) FROM hg_safe.db1__t UNION ALL SELECT * EXCEPT (_rid) FROM hg_unsafe.db1__t WHERE _part NOT IN ('all_1_1_0', 'it''s')"
	if unsafe != want {
		t.Fatalf("unsafe_latest = %q\nwant %q", unsafe, want)
	}
	tbl.ExcludedUnsafeParts = nil
	if got := storageIntegritySurfaceSQL(tbl, &pb.StorageIntegrityArgs{ReadMode: pb.StorageIntegrityArgs_READ_MODE_UNSAFE_LATEST}); strings.Contains(got, "WHERE") {
		t.Fatalf("empty exclusion list must omit WHERE: %q", got)
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
	} {
		ast, _ := e.ParseOne(sql)
		resp, handled, err := RewriteGrant(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
		if err != nil || !handled {
			t.Fatalf("%q: handled=%v err=%v", sql, handled, err)
		}
		if resp.Code != pb.RewriteCode_UnsupportedStatement || !strings.Contains(resp.Message, "storage-integrity table db1.t accepts writes only") {
			t.Fatalf("%q: code=%v msg=%q", sql, resp.Code, resp.Message)
		}
	}
	// Database-scoped grants are not table-targeting and stay allowed.
	sql := "GRANT SELECT ON db1.* TO u1"
	ast, _ := e.ParseOne(sql)
	resp, _, _ := RewriteGrant(e, ast, sql, dynOpt(siDyn(pb.StorageIntegrityArgs_READ_MODE_SAFE)))
	if resp.Code != pb.RewriteCode_Success {
		t.Fatalf("db-scoped grant: code=%v msg=%q", resp.Code, resp.Message)
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
