package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// StripCreateTableEmpty recognises `CREATE TABLE … EMPTY AS SELECT …`, whose
// body the pinned polyglot drops on parse (create_table.as_select is null
// and the generator emits the statement without it). It returns sql with the
// EMPTY keyword removed, so the ordinary pipeline sees an ordinary CREATE
// TABLE … AS SELECT and rewrites and reports its body (spec 2026-09-26 §5,
// the INSERT … SELECT / CREATE TABLE … AS SELECT row: "incl. EMPTY"), and
// ok=true. InsertCreateTableEmpty puts the keyword back into the rewritten
// SQL. ok=false, with sql unchanged, for every other statement. err is a
// tokenizer failure on a body-less create_table, which the caller refuses.
func StripCreateTableEmpty(e Engine, sql string) (stripped string, ok bool, err error) {
	if !strings.Contains(strings.ToUpper(sql), "EMPTY") {
		return sql, false, nil // the keyword cannot be there; skip the extra parse
	}
	ast, perr := e.ParseOne(sql)
	if perr != nil {
		return sql, false, nil // the caller reports the SyntaxError itself
	}
	kind, body, _, berr := bodyOf(ast)
	if berr != nil || kind != NodeCreateTable || body == nil {
		return sql, false, nil
	}
	if q, _ := body["as_select"].(map[string]any); q != nil {
		return sql, false, nil
	}
	toks, terr := tokenizeRaw(e, sql)
	if terr != nil {
		return sql, false, terr
	}
	name, _ := json.Marshal(body["name"])
	depth := 0
	for i := 0; i+1 < len(toks); i++ {
		switch toks[i].TokenType {
		case "L_PAREN":
			depth++
			continue
		case "R_PAREN":
			depth--
			continue
		}
		if depth != 0 || toks[i].TokenType != "VAR" || !strings.EqualFold(toks[i].Text, "EMPTY") ||
			toks[i+1].TokenType != "AS" {
			continue
		}
		candidate := sql[:toks[i].Span.Start] + sql[toks[i].Span.End:]
		cast, cerr := e.ParseOne(candidate)
		if cerr != nil {
			continue
		}
		ckind, cbody, _, cberr := bodyOf(cast)
		if cberr != nil || ckind != NodeCreateTable || cbody == nil {
			continue
		}
		if q, _ := cbody["as_select"].(map[string]any); q == nil {
			continue
		}
		// EMPTY must be the modifier, not the table's own name: the name
		// (spans included, since it precedes the keyword) must not move.
		if cname, _ := json.Marshal(cbody["name"]); !bytes.Equal(name, cname) {
			continue
		}
		return candidate, true, nil
	}
	return sql, false, nil
}

// createTableEmptySentinel marks the body position when InsertCreateTableEmpty
// regenerates a statement's head.
const createTableEmptySentinel = "'hg_create_table_empty_sentinel'"

// InsertCreateTableEmpty re-inserts EMPTY in front of the AS that opens the
// body of a generated CREATE TABLE … AS SELECT. The generated statement is
// reparsed and regenerated with a sentinel body; the text before the " AS "
// that opens the sentinel must be an exact prefix of the generated SQL and be
// followed by " AS ", or the output is refused rather than guessed at. The
// generator may place trailing properties such as COMMENT after the body;
// ClickHouse 26.3 accepts `… EMPTY AS (SELECT …) COMMENT 'c'`.
func InsertCreateTableEmpty(e Engine, generated string) (string, error) {
	ast, err := e.ParseOne(generated)
	if err != nil {
		return "", fmt.Errorf("engine: reparse CREATE TABLE … EMPTY output: %w", err)
	}
	kind, body, root, err := bodyOf(ast)
	if err != nil || kind != NodeCreateTable || body == nil {
		return "", fmt.Errorf("engine: CREATE TABLE … EMPTY output is not a create_table")
	}
	if q, _ := body["as_select"].(map[string]any); q == nil {
		return "", fmt.Errorf("engine: CREATE TABLE … EMPTY output lost its body")
	}
	sentinel, err := e.ParseOne("SELECT " + createTableEmptySentinel)
	if err != nil {
		return "", fmt.Errorf("engine: parse CREATE TABLE … EMPTY sentinel: %w", err)
	}
	var node map[string]any
	if err := json.Unmarshal(sentinel, &node); err != nil {
		return "", fmt.Errorf("engine: decode CREATE TABLE … EMPTY sentinel: %w", err)
	}
	body["as_select"] = node
	probe, err := json.Marshal(root)
	if err != nil {
		return "", fmt.Errorf("engine: encode CREATE TABLE … EMPTY probe: %w", err)
	}
	shape, err := e.Generate(AST(probe))
	if err != nil {
		return "", fmt.Errorf("engine: generate CREATE TABLE … EMPTY probe: %w", err)
	}
	mark := strings.Index(shape, createTableEmptySentinel)
	at := -1
	if mark >= 0 {
		at = strings.LastIndex(shape[:mark], " AS ")
	}
	if at < 0 || !strings.HasPrefix(generated, shape[:at]) || !strings.HasPrefix(generated[at:], " AS ") {
		return "", fmt.Errorf("engine: cannot locate the body of %q", generated)
	}
	return generated[:at] + " EMPTY" + generated[at:], nil
}
