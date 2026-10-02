package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// View column comments. The pinned Polyglot parses a CREATE [MATERIALIZED]
// VIEW's typed column list into create_view.schema, but its view-column
// parser drops a column's COMMENT '<text>' (the column_def's comment stays
// null), and its view-column generator prints only each column's name and
// type, so `(`id` String COMMENT 'x')` regenerates as `("id" String)` and the
// mid-statement drop gate refuses the statement. The Sentio driver gives every
// view column a comment (chx buildCreateViewSQL), so every entity view was
// refused.
//
// restoreViewColumnComments records each column's comment in its column_def
// "comment" field (a field of Polyglot's ColumnDef, so it survives every AST
// round trip through the engine) right after parsing, and
// appendViewColumnComments prints it back after the column's type in every
// SQL generated from that AST. A comment is a string literal: it names
// nothing, so no policy check reads it. Only the comment is restored: a
// column's DEFAULT / MATERIALIZED / ALIAS / EPHEMERAL / CODEC / TTL / NULL,
// which the generator also drops, stays dropped and the drop gate keeps
// refusing such a statement. With nothing else dropped, a column with a
// comment regenerates as `name type COMMENT '<text>'`, the order ClickHouse
// requires, and the drop gate compares exactly that text.

// restoreViewColumnComments sets the comment of every create_view.schema
// column_def whose source item, located by viewColumnListItemSpans and paired
// one to one with the parsed items, has exactly one COMMENT keyword at its own
// nesting level followed by a '…' string literal ClickHouse decodes. Any other
// item, a list that cannot be paired, or a comment Polyglot did record is left
// alone, so the drop gate decides as before.
func restoreViewColumnComments(e Engine, sql string, ast AST) (AST, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(ast), []byte(`{"create_view"`)) || !strings.Contains(strings.ToUpper(sql), "COMMENT") {
		return ast, nil
	}
	dec := json.NewDecoder(bytes.NewReader(ast))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("engine: parse: decode AST for view column comments: %w", err)
	}
	body, _ := root[NodeCreateView].(map[string]any)
	schema, _ := body["schema"].(map[string]any)
	items, _ := schema["expressions"].([]any)
	if len(items) == 0 {
		return ast, nil
	}
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return ast, nil // the parse gate reports a tokenizer failure itself
	}
	spans, ok := viewColumnListItemSpans(toks)
	if !ok || len(spans) != len(items) {
		return ast, nil
	}
	changed := false
	for i, item := range items {
		m, _ := item.(map[string]any)
		cd, _ := m["column_def"].(map[string]any)
		if cd == nil || cd["comment"] != nil || hasCommentConstraint(cd) {
			continue
		}
		text, ok := itemComment(toks, spans[i])
		if !ok {
			continue
		}
		cd["comment"] = text
		changed = true
	}
	if !changed {
		return ast, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("engine: parse: encode AST after view column comments: %w", err)
	}
	return AST(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

func hasCommentConstraint(cd map[string]any) bool {
	cs, _ := cd["constraints"].([]any)
	for _, c := range cs {
		if m, ok := c.(map[string]any); ok {
			if _, has := m["Comment"]; has {
				return true
			}
		}
	}
	return false
}

// itemComment returns the value of the one COMMENT '<text>' clause at the
// item's own nesting level, decoded the way ClickHouse reads the literal.
func itemComment(toks []rawToken, span [2]int) (string, bool) {
	depth, found := 0, -1
	for i, tk := range toks {
		if tk.Span.Start <= span[0] || tk.Span.End > span[1] {
			continue // outside the item, or the column's own name
		}
		switch tk.TokenType {
		case "L_PAREN", "L_BRACKET", "L_BRACE":
			depth++
			continue
		case "R_PAREN", "R_BRACKET", "R_BRACE":
			depth--
			continue
		}
		if depth != 0 || isQuotedLexeme(tk.TokenType) || !strings.EqualFold(tk.Source, "COMMENT") {
			continue
		}
		if found >= 0 || i+1 >= len(toks) {
			return "", false // a second COMMENT word: not a single clause
		}
		found = i + 1
	}
	if found < 0 {
		return "", false
	}
	lit := toks[found]
	if lit.TokenType != "STRING" || !strings.HasPrefix(lit.Source, "'") || lit.Span.End > span[1] {
		return "", false
	}
	v, ok := clickhouseUnquote(lit.Source)
	return v, ok
}

// viewColumnComments returns, by item index, the comments
// restoreViewColumnComments recorded on a create_view AST.
func viewColumnComments(ast AST) (map[int]string, int) {
	if !bytes.HasPrefix(bytes.TrimSpace(ast), []byte(`{"create_view"`)) || !bytes.Contains(ast, []byte(`"comment":"`)) {
		return nil, 0
	}
	var root map[string]any
	if json.Unmarshal(ast, &root) != nil {
		return nil, 0
	}
	body, _ := root[NodeCreateView].(map[string]any)
	schema, _ := body["schema"].(map[string]any)
	items, _ := schema["expressions"].([]any)
	out := map[int]string{}
	for i, item := range items {
		m, _ := item.(map[string]any)
		cd, _ := m["column_def"].(map[string]any)
		if c, ok := cd["comment"].(string); ok {
			out[i] = c
		}
	}
	return out, len(items)
}

// appendViewColumnComments prints the recorded column comments into gen, the
// SQL Polyglot generated for a create_view AST, after each column's printed
// name and type. A generated column list that cannot be located, or whose
// item count differs from the AST's, is an error: the statement would
// otherwise lose its comments.
func appendViewColumnComments(e Engine, ast AST, gen string) (string, error) {
	comments, n := viewColumnComments(ast)
	if len(comments) == 0 {
		return gen, nil
	}
	toks, err := tokenizeRaw(e, gen)
	if err != nil {
		return "", fmt.Errorf("engine: generate: view column comments: %w", err)
	}
	spans, ok := viewColumnListItemSpans(toks)
	if !ok || len(spans) != n {
		return "", fmt.Errorf("engine: generate: the generated view column list does not match its parsed items")
	}
	at := make([]int, 0, len(comments))
	for i := range comments {
		at = append(at, i)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(at)))
	for _, i := range at {
		end := spans[i][1]
		gen = gen[:end] + " COMMENT " + clickhouseQuote(comments[i]) + gen[end:]
	}
	return gen, nil
}

// clickhouseQuote spells s as a single-quoted ClickHouse string literal that
// ClickHouse reads back as s: a backslash and a quote are escaped.
func clickhouseQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
