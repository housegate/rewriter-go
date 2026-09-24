package nameresolve

import (
	"reflect"
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

func reservedArgs(version pb.StorageIntegrityContractVersion, withTable bool) *pb.RewriteTableDynamicArgs {
	si := &pb.StorageIntegrityArgs{
		ContractVersion:   version,
		ReservedDatabases: []string{"hg_safe", "hg_unsafe", "hg_promote"},
	}
	if withTable {
		si.Tables = map[string]*pb.StorageIntegrityArgs_Table{
			"db1.t": {SafeTable: "safe_x.db1__t", UnsafeTable: "unsafe_x.db1__t"},
		}
	}
	return &pb.RewriteTableDynamicArgs{StorageIntegrity: si}
}

func TestReservedDatabasesProtectedUnderV2(t *testing.T) {
	v2 := reservedArgs(pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2, false)
	for _, db := range []string{"hg_safe", "hg_unsafe", "hg_promote"} {
		if !IsStorageIntegrityPhysicalDatabase(db, v2) {
			t.Errorf("IsStorageIntegrityPhysicalDatabase(%q) = false under V2 with an empty map", db)
		}
		if _, ok := LookupStorageIntegrityPhysical(db, "x", v2); !ok {
			t.Errorf("LookupStorageIntegrityPhysical(%q, x) = false under V2 with an empty map", db)
		}
	}
	if IsStorageIntegrityPhysicalDatabase("phys", v2) {
		t.Error("an unreserved database must stay ordinary")
	}
	withContext := reservedArgs(pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2, false)
	withContext.UpstreamLogicalDatabaseInContext = "hg_unsafe"
	if _, ok := LookupStorageIntegrityPhysical("", "x", withContext); !ok {
		t.Error("an unqualified table in a reserved session database must be protected")
	}
	if got, want := StorageIntegrityPhysicalDatabases(v2), []string{"hg_promote", "hg_safe", "hg_unsafe"}; !reflect.DeepEqual(got, want) {
		t.Errorf("StorageIntegrityPhysicalDatabases = %v, want %v", got, want)
	}
}

func TestReservedDatabasesUnionWithMapDerivedDatabases(t *testing.T) {
	v2 := reservedArgs(pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2, true)
	want := []string{"hg_promote", "hg_safe", "hg_unsafe", "safe_x", "unsafe_x"}
	if got := StorageIntegrityPhysicalDatabases(v2); !reflect.DeepEqual(got, want) {
		t.Errorf("StorageIntegrityPhysicalDatabases = %v, want %v", got, want)
	}
	for _, db := range want {
		if !IsStorageIntegrityPhysicalDatabase(db, v2) {
			t.Errorf("IsStorageIntegrityPhysicalDatabase(%q) = false", db)
		}
	}
}

func TestReservedDatabasesIgnoredUnderV1(t *testing.T) {
	v1 := reservedArgs(pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1, true)
	if IsStorageIntegrityPhysicalDatabase("hg_safe", v1) {
		t.Error("V1 must ignore reserved_databases")
	}
	if _, ok := LookupStorageIntegrityPhysical("hg_unsafe", "x", v1); ok {
		t.Error("V1 must ignore reserved_databases")
	}
	if got, want := StorageIntegrityPhysicalDatabases(v1), []string{"safe_x", "unsafe_x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("StorageIntegrityPhysicalDatabases = %v, want %v", got, want)
	}
	v1.StorageIntegrity.ReservedDatabases = []string{"not a name"}
	if err := ValidateStorageIntegrity(v1); err != nil {
		t.Errorf("V1 must not validate reserved_databases: %v", err)
	}
}

func TestValidateStorageIntegrityReservedDatabasesUnderV2(t *testing.T) {
	for _, bad := range []string{"", "hg-safe", "hg_safe.x", "1hg"} {
		a := reservedArgs(pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2, false)
		a.StorageIntegrity.ReservedDatabases = []string{"hg_safe", bad}
		err := ValidateStorageIntegrity(a)
		if err == nil || !strings.Contains(err.Error(), "reserved_databases entry") {
			t.Errorf("reserved database %q: err = %v, want a reserved_databases entry error", bad, err)
		}
	}
	if err := ValidateStorageIntegrity(reservedArgs(pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2, true)); err != nil {
		t.Errorf("valid V2 args: %v", err)
	}
}
