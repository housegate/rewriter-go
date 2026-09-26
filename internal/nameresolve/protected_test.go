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

func TestProtectedKnownPhysicalIsNotAPassThrough(t *testing.T) {
	a := protectedArgs()
	if _, ok := resolvePhysicalDatabase("phys", a); ok {
		t.Fatal("phys is protected and must not resolve as a pass-through logical")
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
