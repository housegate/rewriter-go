package nameresolve

import (
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

func TestStorageIntegritySurfaceActive(t *testing.T) {
	table := map[string]*pb.StorageIntegrityArgs_Table{
		"db1.t": {SafeTable: "hg_safe.db1__t", UnsafeTable: "hg_unsafe.db1__t"},
	}
	cases := []struct {
		name string
		args *pb.RewriteTableDynamicArgs
		want bool
	}{
		{"nil args", nil, false},
		{"no SI block", &pb.RewriteTableDynamicArgs{}, false},
		{"V1 with tables", &pb.RewriteTableDynamicArgs{StorageIntegrity: &pb.StorageIntegrityArgs{
			Tables: table, ContractVersion: pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1}}, true},
		{"V1 empty map stays inactive", &pb.RewriteTableDynamicArgs{StorageIntegrity: &pb.StorageIntegrityArgs{
			ContractVersion: pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1}}, false},
		{"V2 with tables", &pb.RewriteTableDynamicArgs{StorageIntegrity: &pb.StorageIntegrityArgs{
			Tables: table, ContractVersion: pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2}}, true},
		{"V2 empty map activates", &pb.RewriteTableDynamicArgs{StorageIntegrity: &pb.StorageIntegrityArgs{
			ContractVersion: pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2}}, true},
		{"unspecified with tables is active (rejected later as invalid)", &pb.RewriteTableDynamicArgs{StorageIntegrity: &pb.StorageIntegrityArgs{
			Tables: table}}, true},
		{"unknown version with empty map", &pb.RewriteTableDynamicArgs{StorageIntegrity: &pb.StorageIntegrityArgs{
			ContractVersion: pb.StorageIntegrityContractVersion(99)}}, false},
	}
	for _, c := range cases {
		if got := StorageIntegritySurfaceActive(c.args); got != c.want {
			t.Errorf("%s: StorageIntegritySurfaceActive = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStorageIntegrityDropContract(t *testing.T) {
	v2 := &pb.RewriteTableDynamicArgs{StorageIntegrity: &pb.StorageIntegrityArgs{
		ContractVersion: pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2}}
	v1 := &pb.RewriteTableDynamicArgs{StorageIntegrity: &pb.StorageIntegrityArgs{
		ContractVersion: pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1}}
	cases := []struct {
		name string
		sel  Selection
		want bool
	}{
		{"dynamic V2", Selection{Mode: ModeDynamic, Dynamic: v2}, true},
		{"dynamic V1", Selection{Mode: ModeDynamic, Dynamic: v1}, false},
		{"static selection shadows V2", Selection{Mode: ModeStatic, Static: &pb.RewriteTableStaticArgs{}}, false},
		{"no selection", Selection{Mode: ModeNone}, false},
	}
	for _, c := range cases {
		if got := StorageIntegrityDropContract(c.sel); got != c.want {
			t.Errorf("%s: StorageIntegrityDropContract = %v, want %v", c.name, got, c.want)
		}
	}
}
