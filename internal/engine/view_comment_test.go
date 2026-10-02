package engine

import "testing"

func TestStripViewComment(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql, stripped, comment string // stripped "" = not taken
	}{
		{"CREATE OR REPLACE VIEW `d`.`v` (`a` String COMMENT 'x') AS (SELECT a FROM d.t) COMMENT 'c'",
			"CREATE OR REPLACE VIEW `d`.`v` (`a` String COMMENT 'x') AS (SELECT a FROM d.t)", "'c'"},
		{"CREATE MATERIALIZED VIEW d.mv TO d.t (a String) AS (SELECT * FROM d.s) COMMENT 'c' ; ",
			"CREATE MATERIALIZED VIEW d.mv TO d.t (a String) AS (SELECT * FROM d.s)", "'c'"},
		{"create view d.v as (select 1) comment 'it\\'s'", "create view d.v as (select 1)", "'it\\'s'"},
		{"CREATE VIEW d.v AS SELECT 1 COMMENT 'c'", "", ""},
		{"CREATE VIEW d.v AS SELECT f(1) COMMENT 'c'", "", ""},
		{"CREATE VIEW d.v AS (SELECT 1) COMMENT \"c\"", "", ""},
		{"CREATE VIEW d.v AS (SELECT 1) COMMENT 'c' COMMENT 'd'", "", ""},
		{"CREATE VIEW d.v AS (SELECT 1) COMMENT 'c' /* x */", "", ""},
		{"CREATE LIVE VIEW d.v AS (SELECT 1) COMMENT 'c'", "", ""},
		{"CREATE TABLE d.t ENGINE = Memory AS (SELECT 1) COMMENT 'c'", "", ""},
		{"SELECT 'VIEW' COMMENT 'c'", "", ""},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			stripped, comment, ok := StripViewComment(e, tc.sql)
			if tc.stripped == "" {
				if ok || stripped != tc.sql {
					t.Fatalf("took %q / %q", stripped, comment)
				}
				return
			}
			if !ok || stripped != tc.stripped || comment != tc.comment {
				t.Fatalf("got %q, %q, %v", stripped, comment, ok)
			}
		})
	}
	if out, ok := AppendViewComment(`CREATE VIEW d.v AS (SELECT 1)`, "'c'"); !ok || out != `CREATE VIEW d.v AS (SELECT 1) COMMENT 'c'` {
		t.Fatalf("AppendViewComment = %q, %v", out, ok)
	}
	if _, ok := AppendViewComment(`CREATE VIEW d.v AS SELECT 1`, "'c'"); ok {
		t.Fatal("AppendViewComment appended after an unparenthesised query")
	}
}

// TestViewColumnComments pins the column comments a view keeps through
// parse and generate, and the drop gate's answer on each.
func TestViewColumnComments(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql, gen string
		gate     bool // CheckRegenerated passes
	}{
		{"CREATE VIEW d.v (a String COMMENT 'x', b Int32, `c` Nullable(Enum('A' = 1, 'B' = 2)) COMMENT 'enum, (c)') AS (SELECT 1)",
			`CREATE VIEW d.v (a String COMMENT 'x', b Int32, "c" Nullable(Enum('A' = 1, 'B' = 2)) COMMENT 'enum, (c)') AS (SELECT 1)`, true},
		{"CREATE MATERIALIZED VIEW d.mv TO d.t (a String COMMENT 'it\\'s', b String COMMENT 'a''b') AS (SELECT 1)",
			`CREATE MATERIALIZED VIEW d.mv TO d.t (a String COMMENT 'it\'s', b String COMMENT 'a\'b') AS (SELECT 1)`, true},
		{"CREATE VIEW d.v (comment String COMMENT 'x') AS (SELECT 1)",
			`CREATE VIEW d.v (comment String COMMENT 'x') AS (SELECT 1)`, true},
		// Only the comment is restored; a DEFAULT / CODEC stays dropped and
		// the gate refuses.
		{"CREATE VIEW d.v (a String DEFAULT 'q' COMMENT 'x') AS (SELECT 1)",
			`CREATE VIEW d.v (a String COMMENT 'x') AS (SELECT 1)`, false},
		{"CREATE VIEW d.v (a String COMMENT 'x' CODEC(ZSTD(1))) AS (SELECT 1)",
			`CREATE VIEW d.v (a String COMMENT 'x') AS (SELECT 1)`, false},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			gen, err := e.Generate(ast)
			if err != nil {
				t.Fatal(err)
			}
			if gen != tc.gen {
				t.Fatalf("generated %q, want %q", gen, tc.gen)
			}
			if err := CheckRegenerated(e, tc.sql, ast); (err == nil) != tc.gate {
				t.Fatalf("CheckRegenerated = %v, want pass=%v", err, tc.gate)
			}
		})
	}
}
