package reverse

import "testing"

func TestContainsIdentifier(t *testing.T) {
	for _, c := range []struct {
		s, ident string
		want     bool
	}{
		{"Database physical_db_1 does not exist", "physical_db_1", true},
		{"Table `physical_db_1`.`t`", "physical_db_1", true},
		{"physical_db_1", "physical_db_1", true},
		{"x physical_db_1x physical_db_1.t", "physical_db_1", true},
		{"Database physical_db_10 does not exist", "physical_db_1", false},
		{"Database my_physical_db_1", "physical_db_1", false},
		{"Missing DEFAULT expression", "default", false},
		{"anything", "", false},
		{"ab", "abc", false},
	} {
		if got := ContainsIdentifier(c.s, c.ident); got != c.want {
			t.Errorf("ContainsIdentifier(%q, %q) = %v, want %v", c.s, c.ident, got, c.want)
		}
	}
}

func TestMentionsHidden(t *testing.T) {
	ids := []string{"physical_db_1", "hg_safe"}
	lits := []string{"ch-remote-1.internal:9000", "ch-remote-1.internal", "s3cret-pass"}
	for s, want := range map[string]bool{
		"Table tenant1.events does not exist":            false,
		"Database physical_db_1 does not exist":          true,
		"privileges on hg_safe.`t`":                      true,
		"Connection refused (ch-remote-1.internal:9000)": true,
		"Host ch-remote-1.internal is unreachable":       true,
		"password s3cret-pass":                           true,
	} {
		if got := MentionsHidden(s, ids, lits); got != want {
			t.Errorf("MentionsHidden(%q) = %v, want %v", s, got, want)
		}
	}
}
