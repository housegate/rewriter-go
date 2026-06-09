package engine

import (
	"strings"
	"testing"
)

func TestExceedsNestingDepth(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{"flat", "SELECT 1", false},
		{"shallow", "SELECT f(g(h(x)))", false},
		{"large flat IN (depth 1)", "SELECT x IN (" + strings.Repeat("1,", 1000) + "1)", false},
		{"at limit", "SELECT " + strings.Repeat("(", 100) + "1" + strings.Repeat(")", 100), false},
		{"over limit parens", "SELECT " + strings.Repeat("(", 101) + "1" + strings.Repeat(")", 101), true},
		{"over limit brackets", "SELECT " + strings.Repeat("[", 101) + "1" + strings.Repeat("]", 101), true},
		{"deep parens in STRING literal do not count", "SELECT '" + strings.Repeat("(", 500) + "'", false},
		{"doubled-quote escape inside string", "SELECT 'O''" + strings.Repeat("(", 500) + "'", false},
	}
	for _, c := range cases {
		if got := exceedsNestingDepth(c.sql, maxParseDepth); got != c.want {
			t.Errorf("%s: exceedsNestingDepth=%v want %v", c.name, got, c.want)
		}
	}
}

func TestParseDeepNestingFailsOpen(t *testing.T) {
	e := newTestEngine(t) // skips when FFI absent
	// ~600-deep — far past the ~180 crash point. Must return an error, NOT crash.
	sql := "SELECT ~" + strings.Repeat("(", 600) + "1" + strings.Repeat(")", 600)
	if _, err := e.ParseOne(sql); err == nil {
		t.Error("ParseOne deep-nest: err=nil, want guard error")
	}
	if _, err := e.ParseGeneric(sql); err == nil {
		t.Error("ParseGeneric deep-nest: err=nil, want guard error")
	}
}
