package rewriter

import (
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
	"google.golang.org/protobuf/proto"
)

var siReservedDatabases = []string{"hg_safe", "hg_unsafe", "hg_promote"}

// v1ReservedEquivalent names every reserved database through a V1 table map,
// which is how V1 learns its protected namespace.
func v1ReservedEquivalent() *pb.RewriteTableDynamicArgs {
	d := v2Dynamic(siV1, false)
	d.StorageIntegrity.Tables = map[string]*pb.StorageIntegrityArgs_Table{
		"db1.t": {SafeTable: "hg_safe.db1__t", UnsafeTable: "hg_unsafe.db1__t"},
		"db1.p": {SafeTable: "hg_promote.db1__p", UnsafeTable: "hg_unsafe.db1__p"},
	}
	return d
}

func v2Reserved(withTable bool) *pb.RewriteTableDynamicArgs {
	d := v2Dynamic(siV2, withTable)
	d.StorageIntegrity.ReservedDatabases = siReservedDatabases
	return d
}

var reservedNamespaceSQL = []string{
	"SELECT * FROM hg_safe.x",
	"SELECT * FROM hg_safe.db1__t",
	"INSERT INTO hg_unsafe.x VALUES (1)",
	"INSERT INTO hg_unsafe.x SELECT 1",
	"TRUNCATE TABLE hg_promote.x",
	"DROP TABLE hg_safe.x",
	"TRUNCATE DATABASE hg_safe",
	"SELECT * FROM merge($tag$hg_safe$tag$, 'x')",
	"USE hg_unsafe",
	"SHOW TABLES FROM hg_promote",
	"SYSTEM START MERGES hg_unsafe.x",
}

// TestStorageIntegrityContractV2_ReservedDatabasesMatchV1Protection proves the
// V2 reserved namespace is protected with an empty table map exactly as V1
// protects the physical databases it derives from a non-empty map.
func TestStorageIntegrityContractV2_ReservedDatabasesMatchV1Protection(t *testing.T) {
	e := newEngine(t)
	for _, sql := range reservedNamespaceSQL {
		t.Run(sql, func(t *testing.T) {
			want, err := doRewrite(e, sql, []*pb.RewriteOption{tableRewriteDynamic(v1ReservedEquivalent())})
			if err != nil {
				t.Fatalf("V1 doRewrite: %v", err)
			}
			got, err := doRewrite(e, sql, []*pb.RewriteOption{tableRewriteDynamic(v2Reserved(false))})
			if err != nil {
				t.Fatalf("V2 doRewrite: %v", err)
			}
			if got.GetCode() == pb.RewriteCode_Success {
				t.Fatalf("V2 with reserved databases forwarded %q: %+v", sql, got)
			}
			if got.GetStorageIntegrityContractVersion() != siV2 {
				t.Fatalf("ack = %v, want V2", got.GetStorageIntegrityContractVersion())
			}
			want.StorageIntegrityContractVersion = siV2
			if !proto.Equal(got, want) {
				t.Fatalf("V2 reserved response differs from the V1 map-derived one:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestStorageIntegrityContractV2_ReservedDatabasesUnionWithTableMap(t *testing.T) {
	e := newEngine(t)
	dyn := v2Reserved(false)
	dyn.StorageIntegrity.Tables = map[string]*pb.StorageIntegrityArgs_Table{
		"db1.t": {SafeTable: "safe_x.db1__t", UnsafeTable: "unsafe_x.db1__t"},
	}
	for sql, msg := range map[string]string{
		"SELECT * FROM safe_x.y":  "storage-integrity physical table safe_x.y is not directly addressable",
		"SELECT * FROM hg_safe.y": "storage-integrity physical table hg_safe.y is not directly addressable",
	} {
		resp, err := doRewrite(e, sql, []*pb.RewriteOption{tableRewriteDynamic(dyn)})
		if err != nil {
			t.Fatalf("doRewrite(%q): %v", sql, err)
		}
		if resp.GetCode() != pb.RewriteCode_RewriteError || resp.GetMessage() != msg {
			t.Fatalf("%q: resp = %+v, want RewriteError %q", sql, resp, msg)
		}
	}
}

func TestStorageIntegrityContractV1_IgnoresReservedDatabases(t *testing.T) {
	e := newEngine(t)
	for _, withTable := range []bool{false, true} {
		plain := v2Dynamic(siV1, withTable)
		reserved := proto.Clone(plain).(*pb.RewriteTableDynamicArgs)
		reserved.StorageIntegrity.ReservedDatabases = append([]string{"not a name"}, siReservedDatabases...)
		for _, sql := range reservedNamespaceSQL {
			want, err := doRewrite(e, sql, []*pb.RewriteOption{tableRewriteDynamic(plain)})
			if err != nil {
				t.Fatalf("doRewrite: %v", err)
			}
			got, err := doRewrite(e, sql, []*pb.RewriteOption{tableRewriteDynamic(reserved)})
			if err != nil {
				t.Fatalf("doRewrite: %v", err)
			}
			if !proto.Equal(got, want) {
				t.Fatalf("V1 (tables=%v) %q changed by reserved_databases:\n got %+v\nwant %+v", withTable, sql, got, want)
			}
		}
	}
}

func TestStorageIntegrityContractV2_RejectsMalformedReservedDatabase(t *testing.T) {
	e := newEngine(t)
	dyn := v2Reserved(false)
	dyn.StorageIntegrity.ReservedDatabases = []string{"hg_safe", "hg-unsafe"}
	resp, err := doRewrite(e, "SELECT 1", []*pb.RewriteOption{tableRewriteDynamic(dyn)})
	if err != nil {
		t.Fatalf("doRewrite: %v", err)
	}
	if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest ||
		resp.GetMessage() != `storage-integrity reserved_databases entry "hg-unsafe" must be a simple identifier` ||
		resp.GetStorageIntegrityContractVersion() != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED {
		t.Fatalf("resp = %+v, want an unacknowledged InvalidRewriteRequest", resp)
	}
}
