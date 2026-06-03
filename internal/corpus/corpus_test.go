package corpus

import "testing"

func TestLoadSeed(t *testing.T) {
	cases, err := Load("testdata/seed.sql")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cases) != 12 {
		t.Fatalf("got %d cases, want 12", len(cases))
	}
	if cases[0] != "SELECT a FROM db.t WHERE x IN (1, 2, 3)" {
		t.Fatalf("first case = %q", cases[0])
	}
}
