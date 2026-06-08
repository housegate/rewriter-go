package nameresolve

import (
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
)

func TestLookupStatic_precedence(t *testing.T) {
	args := &pb.RewriteTableStaticArgs{
		TableMap:             map[string]string{"db.t": "t_phys"},
		RemoteTableMap:       map[string]*pb.RewriteTableStaticArgs_RemoteTable{"db.r": {Addr: "h", Database: "pd", Table: "rt", User: "u", Password: "p"}},
		TableWithDatabaseMap: map[string]*pb.RewriteTableStaticArgs_TableWithDatabase{"db.w": {Database: "pw", Table: "wt"}},
	}
	cases := []struct {
		db, table string
		want      Outcome
	}{
		{"db", "t", Outcome{Status: StatusRewrite, PhysicalDB: "db", NewTable: "t_phys", LogicalDB: "db"}},
		{"db", "r", Outcome{Status: StatusRemote, PhysicalDB: "pd", NewTable: "rt", LogicalDB: "db", RemoteAddr: "h", RemoteUser: "u", RemotePassword: "p"}},
		{"db", "w", Outcome{Status: StatusRewrite, PhysicalDB: "pw", NewTable: "wt", LogicalDB: "db"}},
		{"db", "x", Outcome{Status: StatusPassthrough}},
	}
	for _, c := range cases {
		got := LookupStatic(c.db, c.table, args)
		if got != c.want {
			t.Errorf("LookupStatic(%q,%q) = %+v, want %+v", c.db, c.table, got, c.want)
		}
	}
}

func TestLookupStatic_withDatabaseEmptyKeepsOriginDB(t *testing.T) {
	args := &pb.RewriteTableStaticArgs{
		TableWithDatabaseMap: map[string]*pb.RewriteTableStaticArgs_TableWithDatabase{"db.w": {Database: "", Table: "wt"}},
	}
	got := LookupStatic("db", "w", args)
	want := Outcome{Status: StatusRewrite, PhysicalDB: "db", NewTable: "wt", LogicalDB: "db"} // empty database keeps origin
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func dynArgs() *pb.RewriteTableDynamicArgs {
	return &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"tenant1": "testnet"},
		KnownPhysicalDatabases: []string{"system"},
		Delim:                  "_",
	}
}

func TestApplyDynamic_basic(t *testing.T) {
	got := ApplyDynamic("tenant1", "events", dynArgs())
	want := Outcome{Status: StatusRewrite, PhysicalDB: "testnet", NewTable: "tenant1.events", LogicalDB: "tenant1"}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestApplyDynamic_extraArguments(t *testing.T) {
	a := dynArgs()
	a.ExtraArguments = []string{"shard0"}
	got := ApplyDynamic("tenant1", "events", a)
	if got.NewTable != "tenant1_shard0.events" {
		t.Fatalf("new_table = %q, want tenant1_shard0.events", got.NewTable)
	}
}

func TestApplyDynamic_knownPhysicalPassthrough(t *testing.T) {
	got := ApplyDynamic("system", "tables", dynArgs())
	want := Outcome{Status: StatusRewrite, PhysicalDB: "system", NewTable: "system.tables", LogicalDB: "system"}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestApplyDynamic_unqualifiedNoContext_invalid(t *testing.T) {
	got := ApplyDynamic("", "events", dynArgs())
	if got.Status != StatusInvalid {
		t.Fatalf("status = %v, want StatusInvalid", got.Status)
	}
}

func TestApplyDynamic_unqualifiedUsesContext(t *testing.T) {
	a := dynArgs()
	a.UpstreamLogicalDatabaseInContext = "tenant1"
	got := ApplyDynamic("", "events", a)
	if got.PhysicalDB != "testnet" || got.NewTable != "tenant1.events" {
		t.Fatalf("got %+v", got)
	}
}

func TestApplyDynamic_unknownLogical_invalid(t *testing.T) {
	got := ApplyDynamic("nope", "events", dynArgs())
	if got.Status != StatusInvalid {
		t.Fatalf("status = %v, want StatusInvalid", got.Status)
	}
}

func TestApplyDynamic_remoteUpstream(t *testing.T) {
	a := dynArgs()
	a.LogicalDatabaseToRemoteUpstreamIndex = map[string]string{"tenant1": "us"}
	a.RemoteUpstreams = map[string]*pb.RewriteTableDynamicArgs_RemoteUpstream{
		"us": {Addr: "h:9000", User: "ru", Password: "rp"},
	}
	got := ApplyDynamic("tenant1", "events", a)
	want := Outcome{
		Status: StatusRemote, PhysicalDB: "testnet", NewTable: "tenant1.events", LogicalDB: "tenant1",
		RemoteAddr: "h:9000", RemoteUser: "ru", RemotePassword: "rp",
	}
	if got != want {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestApplyDynamic_remoteUpstreamMissingKey_invalid(t *testing.T) {
	a := dynArgs()
	a.LogicalDatabaseToRemoteUpstreamIndex = map[string]string{"tenant1": "ghost"}
	got := ApplyDynamic("tenant1", "events", a)
	if got.Status != StatusInvalid {
		t.Fatalf("status = %v, want StatusInvalid", got.Status)
	}
}
