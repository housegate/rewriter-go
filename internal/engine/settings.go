package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// sqlBearingSettings are the ClickHouse settings dynamic mode refuses
// wherever they appear (spec 2026-09-26 R5): those whose value is SQL
// evaluated against tables (a filter expression, or a map of per-table filter
// expressions ClickHouse parses and executes with the query — it can read any
// table the rewriter never sees), the dialect switches, which make
// ClickHouse parse later SQL with a grammar the rewriter does not model (the
// polyglot dialect transpiles `IN [db2.x]` into a table operand), and the
// name-resolution settings that change what a name the rewriter trusts binds
// to (review round 6, N9; measured on ClickHouse 26.2 and 25.8 under both
// analyzers by flipping every Bool setting and eleven `compatibility`
// versions over the binding shapes):
//
//   - enable_global_with_statement = 0 stops a WITH alias or CTE name from
//     reaching nested queries and later set arms, so ClickHouse reads the
//     table of that name there (the only Bool flip that did);
//   - compatibility restores older defaults as a group; the versions 20.1,
//     20.8 and 21.1 restore enable_global_with_statement = 0 and read the
//     same tables, and any version can change defaults nobody measured;
//   - implicit_table_at_top_level names the table a FROM-less SELECT reads
//     (`SELECT a SETTINGS implicit_table_at_top_level = 'z'` reads phys.z).
//
// enable_analyzer / allow_experimental_analyzer stay accepted: every binding
// rule was measured under both analyzers, and the sweep found no read the
// rules do not already model.
var sqlBearingSettings = map[string]bool{
	"additional_table_filters":            true,
	"additional_result_filter":            true,
	"parallel_replicas_custom_key":        true,
	"dialect":                             true,
	"polyglot_dialect":                    true,
	"allow_experimental_polyglot_dialect": true,
	"allow_experimental_prql_dialect":     true,
	"allow_experimental_kusto_dialect":    true,
	"enable_global_with_statement":        true,
	"compatibility":                       true,
	"implicit_table_at_top_level":         true,
	// promql_table / promql_database name the TimeSeries table the promql
	// dialect reads. They act only under dialect = 'promql', which is refused
	// above; they are refused too as defence in depth (review round 7, N12).
	"promql_table":    true,
	"promql_database": true,
}

// SQLBearingSetting reports whether name is one of sqlBearingSettings or any
// other setting whose name ends in "_dialect" (case-insensitively, so a
// spelling ClickHouse might accept is never let through). name is a
// Polyglot-decoded setting name; the ClickHouse identifier escapes are finished
// first (decodeIdentifierEscapes) so a quoted `\Ndialect` — which ClickHouse
// resolves to `dialect` and applies — is caught, not bypassed. A name that
// cannot be decoded fails closed. The lower-case comparison is a deliberate
// over-match: ClickHouse setting names are case-sensitive, so a wrong-case
// spelling is simply an unknown setting.
func SQLBearingSetting(name string) bool {
	decoded, ok := decodeIdentifierEscapes(name)
	if !ok {
		return true
	}
	lower := strings.ToLower(decoded)
	return sqlBearingSettings[lower] || strings.HasSuffix(lower, "_dialect")
}

// SettingAssignment is one `name = value` of a SET statement or a query-level
// SETTINGS clause. PlainValue reports a value that is a numeric literal
// (optionally signed), a string literal, or a bare identifier / keyword —
// the only values a setting may carry in dynamic mode (spec 2026-09-26 R5).
type SettingAssignment struct {
	Name       string
	PlainValue bool
	// EscapedName reports a name whose source spelling is not its plain text
	// (settingNameVerbatim): ClickHouse would decode it, and the rewriter
	// forwards the statement text verbatim, so it cannot tell which setting
	// the name is (review round 7, N11: `\N` decodes to nothing, so
	// `\Nenable_global_with_statement` is the refused setting).
	EscapedName bool
}

// SessionSettingAssignments parses a `command` node's text as a session SET
// statement. isSet reports a settings assignment (SET <name> = …; SET ROLE /
// SET DEFAULT ROLE are access management, not settings, and report false).
// wellFormed=false means the text is a SET statement whose assignment list
// does not parse as `name = value [, name = value …]`; the caller refuses it.
func SessionSettingAssignments(e Engine, text string) (assignments []SettingAssignment, isSet, wellFormed bool) {
	toks, err := tokenizeRaw(e, text)
	if err != nil || len(toks) < 3 || !strings.EqualFold(toks[0].Text, "SET") {
		return nil, false, false
	}
	if !isNameTok(toks[1].TokenType) || strings.EqualFold(toks[1].Text, "ROLE") || strings.EqualFold(toks[1].Text, "DEFAULT") {
		return nil, false, false
	}
	if toks[2].TokenType != "EQ" {
		return nil, false, false
	}
	assignments, end, ok := parseSettingAssignments(toks, 1)
	if !ok || end != len(toks) {
		return assignments, true, false
	}
	return assignments, true, true
}

// parseSettingAssignments parses `name = value [, name = value …]` starting at
// toks[i] and returns the index just past the list. A trailing semicolon ends
// the list. ok=false when an assignment is malformed.
func parseSettingAssignments(toks []rawToken, i int) ([]SettingAssignment, int, bool) {
	var out []SettingAssignment
	for {
		if i+2 > len(toks) || !settingNameToken(toks[i]) || toks[i+1].TokenType != "EQ" {
			return out, i, false
		}
		name := toks[i].Text
		j := i + 2
		depth := 0
		for j < len(toks) {
			switch toks[j].TokenType {
			case "L_PAREN", "L_BRACKET", "L_BRACE":
				depth++
			case "R_PAREN", "R_BRACKET", "R_BRACE":
				depth--
			}
			if depth == 0 && (toks[j].TokenType == "COMMA" || toks[j].TokenType == "SEMICOLON" || settingsListEnd(toks[j])) {
				break
			}
			j++
		}
		value := toks[i+2 : j]
		if len(value) == 0 {
			return out, j, false
		}
		out = append(out, SettingAssignment{Name: name, PlainValue: plainSettingValueTokens(value),
			EscapedName: !settingNameVerbatim(toks[i])})
		if j < len(toks) && settingsListEnd(toks[j]) {
			return out, j, true // an INSERT's query follows its SETTINGS list
		}
		if j >= len(toks) || toks[j].TokenType == "SEMICOLON" {
			if j < len(toks) {
				j++
			}
			return out, j, true
		}
		i = j + 1 // past the comma
	}
}

// settingsListEnd reports a keyword that ends a SETTINGS assignment list: the
// query or data clause of an INSERT (`INSERT INTO t (a) SETTINGS x = 1 SELECT
// …`), which follows the list without a separator.
func settingsListEnd(tok rawToken) bool {
	if !opaqueKeyword(tok) || tok.TokenType == "VAR" {
		return false
	}
	switch strings.ToUpper(tok.Text) {
	case "SELECT", "WITH", "VALUES", "FORMAT":
		return true
	}
	return false
}

// SettingsBackstop is a token-level backstop for R5 (spec 2026-09-26): it
// reports the first SQL-bearing or dialect setting name (quoted or not, any
// case) assigned (`name =`) anywhere after a SETTINGS keyword in sql, so no AST shape
// can hide one from the structured check. A tokenizer failure reports
// hit=false: the structured checks and the fail-closed tokenizer paths
// elsewhere still apply.
func SettingsBackstop(e Engine, sql string) (name string, hit bool) {
	// A SETTINGS keyword is plain ASCII in the source text; skip the tokenizer
	// when it cannot be there.
	if !strings.Contains(strings.ToUpper(sql), "SETTINGS") {
		return "", false
	}
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return "", false
	}
	seen := false
	for i, tok := range toks {
		if !seen {
			seen = opaqueKeyword(tok) && strings.EqualFold(tok.Text, "SETTINGS")
			continue
		}
		// An assignment: the denylisted name must be followed by "=", so a
		// column alias named settings / dialect is not a setting.
		if tok.TokenType != "STRING" && SQLBearingSetting(tok.Text) && i+1 < len(toks) && toks[i+1].TokenType == "EQ" {
			return tok.Text, true
		}
	}
	return "", false
}

// settingNameVerbatim reports whether a setting-name token's source text is
// exactly its name: a bare word, or a name in one pair of backticks or double
// quotes with no escape sequence (no backslash) and no doubled quote. Any
// other spelling needs decoding, and ClickHouse's decoding differs from
// Polyglot's (`\N` is decoded to nothing by ClickHouse and kept by Polyglot),
// so a path that forwards the text verbatim refuses it.
func settingNameVerbatim(tok rawToken) bool {
	src := tok.Source
	if src == "" || strings.Contains(src, "\\") {
		return false
	}
	if src == tok.Text {
		return true
	}
	if len(src) < 2 {
		return false
	}
	quote := src[0]
	if (quote != '`' && quote != '"') || src[len(src)-1] != quote {
		return false
	}
	inner := src[1 : len(src)-1]
	return inner == tok.Text && !strings.ContainsRune(inner, rune(quote))
}

func settingNameToken(tok rawToken) bool {
	return isNameTok(tok.TokenType) || mutationProbeKeywordToken(tok)
}

// plainSettingValueTokens accepts exactly one number (optionally preceded by
// a sign), string, identifier, quoted identifier or keyword token — never a
// parenthesis, brace, bracket or SELECT / WITH.
func plainSettingValueTokens(value []rawToken) bool {
	if len(value) == 2 && (value[0].TokenType == "DASH" || value[0].TokenType == "PLUS") {
		return value[1].TokenType == "NUMBER"
	}
	if len(value) != 1 {
		return false
	}
	tok := value[0]
	switch tok.TokenType {
	case "NUMBER", "STRING", "VAR", "QUOTED_IDENTIFIER":
		return true
	}
	if strings.EqualFold(tok.Text, "SELECT") || strings.EqualFold(tok.Text, "WITH") {
		return false
	}
	return mutationProbeKeywordToken(tok)
}

// RawSettingsClauses returns the assignments of every SETTINGS clause in an
// opaque text (a command node, a Raw ALTER action, the query text after an
// INSERT column list), which the rewriter forwards verbatim. It fails closed
// (ok=false; the caller refuses) whenever it cannot fully parse a clause into
// `name = value` pairs with simple names (review round 8, N14): the text
// cannot be tokenized; a SETTINGS keyword followed by a name is nested in
// parentheses or is not followed by an assignment list; a SETTINGS keyword
// ends the text; or a clause does not parse,
// which includes a compound (dotted) setting name such as `SQL_a.b` — a
// valid custom setting to ClickHouse, but one the scanner cannot tell apart
// from an escaped refused name. Only the non-assignment forms `SHOW
// [CHANGED] SETTINGS …` are skipped.
func RawSettingsClauses(e Engine, text string) (assignments []SettingAssignment, ok bool) {
	toks, err := tokenizeRaw(e, text)
	if err != nil {
		return nil, false
	}
	depth := 0
	for i := 0; i < len(toks); i++ {
		switch toks[i].TokenType {
		case "L_PAREN":
			depth++
			continue
		case "R_PAREN":
			depth--
			continue
		}
		if !opaqueKeyword(toks[i]) || !strings.EqualFold(toks[i].Text, "SETTINGS") {
			continue
		}
		if showSettingsKeyword(toks, i) && !settingsListFollows(toks, i) {
			continue // SHOW [CHANGED] SETTINGS [LIKE | ILIKE …]: not an assignment list
		}
		if i+1 < len(toks) && !settingNameToken(toks[i+1]) {
			// A SETTINGS clause starts with a setting name; followed by
			// anything else (`UPDATE settings = 1`) the word is a column,
			// not a clause ClickHouse could apply.
			continue
		}
		if depth != 0 || i+2 >= len(toks) || toks[i+2].TokenType != "EQ" {
			return assignments, false
		}
		parsed, end, pok := parseSettingAssignments(toks, i+1)
		assignments = append(assignments, parsed...)
		if !pok {
			return assignments, false
		}
		i = end - 1
	}
	return assignments, true
}

// showSettingsKeyword reports whether toks[i] (a SETTINGS keyword) is the
// object of a SHOW SETTINGS / SHOW CHANGED SETTINGS statement: SHOW must be
// the statement's first token (comments are not tokens), optionally followed
// by CHANGED, and SETTINGS the next one. A column or alias named show in
// front of a real SETTINGS clause is not exempt (review round 9, N16).
func showSettingsKeyword(toks []rawToken, i int) bool {
	first := func(j int, word string) bool {
		return opaqueKeyword(toks[j]) && strings.EqualFold(toks[j].Text, word)
	}
	switch i {
	case 1:
		return first(0, "SHOW")
	case 2:
		return first(0, "SHOW") && first(1, "CHANGED")
	}
	return false
}

// settingsListFollows reports whether the SETTINGS keyword toks[i] is
// followed by what could start an assignment list: a setting-name token that
// is not the LIKE / ILIKE of SHOW SETTINGS LIKE. `SHOW SETTINGS max_threads =
// [1]` is then scanned like any other clause (review round 9: main refused
// it, and an exemption must not turn a refusal into Success).
func settingsListFollows(toks []rawToken, i int) bool {
	if i+1 >= len(toks) || !settingNameToken(toks[i+1]) {
		return false
	}
	next := toks[i+1]
	return !(opaqueKeyword(next) && (strings.EqualFold(next.Text, "LIKE") || strings.EqualFold(next.Text, "ILIKE")))
}

// SettingsEscapeBackstop is a token-level backstop for N11 / N14 / N17
// (review rounds 7-9): it reports a quoted name followed by `=` anywhere
// after a SETTINGS keyword, or the SETTING keyword of ALTER … MODIFY SETTING,
// in sql whose source spelling is not its plain text (a backslash escape or a
// doubled quote). ClickHouse decodes such a name
// (`\N` to nothing), so it may be a refused setting whatever position the
// structured checks think it is in. A tokenizer failure reports false: the
// fail-closed paths elsewhere still apply.
func SettingsEscapeBackstop(e Engine, sql string) bool {
	if !strings.Contains(strings.ToUpper(sql), "SETTING") {
		return false
	}
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return false
	}
	seen := false
	for i, tok := range toks {
		if !seen {
			seen = opaqueKeyword(tok) && (strings.EqualFold(tok.Text, "SETTINGS") || strings.EqualFold(tok.Text, "SETTING"))
			continue
		}
		if tok.TokenType == "QUOTED_IDENTIFIER" && i+1 < len(toks) && toks[i+1].TokenType == "EQ" && !settingNameVerbatim(tok) {
			return true
		}
	}
	return false
}

// QuerySettings returns, in document order, every query-level SETTINGS
// assignment in a structured statement: each "settings" list polyglot attaches
// to a SELECT / set operation / INSERT, including one nested in an embedded
// body. A CREATE TABLE / materialized-view storage SETTINGS clause is a
// settings_property, not a query-level clause, and is governed by the T5
// allowlist instead.
func QuerySettings(ast AST) ([]SettingAssignment, error) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, fmt.Errorf("engine: decode query settings: %w", err)
	}
	var out []SettingAssignment
	var walk func(node any)
	walk = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			for _, k := range sortedMapKeys(n) {
				if k == "settings" {
					if list, ok := n[k].([]any); ok {
						for _, item := range list {
							out = append(out, decodeSettingAssignment(item))
						}
						continue
					}
				}
				walk(n[k])
			}
		case []any:
			for _, v := range n {
				walk(v)
			}
		}
	}
	walk(root)
	return out, nil
}

// decodeSettingAssignment decodes one {"eq":{"left":<name>,"right":<value>}}
// settings item. An item of any other shape is reported with an empty name and
// a non-plain value, so the caller refuses it.
func decodeSettingAssignment(item any) SettingAssignment {
	m, _ := item.(map[string]any)
	eq, _ := m["eq"].(map[string]any)
	if eq == nil {
		return SettingAssignment{}
	}
	var name string
	if left, ok := eq["left"].(map[string]any); ok {
		if col, ok := left["column"].(map[string]any); ok && col["table"] == nil {
			name = identName(col["name"])
		} else {
			name = identName(left)
		}
	}
	return SettingAssignment{Name: name, PlainValue: name != "" && plainSettingValueNode(eq["right"])}
}

// plainSettingValueNode is plainSettingValueTokens for a structured value:
// a literal (optionally negated), an unqualified column / identifier, or a
// boolean.
func plainSettingValueNode(node any) bool {
	m, ok := node.(map[string]any)
	if !ok || len(m) != 1 {
		return false
	}
	for kind, body := range m {
		switch kind {
		case "literal", "boolean", "null":
			return true
		case "neg":
			inner, _ := body.(map[string]any)
			lit, _ := inner["this"].(map[string]any)
			l, ok := lit["literal"].(map[string]any)
			return ok && l["literal_type"] == "number"
		case "column":
			col, _ := body.(map[string]any)
			return col["table"] == nil && identName(col["name"]) != ""
		case "identifier":
			return identName(body) != ""
		}
	}
	return false
}
