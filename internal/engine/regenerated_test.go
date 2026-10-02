package engine

import (
	"errors"
	"strings"
	"testing"
)

const differs = "engine: generate: the regenerated statement differs from the input: "

const hash = " contains a #, which ClickHouse reads as a comment or an unrecognised token"

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
		{"grant command keeps its text", "GRANT SELECT ON db1.o TO u1", ""},
		{"raw node keeps its text", "CREATE LIVE VIEW other.v AS SELECT 1", ""},
		{"streamed values with columns and settings is an insert node", "INSERT INTO db1.o (a, b) SETTINGS async_insert = 1 VALUES", ""},

		// A command or raw node whose own text is not the input (final review I1).
		{"streamed values", "INSERT INTO db1.o (a, b) VALUES", differs + "lost [A B DB1 O], added nothing"},
		{"streamed values without columns", "INSERT INTO db1.o VALUES", differs + "lost [DB1 O], added nothing"},
		{"streamed values with settings", "INSERT INTO db1.o SETTINGS async_insert = 1 VALUES",
			differs + "lost [1 EQ ASYNC_INSERT DB1 O SETTINGS], added nothing"},
		{"streamed values into a function", "INSERT INTO FUNCTION remote('x', db1.o) VALUES",
			differs + "lost [x DB1 FUNCTION O REMOTE], added nothing"},
		{"raw live view loses sql security", "CREATE SQL SECURITY DEFINER LIVE VIEW other.v AS SELECT 1",
			differs + "lost [DEFINER SECURITY SQL], added nothing"},

		// Column modifiers are compared in order: Polyglot reorders them, and
		// ClickHouse accepts one order only (final review Minor 1).
		{"column modifiers in clickhouse order", "CREATE TABLE db1.n (d Date, a Int32 DEFAULT 1 COMMENT 'x' CODEC(ZSTD) TTL d + INTERVAL 1 DAY) ENGINE = MergeTree ORDER BY d TTL d + INTERVAL 1 DAY COMMENT 'h'", ""},
		{"codec before comment", "CREATE TABLE db1.n (a Int32 CODEC(ZSTD) COMMENT 'x') ENGINE = Memory",
			differs + "column modifiers reordered from [CODEC COMMENT] to [COMMENT CODEC]"},
		{"ttl before default", "CREATE TABLE db1.n (d Date, a Int32 TTL d + INTERVAL 1 DAY DEFAULT 1) ENGINE = MergeTree ORDER BY d",
			differs + "column modifiers reordered from [TTL DEFAULT] to [DEFAULT TTL]"},
		{"add column codec before comment", "ALTER TABLE db1.o ADD COLUMN a Int32 CODEC(ZSTD) COMMENT 'x'",
			differs + "column modifiers reordered from [CODEC COMMENT] to [COMMENT CODEC]"},
		{"materialized before comment", "CREATE TABLE db1.n (a Int32 MATERIALIZED 1 COMMENT 'x') ENGINE = Memory",
			differs + "column modifiers reordered from [MATERIALIZED COMMENT] to [COMMENT MATERIALIZED]"},
		{"codec before primary key", "CREATE TABLE db1.n (a Int32 CODEC(ZSTD) PRIMARY KEY) ENGINE = MergeTree",
			differs + "column modifiers reordered from [CODEC PRIMARY KEY] to [PRIMARY KEY CODEC]"},
		{"two added columns, one reordered", "ALTER TABLE db1.o ADD COLUMN a Int32 CODEC(ZSTD) COMMENT 'x', ADD COLUMN b Int32 DEFAULT 1",
			differs + "column modifiers reordered from [CODEC COMMENT; DEFAULT] to [COMMENT CODEC; DEFAULT]"},
		{"reordered column beside an index", "CREATE TABLE db1.n (a Int32 TTL d + INTERVAL 1 DAY DEFAULT 1, d Date, INDEX i a TYPE minmax GRANULARITY 1) ENGINE = MergeTree ORDER BY d",
			differs + "column modifiers reordered from [TTL DEFAULT] to [DEFAULT TTL]"},
		// Only column declarations are scanned (re-review N2): a CTAS table
		// COMMENT that Polyglot prints after the SELECT, a column named like a
		// modifier, and an INDEX moved after the columns pass.
		{"ctas comment and a column named ttl", "CREATE TABLE db1.n ENGINE = MergeTree ORDER BY a COMMENT 'h' AS SELECT a, ttl FROM db1.o", ""},
		{"ctas ttl and an alias named default", "CREATE TABLE db1.n ENGINE = MergeTree ORDER BY a TTL d + INTERVAL 1 DAY COMMENT 'h' AS SELECT a AS default FROM db1.o", ""},
		{"ctas order by codec", "CREATE TABLE db1.n ENGINE = MergeTree ORDER BY codec COMMENT 'h' AS SELECT a, codec FROM db1.o", ""},
		{"index moved after a column named alias", "CREATE TABLE db1.n (a Int32, INDEX i alias TYPE minmax GRANULARITY 1, alias Int32 DEFAULT 1) ENGINE = MergeTree ORDER BY a", ""},

		// A command or raw node spells a quoted identifier apart from a bare
		// word (re-review N1): a raw node that drops the quotes from `null`
		// names another statement.
		{"raw row policy drops the quotes from null", "CREATE ROW POLICY p1 ON db1.o FOR SELECT USING `null` = 1 TO u1",
			differs + "lost [`null`], added [NULL]"},
		{"raw role drops the quotes from true", `CREATE ROLE "true"`, differs + `lost ["true"], added [TRUE]`},
		{"raw role with a bare name", "CREATE ROLE r1", ""},
		{"grant keeps a quoted grantee", "GRANT SELECT ON db1.o TO `null`", ""},
		{"structured node keeps a quoted null", "SELECT `null` FROM db1.o WHERE `true` = 1", ""},

		// Fix round 1: shapes the stricter rules must keep passing.
		{"clickhouse escapes kept", `SELECT 'it''s', 'a\'b', '\\', '\n', '\x41', $$a\_b$$, '''q''' FROM db1.o`, ""},
		{"quoted identifier escapes kept", "SELECT `a``b`, \"a\"\"b\", `a\\`b` FROM db1.o", ""},
		{"literal spellings kept", "SELECT x'41', X'4A', b'0101', 0x1F, 0b101, 1_000, 1e3, 1.50, 0x1Fp1, E'x' FROM db1.o", ""},
		{"unicode quotes are verbatim", `SELECT ‘a\_b’, “x\_y”, ‘it's’ FROM db1.o`, ""},
		{"leading-dot float", "SELECT .5 FROM db1.o", ""},
		{"not like isolated", "SELECT a FROM db1.o WHERE s NOT LIKE 'x%'", ""},
		{"not like beside and / or", "SELECT a FROM db1.o WHERE s NOT LIKE 'x%' AND b = 1 OR c NOT ILIKE {p:String}", ""},
		{"null-safe equality beside and / or", "SELECT a FROM db1.o WHERE a <=> b AND c OR t.d <=> 1", ""},
		{"div beside alias and order", "SELECT a DIV 2 AS x, t.a MOD {p:UInt8} FROM db1.o AS t ORDER BY a DIV 2 DESC", ""},
		{"ternary of lower operators", "SELECT a OR b ? c + 1 : d AND e, NOT a ? b IS NULL : c IN (1) FROM db1.o", ""},
		{"insert select column named format", "INSERT INTO db1.o SELECT format FROM db1.p", ""},

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
		// restoreFunctionSpellings puts the client's spelling back, so these
		// regenerate faithfully; a statement whose calls of one kind are
		// spelled two ways keeps Polyglot's respelling and is refused.
		{"char length keeps its spelling", "SELECT CHAR_LENGTH(s) FROM db1.o", ""},
		{"char length beside length", "SELECT CHAR_LENGTH(s), length(s) FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost [CHAR_LENGTH], added [LENGTH]"},
		{"group_concat separator", "SELECT group_concat(s, '-') FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost nothing, added [CONCAT]"},
		{"startswith keeps its spelling", "SELECT startsWith(s, 'x') FROM db1.o", ""},
		{"startswith spelled two ways", "SELECT startsWith(s, 'x') OR STARTSWITH(s, 'y') FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost [STARTSWITH STARTSWITH], added [STARTS_WITH STARTS_WITH]"},
		{"max_by is not on every 26.x", "SELECT max_by(a, b) FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost [MAX_BY], added [ARGMAX]"},
		{"first_value drops an argument", "SELECT first_value(a, b) FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost [B], added nothing"},
		{"json_query gains a path", "SELECT JSON_QUERY(s) FROM db1.o",
			"engine: generate: the regenerated statement differs from the input: lost nothing, added [$]"},
		{"live view becomes a view", "CREATE DEFINER=alice LIVE VIEW db1.v AS SELECT 1",
			"engine: generate: the regenerated statement differs from the input: lost [ALICE LIVE], added [ALICELIVE]"},

		// Fix round 1 (review C1): ClickHouse keeps the backslash of an
		// unknown escape; Polyglot drops it.
		{"like escape in delete", `DELETE FROM db1.o WHERE s LIKE 'x\_%'`, differs + `lost [x\_%], added [x_%]`},
		{"like escape in alter delete", `ALTER TABLE db1.o DELETE WHERE s LIKE 'x\_%'`, differs + `lost [x\_%], added [x_%]`},
		{"like percent escape", `SELECT a FROM db1.o WHERE s LIKE '100\%'`, differs + `lost [100\%], added [100%]`},
		{"ilike escape", `SELECT a FROM db1.o WHERE s ILIKE '%\_%'`, differs + `lost [%\_%], added [%_%]`},
		// A quoted identifier is decoded as ClickHouse decodes it when it is
		// parsed (decodeASTIdentifiers keeps the backslash of \_), and the
		// generator escapes that backslash, so the regenerated name is the
		// one ClickHouse reads from the input: regenerated faithfully. (Before
		// the ingestion decode, Polyglot's a_b was refused here.)
		{"escaped quoted table", "SELECT * FROM db1.`a\\_b`", ""},
		{"escaped double-quoted table", `SELECT * FROM db1."a\_b"`, ""},
		{"escaped quoted column", "SELECT `a\\_b` FROM db1.o", ""},
		{"escaped value", `INSERT INTO db1.o VALUES (1, 'a\_b')`, differs + `lost [a\_b], added [a_b]`},
		{"null escape value", `INSERT INTO db1.o VALUES ('\N')`, differs + `lost [], added [\N]`},
		{"escape z", `SELECT '\Z'`, differs + "lost [\\Z], added [\x1a]"},
		{"escape slash", `SELECT '\/'`, differs + `lost [/], added [\/]`},
		{"escape e", `SELECT '\e'`, differs + "lost [\x1b], added [\\e]"},
		{"escape bad hex", `SELECT '\xZZ'`, differs + "lost [\xef], added [\\xZZ]"},
		{"escape equals", `SELECT 'a\=b'`, differs + `lost [a=b], added [a\=b]`},
		{"escape backtick in a string", "SELECT 'a\\`b'", differs + "lost [a`b], added [a\\`b]"},
		{"escape hex runs past the quote", `SELECT '\x4', 'b'`, differs + `lost ['\x4'], added [\x4]`},
		{"triple double quote is an identifier", `SELECT """abc""" FROM db1.o`, differs + `lost ["ABC"], added [abc]`},

		// Fix round 1 (review I1 / I2): a respelling that regroups.
		{"null-safe equality under =", "SELECT a <=> b = c FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality under <", "SELECT a <=> b < c FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality under !=", "SELECT a <=> b != c FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality under like", "SELECT a <=> b LIKE c FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality under in", "SELECT a <=> b IN (1) FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality under between", "SELECT a <=> b BETWEEN 1 AND 2 FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality in where", "SELECT a FROM db1.o WHERE a <=> b = 0", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality after between and", "SELECT x BETWEEN 1 AND a <=> b FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"not before div", "SELECT NOT a DIV b FROM db1.o", differs + "lost [DIV], added [INTDIV]"},
		{"not before regexp", "SELECT NOT a REGEXP 'x' FROM db1.o", differs + "lost [REGEXP], added [MATCH]"},
		{"ternary over an unmeasured operator", "SELECT a::Int8 ? 1 : 2 FROM db1.o", differs + "lost [COLON PARAMETER], added [IF]"},
		{"ternary with an implicit alias", "SELECT a ? b : c x FROM db1.o", differs + "lost [COLON PARAMETER], added [IF]"},
		{"not like under =", "SELECT a NOT LIKE b = c FROM db1.o", differs + "lost [NOT LIKE], added [NOT]"},

		// A bare keyword the AST reads as a column is a single-token operand
		// (keywordColumnsAsNames): the Sentio driver's cluster probe.
		{"keyword column not like", "SELECT cluster FROM system.clusters WHERE cluster not like 'all-%'", ""},
		{"keyword column not ilike", "SELECT a FROM db1.o WHERE format NOT ILIKE 'x' AND key NOT LIKE 'y'", ""},
		{"qualified keyword column not like", "SELECT a FROM db1.o AS t WHERE t.cluster NOT LIKE 'x'", ""},
		{"keyword column regexp", "SELECT a FROM db1.o WHERE table REGEXP 'x'", ""},
		{"keyword column null-safe equality", "SELECT a FROM db1.o WHERE cluster <=> 1", ""},
		{"keyword column div", "SELECT a FROM db1.o WHERE date DIV 2 = 1", differs + "lost [DIV], added [INTDIV]"},
		{"keyword column not like under =", "SELECT cluster NOT LIKE b = c FROM db1.o", differs + "lost [NOT LIKE], added [NOT]"},
		{"typed literal is not a keyword column", "SELECT a FROM db1.o WHERE DATE '2020-01-01' NOT LIKE 'x'", differs + "lost [NOT LIKE], added [NOT]"},
		{"interval is not a keyword column", "SELECT a FROM db1.o WHERE interval NOT LIKE 'x'", differs + "lost nothing, added [PLUS INTERVAL]"},

		// Fix round 1 (review I3): a column named format is not a FORMAT clause.
		{"format column hides limit by", "INSERT INTO db1.o SELECT format FROM db1.p LIMIT 1 BY a LIMIT 2", differs + "lost [1 LIMIT], added nothing"},
		{"format column list hides limit by", "INSERT INTO db1.o (format) SELECT a FROM db1.p LIMIT 1 BY a LIMIT 2", differs + "lost [1 LIMIT], added nothing"},
		{"format column hides a cast", "INSERT INTO db1.o SELECT format, a::String FROM db1.p", differs + "lost nothing, added [NULLABLE]"},
		{"format column beside char_length", "INSERT INTO db1.o SELECT a FROM db1.p WHERE format = 1 AND CHAR_LENGTH(s) = 1", ""},
		{"format column hides char_length", "INSERT INTO db1.o SELECT a FROM db1.p WHERE format = 1 AND CHAR_LENGTH(s) = LENGTH(s)", differs + "lost [CHAR_LENGTH], added [LENGTH]"},
		{"format alias hides from", "INSERT INTO db1.o SELECT format x FROM db1.p", differs + "lost [FROM DB1.P], added nothing"},
		{"format column beside startswith", "INSERT INTO db1.o SELECT a FROM db1.p WHERE format = 1 AND startsWith(s, 'x')", ""},
		{"format column hides startswith", "INSERT INTO db1.o SELECT a FROM db1.p WHERE format = 1 AND startsWith(s, 'x') AND STARTS_WITH(s, 'y')", differs + "lost [STARTSWITH], added [STARTS_WITH]"},

		// Fix round 2 (re-review N1): a balanced CASE … END lower bound does
		// not hide the AND of its BETWEEN; the regeneration regroups
		// x BETWEEN … AND a <=> b as (x BETWEEN … AND a) <=> b.
		{"null-safe equality after between case", "SELECT x BETWEEN CASE WHEN c THEN 1 ELSE 2 END AND a <=> b FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality after not between case", "SELECT x NOT BETWEEN CASE WHEN c THEN 1 ELSE 2 END AND a <=> b FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality after between simple case", "SELECT x BETWEEN CASE c WHEN 1 THEN 1 END AND a <=> b FROM db1.o", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality after between case in where", "SELECT a FROM db1.o WHERE x BETWEEN CASE WHEN c THEN 1 ELSE 2 END AND a <=> b", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"null-safe equality after between case in delete", "DELETE FROM db1.o WHERE x BETWEEN CASE WHEN c THEN 1 ELSE 2 END AND a <=> b", differs + "lost [NULLSAFE_EQ], added [DISTINCT FROM IS NOT]"},
		{"not like after between case", "SELECT x BETWEEN CASE WHEN c THEN 1 ELSE 2 END AND a NOT LIKE b FROM db1.o", differs + "lost [NOT LIKE], added [NOT]"},
		{"not ilike after between case", "SELECT x BETWEEN CASE WHEN c THEN 1 ELSE 2 END AND a NOT ILIKE b FROM db1.o", differs + "lost [NOT ILIKE], added [NOT]"},
		{"regexp after between case", "SELECT x BETWEEN CASE WHEN c THEN 1 ELSE 2 END AND a REGEXP b FROM db1.o", differs + "lost [REGEXP], added [MATCH]"},
		{"null-safe equality after is not distinct from", "SELECT x IS NOT DISTINCT FROM y AND a <=> b, x BETWEEN 1 AND 2 AND y IS DISTINCT FROM z AND s NOT LIKE 'x' FROM db1.o", ""},
		{"between and then a logical and", "SELECT x BETWEEN 1 AND 2 AND a <=> b, x NOT BETWEEN CASE WHEN c THEN 1 END AND 2 AND s NOT LIKE 'x' FROM db1.o", ""},
		{"logical and after case", "SELECT CASE WHEN c THEN 1 END AND a <=> b, CASE WHEN c AND s REGEXP 'x' THEN 1 END FROM db1.o", ""},
		{"logical and in join on", "SELECT * FROM db1.o JOIN db1.p ON o.a = p.a AND o.b <=> p.b", ""},

		// Fix round 2 (re-review N2): the text after FORMAT <name> in an
		// INSERT … SELECT is dropped only when it is comments: with input()
		// it is the rows (measured on 26.2: FORMAT TSV 9 inserts 9, and
		// FORMAT TSV -- c inserts the row "-- c").
		{"insert select format comments", "INSERT INTO db1.o SELECT a FROM db1.p FORMAT JSON /* c */ -- d", ""},
		{"insert select input data", "INSERT INTO db1.o SELECT * FROM input('a UInt8') FORMAT TSV 7", differs + `lost input() data " 7", added nothing`},
		{"insert select input comment is data", "INSERT INTO db1.o SELECT * FROM input('a String') FORMAT TSV -- c", differs + `lost input() data " -- c", added nothing`},

		// Fix round 3 (re-review 2 R1): with input() anywhere in the statement,
		// any text after the FORMAT name beyond [ \t]*\n? is data, whether or
		// not the tokenizer emitted a data token (measured on 26.2 over HTTP:
		// FORMAT CSV, CSV\n and CSV \t\n insert nothing; CSV\n \n and
		// CSV \t\n\t\n insert a row; a CTE's FORMAT TSV -- c inserts "-- c").
		{"insert cte input comment is data", "INSERT INTO db1.o WITH x AS (SELECT * FROM input('a String')) SELECT * FROM x FORMAT TSV -- c", differs + `lost input() data " -- c", added nothing`},
		{"insert cte input block comment is data", "INSERT INTO db1.o WITH x AS (SELECT * FROM input('a String')) SELECT * FROM x FORMAT CSV /* c */", differs + `lost input() data " /* c */", added nothing`},
		{"insert select input blank line is data", "INSERT INTO db1.o SELECT * FROM input('a String') FORMAT CSV\n \n", differs + `lost input() data "\n \n", added nothing`},
		{"insert select input whitespace lines are data", "INSERT INTO db1.o SELECT * FROM input('a String') FORMAT TSV \t\n\t\n", differs + `lost input() data " \t\n\t\n", added nothing`},
		{"insert select input streamed", "INSERT INTO db1.o SELECT * FROM input('a String') FORMAT CSV", ""},
		{"insert select input streamed after a newline", "INSERT INTO db1.o SELECT * FROM input('a String') FORMAT CSV\n", ""},
		{"insert select input streamed after blanks and a newline", "INSERT INTO db1.o SELECT * FROM input('a String') FORMAT CSV \t\n", ""},

		// Fix round 4 (re-review 3 F1): the input() tail rule reads the token
		// stream, not the AST, so a set operation, a CTE before one and
		// SETTINGS before FORMAT are covered (measured on 26.2: the UNION ALL
		// input inserts "" and P, its regeneration only P). A statement that
		// reads input() and has a FORMAT token that is not the closing
		// FORMAT <name> is refused: the check cannot place its data. One with
		// no FORMAT token has no data and is compared in full.
		{"insert union all input blank line is data", "INSERT INTO db1.o SELECT a FROM db1.p UNION ALL SELECT a FROM input('a String') FORMAT CSV\n \n", differs + `lost input() data "\n \n", added nothing`},
		{"insert union all input first blank line is data", "INSERT INTO db1.o SELECT a FROM input('a String') UNION ALL SELECT a FROM db1.p FORMAT CSV\n\n", differs + `lost input() data "\n\n", added nothing`},
		{"insert union distinct input blank line is data", "INSERT INTO db1.o SELECT a FROM db1.p UNION DISTINCT SELECT a FROM input('a String') FORMAT CSV\n \n", differs + `lost input() data "\n \n", added nothing`},
		{"insert union input blank line is data", "INSERT INTO db1.o SELECT a FROM db1.p UNION SELECT a FROM input('a String') FORMAT CSV\n \n", differs + `lost input() data "\n \n", added nothing`},
		{"insert intersect input blank line is data", "INSERT INTO db1.o SELECT a FROM db1.p INTERSECT SELECT a FROM input('a String') FORMAT CSV\n \n", differs + `lost input() data "\n \n", added nothing`},
		{"insert except input first blank line is data", "INSERT INTO db1.o SELECT a FROM input('a String') EXCEPT SELECT a FROM db1.p FORMAT CSV\n \n", differs + `lost input() data "\n \n", added nothing`},
		{"insert cte union all input blank line is data", "INSERT INTO db1.o WITH x AS (SELECT * FROM input('a String')) SELECT * FROM x UNION ALL SELECT a FROM db1.p FORMAT CSV\n \n", differs + `lost input() data "\n \n", added nothing`},
		{"insert three-way union input blank line is data", "INSERT INTO db1.o SELECT a FROM db1.p UNION ALL SELECT a FROM input('a String') UNION ALL SELECT a FROM db1.q FORMAT CSV\n \n", differs + `lost input() data "\n \n", added nothing`},
		{"insert union all input settings then format blank line is data", "INSERT INTO db1.o SELECT a FROM db1.p UNION ALL SELECT a FROM input('a String') SETTINGS max_threads = 1 FORMAT CSV\n \n", differs + `lost input() data "\n \n", added nothing`},
		{"insert union all input streamed", "INSERT INTO db1.o SELECT a FROM db1.p UNION ALL SELECT a FROM input('a String') FORMAT CSV", ""},
		{"insert union all input streamed after a newline", "INSERT INTO db1.o SELECT a FROM db1.p UNION ALL SELECT a FROM input('a String') FORMAT CSV\n", ""},
		{"insert select input column named format", "INSERT INTO db1.o SELECT format FROM input('format String')", differs + "input() data position unknown: a FORMAT token does not end the statement"},
		{"insert select input without format", "INSERT INTO db1.o WITH unused AS (SELECT value FROM input('value Int64')) SELECT 7", ""},
		{"insert select format settings tail", "INSERT INTO db1.o SELECT a FROM db1.p FORMAT JSON SETTINGS max_threads = 1", differs + "lost [SETTINGS MAX_THREADS = 1], added nothing"},
		{"insert select format semicolon tail", "INSERT INTO db1.o SELECT a FROM db1.p FORMAT JSON;", differs + "lost [;], added nothing"},

		// Fix round 2 (re-review N3): ClickHouse reads a number only with each
		// _ between two digits (measured on 26.2: 1e_3, 0x_1F, 1_e3, 1__0,
		// 0x1F_ and 1_ are identifiers, 1._5 and 1.5a syntax errors).
		{"underscore after exponent", "SELECT 1e_3", differs + "lost [1e_3], added [1e3]"},
		{"underscore after exponent in delete", "DELETE FROM db1.o WHERE a = 1e_3", differs + "lost [1e_3], added [1e3]"},
		{"underscore after hex prefix", "SELECT 0x_1F", differs + "lost [0x_1F], added [0x1F]"},
		{"underscore before exponent", "SELECT 1_e3", differs + "lost [1_e3], added [1_e3]"},
		{"double underscore", "SELECT 1__0", differs + "lost [1__0], added [1__0]"},
		{"trailing underscore", "SELECT 0x1F_", differs + "lost [0x1F], added [0x1F]"},
		{"underscore after point", "SELECT 1._5", differs + "lost [1._5], added [1.5]"},
		{"letter after a float", "SELECT 1.5a", differs + "lost [1.5a], added [1.5a]"},
		{"underscores between digits", "SELECT 1_000, 0x1_F, 0b1_01, 1.5_0, 1e1_0, 1_000.5, 0_1, 0x1F_F, 0x1e_3 FROM db1.o", ""},

		// Fix round 2 (re-review N4): ClickHouse rejects OR at the top of the
		// else branch (measured on 26.2; AND, NOT, BETWEEN, IN, LIKE, IS,
		// comparisons and <=> are accepted there); Polyglot makes it valid.
		{"ternary else or", "SELECT a ? b : c OR d FROM db1.o", differs + "lost [COLON PARAMETER], added [IF]"},
		{"ternary else or in where", "SELECT a FROM db1.o WHERE a ? b : c OR d", differs + "lost [COLON PARAMETER], added [IF]"},
		{"ternary else and or", "SELECT a ? b : c AND d OR e FROM db1.o", differs + "lost [COLON PARAMETER], added [IF]"},
		{"ternary else comparison or", "SELECT a ? b : c = d OR e FROM db1.o", differs + "lost [COLON PARAMETER], added [IF]"},
		{"ternary else and, then or", "SELECT a ? b : c AND d, a ? b OR c : d, a ? b : (c OR d) FROM db1.o", ""},

		// Fix round 2 (re-review N5): ALTER … DELETE / UPDATE keeps its
		// expressions verbatim, so an input respelling does not apply where
		// the regeneration still spells the operator as the input does.
		{"alter delete keeps its operators", "ALTER TABLE db1.o DELETE WHERE d <=> e AND s REGEXP 'x' AND t NOT ILIKE 'y'", ""},
		{"alter delete keeps div and a ternary", "ALTER TABLE db1.o DELETE WHERE d DIV 2 AND (a ? b : c)", ""},

		// Review M3: cosmetic respellings still refused (fail closed).
		{"mod of a parenthesised operand", "SELECT a MOD (2 + 1) FROM db1.o", differs + "lost [MOD], added [PERCENT]"},
		{"regexp of a parenthesised operand", "SELECT a REGEXP ('x' = 1) FROM db1.o", differs + "lost [REGEXP], added [MATCH]"},
		{"format values rows", "INSERT INTO db1.o FORMAT Values (1, 'x')", differs + "lost [FORMAT], added nothing"},

		// A # in a regenerated bare word: ClickHouse reads `# ` as a comment
		// to the end of the line, so the tail of the statement disappears, and
		// any other # is an unrecognised token. Polyglot prints the alias of a
		// bare tuple without its quotes ("x#" as x#), so the spellings match.
		{"tuple alias with a hash", `SELECT (1, 2) AS "x#" FROM db1.o`, differs + `the regenerated token "x#"` + hash},
		{"tuple alias with a hash in with", `WITH (1, 2) AS "x#" SELECT a FROM db1.o`, differs + `the regenerated token "x#"` + hash},
		{"tuple alias with a hash inside", `SELECT (1, 2) AS "x#x" FROM db1.o`, differs + `the regenerated token "x#x"` + hash},
		{"tuple alias with a hash between", `WITH (1, 2) AS "a#b" SELECT a FROM db1.o`, differs + `the regenerated token "a#b"` + hash},
		{"tuple alias with a hash backtick", "SELECT (1, 2, 3) AS `x#` FROM db1.o", differs + `the regenerated token "x#"` + hash},
		{"quoted identifier with a hash keeps its quotes", `SELECT a AS "x#" FROM db1.o`, ""},
		{"array alias with a hash keeps its quotes", `SELECT [1, 2] AS "x#" FROM db1.o`, ""},
		{"string with a hash", "SELECT '#', 'x# y' FROM db1.o", ""},
		{"hash comment", "SELECT a # comment\nFROM db1.o", ""},
		{"hash bang comment", "SELECT a #! comment\nFROM db1.o", ""},
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
	// A client's own bare word with a # is refused by the whole-statement
	// parse gate first (the glued-'#' rule), and the drop gate still refuses
	// it on its own (checkBareTokens), so the two gates agree.
	t.Run("bare word with a hash", func(t *testing.T) {
		const sql = "SELECT a#b FROM db1.o"
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("ParseOne: %v", err)
		}
		if err := CheckParsedInFull(e, sql, ast); err == nil || !strings.Contains(err.Error(), `"a#b" is not a token ClickHouse reads`) {
			t.Fatalf("CheckParsedInFull = %v, want the glued-'#' refusal", err)
		}
		if err := CheckRegenerated(e, sql, ast); err == nil || err.Error() != differs+`the regenerated token "a#b"`+hash {
			t.Fatalf("CheckRegenerated = %v, want the regenerated-token refusal", err)
		}
	})
}

// regeneratedAs is the real engine with Generate replaced, so a test can
// present a regeneration the pinned Polyglot does not produce today.
type regeneratedAs struct {
	Engine
	gen string
}

func (r regeneratedAs) Generate(AST) (string, error) { return r.gen, nil }

// TestCheckRegeneratedLiteralValues pins that literals are compared by the
// value ClickHouse reads (review I4): the pinned Polyglot regenerates these
// literals verbatim, so the regeneration is supplied by the test.
func TestCheckRegeneratedLiteralValues(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name, sql, gen string
		want           string // "" = the same statement
	}{
		{"hex number", "SELECT 0x1F", "SELECT 0x20", differs + "lost [0x1F], added [0x20]"},
		{"hex number as decimal", "SELECT 0x1F", "SELECT 31", ""},
		{"binary number", "SELECT 0b101", "SELECT 0b110", differs + "lost [0b101], added [0b110]"},
		{"float is not an integer", "SELECT 1e3", "SELECT 1000", differs + "lost [1e3], added [1000]"},
		{"float spellings", "SELECT 1e3, .5", "SELECT 1000.0, 0.50", ""},
		{"float value", "SELECT 1.5", "SELECT 1.6", differs + "lost [1.5], added [1.6]"},
		{"hex string", "SELECT x'41'", "SELECT x'42'", differs + "lost [x'41'], added [x'42']"},
		{"hex string as string", "SELECT x'41'", "SELECT 'A'", ""},
		{"bit string", "SELECT b'0101'", "SELECT b'0110'", differs + "lost [b'0101'], added [b'0110']"},
		{"escape string case", "SELECT E'A'", "SELECT E'a'", differs + "lost [E'A'], added [E'a']"},
		{"escape string prefix case", "SELECT e'x'", "SELECT E'x'", ""},
		{"string escapes with the same value", `SELECT 'a\_b', '\x41'`, `SELECT 'a\\_b', 'A'`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatalf("ParseOne: %v", err)
			}
			err = CheckRegenerated(regeneratedAs{Engine: e, gen: tc.gen}, tc.sql, ast)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want || !errors.Is(err, ErrNotRegeneratedFaithfully) {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
}

// regenErrEngine fails the call a test names, so the error paths run without
// the FFI library.
type regenErrEngine struct {
	Engine        // nil: any other call panics
	genErr        error
	tokenizeErr   error
	tokenizeOK    int // Tokenize calls that succeed before tokenizeErr applies
	generateCalls int
	tokenizeCalls int
}

func (f *regenErrEngine) Generate(AST) (string, error) {
	f.generateCalls++
	if f.genErr != nil {
		return "", f.genErr
	}
	return "SELECT 1", nil
}

func (f *regenErrEngine) Tokenize(string) (AST, error) {
	f.tokenizeCalls++
	if f.tokenizeErr != nil && f.tokenizeCalls > f.tokenizeOK {
		return nil, f.tokenizeErr
	}
	return AST(`[]`), nil
}

// TestCheckRegeneratedErrors pins that every failure to run the comparison is
// an error (review M4 / M5): a caller refuses on any non-nil result, so no
// error path can pass a statement.
func TestCheckRegeneratedErrors(t *testing.T) {
	errGen := errors.New("fake generate error")
	errTok := errors.New("fake tokenize error")
	selectAST := AST(`{"select":{}}`)

	t.Run("node kind", func(t *testing.T) {
		for _, ast := range []AST{AST(`not json`), AST(`{}`), AST(`{"select":{},"insert":{}}`)} {
			f := &regenErrEngine{}
			if err := CheckRegenerated(f, "SELECT 1", ast); err == nil {
				t.Fatalf("CheckRegenerated(%s) = nil, want the NodeKind error", ast)
			}
			if f.generateCalls != 0 || f.tokenizeCalls != 0 {
				t.Fatalf("%s: generate %d / tokenize %d calls after a NodeKind error, want none", ast, f.generateCalls, f.tokenizeCalls)
			}
		}
	})
	t.Run("generator", func(t *testing.T) {
		f := &regenErrEngine{genErr: errGen}
		if err := CheckRegenerated(f, "SELECT 1", selectAST); !errors.Is(err, errGen) {
			t.Fatalf("err = %v, want the generator error", err)
		}
	})
	t.Run("tokenizer on the input", func(t *testing.T) {
		f := &regenErrEngine{tokenizeErr: errTok}
		if err := CheckRegenerated(f, "SELECT 1", selectAST); !errors.Is(err, errTok) {
			t.Fatalf("err = %v, want the tokenizer error", err)
		}
	})
	t.Run("tokenizer on the regeneration", func(t *testing.T) {
		f := &regenErrEngine{tokenizeErr: errTok, tokenizeOK: 1}
		if err := CheckRegenerated(f, "SELECT 1", selectAST); !errors.Is(err, errTok) {
			t.Fatalf("err = %v, want the tokenizer error", err)
		}
	})
	t.Run("two tokenizer calls on success", func(t *testing.T) {
		f := &regenErrEngine{}
		if err := CheckRegenerated(f, "SELECT 1", selectAST); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if f.generateCalls != 1 || f.tokenizeCalls != 2 {
			t.Fatalf("generate %d / tokenize %d calls, want 1 / 2", f.generateCalls, f.tokenizeCalls)
		}
	})
	// A command or raw node is compared by its own text: Generate is never
	// called, and a tokenizer error or an unreadable text is an error.
	for name, ast := range map[string]AST{
		"command node": AST(`{"command":{"this":"SHOW x"}}`),
		"raw node":     AST(`{"raw":{"sql":"SHOW x"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			f := &regenErrEngine{genErr: errGen}
			if err := CheckRegenerated(f, "SHOW x", ast); err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if f.generateCalls != 0 || f.tokenizeCalls != 2 {
				t.Fatalf("generate %d / tokenize %d calls, want 0 / 2", f.generateCalls, f.tokenizeCalls)
			}
		})
		t.Run(name+" tokenizer on the input", func(t *testing.T) {
			f := &regenErrEngine{tokenizeErr: errTok}
			if err := CheckRegenerated(f, "SHOW x", ast); !errors.Is(err, errTok) {
				t.Fatalf("err = %v, want the tokenizer error", err)
			}
		})
		t.Run(name+" tokenizer on its text", func(t *testing.T) {
			f := &regenErrEngine{tokenizeErr: errTok, tokenizeOK: 1}
			if err := CheckRegenerated(f, "SHOW x", ast); !errors.Is(err, errTok) {
				t.Fatalf("err = %v, want the tokenizer error", err)
			}
		})
	}
	t.Run("unreadable node text", func(t *testing.T) {
		for _, ast := range []AST{AST(`{"command":{"this":1}}`), AST(`{"raw":{"sql":1}}`)} {
			f := &regenErrEngine{}
			if err := CheckRegenerated(f, "SHOW x", ast); err == nil {
				t.Fatalf("CheckRegenerated(%s) = nil, want the decode error", ast)
			}
		}
	})
}

// TestClickHouseUnquote pins the decoder to ClickHouse's own reading, measured
// on clickhouse-server 26.2 and 26.7.5.10 with hex() of each string literal
// and the system.columns name of each quoted identifier.
func TestClickHouseUnquote(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
	}{
		{`'\a\b\e\f\n\r\t\v\0'`, "\a\b\x1b\f\n\r\t\v\x00"},
		{`'\\\'\"\/\='`, "\\'\"/="},
		{"'\\`'", "`"},
		{"'a\\\tb\\\nc'", "a\tb\nc"},
		{`'a\Nb'`, "ab"},
		{`'\x41\x4a\xZZ\xaZ\xg1'`, "\x41\x4a\xef\x9f\xf1"},
		{`'a\_b\%c\Zd\ue\1f'`, `a\_b\%c\Zd\ue\1f`},
		{"'a\\\x7fb\\\xc3\xa9'", "a\\\x7fb\\\xc3\xa9"},
		{`'it''s'`, "it's"},
		{`'''abc'''`, "'abc'"},
		{"`a\\_b`", `a\_b`},
		{"`a``b`", "a`b"},
		{"`a\\`b`", "a`b"},
		{"`a\\'b`", "a'b"},
		{`"a\_b"`, `a\_b`},
		{`"a""b"`, `a"b`},
		{`"""abc"""`, `"abc"`},
		{`"a\x41b"`, "aAb"},
	} {
		if got, ok := clickhouseUnquote(tc.raw); !ok || got != tc.want {
			t.Errorf("clickhouseUnquote(%q) = %q, %v; want %q", tc.raw, got, ok, tc.want)
		}
	}
	// ClickHouse would not read these with the same extent.
	for _, raw := range []string{`'\x4'`, `'\x'`, `'\x'''`, `'a\'`, `'a'b'`, `'a`, `x`, `'a"`} {
		if got, ok := clickhouseUnquote(raw); ok {
			t.Errorf("clickhouseUnquote(%q) = %q, want a failure", raw, got)
		}
	}
}

func TestNumberKey(t *testing.T) {
	for _, tc := range []struct {
		raw, want string // want "" = not decoded
	}{
		{"31", "31"},
		{"0031", "31"},
		{"0x1F", "31"},
		{"0X1f", "31"},
		{"0b11111", "31"},
		{"1_000", "1000"},
		{"18446744073709551615", "18446744073709551615"},
		{"18446744073709551616", "F18446744073709551616"},
		{"1e3", "F1000"},
		{"1000.0", "F1000"},
		{".5", "F1/2"},
		{"0.50", "F1/2"},
		{"1.", "F1"},
		{"1.5e-3", "F3/2000"},
		{"0x1Fp1", "F62"},
		{"0x1.8p1", "F3"},
		{"1e4000", ""},
		{"0x1p99999", ""},
		{"0x", ""},
		{"0b12", ""},
		{"1a", ""},
		// Fix round 2 (re-review N3): an _ must sit between two digits of the
		// literal's radix (measured on 26.2).
		{"0x1_F", "31"},
		{"0x1F_F", "511"},
		{"0x1e_3", "483"},
		{"0b1_01", "5"},
		{"0_1", "1"},
		{"1.5_0", "F3/2"},
		{"1e1_0", "F10000000000"},
		{"1_000.5", "F2001/2"},
		{"0x1p1_0", "F1024"},
		{"1e_3", ""},
		{"0x_1F", ""},
		{"1_e3", ""},
		{"1__0", ""},
		{"0x1F_", ""},
		{"1._5", ""},
		{"1_", ""},
		{"_1", ""},
		{"0b_1", ""},
		{"0x1F_p1", ""},
		{"", ""},
	} {
		got, ok := numberKey(tc.raw)
		if tc.want == "" {
			if ok {
				t.Errorf("numberKey(%q) = %q, want no value", tc.raw, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("numberKey(%q) = %q, %v; want %q", tc.raw, got, ok, tc.want)
		}
	}
}

func TestLiteralSpelling(t *testing.T) {
	for _, tc := range []struct {
		raw, key string
	}{
		{`'a\_b'`, `S:a\_b`},
		{`$$a\_b$$`, `S:a\_b`},
		{`$t$x$y$t$`, `S:x$y`},
		{`x'41'`, "S:A"},
		{`X'414'`, "S:\x04\x14"},
		{`b'0101'`, "S:\x05"},
		{`B'0100000101000010'`, "S:AB"},
		{`E'a\nb'`, "S:E'a\nb"},
		{`e'a\nb'`, "S:E'a\nb"},
		{`N'abc'`, "S:N'abc"},
		{"`a\\_b`", `W:A\_B`},
		{`"int"`, "W:INT32"},
		{`'\x4'`, `U:'\x4'`},
		{`x'4G'`, `U:x'4G'`},
		{`b'012'`, `U:b'012'`},
		{`$a$x$b$`, `U:$a$x$b$`},
		{`‘a\_b’`, `S:a\_b`},
		{`“a\_b”`, `W:A\_B`},
		{`‘a’b’`, `U:‘a’b’`},
	} {
		if got := literalSpelling(tc.raw, "", true); got.key != tc.key {
			t.Errorf("literalSpelling(%q) = %q, want %q", tc.raw, got.key, tc.key)
		}
	}
	if got := literalSpelling(`'\x4'`, "", false); got.key != `V:'\x4'` {
		t.Errorf("an undecodable regeneration lexeme = %q, want the V: side", got.key)
	}
	if got := literalSpelling(`'01'`, "W:INTERVAL", true); got.key != "N:1" {
		t.Errorf("INTERVAL '01' = %q, want the number 1", got.key)
	}
}

// TestIsLogicalAnd pins the backward scan behind a respelling after an AND
// (re-review N1): a balanced bracket or CASE … END is one unit, each BETWEEN
// owns the next AND, and a chain the scan cannot classify is not proven
// logical, so the respelling is refused. Each row is a token-type stream
// whose last token is the AND in question.
func TestIsLogicalAnd(t *testing.T) {
	for _, tc := range []struct {
		types string
		want  bool
	}{
		{"SELECT VAR AND", true},
		{"SELECT VAR BETWEEN NUMBER AND", false},
		{"SELECT VAR NOT BETWEEN NUMBER AND", false},
		{"SELECT VAR BETWEEN NUMBER AND NUMBER AND", true},
		{"SELECT VAR BETWEEN CASE WHEN VAR THEN NUMBER ELSE NUMBER END AND", false},
		{"SELECT VAR BETWEEN CASE VAR WHEN NUMBER THEN NUMBER END AND", false},
		{"SELECT VAR BETWEEN L_PAREN NUMBER R_PAREN AND", false},
		{"SELECT CASE WHEN VAR THEN NUMBER END AND", true},
		{"SELECT CASE WHEN VAR AND", true},
		{"WHERE VAR BETWEEN NUMBER AND NUMBER OR VAR AND", true},
		{"L_PAREN VAR BETWEEN NUMBER AND", false},
		{"VAR BETWEEN L_PAREN VAR AND", true}, // the AND is inside the bracket
		{"ON VAR DOT VAR EQ VAR DOT VAR AND", true},
		{"WHERE EXISTS L_PAREN SELECT NUMBER R_PAREN AND", true},
		{"WHERE VAR IS NOT DISTINCT FROM VAR AND", true},
		{"SELECT DISTINCT VAR AND", true},
		{"WHERE VAR BETWEEN VAR BETWEEN NUMBER AND NUMBER AND", false}, // a BETWEEN inside a BETWEEN's bounds
		{"WHERE VAR FROM VAR AND", false},                              // not IS [NOT] DISTINCT FROM
		{"SELECT VAR PARAMETER VAR COLON VAR AND", false},              // a ternary: not classified
		{"SELECT VAR SOMETHING_ELSE AND", false},                       // an unknown token
		{"SELECT NUMBER R_PAREN AND", false},                           // unbalanced
		{"SELECT L_PAREN NUMBER END AND", false},                       // mismatched
		{"SELECT CASE NUMBER R_PAREN AND", false},                      // mismatched
		{"VAR AND", true},
	} {
		var toks []rawToken
		for _, tt := range strings.Fields(tc.types) {
			toks = append(toks, rawToken{TokenType: tt, Text: tt})
		}
		if got := isLogicalAnd(toks, len(toks)-1); got != tc.want {
			t.Errorf("isLogicalAnd(%s) = %v, want %v", tc.types, got, tc.want)
		}
	}
}

// TestPolyglotCommandSQLKeepsPolyglotText pins the rebase of #54 onto #50:
// ParseOne stores the original statement in a command's "this" (every command
// check and splice reads it), but the drop gate must compare Polyglot's own
// text, or a streamed-VALUES INSERT that Polyglot kept as the command
// INSERT INTO VALUES (table and columns gone) is compared with itself and
// answered Success in the no-rewrite and static modes.
func TestPolyglotCommandSQLKeepsPolyglotText(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct{ sql, polyglot string }{
		{"INSERT INTO db1.o (a, b) VALUES", "INSERT INTO VALUES"},
		{"/* c */ INSERT INTO db1.o VALUES ;", "INSERT INTO VALUES"},
		{"SET max_threads = 1", "SET max_threads = 1"},
	} {
		ast, err := e.ParseOne(tc.sql)
		if err != nil {
			t.Fatalf("ParseOne(%q): %v", tc.sql, err)
		}
		orig, err := CommandSQL(ast)
		if err != nil {
			t.Fatalf("CommandSQL(%q): %v", tc.sql, err)
		}
		if want, _ := commandSourceText(e, tc.sql); orig != want {
			t.Fatalf("CommandSQL(%q) = %q, want the original %q", tc.sql, orig, want)
		}
		got, err := PolyglotCommandSQL(ast)
		if err != nil || got != tc.polyglot {
			t.Fatalf("PolyglotCommandSQL(%q) = %q, %v; want %q", tc.sql, got, err, tc.polyglot)
		}
		if tc.polyglot != orig {
			if err := CheckRegenerated(e, tc.sql, ast); !errors.Is(err, ErrNotRegeneratedFaithfully) {
				t.Fatalf("CheckRegenerated(%q) = %v, want ErrNotRegeneratedFaithfully", tc.sql, err)
			}
		}
	}
}

// TestKeywordColumnOperands pins the measured keyword-name operands
// (keywordNameTokens): each word, which the tokenizer types as a keyword,
// is read by Polyglot as a column in `<word> NOT LIKE 'x'`, the drop gate
// accepts the NOT LIKE respelling for it, and the regeneration is the one
// measured equivalent on ClickHouse 26.8 (NOT (<word> LIKE 'x'), the word an
// identifier there). The words keywordNameTokens leaves out keep the
// refusal.
func TestKeywordColumnOperands(t *testing.T) {
	e := newTestEngine(t)
	words := strings.Fields(`add after alter and anti any apply as asc asof auto_increment by cascade check cluster collate column
		comment commit constraint copy create cross cube current database date default delete desc describe distinct
		drop else end escape except execute fetch filter final first following for foreign format from full function
		grant groups having index inner intersect join key kill language last left limit local match materialized
		natural next nulls offset on only or outer over partition placing preceding prepare prewhere primary qualify
		range recursive references refresh rename replace restrict returns revoke right rollback rollup row rows
		sample select semi set settings show some system table temporary timestamp to transaction trigger truncate
		type unbounded union unique update use values view when where window with`)
	if len(words) != len(keywordNameTokens) {
		t.Fatalf("%d words for %d keyword token types", len(words), len(keywordNameTokens))
	}
	seen := map[string]bool{}
	for _, w := range words {
		toks, err := tokenizeRaw(e, w)
		if err != nil || len(toks) != 1 || !keywordNameTokens[toks[0].TokenType] {
			t.Fatalf("%s: token %+v is not a keyword name token", w, toks)
		}
		seen[toks[0].TokenType] = true
		sql := "SELECT a FROM db1.o WHERE " + w + " NOT LIKE 'x'"
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		gen, err := e.Generate(ast)
		if err != nil {
			t.Fatal(err)
		}
		if want := "SELECT a FROM db1.o WHERE NOT " + w + " LIKE 'x'"; gen != want {
			t.Fatalf("generated %q, want %q", gen, want)
		}
		if err := CheckRegenerated(e, sql, ast); err != nil {
			t.Errorf("%s: %v", sql, err)
		}
	}
	if len(seen) != len(keywordNameTokens) {
		t.Fatalf("words cover %d of %d keyword token types", len(seen), len(keywordNameTokens))
	}
	for _, w := range []string{"all", "then", "between", "exists", "ilike", "in", "regexp", "top"} {
		sql := "SELECT a FROM db1.o WHERE " + w + " NOT LIKE 'x'"
		ast, err := e.ParseOne(sql)
		if err != nil {
			continue
		}
		if CheckRegenerated(e, sql, ast) == nil {
			t.Fatalf("%s: the drop gate passed an unmeasured keyword operand", sql)
		}
	}
}
