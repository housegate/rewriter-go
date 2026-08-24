package engine

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

// Statements chosen to exercise the discriminator across the families the
// rewriter must classify in later phases.
var characterizeCases = map[string]string{
	"select": "SELECT a FROM db.t WHERE x IN (1,2)",
	"insert": "INSERT INTO db.t (a) VALUES (1)",
	// NOTE: create_table / drop_table SQL below is the Phase-2 form (bare table,
	// no IF [NOT] EXISTS). Both still parse to the same top-level discriminator
	// key (create_table / drop_table), so the NodeKind snapshot assertions in
	// ast_test.go remain green. The IF-EXISTS variants live under their own keys
	// (create_table_ifne / drop_table_ife) below.
	"create_table": "CREATE TABLE db.t (x Int32) ENGINE=Memory",
	"drop_table":   "DROP TABLE db.t",
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

	// Phase-2 WRITE shapes (golden contracts for the writes.cc port).
	// create_table / drop_table are defined above (shared discriminator keys).
	"create_table_as":   "CREATE TABLE db.t AS db2.src",
	"create_table_ifne": "CREATE TABLE IF NOT EXISTS db.t (x Int32) ENGINE=Memory",
	"drop_table_ife":    "DROP TABLE IF EXISTS db.t",
	"drop_view":         "DROP VIEW db.v",
	"truncate":          "TRUNCATE TABLE db.t",
	"alter_add":         "ALTER TABLE db.t ADD COLUMN y Int32",
	"alter_delete":      "ALTER TABLE db.t DELETE WHERE y = 2",
	"alter_attach_from": "ALTER TABLE db.t ATTACH PARTITION 1 FROM db.src",
	"insert_values":     "INSERT INTO db.t (x) VALUES (1)",
	"insert_select":     "INSERT INTO db.t SELECT * FROM db.s",
	"update":            "UPDATE db.t SET x = 1 WHERE y = 2",
	"delete":            "DELETE FROM db.t WHERE x = 1",
	"rename":            "RENAME TABLE db.a TO db.b",
	"exchange":          "EXCHANGE TABLES db.a AND db.b",
	"alter_update":      "ALTER TABLE db.t UPDATE x = 1 WHERE y = 2",
	"create_view":       "CREATE VIEW db.v AS SELECT * FROM db.s",
	"create_mv_to":      "CREATE MATERIALIZED VIEW db.mv TO db.dst AS SELECT * FROM db.s",
}

// updateGoldenEnv mirrors harness.UpdateGoldenEnv. internal/harness imports
// internal/engine, so the constant cannot be shared without an import cycle.
const updateGoldenEnv = "UPDATE_GOLDEN"

func sortedCharacterizeCaseNames(cases map[string]string) []string {
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestSortedCharacterizeCaseNamesStableAcrossInsertionOrder(t *testing.T) {
	want := []string{"alter", "create", "delete", "insert", "select", "update"}

	for seed := int64(0); seed < 32; seed++ {
		insertionOrder := slices.Clone(want)
		rand.New(rand.NewSource(seed)).Shuffle(len(insertionOrder), func(i, j int) {
			insertionOrder[i], insertionOrder[j] = insertionOrder[j], insertionOrder[i]
		})
		cases := make(map[string]string, len(insertionOrder))
		for _, name := range insertionOrder {
			cases[name] = name
		}

		if got := sortedCharacterizeCaseNames(cases); !slices.Equal(got, want) {
			t.Fatalf("seed %d: sorted case names = %v, want %v", seed, got, want)
		}
	}
}

// TestCharacterizeAST pins the polyglot AST shapes that ast_test.go and
// nodes_test.go read. It compares by default and only regenerates under
// UPDATE_GOLDEN=1 (Spec J D7). It used to overwrite the fixtures on every run
// and assert nothing, which meant a semantic change in the pinned engine
// rewrote the goldens silently and left the suite order-dependent between this
// writer and its two readers.
func TestCharacterizeAST(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("POLYGLOT_SQL_FFI_PATH not set; run via `make test`")
	}
	update := os.Getenv(updateGoldenEnv) == "1"

	c, err := polyglot.OpenDefault()
	if err != nil {
		t.Fatalf("OpenDefault: %v", err)
	}
	defer c.Close()

	dir := filepath.Join("testdata", "ast-shapes")
	if update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range sortedCharacterizeCaseNames(characterizeCases) {
		sql := characterizeCases[name]
		t.Run(name, func(t *testing.T) {
			ast, err := c.ParseOne(sql, "clickhouse")
			if err != nil {
				t.Fatalf("ParseOne(%q): %v", sql, err)
			}
			// json.MarshalIndent over a RawMessage is how the committed
			// fixtures were produced; keep it byte-for-byte so a formatting
			// change never masquerades as an AST change.
			got, err := json.MarshalIndent(json.RawMessage(ast), "", "  ")
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			out := filepath.Join(dir, name+".json")
			if update {
				if err := os.WriteFile(out, got, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("wrote %s (%d bytes)", out, len(got))
				return
			}
			want, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("read fixture %s: %v\nregenerate with: %s=1 make test", out, err, updateGoldenEnv)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s is stale against the pinned polyglot.\n"+
					"--- committed ---\n%s\n--- produced ---\n%s\n"+
					"If the new shape is correct, regenerate with: %s=1 make test",
					out, want, got, updateGoldenEnv)
			}
		})
	}
}
