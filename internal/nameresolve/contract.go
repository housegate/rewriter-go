package nameresolve

import "github.com/housegate/rewriter-proto/gen/pb"

// StorageIntegritySurfaceActive reports whether a request's dynamic args
// activate the storage-integrity surface. V2 activates it by version, even
// with an empty table map (housegate sub-project 3, H6); V1 and every other
// version keep the original table-count rule, so a V1 request with an empty
// map stays legacy byte-for-byte. doRewrite rejects a non-empty map whose
// version is neither V1 nor V2 before any handler runs.
func StorageIntegritySurfaceActive(a *pb.RewriteTableDynamicArgs) bool {
	si := a.GetStorageIntegrity()
	if si.GetContractVersion() == pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2 {
		return true
	}
	return len(si.GetTables()) > 0
}

// StorageIntegrityDropContract reports whether the effective selection
// carries the V2 contract, whose only write-policy difference from V1 is that
// DROP TABLE of a logical storage-integrity table is accepted.
func StorageIntegrityDropContract(sel Selection) bool {
	return sel.Mode == ModeDynamic &&
		sel.Dynamic.GetStorageIntegrity().GetContractVersion() == pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2
}

// storageIntegrityReservedDatabase reports whether db is one of the V2
// reserved_databases. V1 ignores the field, so its protected namespace stays
// the databases derived from the table map, byte-for-byte.
func storageIntegrityReservedDatabase(db string, a *pb.RewriteTableDynamicArgs) bool {
	si := a.GetStorageIntegrity()
	if db == "" || si.GetContractVersion() != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2 {
		return false
	}
	for _, reserved := range si.GetReservedDatabases() {
		if reserved == db {
			return true
		}
	}
	return false
}
