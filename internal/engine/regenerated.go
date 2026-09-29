package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrNotRegeneratedFaithfully marks a statement whose regenerated SQL does
// not spell the input. The pinned Polyglot consumes every token of such a
// statement, so the whole-statement parse gate passes it, but it parses some
// clauses into nothing (DELETE … IN PARTITION, DROP … ON CLUSTER, DROP
// TEMPORARY, WITH TIES before FORMAT / SETTINGS or inside a subquery, a
// column's EPHEMERAL) and prints others differently (LIMIT n BY … LIMIT m as
// LIMIT m BY …, a :: cast wrapped in Nullable, CHAR_LENGTH as LENGTH). The SQL
// generated from that AST means something other than the input.
var ErrNotRegeneratedFaithfully = errors.New("engine: generate: the regenerated statement differs from the input")

// CheckRegenerated returns nil when Generate(ast), which e.ParseOne produced
// for sql, spells the same statement as sql, and an error wrapping
// ErrNotRegeneratedFaithfully otherwise. Tokenizer and generator errors are
// returned as is.
//
// Both texts are tokenized by the engine and reduced to a multiset of
// spellings (fidelitySpellings): commas, parentheses, dots and semicolons are
// dropped, words are upper-cased, strings and numbers stay distinct, the
// ClickHouse aliases in spellingClass are folded, and the measured cosmetic
// respellings of the input are applied. The multisets must be equal, apart
// from the neutral additions spellingDiff allows. Every rewrite a caller
// applies afterwards changes only names, so a faithful identity regeneration
// is what makes the rewritten SQL faithful.
//
// A command or raw node carries its text and is regenerated verbatim, so it
// always passes; the handlers that re-render a command from parsed fields
// check their own coverage. An INSERT statement is compared only up to the
// name after its last FORMAT keyword: what follows is data that
// GenerateInsert splices back verbatim (the parse gate's payload rule).
func CheckRegenerated(e Engine, sql string, ast AST) error {
	kind, _ := NodeKind(ast)
	if kind == NodeCommand || kind == NodeRaw {
		return nil
	}
	gen, err := e.Generate(ast)
	if err != nil {
		return err
	}
	in, err := tokenizeRaw(e, sql)
	if err != nil {
		return err
	}
	out, err := tokenizeRaw(e, gen)
	if err != nil {
		return err
	}
	if kind == NodeInsert {
		in, out = throughFormatName(in), throughFormatName(out)
	}
	lost, added := spellingDiff(fidelitySpellings(in, true), fidelitySpellings(out, false))
	if len(lost) == 0 && len(added) == 0 {
		return nil
	}
	return fmt.Errorf("%w: lost %s, added %s", ErrNotRegeneratedFaithfully, spellingList(lost), spellingList(added))
}

// throughFormatName keeps the tokens up to and including the name after the
// last FORMAT keyword.
func throughFormatName(toks []rawToken) []rawToken {
	for i := len(toks) - 1; i >= 0; i-- {
		if toks[i].TokenType == "FORMAT" && i+1 < len(toks) {
			return toks[:i+2]
		}
	}
	return toks
}

// spellingClass folds a ClickHouse alias the generator respells into the name
// it prints. Each pair resolves to the same data type or function in
// ClickHouse's system.data_type_families / system.functions alias_to
// (measured on 26.2 and 26.7.5.10). A respelling that is not an alias stays a
// difference: CHAR_LENGTH → LENGTH (characters → bytes), instr → POSITION
// (case-insensitive → case-sensitive), toStartOfDay → dateTrunc('DAY', …)
// (DateTime64 / Date32 results differ), startsWith → STARTS_WITH and
// toTypeName → TYPEOF (no such functions).
var spellingClass = map[string]string{
	"BOOLEAN":    "BOOL",
	"INT":        "INT32",
	"INTEGER":    "INT32",
	"TINYINT":    "INT8",
	"SMALLINT":   "INT16",
	"BIGINT":     "INT64",
	"FLOAT":      "FLOAT32",
	"REAL":       "FLOAT32",
	"DOUBLE":     "FLOAT64",
	"TEXT":       "STRING",
	"CHAR":       "STRING",
	"VARCHAR":    "STRING",
	"TIMESTAMP":  "DATETIME",
	"NUMERIC":    "DECIMAL",
	"POW":        "POWER",
	"LN":         "LOG",
	"CEILING":    "CEIL",
	"SUBSTR":     "SUBSTRING",
	"LCASE":      "LOWER",
	"UCASE":      "UPPER",
	"DATE_TRUNC": "DATETRUNC",
}

// fidelitySpellings reduces a token stream to the spellings the check
// compares. With input set it also applies the measured cosmetic respellings
// of the input, each one ClickHouse's own formatter or execution shows to be
// the same statement: NULLS LAST, ClickHouse's default in both sort
// directions, is dropped; SELECT ALL is SELECT; TOP n is LIMIT n; LIMIT n, m
// is LIMIT m OFFSET n; x DIV y is intDiv(x, y); x MOD y is x % y; x REGEXP y
// is match(x, y); a <=> b is a IS NOT DISTINCT FROM b; a ? b : c is
// if(a, b, c); POSITION(x IN y) is POSITION(y, x).
func fidelitySpellings(toks []rawToken, input bool) []string {
	var out []string
	last := func() string {
		if len(out) == 0 {
			return ""
		}
		return out[len(out)-1]
	}
	depth, positionAt, ternary := 0, -1, 0
	for i := 0; i < len(toks); i++ {
		tk := toks[i]
		switch tk.TokenType {
		case "L_PAREN":
			depth++
		case "R_PAREN":
			depth--
			if depth < positionAt {
				positionAt = -1
			}
		}
		k := spellingOf(tk, last())
		switch {
		case k == "" || k == "W:TABLE":
			continue // punctuation; TRUNCATE t / INSERT INTO TABLE t: TABLE is optional
		case last() == "W:STRING" && tk.TokenType == "NUMBER" && i > 0 && toks[i-1].TokenType == "L_PAREN":
			continue // VARCHAR(n) / CHAR(n): ClickHouse ignores the length
		case tk.TokenType == "STRING" && len(out) >= 2 && out[len(out)-2] == "W:DEFINER" && last() == "O:EQ":
			k = "W:" + strings.ToUpper(tk.Text) // DEFINER = 'u' names the same user as DEFINER = u
		}
		if input {
			callNext := i+1 < len(toks) && toks[i+1].TokenType == "L_PAREN"
			switch {
			case k == "W:NULLS" && i+1 < len(toks) && spellingOf(toks[i+1], k) == "W:LAST":
				i++
				continue
			case k == "W:ALL" && last() == "W:SELECT":
				continue
			case k == "W:TOP":
				k = "W:LIMIT"
			case k == "W:DIV":
				k = "W:INTDIV"
			case k == "W:MOD" && !callNext:
				k = "O:PERCENT"
			case k == "W:REGEXP" && !callNext:
				k = "W:MATCH"
			case k == "O:NULLSAFE_EQ":
				out = append(out, "W:IS", "W:NOT", "W:DISTINCT")
				k = "W:FROM"
			case tk.TokenType == "PARAMETER" && tk.Text == "?":
				k = "W:IF"
				ternary++
			case k == "O:COLON" && ternary > 0:
				ternary--
				continue
			case k == "W:POSITION" && callNext:
				positionAt = depth + 1
			case k == "W:IN" && positionAt == depth:
				positionAt = -1
				continue
			case k == "W:LIMIT" && i+3 < len(toks) && toks[i+2].TokenType == "COMMA":
				out = append(out, k, spellingOf(toks[i+1], k), spellingOf(toks[i+3], ""), "W:OFFSET")
				i += 3
				continue
			}
		}
		out = append(out, k)
	}
	return out
}

// spellingOf is one token's spelling: "" for punctuation, N:/S: for a number
// or a string (a heredoc's body; INTERVAL '1' DAY's '1' counts as the number
// the input wrote), W: for a word or identifier, O: for an operator.
func spellingOf(tk rawToken, prev string) string {
	switch tk.TokenType {
	case "COMMA", "L_PAREN", "R_PAREN", "DOT", "SEMICOLON":
		return ""
	case "NUMBER":
		return "N:" + tk.Text
	case "STRING", "NATIONAL_STRING", "TRIPLE_DOUBLE_QUOTED_STRING", "TRIPLE_SINGLE_QUOTED_STRING":
		if prev == "W:INTERVAL" && isDigits(tk.Text) {
			return "N:" + tk.Text
		}
		return "S:" + tk.Text
	case "DOLLAR_STRING":
		// The tokenizer reports a tagged heredoc as tag\x00body.
		if i := strings.IndexByte(tk.Text, 0); i >= 0 {
			return "S:" + tk.Text[i+1:]
		}
		return "S:" + tk.Text
	}
	if tk.Text == "" {
		return ""
	}
	if tk.TokenType == "VAR" || tk.TokenType == "QUOTED_IDENTIFIER" || isWordStart(tk.Text) {
		w := strings.ToUpper(tk.Text)
		if c, ok := spellingClass[w]; ok {
			w = c
		}
		return "W:" + w
	}
	return "O:" + tk.TokenType
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func isWordStart(s string) bool {
	c := s[0]
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// spellingDiff returns the spellings of in missing from out and those of out
// missing from in, both sorted. The generator may add an alias's AS and the
// = of ENGINE = / SETTINGS k = v, and prints a comma join as CROSS JOIN, so
// those additions are neutral.
func spellingDiff(in, out []string) (lost, added []string) {
	count := map[string]int{}
	for _, s := range in {
		count[s]++
	}
	for _, s := range out {
		count[s]--
	}
	if c, j := count["W:CROSS"], count["W:JOIN"]; c < 0 && j < 0 {
		n := min(-c, -j)
		count["W:CROSS"] += n
		count["W:JOIN"] += n
	}
	for s, n := range count {
		if n < 0 && (s == "W:AS" || s == "O:EQ") {
			continue
		}
		for ; n > 0; n-- {
			lost = append(lost, s)
		}
		for ; n < 0; n++ {
			added = append(added, s)
		}
	}
	sort.Strings(lost)
	sort.Strings(added)
	return lost, added
}

// spellingList renders spellings for an error message, without their class
// prefix.
func spellingList(s []string) string {
	if len(s) == 0 {
		return "nothing"
	}
	r := make([]string, len(s))
	for i, v := range s {
		r[i] = v[2:]
	}
	return "[" + strings.Join(r, " ") + "]"
}
