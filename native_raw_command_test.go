package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// rawCommandTrickyNames are table names whose quoting Polyglot's re-rendered
// command text got wrong (it re-quotes in "…" without escaping), each with the
// name ClickHouse reads. Before round 2 the RENAME / EXCHANGE / ALTER … UPDATE
// checks tokenized that re-rendering while the original SQL was forwarded, so
// token boundaries shifted and a target present in the original was never
// decided.
var rawCommandTrickyNames = []struct{ src, name string }{
	{"`\"`", `"`},
	{"`a\"b`", `a"b`},
	{"\"a\"\"b\"", `a"b`},
	{"`a``b`", "a`b"},
	{"`a\\\\`", `a\`}, // a name ending in a backslash
	{"`a\\N`", "a"},
	{"`a,b`", "a,b"},
	{"`a b`", "a b"},
	{"`TO`", "TO"},
	{"`AND`", "AND"},
	{"`x TO phys.y`", "x TO phys.y"},
}

// TestTableRef_RawCommandTargetsDecidedFromSource pins round 2, F1: every
// table target of a RENAME / EXCHANGE / ALTER … UPDATE / GRANT is read from the
// original statement and decided, in both SI states. A statement that reaches
// the protected phys anywhere is refused; one that does not is rewritten with
// every target physical.
func TestTableRef_RawCommandTargetsDecidedFromSource(t *testing.T) {
	e := newEngine(t)
	const other = "phys.`db2.x`"
	for _, si := range []bool{false, true} {
		for _, n := range rawCommandTrickyNames {
			refused := []string{
				"ALTER TABLE phys." + n.src + " UPDATE a = 1 WHERE 1",
				"GRANT SELECT ON phys." + n.src + " TO u",
			}
			// The protected target at each position of a two-pair RENAME and of
			// an EXCHANGE, with the tricky name at every subset of the others
			// (the review's reproducer is RENAME TABLE db1.`"` TO db1.y,
			// phys.`db2.x` TO db1.`"`).
			fill := []string{"db1.y", "db1.z", "db1.w", "db1.v"}
			for protected := 0; protected < 4; protected++ {
				for mask := 1; mask < 16; mask++ {
					if mask&(1<<protected) != 0 {
						continue
					}
					pos := make([]string, 4)
					for i := range pos {
						switch {
						case i == protected:
							pos[i] = other
						case mask&(1<<i) != 0:
							pos[i] = "db1." + n.src
						default:
							pos[i] = fill[i]
						}
					}
					refused = append(refused, "RENAME TABLE "+pos[0]+" TO "+pos[1]+", "+pos[2]+" TO "+pos[3])
					if protected < 2 && mask < 4 {
						refused = append(refused, "EXCHANGE TABLES "+pos[0]+" AND "+pos[1])
					}
				}
			}
			for _, sql := range refused {
				resp, err := doRewrite(e, sql, tablerefOpts(si))
				if err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
				if resp.GetCode() == pb.RewriteCode_Success {
					t.Errorf("si=%v %s: Success, forwarded %q; want refused", si, sql, resp.GetSqlAfterRewrite())
				}
			}
			rewritten := map[string][]string{
				"RENAME TABLE db1." + n.src + " TO db1.y":                 {"db1." + n.name, "db1.y"},
				"RENAME TABLE db1.o TO db1." + n.src + ", db1.y TO db1.z": {"db1.o", "db1." + n.name, "db1.y", "db1.z"},
				"EXCHANGE TABLES db1." + n.src + " AND db1.o":             {"db1." + n.name, "db1.o"},
				"ALTER TABLE db1." + n.src + " UPDATE a = 1 WHERE 1":      {"db1." + n.name},
				"/* c */ RENAME TABLE db1.o TO db1." + n.src + " ;":       {"db1.o", "db1." + n.name},
			}
			for sql, want := range rewritten {
				resp, err := doRewrite(e, sql, tablerefOpts(si))
				if err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
				if resp.GetCode() != pb.RewriteCode_Success {
					t.Errorf("si=%v %s: %s %q; want Success", si, sql, resp.GetCode(), resp.GetMessage())
					continue
				}
				out := resp.GetSqlAfterRewrite()
				ast, err := e.ParseOne(out)
				if err != nil {
					t.Errorf("si=%v %s: emitted %q does not parse: %v", si, sql, out, err)
					continue
				}
				refs, _, err := engine.RawTableRefs(e, ast)
				if err != nil {
					t.Fatalf("%s: %v", out, err)
				}
				var got []string
				for _, r := range refs {
					if r.DB != "phys" {
						t.Errorf("si=%v %s: emitted %q keeps a non-physical target %+v", si, sql, out, r)
					}
					got = append(got, r.Table)
				}
				if strings.Join(got, "|") != strings.Join(want, "|") {
					t.Errorf("si=%v %s: emitted %q names %q, want %q", si, sql, out, got, want)
				}
			}
		}
	}
}

// TestTableRef_Round2Pins pins round 2's F2 and F3.
func TestTableRef_Round2Pins(t *testing.T) {
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		cases = append(cases,
			// F2: a string literal is never refused for its bytes; ClickHouse
			// accepts '\xFF' (hex '\xFF' = FF) and a non-hex '\xZZ'.
			tablerefCase{name: "select_binary_literal", si: si, sql: "SELECT '\\xFF'", wantCode: pb.RewriteCode_Success},
			tablerefCase{name: "values_binary_literal", si: si, sql: "INSERT INTO db1.o VALUES (1, '\\xFF')", wantCode: pb.RewriteCode_Success},
			tablerefCase{name: "select_garbage_hex_literal", si: si, sql: "SELECT 'a\\xZZb' FROM db1.o", wantCode: pb.RewriteCode_Success},
			// F2: ClickHouse accepts a non-UTF-8 identifier, so it is not refused.
			tablerefCase{name: "non_utf8_identifier", si: si, sql: "SELECT * FROM db1.`\\xFF`", wantCode: pb.RewriteCode_Success},
			// ClickHouse's unhex makes `n\x7ZtIn` notIn: refused like the plain spelling.
			tablerefCase{name: "garbage_hex_notin_alter", si: si, sql: "ALTER TABLE db1.o DELETE WHERE `n\\x7ZtIn`(a, `db2.x`)",
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: "statement is not supported"},
			tablerefCase{name: "garbage_hex_protected_db", si: si, sql: "SELECT * FROM `ph\\x7Zys`.x",
				wantCode: pb.RewriteCode_Success}, // ph o ys is not phys: an ordinary unknown database
			tablerefCase{name: "garbage_hex_hg_safe", si: si, sql: "SELECT * FROM `hg\\x6Zsafe`.db1__t",
				wantCode: pb.RewriteCode_InvalidRewriteRequest},
		)
		if si {
			cases[len(cases)-1].wantCode = pb.RewriteCode_RewriteError
		}
	}
	// F2: an UPDATE assigning a binary literal (db1.o is an ordinary table in
	// both SI states).
	for _, si := range []bool{false, true} {
		cases = append(cases, tablerefCase{name: "update_binary_literal", si: si, sql: "ALTER TABLE db1.o UPDATE s = '\\xFF' WHERE 1",
			wantCode: pb.RewriteCode_Success, wantSQL: "ALTER TABLE phys.`db1.o` UPDATE s = '\\xFF' WHERE 1"})
	}
	// F3: token text is decoded once. `\\x69n` is the unknown function \x69n on
	// ClickHouse, not in, so the verbatim ALTER is not refused by the IN rule.
	cases = append(cases, tablerefCase{name: "double_backslash_x69n_alter", sql: "ALTER TABLE db1.o UPDATE b = 1 WHERE `\\\\x69n`(a, `db2.x`)",
		wantCode: pb.RewriteCode_Success, wantSQL: "ALTER TABLE phys.`db1.o` UPDATE b = 1 WHERE `\\\\x69n`(a, `db2.x`)"})
	// F3: a RENAME source `\\x74` is the table \x74, rewritten as that table.
	cases = append(cases, tablerefCase{name: "double_backslash_rename", sql: "RENAME TABLE db1.`\\\\x74` TO db1.p",
		wantCode: pb.RewriteCode_Success, wantSQL: "RENAME TABLE phys.`db1.\\\\x74` TO phys.`db1.p`"})
	runTablerefCases(t, cases)
}
