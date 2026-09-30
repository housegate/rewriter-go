package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// escapedNameTwin is one statement written twice: plain, and with a quoted
// database / table / column name spelled through ClickHouse escapes that
// ClickHouse decodes to the same name (`ph\Nys` is phys, `hg_\Nsafe` is
// hg_safe, db1.`t\N` is db1.t, `_hg_\Nrow_id` is _hg_row_id — each measured on
// ClickHouse 26.2). The escaped spelling must answer exactly like the plain
// one: before round 1 the rewriter compared Polyglot's undecoded `ph\Nys`, so
// a protected, reserved or Active object escaped every check.
type escapedNameTwin struct{ plain, escaped string }

var escapedNameTwins = []escapedNameTwin{
	// Protected physical database phys.
	{"SELECT * FROM phys.`db2.x`", "SELECT * FROM `ph\\Nys`.`db2.x`"},
	{"SELECT * FROM phys.`db2.x`", "SELECT * FROM \"ph\\Nys\".\"db2.x\""},
	{"SELECT * FROM db1.o JOIN phys.`db2.x` USING (a)", "SELECT * FROM db1.o JOIN `ph\\Nys`.`db2.x` USING (a)"},
	{"SELECT a FROM db1.o WHERE a IN phys.`db2.x`", "SELECT a FROM db1.o WHERE a IN `ph\\Nys`.`db2.x`"},
	{"INSERT INTO db1.o SELECT * FROM phys.`db2.x`", "INSERT INTO db1.o SELECT * FROM `ph\\Nys`.`db2.x`"},
	{"ALTER TABLE phys.`db2.x` DELETE WHERE 1", "ALTER TABLE `ph\\Nys`.`db2.x` DELETE WHERE 1"},
	{"ALTER TABLE db1.o UPDATE b = 1 WHERE a IN (SELECT a FROM phys.`db2.x`)", "ALTER TABLE db1.o UPDATE b = 1 WHERE a IN (SELECT a FROM `ph\\Nys`.`db2.x`)"},
	{"DROP TABLE phys.`db2.x`", "DROP TABLE `ph\\Nys`.`db2.x`"},
	{"DESCRIBE TABLE phys.`db2.x`", "DESCRIBE TABLE `ph\\Nys`.`db2.x`"},
	{"EXISTS TABLE phys.`db2.x`", "EXISTS TABLE `ph\\Nys`.`db2.x`"},
	{"SHOW CREATE TABLE phys.`db2.x`", "SHOW CREATE TABLE `ph\\Nys`.`db2.x`"},
	{"RENAME TABLE db1.o TO phys.`db2.y`", "RENAME TABLE db1.o TO `ph\\Nys`.`db2.y`"},
	{"USE phys", "USE `ph\\Nys`"},
	// Reserved storage-integrity database hg_safe (the SI message with the
	// surface active, the protected-database message without it).
	{"SELECT * FROM hg_safe.db1__t", "SELECT * FROM `hg_\\Nsafe`.db1__t"},
	{"SELECT a, _hg_row_id FROM hg_safe.db1__t", "SELECT a, _hg_row_id FROM `hg_\\Nsafe`.db1__t"},
	{"SELECT * FROM db1.o JOIN hg_safe.db1__t USING (a)", "SELECT * FROM db1.o JOIN `hg_\\Nsafe`.db1__t USING (a)"},
	{"SELECT a FROM db1.o WHERE a IN hg_safe.db1__t", "SELECT a FROM db1.o WHERE a IN `hg_\\Nsafe`.db1__t"},
	{"INSERT INTO db1.o SELECT a FROM hg_safe.db1__t", "INSERT INTO db1.o SELECT a FROM `hg_\\Nsafe`.db1__t"},
	{"DROP TABLE hg_safe.db1__t", "DROP TABLE `hg_\\Nsafe`.db1__t"},
	{"DESCRIBE TABLE hg_safe.db1__t", "DESCRIBE TABLE `hg_\\Nsafe`.db1__t"},
	// The Active table db1.t (SI on) and its escaped twin: the twin must take
	// the same derived hg_safe read, not the ordinary physical table.
	{"SELECT * FROM db1.t", "SELECT * FROM db1.`t\\N`"},
	{"SELECT * FROM db1.t", "SELECT * FROM db1.`\\x74`"},
	{"SELECT * FROM db1.t", "SELECT * FROM `db\\N1`.t"},
	{"SELECT * FROM db1.o JOIN db1.t USING (a)", "SELECT * FROM db1.o JOIN db1.`t\\N` USING (a)"},
	{"SELECT a FROM db1.o WHERE a IN db1.t", "SELECT a FROM db1.o WHERE a IN db1.`t\\N`"},
	{"INSERT INTO db1.o SELECT * FROM db1.t", "INSERT INTO db1.o SELECT * FROM db1.`t\\N`"},
	{"ALTER TABLE db1.t DELETE WHERE 1", "ALTER TABLE db1.`t\\N` DELETE WHERE 1"},
	{"ALTER TABLE db1.t UPDATE a = 1 WHERE 1", "ALTER TABLE db1.`t\\N` UPDATE a = 1 WHERE 1"},
	{"DROP TABLE db1.t", "DROP TABLE db1.`t\\N`"},
	{"EXISTS TABLE db1.t", "EXISTS TABLE db1.`t\\N`"},
	{"INSERT INTO db1.t VALUES (1)", "INSERT INTO db1.`t\\N` VALUES (1)"},
	// Reserved row-id column on the Active table.
	{"SELECT `_hg_row_id` FROM db1.t", "SELECT `_hg_\\Nrow_id` FROM db1.t"},
	{"SELECT a FROM db1.t WHERE `_hg_row_id` = ''", "SELECT a FROM db1.t WHERE `\\N_hg_row_id` = ''"},
	// F4 (round 2): Polyglot marks an EXCEPT list quoted: false.
	{"SELECT * EXCEPT (`_hg_row_id`) FROM db1.t", "SELECT * EXCEPT (`_hg_\\Nrow_id`) FROM db1.t"},
	// An ordinary tenant table and its twin resolve through database_map the same way.
	{"SELECT * FROM db1.o", "SELECT * FROM `db\\N1`.`o\\N`"},
	{"ALTER TABLE db1.o UPDATE b = 1 WHERE 1", "ALTER TABLE db1.`o\\N` UPDATE b = 1 WHERE 1"},
	{"RENAME TABLE db1.o TO db1.p", "RENAME TABLE db1.`o\\N` TO `db\\N1`.p"},
	{"SHOW CREATE TABLE db1.o", "SHOW CREATE TABLE db1.`o\\N`"},
}

// TestTableRef_EscapedNamesAnswerLikePlain pins round 1: every quoted database,
// table and column name is decoded as ClickHouse decodes it when it is read
// from the Polyglot AST or tokens, so the escaped twin gets the plain
// spelling's code and message in both SI states, and a successful rewrite
// emits the same SQL (a statement forwarded verbatim is echoed as written, so
// there only the verdict is compared).
func TestTableRef_EscapedNamesAnswerLikePlain(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, tw := range escapedNameTwins {
			name := tw.escaped
			if si {
				name = "si/" + name
			}
			t.Run(name, func(t *testing.T) {
				plain, err := doRewrite(e, tw.plain, tablerefOpts(si))
				if err != nil {
					t.Fatalf("plain doRewrite: %v", err)
				}
				esc, err := doRewrite(e, tw.escaped, tablerefOpts(si))
				if err != nil {
					t.Fatalf("escaped doRewrite: %v", err)
				}
				if esc.GetCode() != plain.GetCode() || esc.GetMessage() != plain.GetMessage() {
					t.Fatalf("escaped = %s %q, plain = %s %q", esc.GetCode(), esc.GetMessage(), plain.GetCode(), plain.GetMessage())
				}
				if plain.GetCode() != pb.RewriteCode_Success {
					return
				}
				if plain.GetSqlAfterRewrite() == tw.plain {
					if esc.GetSqlAfterRewrite() != tw.escaped {
						t.Fatalf("verbatim plain, but escaped emitted %q", esc.GetSqlAfterRewrite())
					}
					return
				}
				// Identifier double quotes are compared away: the decoded twin of a
				// quote-lost node (an EXCEPT list) is re-emitted quoted, the plain
				// one bare, and no twin name contains a double quote.
				unq := func(s string) string { return strings.ReplaceAll(s, `"`, "") }
				if unq(esc.GetSqlAfterRewrite()) != unq(plain.GetSqlAfterRewrite()) {
					t.Fatalf("escaped sql = %q, plain sql = %q", esc.GetSqlAfterRewrite(), plain.GetSqlAfterRewrite())
				}
			})
		}
	}
}

// TestTableRef_EscapedNamePins spells out the headline answers of round 1, so a
// reader does not have to derive them from the twin table.
func TestTableRef_EscapedNamePins(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases,
			tablerefCase{name: "phys", si: si, sql: "SELECT * FROM `ph\\Nys`.`db2.x`",
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable"},
			tablerefCase{name: "describe_phys", si: si, sql: "DESCRIBE TABLE `ph\\Nys`.`db2.x`",
				wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database phys is not addressable"},
			// A doubled backslash is a real backslash in ClickHouse's name:
			// `t\\N` names the table t\N, emitted escaped so ClickHouse reads it back.
			tablerefCase{name: "escaped_backslash_kept", si: si, sql: "SELECT * FROM db1.`t\\\\N`",
				wantCode: pb.RewriteCode_Success, wantSQL: `SELECT * FROM phys."db1.t\\N" "db1.t\\N"`},
			// \/ and \e decode (/ and ESC), \: keeps its backslash; the alias is
			// emitted as the decoded name.
			tablerefCase{name: "alias_fidelity", si: si, sql: "SELECT 1 AS `a\\/b`, 2 AS `c\\Nd`, 3 AS `e\\:f` FROM db1.o",
				wantCode: pb.RewriteCode_Success, wantSQL: `SELECT 1 AS "a/b", 2 AS "cd", 3 AS "e\\:f" FROM phys."db1.o" "db1.o"`},
			// An identifier is refused only where ClickHouse rejects it: a name
			// that decodes to nothing, or a \x that swallows the closing quote.
			tablerefCase{name: "rejected_swallowed_quote", si: si, sql: "SELECT * FROM `ab\\x6`.x",
				wantCode: pb.RewriteCode_SyntaxError, wantMsg: "is not a name ClickHouse accepts"},
			tablerefCase{name: "empty_after_decode", si: si, sql: "SELECT * FROM `\\N`.x",
				wantCode: pb.RewriteCode_SyntaxError, wantMsg: "is not a name ClickHouse accepts"},
			// Low (review of c3eca8c), pinned as it behaves: Polyglot collapses
			// the source `\\` before any decode, so `\\Nin` is read one level too
			// far as in. In a SELECT the IN operand is then rewritten into the
			// caller's own namespace; on the verbatim ALTER path it is refused.
			// ClickHouse would call the unknown function \Nin (no read), so both
			// are the safe direction.
			tablerefCase{name: "low_double_backslash_select", si: si, sql: "SELECT * FROM db1.o WHERE `\\\\Nin`(a, `db2.x`)",
				wantCode: pb.RewriteCode_Success, wantSQL: `SELECT * FROM phys."db1.o" "db1.o" WHERE \Nin(a, phys."db1.db2.x")`},
			tablerefCase{name: "low_double_backslash_alter", si: si, sql: "ALTER TABLE db1.o DELETE WHERE `\\\\Nin`(a, `db2.x`)",
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"},
		)
	}
	cases = append(cases,
		tablerefCase{name: "active_twin_derived_read", si: true, sql: "SELECT * FROM db1.`t\\N`",
			wantCode: pb.RewriteCode_Success, wantSQL: `SELECT * FROM (SELECT * EXCEPT (_hg_row_id) FROM hg_safe.db1__t) AS "db1.t"`},
		tablerefCase{name: "hg_safe_si", si: true, sql: "SELECT a, _hg_row_id FROM `hg_\\Nsafe`.db1__t",
			wantMsg: "storage-integrity physical table hg_safe.db1__t", wantCode: pb.RewriteCode_RewriteError},
		tablerefCase{name: "hg_safe_off", sql: "SELECT a, _hg_row_id FROM `hg_\\Nsafe`.db1__t",
			wantCode: pb.RewriteCode_InvalidRewriteRequest, wantMsg: "protected database hg_safe is not addressable"},
		tablerefCase{name: "reserved_column", si: true, sql: "SELECT `_hg_\\Nrow_id` FROM db1.t",
			wantCode: pb.RewriteCode_RewriteError, wantMsg: "reserved column _hg_row_id is not addressable"},
	)
	runTablerefCases(t, cases)
	// The emitted ESC byte is the decoded \e, not a dropped or kept escape.
	e := newEngine(t)
	resp, err := doRewrite(e, "SELECT 1 AS `i\\ej` FROM db1.o", tablerefOpts(false))
	if err != nil || !strings.Contains(resp.GetSqlAfterRewrite(), "i\x1bj") {
		t.Fatalf("alias \\e: %v %q", err, resp.GetSqlAfterRewrite())
	}
}
