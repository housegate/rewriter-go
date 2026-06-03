package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

// Statements chosen to exercise the discriminator across the families the
// rewriter must classify in later phases.
var characterizeCases = map[string]string{
	"select":       "SELECT a FROM db.t WHERE x IN (1,2)",
	"insert":       "INSERT INTO db.t (a) VALUES (1)",
	"create_table": "CREATE TABLE db.t (a Int64) ENGINE = MergeTree ORDER BY a",
	"drop_table":   "DROP TABLE IF EXISTS db.t",
	"alter_table":  "ALTER TABLE db.t ADD COLUMN b Int64",
	"rename_table": "RENAME TABLE db.a TO db.b",
	"use":          "USE db",
	"show_tables":  "SHOW TABLES FROM db",
	"show_create":  "SHOW CREATE TABLE db.t",
	"exists_table": "EXISTS TABLE db.t",
	"grant":        "GRANT SELECT ON db.t TO u",
	"select_join":  "SELECT * FROM a GLOBAL JOIN b ON a.id = b.id",
}

func TestCharacterizeAST(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("POLYGLOT_SQL_FFI_PATH not set; run via `make test`")
	}
	c, err := polyglot.OpenDefault()
	if err != nil {
		t.Fatalf("OpenDefault: %v", err)
	}
	defer c.Close()

	dir := filepath.Join("testdata", "ast-shapes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, sql := range characterizeCases {
		ast, err := c.ParseOne(sql, "clickhouse")
		if err != nil {
			t.Errorf("%s: ParseOne(%q): %v", name, sql, err)
			continue
		}
		var pretty json.RawMessage = ast
		buf, _ := json.MarshalIndent(json.RawMessage(pretty), "", "  ")
		out := filepath.Join(dir, name+".json")
		if err := os.WriteFile(out, buf, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", out, len(buf))
	}
}
