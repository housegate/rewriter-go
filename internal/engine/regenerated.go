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
// LIMIT m BY …, a :: cast wrapped in Nullable, CHAR_LENGTH as LENGTH, 'x\_%'
// as 'x_%', a <=> b = c as a IS NOT DISTINCT FROM b = c). The SQL generated
// from that AST means something other than the input.
var ErrNotRegeneratedFaithfully = errors.New("engine: generate: the regenerated statement differs from the input")

// CheckRegenerated returns nil when Generate(ast), which e.ParseOne produced
// for sql, spells the same statement as sql, and an error wrapping
// ErrNotRegeneratedFaithfully otherwise. An AST whose kind cannot be read and
// a tokenizer or generator error are returned as is: every non-nil result
// means the statement is not known to be regenerated faithfully.
//
// Both texts are tokenized by the engine and reduced to a multiset of
// spellings (fidelitySpellings): commas, parentheses, dots and semicolons are
// dropped, words are upper-cased, the ClickHouse aliases in spellingClass are
// folded, and the measured cosmetic respellings of the input are applied.
// Literals and quoted identifiers are compared by the value ClickHouse reads
// from their source text (literalSpelling, numberKey), never by the value
// Polyglot decoded. The multisets must be equal, apart from the neutral
// additions spellingDiff allows. Every rewrite a caller applies afterwards
// changes only names, so a faithful identity regeneration is what makes the
// rewritten SQL faithful.
//
// A command or raw node carries its text and is regenerated verbatim, so it
// always passes; the handlers that re-render a command from parsed fields
// check their own coverage. An INSERT statement whose AST carries a FORMAT
// data clause (insertHasFormatClause, the gate GenerateInsert splices on) is
// compared only up to the name after its last FORMAT keyword: what follows is
// data that GenerateInsert splices back verbatim (the parse gate's payload
// rule). A column or alias named format is not such a clause. Any other
// INSERT that reads input() anywhere (readsInput) is checked on its token
// stream, whatever the AST's shape (a plain SELECT, a set operation, a CTE
// before either): when it ends in FORMAT <name> after a SELECT, the text after
// the name must be the client-streaming form, [ \t]*\n? (any more is rows,
// isStreamedDataTail); a FORMAT token anywhere else is refused; without one
// the statement has no data and is compared in full (checkInputData). An
// INSERT … SELECT … FORMAT <name> without input() is compared in full, apart
// from a text of only comments after the name, which ClickHouse ignores
// (withoutIgnoredData).
func CheckRegenerated(e Engine, sql string, ast AST) error {
	kind, err := NodeKind(ast)
	if err != nil {
		return err
	}
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
		switch {
		case insertHasFormatClause(ast):
			in, out = throughFormatName(in), throughFormatName(out)
		case readsInput(in):
			if err := checkInputData(sql, in); err != nil {
				return err
			}
		case insertSelectHasFormat(ast):
			in = withoutIgnoredData(sql, in)
		}
	}
	lost, added := spellingDiff(fidelitySpellings(sql, in, true, operatorsKept(out)), fidelitySpellings(gen, out, false, nil))
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

// insertSelectHasFormat reports whether an INSERT's source is a SELECT whose
// AST carries its own FORMAT clause (INSERT INTO t SELECT … FORMAT JSON).
func insertSelectHasFormat(ast AST) bool {
	_, body, _, err := bodyOf(ast)
	if err != nil || body == nil {
		return false
	}
	q, _ := body["query"].(map[string]any)
	sel, _ := q["select"].(map[string]any)
	return sel["format"] != nil
}

// withoutIgnoredData drops the tokenizer's inline-data token (a zero-width
// token at EOF right after FORMAT <name>, whose text is the rest of the
// statement) from an INSERT … SELECT … FORMAT <name> <text> that reads no
// input() when <text> is only comments. ClickHouse then ignores the text
// (measured on 26.2: formatQuery drops it), and Generate drops it too. Any
// other text is kept and compared, so its loss refuses: a text that is not a
// comment is not known to be ignored. A statement that reads input() never
// gets here (CheckRegenerated checks its tail instead). No other token
// is dropped: in INSERT … SELECT format x FROM p the select has no FORMAT
// clause, so the FROM p the tokenizer took for data is compared, and its
// loss refused.
func withoutIgnoredData(sql string, toks []rawToken) []rawToken {
	n := len(toks)
	if n < 3 || toks[n-1].Span.Start != toks[n-1].Span.End || toks[n-3].TokenType != "FORMAT" {
		return toks
	}
	if !onlyComments(sql[toks[n-2].Span.End:]) {
		return toks
	}
	return toks[:n-1]
}

// checkInputData refuses an INSERT that reads input() when the rows ClickHouse
// reads after its FORMAT <name> are not the client-streaming form. It reads the
// token stream only, so the query's shape (a plain SELECT, UNION / INTERSECT /
// EXCEPT, a CTE before either, SETTINGS before FORMAT) does not matter. When
// the last two tokens, before at most the tokenizer's zero-width inline-data
// token, are FORMAT and a name after a SELECT, the source text after the name
// is the rows, and it must satisfy isStreamedDataTail. A stream with no FORMAT
// token has no data position at all (ClickHouse itself refuses such an input()
// statement, code 477 on 26.2), so every token is compared and nothing is
// checked here. Any other FORMAT token (a column named format, a FORMAT inside
// a subquery) is refused: the check cannot say where the rows would be.
func checkInputData(sql string, toks []rawToken) error {
	n := len(toks)
	if n > 0 && toks[n-1].Span.Start == toks[n-1].Span.End {
		n--
	}
	if n >= 3 && toks[n-2].TokenType == "FORMAT" && toks[n-1].Text != "" && isWordStart(toks[n-1].Text) && hasTokenType(toks[:n-2], "SELECT") {
		if tail := sql[toks[n-1].Span.End:]; !isStreamedDataTail(tail) {
			return fmt.Errorf("%w: lost input() data %q, added nothing", ErrNotRegeneratedFaithfully, tail)
		}
		return nil
	}
	if hasTokenType(toks[:n], "FORMAT") {
		return fmt.Errorf("%w: input() data position unknown: a FORMAT token does not end the statement", ErrNotRegeneratedFaithfully)
	}
	return nil
}

func hasTokenType(toks []rawToken, typ string) bool {
	for _, tk := range toks {
		if tk.TokenType == typ {
			return true
		}
	}
	return false
}

// isStreamedDataTail reports whether the text after the FORMAT name of an
// INSERT that reads input() carries no rows: ClickHouse skips [ \t]*\n? there
// and reads the rest as data, even blank lines and comments (measured on 26.2
// over HTTP: FORMAT CSV, CSV\n and CSV \t\n insert nothing, while CSV\n \n
// and CSV \t\n\t\n insert a row, and FORMAT TSV -- c inserts the row "-- c";
// the same holds after a UNION ALL). An empty text is the real
// client-streaming form, where the rows arrive separately.
func isStreamedDataTail(tail string) bool {
	tail = strings.TrimLeft(tail, " \t")
	return tail == "" || tail == "\n"
}

// onlyComments reports whether s is whitespace, -- line comments and /* */
// comments, none of them nested.
func onlyComments(s string) bool {
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return true
			}
			s = s[i+1:]
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s, "*/")
			if i < 0 || strings.Contains(s[2:i], "/*") {
				return false
			}
			s = s[i+2:]
		default:
			return false
		}
	}
	return true
}

// readsInput reports whether a statement calls the input() table function
// anywhere, in a CTE or a subquery too: input followed by (, in any case.
func readsInput(toks []rawToken) bool {
	for i := 0; i+1 < len(toks); i++ {
		if strings.EqualFold(toks[i].Text, "input") && toks[i+1].TokenType == "L_PAREN" {
			return true
		}
	}
	return false
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

// spelling is one token as the check compares it: key is what is counted,
// show is how an error names it.
type spelling struct {
	key, show string
}

func word(w string) spelling { return spelling{"W:" + w, w} }

// negatedOperators are the operators a NOT directly before them negates
// (x NOT LIKE y, x NOT IN y, …). Such a NOT is spelled together with its
// operator, so a regeneration that moves it (x NOT LIKE y printed as
// NOT x LIKE y) is a difference unless the input respelling below applies.
var negatedOperators = map[string]bool{"W:LIKE": true, "W:ILIKE": true, "W:IN": true, "W:BETWEEN": true, "W:REGEXP": true}

// fidelitySpellings reduces a token stream of src to the spellings the check
// compares. With input set it also applies the measured cosmetic respellings
// of the input, each one ClickHouse's own formatter or execution shows to be
// the same statement: NULLS LAST, ClickHouse's default in both sort
// directions, is dropped; SELECT ALL is SELECT; TOP n is LIMIT n; LIMIT n, m
// is LIMIT m OFFSET n.
//
// The remaining respellings change an operator's precedence class, so the
// regeneration can regroup its operands: Polyglot prints a <=> b = c as
// a IS NOT DISTINCT FROM b = c, NOT a DIV b as intDiv(NOT a, b) and
// a NOT LIKE b = c as NOT a LIKE b = c, each of which ClickHouse reads
// differently. Each is cosmetic only where nothing can regroup it: x DIV y is
// intDiv(x, y), x MOD y is x % y, x REGEXP y is match(x, y), a <=> b is
// a IS NOT DISTINCT FROM b and x NOT LIKE / NOT ILIKE y is NOT x LIKE /
// ILIKE y (measured equal by execution, NULLs included) only when both
// operands are single operands and the application is bordered on both sides
// (isolated); POSITION(x IN y) is POSITION(y, x) only when x and y are single
// operands; a ? b : c is if(a, b, c) only when the whole ternary is bordered
// and made of measured operators (ternaryColon). Otherwise the input keeps
// its own spelling, which the regeneration does not contain, and the
// statement is refused. None of these respellings applies to an operator in
// kept, which the regeneration still spells the input's way (operatorsKept):
// Polyglot keeps the expressions of ALTER … DELETE / UPDATE verbatim.
func fidelitySpellings(src string, toks []rawToken, input bool, kept map[string]bool) []spelling {
	var out []spelling
	last := func() string {
		if len(out) == 0 {
			return ""
		}
		return out[len(out)-1].key
	}
	spell := func(i int, prev string) spelling {
		return spellingOf(src, toks[i], prev, input)
	}
	skip := map[int]bool{} // the IN of POSITION(x IN y) and the : of a ternary
	for i := 0; i < len(toks); i++ {
		if skip[i] {
			continue
		}
		tk := toks[i]
		sp := spell(i, last())
		switch {
		case sp.key == "" || sp.key == "W:TABLE":
			continue // punctuation; TRUNCATE t / INSERT INTO TABLE t: TABLE is optional
		case last() == "W:STRING" && tk.TokenType == "NUMBER" && i > 0 && toks[i-1].TokenType == "L_PAREN":
			continue // VARCHAR(n) / CHAR(n): ClickHouse ignores the length
		case tk.TokenType == "STRING" && strings.HasPrefix(sp.key, "S:") &&
			len(out) >= 2 && out[len(out)-2].key == "W:DEFINER" && last() == "O:EQ":
			sp = word(strings.ToUpper(sp.key[2:])) // DEFINER = 'u' names the same user as DEFINER = u
		case sp.key == "W:NOT" && i+1 < len(toks):
			if next := spell(i+1, sp.key); negatedOperators[next.key] {
				sp = word("NOT " + next.key[2:])
			}
		}
		if input {
			callNext := i+1 < len(toks) && toks[i+1].TokenType == "L_PAREN"
			switch {
			case sp.key == "W:NULLS" && i+1 < len(toks) && spell(i+1, sp.key).key == "W:LAST":
				i++
				continue
			case sp.key == "W:ALL" && last() == "W:SELECT":
				continue
			case sp.key == "W:TOP":
				sp = word("LIMIT")
			case sp.key == "W:DIV" && !kept["DIV"] && isolated(toks, i, i):
				sp = word("INTDIV")
			case sp.key == "W:MOD" && !callNext && !kept["MOD"] && isolated(toks, i, i):
				sp = spelling{"O:PERCENT:%", "PERCENT"}
			case sp.key == "W:REGEXP" && !callNext && !kept["REGEXP"] && isolated(toks, i, i):
				sp = word("MATCH")
			case tk.TokenType == "NULLSAFE_EQ" && !kept["<=>"] && isolated(toks, i, i):
				out = append(out, word("IS"), word("NOT"), word("DISTINCT"))
				sp = word("FROM")
			case (sp.key == "W:NOT LIKE" || sp.key == "W:NOT ILIKE") && !kept[sp.key[2:]] && isolated(toks, i, i+1):
				sp = word("NOT")
			case tk.TokenType == "PARAMETER" && tk.Text == "?" && !kept["?"]:
				if c := ternaryColon(toks, i); c >= 0 {
					sp = word("IF")
					skip[c] = true
				}
			case sp.key == "W:POSITION" && callNext:
				if in := operandAfter(toks, i+2); in > 0 && in < len(toks) && toks[in].TokenType == "IN" {
					if end := operandAfter(toks, in+1); end > 0 && end < len(toks) && toks[end].TokenType == "R_PAREN" {
						skip[in] = true
					}
				}
			case sp.key == "W:LIMIT" && i+3 < len(toks) && toks[i+2].TokenType == "COMMA":
				out = append(out, sp, spell(i+1, sp.key), spell(i+3, ""), word("OFFSET"))
				i += 3
				continue
			}
		}
		out = append(out, sp)
	}
	return out
}

// operatorsKept returns the precedence-changing operators of
// fidelitySpellings that the regeneration toks still spells the input's way:
// <=>, ? (a ternary), DIV, MOD and REGEXP not called as a function, and
// NOT LIKE / NOT ILIKE. Keeping such an operator's input spelling can only
// match a regeneration that prints the same operator, which ClickHouse reads
// at the same precedence.
func operatorsKept(toks []rawToken) map[string]bool {
	kept := map[string]bool{}
	for i, tk := range toks {
		callNext := i+1 < len(toks) && toks[i+1].TokenType == "L_PAREN"
		switch w := strings.ToUpper(tk.Text); {
		case tk.TokenType == "NULLSAFE_EQ", tk.TokenType == "PARAMETER" && tk.Text == "?":
			kept[tk.Text] = true
		case (w == "DIV" || w == "MOD" || w == "REGEXP") && !callNext:
			kept[w] = true
		case tk.TokenType == "NOT" && i+1 < len(toks) && (toks[i+1].TokenType == "LIKE" || toks[i+1].TokenType == "I_LIKE"):
			kept["NOT "+strings.ToUpper(toks[i+1].Text)] = true
		}
	}
	return kept
}

// spellingOf is one token's spelling: none for punctuation, N: for a number
// (by value, numberKey), S: for a string (by the value ClickHouse reads,
// literalSpelling; INTERVAL '1' DAY's '1' counts as the number the input
// wrote), W: for a word or identifier (a quoted identifier by the name
// ClickHouse reads), O: for any other token with its text (= / == and
// <> / != are the same ClickHouse operator, so EQ and NEQ drop it). A literal
// ClickHouse would not read as one lexeme of the same extent is spelled by
// side (U: in the input, V: in the regeneration), so it never matches.
func spellingOf(src string, tk rawToken, prev string, input bool) spelling {
	raw := src[tk.Span.Start:tk.Span.End]
	switch tk.TokenType {
	case "COMMA", "L_PAREN", "R_PAREN", "DOT", "SEMICOLON":
		return spelling{}
	case "NUMBER", "HEX_NUMBER":
		return numberSpelling(src, tk, input)
	case "IDENTIFIER":
		if isNumberLike(raw) {
			return numberSpelling(src, tk, input)
		}
		return spelling{"O:IDENTIFIER:" + raw, raw}
	case "EQ", "NEQ":
		return spelling{"O:" + tk.TokenType, tk.TokenType}
	}
	if isQuotedLexeme(tk.TokenType) {
		return literalSpelling(raw, prev, input)
	}
	if tk.Text == "" {
		return spelling{}
	}
	if tk.TokenType == "VAR" || isWordStart(tk.Text) {
		return word(foldClass(strings.ToUpper(tk.Text)))
	}
	return spelling{"O:" + tk.TokenType + ":" + tk.Text, tk.TokenType}
}

func foldClass(w string) string {
	if c, ok := spellingClass[w]; ok {
		return c
	}
	return w
}

func undecodable(raw string, input bool) spelling {
	if input {
		return spelling{"U:" + raw, raw}
	}
	return spelling{"V:" + raw, raw}
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
// missing from in, both sorted by key, as their show text. The generator may
// add an alias's AS and the = of ENGINE = / SETTINGS k = v, and prints a
// comma join as CROSS JOIN, so those additions are neutral.
func spellingDiff(in, out []spelling) (lost, added []string) {
	count := map[string]int{}
	show := map[string]string{}
	for _, s := range in {
		count[s.key]++
		if _, ok := show[s.key]; !ok {
			show[s.key] = s.show
		}
	}
	for _, s := range out {
		if count[s.key]--; count[s.key] < 0 {
			show[s.key] = s.show
		}
	}
	if c, j := count["W:CROSS"], count["W:JOIN"]; c < 0 && j < 0 {
		n := min(-c, -j)
		count["W:CROSS"] += n
		count["W:JOIN"] += n
	}
	var lostKeys, addedKeys []string
	for s, n := range count {
		if n < 0 && (s == "W:AS" || s == "O:EQ") {
			continue
		}
		for ; n > 0; n-- {
			lostKeys = append(lostKeys, s)
		}
		for ; n < 0; n++ {
			addedKeys = append(addedKeys, s)
		}
	}
	sort.Strings(lostKeys)
	sort.Strings(addedKeys)
	for _, k := range lostKeys {
		lost = append(lost, show[k])
	}
	for _, k := range addedKeys {
		added = append(added, show[k])
	}
	return lost, added
}

// spellingList renders the shown spellings for an error message.
func spellingList(s []string) string {
	if len(s) == 0 {
		return "nothing"
	}
	return "[" + strings.Join(s, " ") + "]"
}
