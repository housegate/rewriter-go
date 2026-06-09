package engine

import (
	"encoding/json"
	"errors"
	"testing"
)

var errFakeParse = errors.New("fake parse error")

// fakeEngine makes the metric logic testable deterministically (no FFI).
// The AST carries the original sql so Generate can map it through f.gen.
type fakeEngine struct {
	parseErr map[string]bool
	gen      map[string]string // sql -> canonical generated form
}

func (f *fakeEngine) ParseOne(sql string) (AST, error) {
	if f.parseErr[sql] {
		return nil, errFakeParse
	}
	b, _ := json.Marshal(map[string]string{"sql": sql})
	return AST(b), nil
}
func (f *fakeEngine) ParseGeneric(sql string) (AST, error) {
	if f.parseErr[sql] {
		return nil, errFakeParse
	}
	b, _ := json.Marshal(map[string]string{"sql": sql})
	return AST(b), nil
}
func (f *fakeEngine) Generate(ast AST) (string, error) {
	var head struct {
		SQL string `json:"sql"`
	}
	if err := json.Unmarshal(ast, &head); err != nil {
		return "", err
	}
	if g, ok := f.gen[head.SQL]; ok {
		return g, nil
	}
	return head.SQL, nil
}
func (f *fakeEngine) RenameTables(a AST, m map[string]string) (AST, error) { return a, nil }
func (f *fakeEngine) QualifyTables(a AST, db string) (AST, error)          { return a, nil }
func (f *fakeEngine) Tokenize(string) (AST, error)                         { return AST("[]"), nil }
func (f *fakeEngine) DiffSQL(string, string) (AST, error)                  { return AST("{}"), nil }
func (f *fakeEngine) Close() error                                         { return nil }

func TestCheckFidelity(t *testing.T) {
	f := &fakeEngine{
		parseErr: map[string]bool{"BAD SQL": true},
		gen: map[string]string{
			"SELECT 1":    "SELECT 1",    // idempotent
			"SELECT a,b":  "SELECT a, b", // first gen normalizes
			"SELECT a, b": "SELECT a,b",  // second gen ping-pongs back -> non-idempotent
		},
	}
	if got := CheckFidelity(f, "SELECT 1"); got.Status != FidelityOK {
		t.Errorf("SELECT 1: %v, want OK", got.Status)
	}
	if got := CheckFidelity(f, "BAD SQL"); got.Status != FidelityParseError {
		t.Errorf("BAD SQL: %v, want ParseError", got.Status)
	}
	if got := CheckFidelity(f, "SELECT a,b"); got.Status != FidelityNonIdempotent {
		t.Errorf("SELECT a,b: %v, want NonIdempotent", got.Status)
	}
}
