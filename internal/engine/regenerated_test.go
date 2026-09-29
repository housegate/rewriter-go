package engine

import (
	"errors"
	"testing"
)

func TestCheckRegenerated(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want string // "" = regenerated faithfully; otherwise the exact error text
	}{
		// Regenerated faithfully, including every measured cosmetic respelling.
		{"plain select", "SELECT a, count() FROM db1.o WHERE b = 1 GROUP BY a ORDER BY a DESC LIMIT 10", ""},
		{"nulls last is the default", "SELECT a FROM db1.o ORDER BY a DESC NULLS LAST, b ASC NULLS LAST", ""},
		{"nulls first is kept", "SELECT a FROM db1.o ORDER BY a NULLS FIRST", ""},
		{"select all", "SELECT ALL a FROM db1.o", ""},
		{"top", "SELECT TOP 5 a FROM db1.o", ""},
		{"limit n, m", "SELECT a FROM db1.o LIMIT 5, 10", ""},
		{"limit by n, m", "SELECT a FROM db1.o LIMIT 1, 2 BY a", ""},
		{"comma join", "SELECT * FROM db1.o, db1.p", ""},
		{"implicit alias", "SELECT a x FROM db1.o t1", ""},
		{"div mod", "SELECT a DIV 2, a MOD 2, mod(a, 2) FROM db1.o", ""},
		{"regexp", "SELECT a FROM db1.o WHERE s REGEXP 'x'", ""},
		{"null-safe equality", "SELECT a <=> b FROM db1.o", ""},
		{"ternary", "SELECT a > 1 ? 'x' : 'y' FROM db1.o WHERE b = {p:UInt8}", ""},
		{"position in", "SELECT POSITION('a' IN s) FROM db1.o", ""},
		{"interval number", "SELECT now() - INTERVAL 1 DAY", ""},
		{"heredoc", "SELECT * FROM merge($tag$hg_safe$tag$, 'db1__t')", ""},
		{"settings then format", "SELECT a FROM db1.o FORMAT JSON SETTINGS max_threads = 1", ""},
		{"function aliases", "SELECT pow(a, 2), log(a), ceiling(a), substr(s, 1), lcase(s), ucase(s), DATE_TRUNC('day', t) FROM db1.o", ""},
		{"type aliases", "CREATE TABLE db1.n (a INT, b BIGINT, c TEXT, d VARCHAR(255), e DOUBLE, f BOOLEAN, g TIMESTAMP, h NUMERIC(10, 2)) ENGINE = Memory", ""},
		{"truncate table keyword", "TRUNCATE db1.o", ""},
		{"insert into table", "INSERT INTO TABLE db1.o (a, b) VALUES (1, 'a')", ""},
		{"insert format payload", "INSERT INTO db1.o FORMAT CSV 1,(2", ""},
		{"insert select format comment", "INSERT INTO db1.o SELECT * FROM db1.p FORMAT JSON -- c", ""},
		{"definer string", "CREATE DEFINER='live' VIEW db1.v AS SELECT 1", ""},
		{"driver table", "CREATE TABLE `db1`.`p_e` (`id` String, `n` UInt64 DEFAULT 0 COMMENT 'c' CODEC(Delta, ZSTD(1)), INDEX `i` id TYPE bloom_filter GRANULARITY 1, PROJECTION `p` (SELECT * ORDER BY `n`)) ENGINE = ReplacingMergeTree(`n`) PARTITION BY `n` ORDER BY (`id`) SETTINGS index_granularity=8192 COMMENT 'h'", ""},
		{"command keeps its text", "SHOW CREATE TABLE db1.o XYZ", ""},

		// Dropped or respelled with a different meaning.
		{"delete in partition", "DELETE FROM db1.o IN PARTITION '2024-01' WHERE a = 1",
			"engine: generate: the regenerated statement differs from the input: lost [2024-01 IN PARTITION], added nothing"},
		{"with ties before format", "SELECT a FROM db1.o ORDER BY a LIMIT 1 WITH TIES FORMAT JSON",
			"engine: generate: the regenerated statement differs from the input: lost [TIES WITH], added nothing"},
		{"with ties in a subquery", "SELECT * FROM (SELECT a FROM db1.o ORDER BY a LIMIT 1 WITH TIES) AS s",
			"engine: generate: the regenerated statement differs from the input: lost [TIES WITH], added nothing"},
		{"drop on cluster", "DROP TABLE db1.o ON CLUSTER c",
			"engine: generate: the regenerated statement differs from the input: lost [C CLUSTER ON], added nothing"},
		{"drop temporary", "DROP TEMPORARY TABLE n",
			"engine: generate: the regenerated statement differs from the input: lost [TEMPORARY], added nothing"},
		{"drop if empty", "DROP TABLE IF EMPTY db1.o",
			"engine: generate: the regenerated statement differs from the input: lost [EMPTY IF], added nothing"},
		{"limit by then limit", "SELECT a FROM db1.o LIMIT 2 BY a LIMIT 10",
			"engine: generate: the regenerated statement differs from the input: lost [2 LIMIT], added nothing"},
		{"ephemeral", "CREATE TABLE db1.n (a Int32, e Int32 EPHEMERAL) ENGINE = Memory",
			"engine: generate: the regenerated statement differs from the input: lost [EPHEMERAL], added nothing"},
		{"insert column transformer", "INSERT INTO db1.o (* EXCEPT (b)) VALUES (1)",
			"engine: generate: the regenerated statement differs from the input: lost [STAR B EXCEPT], added nothing"},
		{"truncate settings", "TRUNCATE TABLE db1.o SETTINGS max_threads = 1",
			"engine: generate: the regenerated statement differs from the input: lost [1 EQ MAX_THREADS SETTINGS], added nothing"},
		{"cast gains nullable", "SELECT a::String FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost nothing, added [NULLABLE]"},
		{"char length is not length", "SELECT CHAR_LENGTH(s) FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost [CHAR_LENGTH], added [LENGTH]"},
		{"group_concat separator", "SELECT group_concat(s, '-') FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost nothing, added [CONCAT]"},
		{"no such function", "SELECT startsWith(s, 'x') FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost [STARTSWITH], added [STARTS_WITH]"},
		{"live view becomes a view", "CREATE DEFINER=alice LIVE VIEW db1.v AS SELECT 1",
			"engine: generate: the regenerated statement differs from the input: lost [ALICE LIVE], added [ALICELIVE]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatalf("ParseOne: %v", err)
			}
			if err := CheckParsedInFull(e, tc.sql, ast); err != nil {
				t.Fatalf("CheckParsedInFull: %v (the input must pass the parse gate)", err)
			}
			err = CheckRegenerated(e, tc.sql, ast)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
			if !errors.Is(err, ErrNotRegeneratedFaithfully) {
				t.Fatalf("err = %v does not wrap ErrNotRegeneratedFaithfully", err)
			}
		})
	}
}
