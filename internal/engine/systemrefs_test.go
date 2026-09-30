package engine

import (
	"reflect"
	"testing"
)

// TestClickHouseIdentifierValue pins the second, ClickHouse-rule decode of a
// name a parser already unquoted (measured on 26.2: `system\N` reads system,
// `parts\_columns` keeps its backslash and is UNKNOWN_TABLE).
func TestClickHouseIdentifierValue(t *testing.T) {
	for in, want := range map[string]string{
		"system":         "system",
		`system\N`:       "system",
		`sys\Ntem`:       "system",
		`\x73ystem`:      "system",
		`parts\_columns`: `parts\_columns`,
		"a`b":            "a`b",
		`trailing\`:      `trailing\`,
		"":               "",
	} {
		if got := ClickHouseIdentifierValue(in); got != want {
			t.Errorf("ClickHouseIdentifierValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsSystemDatabase(t *testing.T) {
	for _, c := range []struct {
		db, ctx string
		want    bool
	}{
		{"system", "", true},
		{"system", "db1", true},
		{"SYSTEM", "", false},
		{"System", "", false},
		{"", "system", true},
		{"", "db1", false},
		{"information_schema", "", false},
	} {
		if got := IsSystemDatabase(c.db, c.ctx); got != c.want {
			t.Errorf("IsSystemDatabase(%q, %q) = %v, want %v", c.db, c.ctx, got, c.want)
		}
	}
}

// TestQualifiedSourceRuns: command text is read from source lexemes with
// ClickHouse's decoding, a keyword-lexed SYSTEM is still a name, the table
// half of a longer run is not a qualifier, and `system.*` names no static
// object.
func TestQualifiedSourceRuns(t *testing.T) {
	e := newTestEngine(t)
	for _, c := range []struct {
		text string
		want []ObjectRef
	}{
		{"DESCRIBE system.processes", []ObjectRef{{DB: "system", Table: "processes", Exact: true}}},
		{"SHOW COLUMNS FROM `system\\N`.`\\x70rocesses`", []ObjectRef{{DB: "system", Table: "processes", Exact: true}}},
		{"RENAME TABLE system /* c */ . query_log TO db1.x", []ObjectRef{
			{DB: "system", Table: "query_log", Exact: true}, {DB: "db1", Table: "x", Exact: true}}},
		{"GRANT SELECT ON system.* TO u", []ObjectRef{{DB: "system", Table: "*"}}},
		{"SELECT a.b.c", []ObjectRef{{DB: "a", Table: "c", Exact: true}, {DB: "a", Table: "b", Exact: true}}},
		{"SELECT 'system.processes'", nil},
		{"DESCRIBE “system”.“processes”", []ObjectRef{{DB: "system", Table: "processes", Exact: true}}},
		{"DESCRIBE ‘system’.processes", nil},
		{"SHOW COLUMNS FROM system.tables.processes", []ObjectRef{
			{DB: "system", Table: "processes", Exact: true}, {DB: "system", Table: "tables", Exact: true}}},
		{"SHOW COLUMNS FROM a.b.c.d", []ObjectRef{{DB: "a", Table: "d", Exact: true}, {DB: "a", Table: "b", Exact: true}}},
		{"SHOW COLUMNS FROM system.tables.*", []ObjectRef{{DB: "system", Table: "*"}, {DB: "system", Table: "tables", Exact: true}}},
	} {
		got, ok := qualifiedSourceRuns(e, c.text)
		if !ok {
			t.Fatalf("%q: tokenize failed", c.text)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %+v, want %+v", c.text, got, c.want)
		}
	}
}
