package handlers

import (
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
)

func grantDyn() *pb.RewriteTableDynamicArgs {
	return &pb.RewriteTableDynamicArgs{
		DatabaseMap:            map[string]string{"logical1": "phys1"},
		KnownPhysicalDatabases: []string{"phys1"},
		Delim:                  "_",
	}
}

func TestRewriteGrant(t *testing.T) {
	e := newEngine(t)
	dyn := grantDyn()

	t.Run("grant select table", func(t *testing.T) {
		resp, handled, err := RewriteGrant(e, parse(t, e, "GRANT SELECT ON logical1.t TO u"), "GRANT SELECT ON logical1.t TO u", dynOpts(dyn))
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if resp.Code != pb.RewriteCode_Success || resp.StatementType != pb.StatementType_STATEMENT_TYPE_GRANT {
			t.Fatalf("code=%v stmt=%v", resp.Code, resp.StatementType)
		}
		if resp.SqlAfterRewrite != "SELECT 'GRANT SELECT ON logical1.t TO u' AS gstmt" {
			t.Errorf("sql=%q", resp.SqlAfterRewrite)
		}
		if len(resp.PrivilegesDeltas) != 1 {
			t.Fatalf("deltas=%+v", resp.PrivilegesDeltas)
		}
		d := resp.PrivilegesDeltas[0]
		if d.GetAction() != pb.PrivilegeDelta_ACTION_GRANT || d.GetScope() != pb.PrivilegeDelta_SCOPE_TABLE ||
			d.GetOriginalDatabase() != "logical1" || d.GetLogicalDatabase() != "logical1" ||
			d.GetPhysicalDatabase() != "phys1" || d.GetOriginalTable() != "t" ||
			d.GetPhysicalTable() != "logical1.t" || d.GetGrantOption() ||
			len(d.GetPrivileges()) != 1 || d.GetPrivileges()[0] != "SELECT" ||
			len(d.GetGrantees()) != 1 || d.GetGrantees()[0].GetName() != "u" {
			t.Errorf("delta=%+v", d)
		}
	})

	t.Run("grant two privs two grantees with option", func(t *testing.T) {
		resp, _, _ := RewriteGrant(e, parse(t, e, "GRANT SELECT, INSERT ON logical1.t TO u1, u2 WITH GRANT OPTION"),
			"GRANT SELECT, INSERT ON logical1.t TO u1, u2 WITH GRANT OPTION", dynOpts(dyn))
		if len(resp.PrivilegesDeltas) != 2 {
			t.Fatalf("deltas=%d", len(resp.PrivilegesDeltas))
		}
		for i, want := range []string{"SELECT", "INSERT"} {
			d := resp.PrivilegesDeltas[i]
			if d.GetPrivileges()[0] != want || !d.GetGrantOption() || len(d.GetGrantees()) != 2 {
				t.Errorf("delta[%d]=%+v", i, d)
			}
		}
	})

	t.Run("grant on db.* is scope database", func(t *testing.T) {
		resp, _, _ := RewriteGrant(e, parse(t, e, "GRANT SELECT ON logical1.* TO u"), "GRANT SELECT ON logical1.* TO u", dynOpts(dyn))
		d := resp.PrivilegesDeltas[0]
		if d.GetScope() != pb.PrivilegeDelta_SCOPE_DATABASE || d.GetOriginalTable() != "" || d.GetPhysicalTable() != "" {
			t.Errorf("delta=%+v", d)
		}
	})

	t.Run("revoke", func(t *testing.T) {
		resp, _, _ := RewriteGrant(e, parse(t, e, "REVOKE SELECT ON logical1.t FROM u"), "REVOKE SELECT ON logical1.t FROM u", dynOpts(dyn))
		if resp.StatementType != pb.StatementType_STATEMENT_TYPE_REVOKE ||
			resp.SqlAfterRewrite != "SELECT 'REVOKE SELECT ON logical1.t FROM u' AS rstmt" ||
			resp.PrivilegesDeltas[0].GetAction() != pb.PrivilegeDelta_ACTION_REVOKE {
			t.Errorf("resp=%+v", resp)
		}
	})

	t.Run("current_user grantee", func(t *testing.T) {
		resp, _, _ := RewriteGrant(e, parse(t, e, "GRANT SELECT ON logical1.t TO CURRENT_USER"), "GRANT SELECT ON logical1.t TO CURRENT_USER", dynOpts(dyn))
		g := resp.PrivilegesDeltas[0].GetGrantees()[0]
		if !g.GetIsCurrentUser() || g.GetName() != "" {
			t.Errorf("grantee=%+v", g)
		}
	})

	t.Run("on cluster stripped in marker", func(t *testing.T) {
		resp, _, _ := RewriteGrant(e, parse(t, e, "GRANT SELECT ON logical1.t ON CLUSTER c TO u"), "GRANT SELECT ON logical1.t ON CLUSTER c TO u", dynOpts(dyn))
		if resp.SqlAfterRewrite != "SELECT 'GRANT SELECT ON logical1.t TO u' AS gstmt" {
			t.Errorf("sql=%q", resp.SqlAfterRewrite)
		}
	})

	// Reject matrix (code is gated; message is not).
	rejects := []struct {
		name, sql string
		opts      []*pb.RewriteOption
		code      pb.RewriteCode
	}{
		{"global scope", "GRANT SELECT ON *.* TO u", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"column level", "GRANT SELECT(c) ON logical1.t TO u", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"role membership", "GRANT role1 TO u", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"attach grant", "ATTACH GRANT SELECT ON logical1.t TO u", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"with replace", "GRANT SELECT ON logical1.t TO u WITH REPLACE OPTION", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"current grants", "GRANT CURRENT GRANTS ON logical1.t TO u", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"to all", "GRANT SELECT ON logical1.t TO ALL", dynOpts(dyn), pb.RewriteCode_UnsupportedStatement},
		{"no dynamic args", "GRANT SELECT ON logical1.t TO u", nil, pb.RewriteCode_UnsupportedStatement},
		{"unresolvable db", "GRANT SELECT ON unknown.t TO u", dynOpts(dyn), pb.RewriteCode_InvalidRewriteRequest},
		{"unqualified no upstream", "GRANT SELECT ON t TO u", dynOpts(dyn), pb.RewriteCode_InvalidRewriteRequest},
	}
	for _, rc := range rejects {
		t.Run("reject/"+rc.name, func(t *testing.T) {
			resp, handled, err := RewriteGrant(e, parse(t, e, rc.sql), rc.sql, rc.opts)
			if err != nil || !handled {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if resp.Code != rc.code {
				t.Errorf("code=%v want %v (%s)", resp.Code, rc.code, resp.Message)
			}
		})
	}

	t.Run("unqualified uses upstream", func(t *testing.T) {
		d2 := grantDyn()
		d2.UpstreamLogicalDatabaseInContext = "logical1"
		resp, _, _ := RewriteGrant(e, parse(t, e, "GRANT SELECT ON t TO u"), "GRANT SELECT ON t TO u", dynOpts(d2))
		if resp.Code != pb.RewriteCode_Success {
			t.Fatalf("code=%v (%s)", resp.Code, resp.Message)
		}
		d := resp.PrivilegesDeltas[0]
		if d.GetOriginalDatabase() != "" || d.GetLogicalDatabase() != "logical1" || d.GetPhysicalTable() != "logical1.t" {
			t.Errorf("delta=%+v", d)
		}
	})

	t.Run("not a grant falls through", func(t *testing.T) {
		_, handled, _ := RewriteGrant(e, parse(t, e, "SELECT 1"), "SELECT 1", dynOpts(dyn))
		if handled {
			t.Fatal("handled=true, want false")
		}
	})
}
