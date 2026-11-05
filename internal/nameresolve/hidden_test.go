package nameresolve

import (
	"reflect"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

func dynamicOption(a *pb.RewriteTableDynamicArgs) []*pb.RewriteOption {
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: a}}}}
}

func TestHiddenNamesDynamic(t *testing.T) {
	ids, lits := HiddenNames(dynamicOption(&pb.RewriteTableDynamicArgs{
		DatabaseMap:                      map[string]string{"tenant1": "physical_db_1", "tenant2": "physical_db_2", "same": "same"},
		KnownPhysicalDatabases:           []string{"shared", "physical_db_1"},
		ProtectedDatabases:               []string{"hg_promote"},
		UpstreamLogicalDatabaseInContext: "tenant1",
		StorageIntegrity: &pb.StorageIntegrityArgs{
			ReservedDatabases: []string{"hg_unsafe"},
			Tables:            map[string]*pb.StorageIntegrityArgs_Table{"tenant1.t": {SafeTable: "hg_safe.tenant1__t", UnsafeTable: "hg_unsafe.tenant1__t"}},
		},
		RemoteUpstreams: map[string]*pb.RewriteTableDynamicArgs_RemoteUpstream{
			"us": {Addr: "ch-remote-1.internal:9000", User: "viewer", Password: "s3cret-pass"},
			"x":  {Addr: "[::1]:9000", Password: "abc"},
		},
	}))
	if want := []string{"hg_promote", "hg_safe", "hg_unsafe", "physical_db_1", "physical_db_2"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("identifiers = %v, want %v", ids, want)
	}
	if want := []string{"ch-remote-1.internal:9000", "ch-remote-1.internal", "s3cret-pass", "[::1]:9000"}; !reflect.DeepEqual(lits, want) {
		t.Errorf("literals = %v, want %v", lits, want)
	}
}

func TestHiddenNamesStatic(t *testing.T) {
	ids, lits := HiddenNames([]*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{StaticArgs: &pb.RewriteTableStaticArgs{
			TableWithDatabaseMap: map[string]*pb.RewriteTableStaticArgs_TableWithDatabase{"logical.t": {Database: "prod_subgraph", Table: "x"}},
			RemoteTableMap:       map[string]*pb.RewriteTableStaticArgs_RemoteTable{"sui.events": {Addr: "chain-b:9000", Database: "chain_db"}},
		}}}}})
	if want := []string{"chain_db", "prod_subgraph"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("identifiers = %v, want %v", ids, want)
	}
	if want := []string{"chain-b:9000", "chain-b"}; !reflect.DeepEqual(lits, want) {
		t.Errorf("literals = %v, want %v", lits, want)
	}
}
