package engine

import "testing"

func TestParseGrant(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		sql         string
		isGrant     bool // IsGrantVerb
		isRevoke    bool
		isAttach    bool
		hasReplace  bool
		hasOn       bool
		structured  bool
		privNames   []string
		securable   string
		principals  []string
		grantOption bool
		marker      string
	}{
		{"GRANT SELECT ON db.t TO u", true, false, false, false, true, true,
			[]string{"SELECT"}, "db.t", []string{"u"}, false, "GRANT SELECT ON db.t TO u"},
		{"REVOKE SELECT ON db.t FROM u", true, true, false, false, true, true,
			[]string{"SELECT"}, "db.t", []string{"u"}, false, "REVOKE SELECT ON db.t FROM u"},
		{"GRANT SELECT, INSERT ON db.t TO u1, u2 WITH GRANT OPTION", true, false, false, false, true, true,
			[]string{"SELECT", "INSERT"}, "db.t", []string{"u1", "u2"}, true, "GRANT SELECT, INSERT ON db.t TO u1, u2 WITH GRANT OPTION"},
		{"GRANT SELECT ON db.* TO u", true, false, false, false, true, true,
			[]string{"SELECT"}, "db.*", []string{"u"}, false, "GRANT SELECT ON db.* TO u"},
		{"GRANT ALTER UPDATE ON db.t TO u", true, false, false, false, true, true,
			[]string{"ALTER UPDATE"}, "db.t", []string{"u"}, false, "GRANT ALTER UPDATE ON db.t TO u"},
		{"GRANT SELECT ON db.t TO CURRENT_USER", true, false, false, false, true, true,
			[]string{"SELECT"}, "db.t", []string{"CURRENT_USER"}, false, "GRANT SELECT ON db.t TO CURRENT_USER"},
		{"REVOKE GRANT OPTION FOR SELECT ON db.t FROM u", true, true, false, false, true, true,
			[]string{"SELECT"}, "db.t", []string{"u"}, true, "REVOKE GRANT OPTION FOR SELECT ON db.t FROM u"},
		// ON CLUSTER stripped from the marker; structure intact.
		{"GRANT SELECT ON db.t ON CLUSTER c TO u", true, false, false, false, true, true,
			[]string{"SELECT"}, "db.t", []string{"u"}, false, "GRANT SELECT ON db.t TO u"},
		// CURRENT GRANTS parses as a privilege (handler rejects it).
		{"GRANT CURRENT GRANTS ON db.t TO u", true, false, false, false, true, true,
			[]string{"CURRENT GRANTS"}, "db.t", []string{"u"}, false, "GRANT CURRENT GRANTS ON db.t TO u"},
		// Unstructured forms — flags set, generic parse skipped.
		{"ATTACH GRANT SELECT ON db.t TO u", true, false, true, false, false, false, nil, "", nil, false, ""},
		{"GRANT SELECT ON db.t TO u WITH REPLACE OPTION", true, false, false, true, true, false, nil, "", nil, false, ""},
		{"GRANT role1 TO u", true, false, false, false, false, false, nil, "", nil, false, ""},
		// Not a grant.
		{"SELECT 1", false, false, false, false, false, false, nil, "", nil, false, ""},
	}
	for _, c := range cases {
		got, err := ParseGrant(e, c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		if got.IsGrantVerb != c.isGrant || got.IsRevoke != c.isRevoke || got.IsAttach != c.isAttach ||
			got.HasReplace != c.hasReplace || got.HasOn != c.hasOn || got.Structured != c.structured ||
			got.Securable != c.securable || got.GrantOption != c.grantOption || got.Marker != c.marker {
			t.Errorf("%q: got %+v", c.sql, got)
		}
		var names []string
		for _, p := range got.Privileges {
			names = append(names, p.Name)
		}
		if !strSliceEq(names, c.privNames) || !strSliceEq(got.Principals, c.principals) {
			t.Errorf("%q: privs=%v principals=%v", c.sql, names, got.Principals)
		}
	}
}

func TestParseGrant_columns(t *testing.T) {
	e := newTestEngine(t)
	got, err := ParseGrant(e, "GRANT SELECT(c1, c2) ON db.t TO u")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Structured || len(got.Privileges) != 1 || got.Privileges[0].Name != "SELECT" ||
		len(got.Privileges[0].Columns) != 2 {
		t.Fatalf("got %+v", got)
	}
}

func strSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
