package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// The old analyzer is refused (user ruling 2026-10-01, rewriter-grpc I3 review
// C2 / C3): its rewrites — the EXISTS rewrite that drops a projection alias,
// name-based WITH propagation that `legacy_column_name_of_tuple_literal = 1`
// defeats — turn an operand the binding rules trust into a table read.
// Measured on ClickHouse 25.8.28 and 26.2.15, phys.`db2.x` a view that throws
// LEAK_db2x when read:
//
//	SELECT EXISTS(SELECT 1) AS "db2.x", count() FROM phys."db1.o" WHERE a IN "db2.x"
//	  SETTINGS enable_analyzer = 0                       -> LEAK (= 1, or unset: no read)
//	WITH tuple(1, 2) AS "db2.x" … WHERE a IN (SELECT a FROM … WHERE a IN "db2.x")
//	  SETTINGS enable_analyzer = 0, legacy_column_name_of_tuple_literal = 1 -> LEAK
//
// So enable_analyzer / allow_experimental_analyzer (one setting; the second is
// an alias) are refused unless the value is a literal ClickHouse reads as true,
// legacy_column_name_of_tuple_literal and profile are refused whatever the
// value, in every settings position and both SI states.

func analyzerRefusal(name string) string { return "table setting " + name + " is not accepted" }

func TestAnalyzerSettings_OldAnalyzerReproducersAreRefused(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, name string }{
			// C2 and its positions.
			{"SELECT EXISTS(SELECT 1) AS `db2.x`, count() FROM db1.o WHERE a IN `db2.x` SETTINGS enable_analyzer = 0", "enable_analyzer"},
			{"SELECT EXISTS(SELECT 1) AS `db2.x`, count() FROM db1.o WHERE a IN `db2.x` SETTINGS allow_experimental_analyzer = 0", "allow_experimental_analyzer"},
			{"SELECT EXISTS(SELECT 1) AS `db2.x`, count() FROM db1.o GROUP BY a HAVING a IN `db2.x` SETTINGS enable_analyzer = 0", "enable_analyzer"},
			{"SELECT EXISTS(SELECT 1) AS `db2.x`, a FROM db1.o ORDER BY a IN `db2.x` SETTINGS enable_analyzer = 0", "enable_analyzer"},
			{"SELECT EXISTS(SELECT 1) AS `db2.x`, a FROM db1.o LIMIT 1 BY a IN `db2.x` SETTINGS enable_analyzer = 0", "enable_analyzer"},
			{"SELECT EXISTS(SELECT 1) AS `db2.x`, a FROM db1.o PREWHERE a IN `db2.x` SETTINGS enable_analyzer = 0", "enable_analyzer"},
			{"SELECT EXISTS(SELECT 1) AS `db2.t`, a FROM db1.o WHERE a IN `db1.t` SETTINGS enable_analyzer = false", "enable_analyzer"},
			{"INSERT INTO db1.o SELECT EXISTS(SELECT 1) AS `db2.x` FROM db1.p WHERE a IN `db2.x` SETTINGS enable_analyzer = 0", "enable_analyzer"},
			{"INSERT INTO db1.o SETTINGS enable_analyzer = 0 SELECT EXISTS(SELECT 1) AS `db2.x` FROM db1.p WHERE a IN `db2.x`", "enable_analyzer"},
			{"CREATE TABLE db1.n ENGINE = Memory AS SELECT EXISTS(SELECT 1) AS `db2.x` FROM db1.p WHERE a IN `db2.x` SETTINGS enable_analyzer = 0", "enable_analyzer"},
			{"CREATE VIEW db1.v AS SELECT EXISTS(SELECT 1) AS `db2.x` FROM db1.p WHERE a IN `db2.x` SETTINGS enable_analyzer = 0", "enable_analyzer"},
			{"SELECT * FROM (SELECT EXISTS(SELECT 1) AS `db2.x` FROM db1.p WHERE a IN `db2.x` SETTINGS enable_analyzer = 0)", "enable_analyzer"},
			// C3.
			{"WITH tuple(1, 2) AS `db2.x` SELECT count() FROM db1.o WHERE a IN (SELECT a FROM db1.o WHERE a IN `db2.x`) SETTINGS enable_analyzer = 0, legacy_column_name_of_tuple_literal = 1", "enable_analyzer"},
			{"WITH tuple(1, 2) AS `db2.x` SELECT count() FROM db1.o WHERE a IN (SELECT a FROM db1.o WHERE a IN `db2.x`) SETTINGS legacy_column_name_of_tuple_literal = 1", "legacy_column_name_of_tuple_literal"},
			{"SELECT 1 SETTINGS legacy_column_name_of_tuple_literal = 0", "legacy_column_name_of_tuple_literal"},
			// A settings profile switches settings as a group (it may carry
			// enable_analyzer = 0).
			{"SELECT a FROM db1.o SETTINGS profile = 'default'", "profile"},
			// A command tail and a storage SETTINGS clause.
			{"ALTER TABLE db1.o UPDATE a = 1 WHERE 1 SETTINGS enable_analyzer = 0", "enable_analyzer"},
			{"CREATE TABLE db1.n (a UInt8) ENGINE = MergeTree ORDER BY a SETTINGS enable_analyzer = 0", "enable_analyzer"},
		} {
			msg := analyzerRefusal(c.name)
			if si && strings.HasPrefix(c.sql, "ALTER TABLE db1.o UPDATE") {
				// With the SI surface active an ALTER … UPDATE command with any
				// SETTINGS tail is refused first (as for every R5 name).
				msg = "statement is not supported"
			}
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: msg, wantSQL: c.sql})
		}
	}
	// A session SET: the carve-out (SI surface inactive) refuses the name;
	// with the surface active every SET is the SI catch-all.
	for _, c := range []struct{ sql, name string }{
		{"SET enable_analyzer = 0", "enable_analyzer"},
		{"SET allow_experimental_analyzer = 'false'", "allow_experimental_analyzer"},
		{"SET max_threads = 1, enable_analyzer = 0", "enable_analyzer"},
		{"SET legacy_column_name_of_tuple_literal = 1", "legacy_column_name_of_tuple_literal"},
		{"SET profile = 'readonly'", "profile"},
	} {
		cases = append(cases,
			tablerefCase{name: c.sql, sql: c.sql, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: analyzerRefusal(c.name), wantSQL: c.sql},
			tablerefCase{name: "si/" + c.sql, sql: c.sql, si: true, wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: StorageIntegrityUnmodelledMessage})
	}
	runTablerefCases(t, cases)
}

// trueSpellings are the values admitted for enable_analyzer /
// allow_experimental_analyzer: measured true on 25.8 and 26.2 (session SET and
// query SETTINGS; 25.8 rejects the string forms in a query-level SETTINGS
// clause, which is harmless). Every other value is refused, including values
// ClickHouse also reads as true (0x1, +1, 1.0, 0.5, 1e0, x'31', $$true$$):
// the list is closed and compared on the raw source lexeme.
var trueSpellings = []string{"1", "true", "TRUE", "True", "'1'", "'true'", "'TRUE'", "'True'"}

var notTrueSpellings = []string{
	"0", "false", "FALSE", "'0'", "'false'", "2", "-1", "1.0", "0.5", "1e0", "0x1", "+1", "01",
	"x'31'", "$$true$$", "'tr\\x75e'", "'01'", "' 1'", "''", "NULL", "yes", "`true`", "\"true\"",
	"(1)", "1 = 1", "toBool(1)", "{v:Bool}", "[1]",
}

func TestAnalyzerSettings_ValueSpellings(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, name := range []string{"enable_analyzer", "allow_experimental_analyzer"} {
			for _, v := range trueSpellings {
				for _, sql := range []string{
					"SELECT a FROM db1.o SETTINGS " + name + " = " + v,
					"SELECT a FROM db1.o SETTINGS max_threads = 1, " + name + " = " + v + " FORMAT JSON",
					"SELECT * FROM (SELECT a FROM db1.o SETTINGS " + name + " = " + v + ")",
				} {
					resp, err := doRewrite(e, sql, tablerefOpts(si))
					if err != nil {
						t.Fatalf("doRewrite(%q): %v", sql, err)
					}
					if resp.GetCode() != pb.RewriteCode_Success {
						t.Errorf("si=%v: true spelling %q refused: %s %q", si, sql, resp.GetCode(), resp.GetMessage())
					}
				}
				if !si {
					sql := "SET " + name + " = " + v
					resp, err := doRewrite(e, sql, tablerefOpts(si))
					if err != nil {
						t.Fatalf("doRewrite(%q): %v", sql, err)
					}
					if resp.GetCode() != pb.RewriteCode_Success {
						t.Errorf("true spelling %q refused: %s %q", sql, resp.GetCode(), resp.GetMessage())
					}
				}
			}
			for _, v := range notTrueSpellings {
				for _, sql := range []string{
					"SELECT a FROM db1.o SETTINGS " + name + " = " + v,
					"SELECT a FROM db1.o SETTINGS " + name + " = " + v + ", max_threads = 1",
					"SELECT * FROM (SELECT a FROM db1.o SETTINGS " + name + " = " + v + ")",
					"SET " + name + " = " + v,
				} {
					resp, err := doRewrite(e, sql, tablerefOpts(si))
					if err != nil {
						t.Fatalf("doRewrite(%q): %v", sql, err)
					}
					if resp.GetCode() == pb.RewriteCode_Success {
						t.Errorf("si=%v: %q answered Success", si, sql)
					}
					if !si && resp.GetCode() == pb.RewriteCode_UnsupportedStatement && !strings.HasPrefix(sql, "SET") &&
						resp.GetMessage() != analyzerRefusal(name) && resp.GetMessage() != "statement is not supported" {
						t.Errorf("%q: message %q", sql, resp.GetMessage())
					}
				}
			}
		}
	}
}
