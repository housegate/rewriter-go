package reverse

import (
	"reflect"
	"testing"
)

func TestBuildOffsetMap(t *testing.T) {
	// identical → identity (and the trailing N→M entry).
	if got := buildOffsetMap("abc", "abc"); !reflect.DeepEqual(got, []int{0, 1, 2, 3}) {
		t.Errorf("identical: %v", got)
	}
	// rewritten inserts a leading 'a': the surviving 'b' remaps to original offset 0.
	if got := buildOffsetMap("ab", "b"); !reflect.DeepEqual(got, []int{0, 0, 1}) {
		t.Errorf("insert: %v", got)
	}
	// empty rewritten → just [M].
	if got := buildOffsetMap("", "xyz"); !reflect.DeepEqual(got, []int{3}) {
		t.Errorf("empty rewritten: %v", got)
	}
	// empty original → all zero, trailing 0.
	if got := buildOffsetMap("ab", ""); !reflect.DeepEqual(got, []int{0, 0, 0}) {
		t.Errorf("empty original: %v", got)
	}
}

func TestRemapErrorPositions(t *testing.T) {
	pm := buildOffsetMap("ab", "b") // [0,0,1], rewritten len 2
	// "position 2" (1-based) → pm[1]+1 = 1.
	if got := remapErrorPositions("failed at position 2 here", pm); got != "failed at position 1 here" {
		t.Errorf("remap: %q", got)
	}
	// out-of-range position left as-is.
	if got := remapErrorPositions("position 99", pm); got != "position 99" {
		t.Errorf("oob: %q", got)
	}
	// no position token → unchanged; empty map → unchanged.
	if got := remapErrorPositions("no number", pm); got != "no number" {
		t.Errorf("none: %q", got)
	}
	if got := remapErrorPositions("position 1", []int{0}); got != "position 1" {
		t.Errorf("emptymap: %q", got)
	}
}
