package nameresolve

import (
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

func protectedArgs() *pb.RewriteTableDynamicArgs {
	return &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"db1": "phys"},
		KnownPhysicalDatabases: []string{"phys"},
		ProtectedDatabases:     []string{"phys", "hg_safe", "hg_unsafe", "hg_promote"},
		Delim:                  "_",
	}
}

func TestProtectedDatabase(t *testing.T) {
	a := protectedArgs()
	for db, want := range map[string]bool{
		"phys": true, "hg_safe": true, "hg_unsafe": true, "hg_promote": true,
		"db1": false, "system": false, "": false,
	} {
		if got := ProtectedDatabase(db, a); got != want {
			t.Errorf("ProtectedDatabase(%q) = %v, want %v", db, got, want)
		}
	}
	// database_map values are protected even when protected_databases is empty.
	bare := &pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"db1": "testnet"}}
	if !ProtectedDatabase("testnet", bare) {
		t.Fatal("a database_map value must be protected without protected_databases")
	}
}

func TestProtectedDatabase_NilArgsAndSIActive(t *testing.T) {
	if ProtectedDatabase("phys", nil) {
		t.Fatal("nil dynamic args protect nothing")
	}
	// With the SI surface active, every SI physical and reserved database is
	// protected even when neither database_map nor protected_databases names it.
	a := &pb.RewriteTableDynamicArgs{
		DatabaseMap: map[string]string{"db1": "phys"},
		StorageIntegrity: &pb.StorageIntegrityArgs{
			Tables: map[string]*pb.StorageIntegrityArgs_Table{
				"db1.t": {SafeTable: "hg_safe.db1__t", UnsafeTable: "hg_unsafe.db1__t"},
			},
			ContractVersion:   pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2,
			ReservedDatabases: []string{"hg_safe", "hg_unsafe", "hg_promote"},
		},
	}
	for db, want := range map[string]bool{
		"hg_safe": true, "hg_unsafe": true, "hg_promote": true, "phys": true, "db1": false,
	} {
		if got := ProtectedDatabase(db, a); got != want {
			t.Errorf("SI-active ProtectedDatabase(%q) = %v, want %v", db, got, want)
		}
	}
}

func TestProtectedKnownPhysicalIsNotAPassThrough(t *testing.T) {
	a := protectedArgs()
	if _, ok := resolvePhysicalDatabase("phys", a); ok {
		t.Fatal("phys is protected and must not resolve as a pass-through logical")
	}
	// A database_map value is protected too, with or without protected_databases.
	mapped := &pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"db1": "phys"}, KnownPhysicalDatabases: []string{"phys"}}
	if _, ok := resolvePhysicalDatabase("phys", mapped); ok {
		t.Fatal("a database_map value must not resolve as a pass-through logical")
	}
	// An unprotected known-physical entry keeps the legacy pass-through role.
	legacy := &pb.RewriteTableDynamicArgs{KnownPhysicalDatabases: []string{"shared"}}
	if got, ok := resolvePhysicalDatabase("shared", legacy); !ok || got != "shared" {
		t.Fatalf("resolvePhysicalDatabase(shared) = %q,%v; want shared,true", got, ok)
	}
}

func TestValidateProtectedDatabases(t *testing.T) {
	a := protectedArgs()
	if err := ValidateProtectedDatabases(a); err != nil {
		t.Fatalf("valid entries rejected: %v", err)
	}
	a.ProtectedDatabases = append(a.ProtectedDatabases, "hg-promote")
	if err := ValidateProtectedDatabases(a); err == nil ||
		err.Error() != `protected_databases entry "hg-promote" must be a simple identifier` {
		t.Fatalf("err = %v, want the simple-identifier message", err)
	}
}

func TestProtectedDatabaseRejectMessage(t *testing.T) {
	if got := ProtectedDatabaseRejectMessage("phys"); got != "protected database phys is not addressable" {
		t.Fatalf("message = %q", got)
	}
}

func TestUnresolvedUnqualifiedTableMessage(t *testing.T) {
	for table, want := range map[string]string{
		"t":     `unqualified table "t" does not resolve through the session's logical database`,
		"db1.t": `unqualified table "db1.t" does not resolve through the session's logical database`,
		// No escaping, like the T6 string-lookup message.
		`a"b`: `unqualified table "a"b" does not resolve through the session's logical database`,
	} {
		got := UnresolvedUnqualifiedTableMessage(table)
		if got != want {
			t.Errorf("message(%q) = %q, want %q", table, got, want)
		}
		if !IsUnresolvedUnqualifiedTableMessage(got) {
			t.Errorf("IsUnresolvedUnqualifiedTableMessage(%q) = false", got)
		}
	}
	for _, other := range []string{"", `unqualified table "`, "protected database phys is not addressable",
		`joinGet target "db1.j" does not resolve through the caller's databases`} {
		if IsUnresolvedUnqualifiedTableMessage(other) {
			t.Errorf("IsUnresolvedUnqualifiedTableMessage(%q) = true", other)
		}
	}
}
