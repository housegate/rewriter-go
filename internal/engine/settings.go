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
// table the rewriter never sees), and the dialect switches, which make
// ClickHouse parse later SQL with a grammar the rewriter does not model (the
// polyglot dialect transpiles `IN [db2.x]` into a table operand).
var sqlBearingSettings = map[string]bool{
	"additional_table_filters":            true,
	"additional_result_filter":            true,
	"parallel_replicas_custom_key":        true,
	"dialect":                             true,
	"polyglot_dialect":                    true,
	"allow_experimental_polyglot_dialect": true,
	"allow_experimental_prql_dialect":     true,
	"allow_experimental_kusto_dialect":    true,
}

// SQLBearingSetting reports whether name is one of sqlBearingSettings or any
// other setting whose name ends in "_dialect" (case-insensitively, so a
// spelling ClickHouse might accept is never let through). A quoted name
// arrives already decoded.
func SQLBearingSetting(name string) bool {
	lower := strings.ToLower(name)
	return sqlBearingSettings[lower] || strings.HasSuffix(lower, "_dialect")
}

// SettingAssignment is one `name = value` of a SET statement or a query-level
// SETTINGS clause. PlainValue reports a value that is a numeric literal
// (optionally signed), a string literal, or a bare identifier / keyword —
// the only values a setting may carry in dynamic mode (spec 2026-09-26 R5).
type SettingAssignment struct {
	Name       string
	PlainValue bool
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
		out = append(out, SettingAssignment{Name: name, PlainValue: plainSettingValueTokens(value)})
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
// case) that appears anywhere after a SETTINGS keyword in sql, so no AST shape
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
	for _, tok := range toks {
		if !seen {
			seen = opaqueKeyword(tok) && strings.EqualFold(tok.Text, "SETTINGS")
			continue
		}
		if tok.TokenType == "STRING" {
			continue
		}
		if SQLBearingSetting(tok.Text) {
			return tok.Text, true
		}
	}
	return "", false
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

// RawSettingsClauses returns the assignments of every query-level SETTINGS
// clause in an opaque text (a command node or a Raw ALTER action): the
// assignment list after each top-level SETTINGS keyword that is followed by
// `name =`. ok=false when the text cannot be tokenized or a clause that
// starts as an assignment list does not parse; the caller refuses.
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
		if depth != 0 || !opaqueKeyword(toks[i]) || !strings.EqualFold(toks[i].Text, "SETTINGS") {
			continue
		}
		if i+2 >= len(toks) || !settingNameToken(toks[i+1]) || toks[i+2].TokenType != "EQ" {
			continue // not an assignment list (e.g. SHOW SETTINGS LIKE …)
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
