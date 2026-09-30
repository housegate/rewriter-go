package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// decodeASTIdentifiers makes a freshly parsed AST carry the names and text
// ClickHouse will read from the original SQL.
//
// Quoted identifiers. Polyglot keeps several ClickHouse escapes verbatim in AST
// names (`ph\Nys` for the database ClickHouse reads as phys, `t\N` for table
// t), so without this every downstream check — the protected / known-physical
// / reserved database checks, database_map resolution, the storage-integrity
// Active-table lookup, the reserved-column check and DDL targets — compared a
// name ClickHouse never uses. Every node with a string "name" and a source
// "span" whose spelling is quoted and holds a backslash gets the name decoded
// from that spelling (decodeQuotedIdentifier), whatever its "quoted" flag:
// Polyglot marks an EXCEPT column list `quoted: false` although its source is
// quoted, and the decoded name is then marked quoted so the generator re-quotes
// it. Polyglot's generator escapes a backslash inside a quoted name, so the
// regenerated SQL carries exactly the decoded name. A spelling ClickHouse
// rejects fails the parse; one ClickHouse reads as non-UTF-8 bytes keeps
// Polyglot's name (see decodedNotUTF8). A function node carries no span and
// keeps Polyglot's name; its matchers finish the decode themselves.
//
// Command text. A statement Polyglot leaves as a `command` node is checked by
// tokenizing its "this" text, but that text is Polyglot's re-rendering: it
// re-quotes names in "…" without escaping them, so a name holding `"` or a
// trailing backslash shifts every later token, while the rewriter forwards (or
// splices) the original SQL. A target present in the original could then go
// unchecked (RENAME TABLE db1.`"` TO db1.y, phys.`db2.x` TO db1.`"` renamed
// another tenant's table). "this" is therefore replaced with the original
// statement text, from its first token to its last token before any trailing
// semicolon, so every command-text check and splice reads the same bytes the
// rewriter forwards.
func decodeASTIdentifiers(e Engine, sql string, ast AST) (AST, error) {
	isCommand := bytes.HasPrefix(bytes.TrimSpace(ast), []byte(`{"command"`))
	if !isCommand && !strings.Contains(sql, "\\") {
		return ast, nil
	}
	dec := json.NewDecoder(bytes.NewReader(ast))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("engine: parse: decode AST for identifier decoding: %w", err)
	}
	changed := false
	if isCommand {
		if err := setCommandSourceText(e, sql, root, &changed); err != nil {
			return nil, err
		}
	}
	if strings.Contains(sql, "\\") {
		stream := newTokenStream(sql)
		var walk func(any) error
		walk = func(n any) error {
			switch v := n.(type) {
			case map[string]any:
				if err := decodeIdentifierNode(sql, stream, v, &changed); err != nil {
					return err
				}
				for _, child := range v {
					if err := walk(child); err != nil {
						return err
					}
				}
			case []any:
				for _, child := range v {
					if err := walk(child); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := walk(root); err != nil {
			return nil, err
		}
	}
	if !changed {
		return ast, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("engine: parse: encode AST after identifier decoding: %w", err)
	}
	return AST(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// setCommandSourceText replaces a top-level command node's "this" with the
// original statement text (see decodeASTIdentifiers).
func setCommandSourceText(e Engine, sql string, root any, changed *bool) error {
	top, _ := root.(map[string]any)
	cmd, _ := top[NodeCommand].(map[string]any)
	if _, ok := cmd["this"].(string); !ok {
		return nil
	}
	text, err := commandSourceText(e, sql)
	if err != nil {
		return err
	}
	if cmd["this"] != text {
		cmd["this"] = text
		*changed = true
	}
	return nil
}

// commandSourceText returns sql from its first token to its last token that is
// neither a semicolon nor zero-width: leading comments and whitespace and the
// trailing terminator are dropped, everything in between is kept byte for byte.
func commandSourceText(e Engine, sql string) (string, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return "", fmt.Errorf("engine: parse: %w", err)
	}
	last := -1
	for i := len(toks) - 1; i >= 0; i-- {
		if toks[i].TokenType != "SEMICOLON" && toks[i].Span.End > toks[i].Span.Start {
			last = i
			break
		}
	}
	if last < 0 {
		return "", nil
	}
	first := 0
	for first < last && toks[first].Span.End == toks[first].Span.Start {
		first++
	}
	return sql[toks[first].Span.Start:toks[last].Span.End], nil
}

func decodeIdentifierNode(sql string, stream tokenStream, node map[string]any, changed *bool) error {
	name, ok := node["name"].(string)
	if !ok {
		return nil
	}
	span, hasSpan := node["span"].(map[string]any)
	if !hasSpan {
		return nil // a function node: no source spelling to decode from
	}
	quoted, _ := node["quoted"].(bool)
	start, sok := jsonInt(span["start"])
	end, eok := jsonInt(span["end"])
	var raw string
	if sok && eok {
		if b0, b1, ok := stream.byteRange(start, end); ok {
			raw = sql[b0:b1]
		}
	}
	if !strings.Contains(raw, "\\") || !isQuotedSpelling(raw) {
		if quoted && strings.Contains(name, "\\") && !isQuotedSpelling(raw) {
			return fmt.Errorf("engine: parse: quoted identifier %q has no verifiable source spelling", name)
		}
		return nil
	}
	decoded, st := decodeQuotedIdentifier(raw)
	switch st {
	case decodedRejected:
		return fmt.Errorf("engine: parse: quoted identifier %s is not a name ClickHouse accepts", raw)
	case decodedNotUTF8:
		return nil
	}
	if decoded != name {
		node["name"] = decoded
		*changed = true
	}
	if !quoted {
		node["quoted"] = true
		*changed = true
	}
	return nil
}

func isQuotedSpelling(raw string) bool {
	return strings.HasPrefix(raw, "`") || strings.HasPrefix(raw, `"`) || strings.HasPrefix(raw, leftDoubleQuote)
}

func jsonInt(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case float64:
		return int(n), true
	}
	return 0, false
}

// sameAST reports whether two ASTs are the same parse. Byte equality is the
// fast path; decodeASTIdentifiers re-encodes a rewritten AST with sorted keys,
// so two parses that agree may differ in key order and are compared
// structurally instead.
func sameAST(a, b AST) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var x, y any
	dx := json.NewDecoder(bytes.NewReader(a))
	dx.UseNumber()
	dy := json.NewDecoder(bytes.NewReader(b))
	dy.UseNumber()
	if dx.Decode(&x) != nil || dy.Decode(&y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
