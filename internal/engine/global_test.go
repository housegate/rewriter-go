package engine

import (
	"os"
	"strings"
	"testing"
)

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func gge(t *testing.T) Engine {
	t.Helper()
	e, err := NewPolyglot("")
	if err != nil {
		t.Skipf("engine unavailable: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// Supported: table on the LEFT, remote() on the RIGHT → GLOBAL synthesizable.
func TestForceGlobal_joinLocalRemoteAsymmetry(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e := gge(t)
	ast, _ := e.ParseOne("SELECT * FROM local_tbl JOIN remote('h', d, t, 'u', 'p') ON x = y")
	out, err := ForceGlobalForRemoteAsymmetry(ast)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	if !contains(got, "GLOBAL JOIN") {
		t.Fatalf("expected GLOBAL JOIN, got %q", got)
	}
}

// IN promotion: FROM is remote, IN RHS is a local table → GLOBAL IN.
func TestForceGlobal_inWithRemoteFrom(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e := gge(t)
	ast, _ := e.ParseOne("SELECT x FROM remote('h', d, t, 'u', 'p') WHERE x IN (SELECT z FROM local_tbl)")
	out, err := ForceGlobalForRemoteAsymmetry(ast)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	if !contains(got, "GLOBAL IN") {
		t.Fatalf("expected GLOBAL IN, got %q", got)
	}
}

// No remote source → no promotion.
func TestForceGlobal_noRemoteNoChange(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e := gge(t)
	ast, _ := e.ParseOne("SELECT * FROM a JOIN b ON a.id = b.id")
	out, err := ForceGlobalForRemoteAsymmetry(ast)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	if contains(got, "GLOBAL") {
		t.Fatalf("no remote source → no GLOBAL, got %q", got)
	}
}

// KNOWN LIMITATION: remote() on the LEFT cannot be GLOBAL-marked via polyglot
// (alias on a function renders as `AS GLOBAL`, corrupting the query). The pass
// must leave it UNCHANGED — no GLOBAL, and crucially no `AS GLOBAL` corruption.
func TestForceGlobal_remoteLeftJoin_notCorrupted(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e := gge(t)
	in := "SELECT * FROM remote('h', d, t, 'u', 'p') JOIN local_tbl ON x = y"
	ast, _ := e.ParseOne(in)
	out, err := ForceGlobalForRemoteAsymmetry(ast)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	if contains(got, "AS GLOBAL") {
		t.Fatalf("remote-left join was corrupted with a spurious alias: %q", got)
	}
	// It also should not have gained a GLOBAL JOIN it cannot represent.
	if contains(got, "GLOBAL JOIN") {
		t.Fatalf("unexpectedly synthesized GLOBAL JOIN on a function left operand: %q", got)
	}
}

// The remote source is in a JOIN (not the FROM); an IN with a local RHS must
// still be promoted to GLOBAL IN (C++ tests the whole table list).
func TestForceGlobal_inWithRemoteInJoin(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e := gge(t)
	ast, _ := e.ParseOne("SELECT x FROM local_a JOIN remote('h', d, t, 'u', 'p') ON p = q WHERE x IN (SELECT z FROM local_b)")
	out, err := ForceGlobalForRemoteAsymmetry(ast)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.Generate(out)
	if !contains(got, "GLOBAL IN") {
		t.Fatalf("expected GLOBAL IN (remote source in JOIN), got %q", got)
	}
}
