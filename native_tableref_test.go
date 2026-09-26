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
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: msg, wantSQL: sql})
		}
	}
	runTablerefCases(t, cases)
}

func TestTableRef_ValueAndColumnParametersStayAllowed(t *testing.T) {
	runTablerefCases(t, []tablerefCase{
		{name: "value", sql: "SELECT * FROM db1.o WHERE a = {v:UInt64}", wantCode: pb.RewriteCode_Success,
			wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE a = {v: UInt64}`},
		{name: "column", sql: "SELECT {c:Identifier} FROM db1.o", wantCode: pb.RewriteCode_Success},
	})
}
