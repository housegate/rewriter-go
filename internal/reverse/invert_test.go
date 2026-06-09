package reverse

import (
	"strings"
	"testing"
)

func TestInvert(t *testing.T) {
	t.Run("per-table dotted dynamic name", func(t *testing.T) {
		got := Invert(
			"Table phys1.`logical1.t` does not exist",
			"EXISTS logical1.t", "EXISTS TABLE phys1.`logical1.t`",
			map[string]string{"logical1.t": "phys1.logical1.t"}, nil)
		if got != "Table logical1.t does not exist" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("per-database", func(t *testing.T) {
		got := Invert("Database phys1 doesn't exist", "USE logical1", "USE phys1",
			nil, map[string]string{"logical1": "phys1"})
		if got != "Database logical1 doesn't exist" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("whole-sql block swap", func(t *testing.T) {
		got := Invert(
			"In scope SELECT 1 FROM phys1.t: oops",
			"SELECT 1 FROM logical1.t", "SELECT 1 FROM phys1.t",
			map[string]string{"logical1.t": "phys1.t"}, nil)
		// whole-SQL swap restores the original SELECT first.
		if got != "In scope SELECT 1 FROM logical1.t: oops" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("empty / no-op", func(t *testing.T) {
		if got := Invert("", "a", "b", nil, nil); got != "" {
			t.Errorf("empty: %q", got)
		}
		// rewritten == original and empty maps → unchanged.
		if got := Invert("anything", "x", "x", nil, nil); got != "anything" {
			t.Errorf("noop: %q", got)
		}
		// equal/empty map entries are skipped.
		if got := Invert("phys1", "s", "s", map[string]string{"a": "a", "b": ""}, nil); got != "phys1" {
			t.Errorf("skip: %q", got)
		}
	})

	t.Run("oversized SQL skips position-remap but still substitutes", func(t *testing.T) {
		// Over the maxRemapSQL cap: buildOffsetMap is skipped (no OOM), but the
		// per-table substitution still runs.
		big := strings.Repeat("x", maxRemapSQL+1)
		got := Invert("Table phys1.t missing", big, big+" changed",
			map[string]string{"logical1.t": "phys1.t"}, nil)
		if got != "Table logical1.t missing" {
			t.Errorf("got %q", got)
		}
	})
}
