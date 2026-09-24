package rewriter

import (
	"context"
	"errors"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// v2Dynamic is the housegate probe shape: db1 and other map to phys, and
// the SI table map is either empty (H6) or carries db1.t.
func v2Dynamic(version pb.StorageIntegrityContractVersion, withTable bool) *pb.RewriteTableDynamicArgs {
	d := &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"db1": "phys", "other": "phys"},
		KnownPhysicalDatabases: []string{"phys"},
		Delim:                  "_",
		StorageIntegrity: &pb.StorageIntegrityArgs{
			Tables:              map[string]*pb.StorageIntegrityArgs_Table{},
			ReadMode:            pb.StorageIntegrityArgs_READ_MODE_SAFE,
			ReservedRowIdColumn: "_hg_row_id",
			ContractVersion:     version,
		},
	}
	if withTable {
		d.StorageIntegrity.Tables["db1.t"] = &pb.StorageIntegrityArgs_Table{
			SafeTable: "hg_safe.db1__t", UnsafeTable: "hg_unsafe.db1__t",
		}
	}
	return d
}

const (
	siV1 = pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1
	siV2 = pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2
)

func TestStorageIntegrityContractV2_EmptyMapRefusesUnmodelledStatements(t *testing.T) {
	e := newEngine(t)
	opts := []*pb.RewriteOption{tableRewriteDynamic(v2Dynamic(siV2, false))}
	for _, sql := range []string{"SYSTEM RELOAD CONFIG", "SET max_threads = 1"} {
		t.Run(sql, func(t *testing.T) {
			resp, err := doRewrite(e, sql, opts)
			if err != nil {
				t.Fatalf("doRewrite: %v", err)
			}
			if resp.GetCode() != pb.RewriteCode_UnsupportedStatement ||
				resp.GetMessage() != StorageIntegrityUnmodelledMessage ||
				resp.GetSqlAfterRewrite() != sql ||
				resp.GetStatementType() != pb.StatementType_STATEMENT_TYPE_UNSPECIFIED ||
				resp.GetStorageIntegrityContractVersion() != siV2 {
				t.Fatalf("resp = %+v, want acknowledged-V2 UnsupportedStatement %q echoing the SQL",
					resp, StorageIntegrityUnmodelledMessage)
			}
		})
	}
}

func TestStorageIntegrityContractV1_EmptyMapStaysLegacy(t *testing.T) {
	e := newEngine(t)
	opts := []*pb.RewriteOption{tableRewriteDynamic(v2Dynamic(siV1, false))}
	for _, sql := range []string{"SYSTEM RELOAD CONFIG", "SET max_threads = 1"} {
		t.Run(sql, func(t *testing.T) {
			resp, err := doRewrite(e, sql, opts)
			if err != nil {
				t.Fatalf("doRewrite: %v", err)
			}
			if resp.GetCode() != pb.RewriteCode_Success || resp.GetSqlAfterRewrite() != sql ||
				resp.GetStorageIntegrityContractVersion() != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED {
				t.Fatalf("resp = %+v, want legacy Success pass-through without acknowledgement", resp)
			}
		})
	}
}

func TestStorageIntegrityContractV2_AcknowledgedOnEveryPath(t *testing.T) {
	e := newEngine(t)
	cases := []struct {
		name      string
		withTable bool
		sql       string
		wantCode  pb.RewriteCode
		wantSQL   string
	}{
		{"empty map ordinary select", false, "SELECT a FROM db1.t", pb.RewriteCode_Success, `SELECT a FROM phys."db1.t" "db1.t"`},
		{"si table select", true, "SELECT a FROM db1.t", pb.RewriteCode_Success,
			`SELECT a FROM (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t) AS "db1.t"`},
		{"si table truncate", true, "TRUNCATE TABLE db1.t", pb.RewriteCode_UnsupportedStatement, "TRUNCATE TABLE db1.t"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := doRewrite(e, c.sql, []*pb.RewriteOption{tableRewriteDynamic(v2Dynamic(siV2, c.withTable))})
			if err != nil {
				t.Fatalf("doRewrite: %v", err)
			}
			if resp.GetCode() != c.wantCode || resp.GetSqlAfterRewrite() != c.wantSQL ||
				resp.GetStorageIntegrityContractVersion() != siV2 {
				t.Fatalf("resp = %+v, want code=%v sql=%q ack=V2", resp, c.wantCode, c.wantSQL)
			}
		})
	}

	parseFail := nativeWithExactOptions(t, &fakeEngine{parseErr: errors.New("parse boom")},
		[]*pb.RewriteOption{tableRewriteDynamic(v2Dynamic(siV2, false))})
	defer parseFail.Close()
	res, err := parseFail.Rewrite(context.Background(), "SELECT (", "acct")
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != pb.RewriteCode_SyntaxError || res.StorageIntegrityContractVersion != siV2 {
		t.Fatalf("syntax = %+v, want SyntaxError acknowledged V2", res)
	}
}
