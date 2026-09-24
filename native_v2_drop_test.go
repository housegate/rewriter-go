package rewriter

import (
	"reflect"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

type wantAccess struct {
	db, table, logical, physical string
	si                           bool
}

func checkAccess(t *testing.T, got []*pb.AccessedTable, want []wantAccess) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("accessed = %v, want %d entries", got, len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.GetOriginalDatabase() != w.db || g.GetOriginalTable() != w.table ||
			g.GetLogicalDatabase() != w.logical || g.GetPhysicalDatabase() != w.physical ||
			g.GetIsStorageIntegrity() != w.si || g.GetIsRemote() {
			t.Fatalf("accessed[%d] = %v, want %+v", i, g, w)
		}
	}
}

func TestStorageIntegrityContractV2_DropTableSucceeds(t *testing.T) {
	e := newEngine(t)
	opts := []*pb.RewriteOption{tableRewriteDynamic(v2Dynamic(siV2, true))}
	siT := wantAccess{"db1", "t", "db1", "phys", true}
	otherU := wantAccess{"other", "u", "other", "phys", false}
	cases := []struct {
		sql      string
		wantSQL  string
		rewrites map[string]string
		accessed []wantAccess
		ec       pb.ExistenceClause
	}{
		{"DROP TABLE db1.t", `DROP TABLE phys."db1.t"`,
			map[string]string{"db1.t": "phys.db1.t"}, []wantAccess{siT}, pb.ExistenceClause_EXISTENCE_CLAUSE_UNSPECIFIED},
		{"DROP TABLE IF EXISTS db1.t SYNC", `DROP TABLE IF EXISTS phys."db1.t" SYNC`,
			map[string]string{"db1.t": "phys.db1.t"}, []wantAccess{siT}, pb.ExistenceClause_EXISTENCE_CLAUSE_IF_EXISTS},
		{"DROP TABLE db1.t, other.u", `DROP TABLE phys."db1.t", phys."other.u"`,
			map[string]string{"db1.t": "phys.db1.t", "other.u": "phys.other.u"}, []wantAccess{siT, otherU}, pb.ExistenceClause_EXISTENCE_CLAUSE_UNSPECIFIED},
		{"DROP TABLE other.u, db1.t", `DROP TABLE phys."other.u", phys."db1.t"`,
			map[string]string{"db1.t": "phys.db1.t", "other.u": "phys.other.u"}, []wantAccess{otherU, siT}, pb.ExistenceClause_EXISTENCE_CLAUSE_UNSPECIFIED},
		{"DROP TABLE other.u, other.v", `DROP TABLE phys."other.u", phys."other.v"`,
			map[string]string{"other.u": "phys.other.u", "other.v": "phys.other.v"},
			[]wantAccess{otherU, {"other", "v", "other", "phys", false}}, pb.ExistenceClause_EXISTENCE_CLAUSE_UNSPECIFIED},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			resp, err := doRewrite(e, c.sql, opts)
			if err != nil {
				t.Fatalf("doRewrite: %v", err)
			}
			if resp.GetCode() != pb.RewriteCode_Success || resp.GetSqlAfterRewrite() != c.wantSQL ||
				resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_DROP_TABLE ||
				resp.GetStorageIntegrityContractVersion() != siV2 || resp.GetExistenceClause() != c.ec {
				t.Fatalf("resp = %+v, want Success DROP_TABLE %q ack V2 existence %v", resp, c.wantSQL, c.ec)
			}
			if !reflect.DeepEqual(resp.GetTableRewrites(), c.rewrites) {
				t.Fatalf("table_rewrites = %v, want %v", resp.GetTableRewrites(), c.rewrites)
			}
			checkAccess(t, resp.GetOriginalAccessedTables(), c.accessed)
		})
	}
}

func TestStorageIntegrityContractV2_DropRejections(t *testing.T) {
	e := newEngine(t)
	signedLane := "storage-integrity table db1.t accepts writes only through the signed statement lane"
	cases := []struct {
		name     string
		version  pb.StorageIntegrityContractVersion
		sql      string
		wantCode pb.RewriteCode
		wantMsg  string
	}{
		{"v2 truncate", siV2, "TRUNCATE TABLE db1.t", pb.RewriteCode_UnsupportedStatement, signedLane},
		{"v2 physical target", siV2, "DROP TABLE hg_safe.db1__t", pb.RewriteCode_UnsupportedStatement,
			"storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		{"v2 on cluster", siV2, "DROP TABLE db1.t ON CLUSTER c", pb.RewriteCode_UnsupportedStatement, signedLane},
		{"v2 multi on cluster", siV2, "DROP TABLE other.u, db1.t ON CLUSTER c", pb.RewriteCode_UnsupportedStatement, signedLane},
		{"v2 ordinary multi on cluster", siV2, "DROP TABLE other.u, other.v ON CLUSTER c", pb.RewriteCode_UnsupportedStatement,
			"multi-table DROP/TRUNCATE is not supported"},
		{"v2 if empty", siV2, "DROP TABLE IF EMPTY db1.t", pb.RewriteCode_UnsupportedStatement, signedLane},
		{"v2 drop view", siV2, "DROP VIEW db1.t", pb.RewriteCode_UnsupportedStatement, signedLane},
		{"v2 drop dictionary", siV2, "DROP DICTIONARY db1.t", pb.RewriteCode_UnsupportedStatement, signedLane},
		{"v1 drop", siV1, "DROP TABLE db1.t", pb.RewriteCode_UnsupportedStatement, signedLane},
		{"v1 drop if exists sync", siV1, "DROP TABLE IF EXISTS db1.t SYNC", pb.RewriteCode_UnsupportedStatement, signedLane},
		{"v1 multi", siV1, "DROP TABLE db1.t, other.u", pb.RewriteCode_UnsupportedStatement, signedLane},
		{"v1 ordinary multi", siV1, "DROP TABLE other.u, other.v", pb.RewriteCode_UnsupportedStatement,
			"multi-table DROP/TRUNCATE is not supported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := doRewrite(e, c.sql, []*pb.RewriteOption{tableRewriteDynamic(v2Dynamic(c.version, true))})
			if err != nil {
				t.Fatalf("doRewrite: %v", err)
			}
			if resp.GetCode() != c.wantCode || resp.GetMessage() != c.wantMsg || resp.GetSqlAfterRewrite() != c.sql ||
				resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_UNSPECIFIED ||
				resp.GetStorageIntegrityContractVersion() != c.version {
				t.Fatalf("resp = %+v, want %v %q echoing the SQL, ack %v", resp, c.wantCode, c.wantMsg, c.version)
			}
		})
	}
}

func TestStorageIntegrityContractV2_DropRequiresAuthorizedLogicalDatabase(t *testing.T) {
	e := newEngine(t)
	dyn := v2Dynamic(siV2, true)
	delete(dyn.DatabaseMap, "db1")
	resp, err := doRewrite(e, "DROP TABLE db1.t", []*pb.RewriteOption{tableRewriteDynamic(dyn)})
	if err != nil {
		t.Fatalf("doRewrite: %v", err)
	}
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest ||
		resp.GetMessage() != "storage-integrity logical database db1 is not authorized by database_map" {
		t.Fatalf("resp = %+v, want the unauthorized-logical rejection", resp)
	}
}

func TestStorageIntegrityContractV2_StaticSelectionShadowsDropExemption(t *testing.T) {
	e := newEngine(t)
	opts := []*pb.RewriteOption{tableRewriteDynamic(v2Dynamic(siV2, true)), tableRewriteStatic()}
	resp, err := doRewrite(e, "DROP TABLE db1.t, other.u", opts)
	if err != nil {
		t.Fatalf("doRewrite: %v", err)
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
		resp.GetMessage() != "multi-table DROP/TRUNCATE is not supported" ||
		resp.GetStorageIntegrityContractVersion() != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED {
		t.Fatalf("resp = %+v, want the legacy multi-table rejection without acknowledgement", resp)
	}
}

func TestStorageIntegrityContractV2_DropResolvesUnqualifiedTargetThroughContext(t *testing.T) {
	e := newEngine(t)
	dyn := v2Dynamic(siV2, true)
	dyn.UpstreamLogicalDatabaseInContext = "db1"
	opts := []*pb.RewriteOption{tableRewriteDynamic(dyn)}

	resp, err := doRewrite(e, "DROP TABLE t", opts)
	if err != nil {
		t.Fatalf("doRewrite: %v", err)
	}
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetSqlAfterRewrite() != `DROP TABLE phys."db1.t"` {
		t.Fatalf("resp = %+v, want the unqualified SI target dropped as phys.\"db1.t\"", resp)
	}
	checkAccess(t, resp.GetOriginalAccessedTables(), []wantAccess{{"", "t", "db1", "phys", true}})

	// Polyglot parses DROP TEMPORARY TABLE as an ordinary drop_table node; the
	// token grammar keeps it on the V1 rejection instead of dropping the
	// session's logical SI table.
	resp, err = doRewrite(e, "DROP TEMPORARY TABLE t", opts)
	if err != nil {
		t.Fatalf("doRewrite: %v", err)
	}
	if resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
		resp.GetMessage() != "storage-integrity table db1.t accepts writes only through the signed statement lane" {
		t.Fatalf("resp = %+v, want DROP TEMPORARY TABLE refused", resp)
	}
}
