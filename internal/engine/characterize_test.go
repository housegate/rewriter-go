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

	// Phase-1 SELECT shapes
	"select_in_list":       "SELECT x FROM db.t WHERE y IN (1, 2)",
	"select_in_subquery":   "SELECT x FROM db.t WHERE y IN (SELECT z FROM db.u)",
	"select_global_in":     "SELECT x FROM db.t WHERE y GLOBAL IN (SELECT z FROM db.u)",
	"select_not_in":        "SELECT x FROM db.t WHERE y NOT IN (1, 2)",
	"select_cte":           "WITH c AS (SELECT 1) SELECT * FROM c",
	"select_cte_join":      "WITH c AS (SELECT * FROM db.t) SELECT * FROM c JOIN db.u ON c.x = db.u.x",
	"select_subquery_from": "SELECT * FROM (SELECT a FROM db.t) sub",
	"select_limit":         "SELECT a FROM db.t LIMIT 10",
	"select_offset":        "SELECT a FROM db.t LIMIT 10 OFFSET 5",
	"select_settings":      "SELECT a FROM db.t SETTINGS max_threads = 4",
	"select_remote_fn":     "SELECT * FROM remote('addr', db, t, 'u', 'p')",
	"select_three_join":    "SELECT * FROM a JOIN b ON a.x = b.x JOIN c ON b.y = c.y",
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
