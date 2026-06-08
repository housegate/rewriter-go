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
