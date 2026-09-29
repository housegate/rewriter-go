package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// decodeASTIdentifiers rewrites the name of every quoted identifier node in a
// freshly parsed AST to the name ClickHouse resolves, decoded from the node's
// source spelling (decodeQuotedIdentifier). Polyglot keeps several ClickHouse
// escapes verbatim in AST names (`ph\Nys` for the database ClickHouse reads as
// phys, `t\N` for table t), so without this every downstream check — the
// protected / known-physical / reserved database checks, database_map
// resolution, the storage-integrity Active-table lookup, the reserved-column
// check and DDL targets — compared a name ClickHouse never uses. Polyglot's
// generator escapes a backslash inside a quoted name, so the regenerated SQL
// then carries exactly the decoded name.
//
// Only an identifier node (an object with a string "name", "quoted": true and a
// source "span") whose source spelling holds a backslash is touched; the
// common case returns ast unchanged, byte for byte. A function node carries no
// span and keeps Polyglot's name; its matchers finish the decode themselves
// (decodeIdentifierEscapes). A quoted identifier whose spelling cannot be
// decoded, or whose span does not cover a quoted spelling while its name holds
// a backslash, fails the parse (the statement is refused).
func decodeASTIdentifiers(sql string, ast AST) (AST, error) {
	if !strings.Contains(sql, "\\") {
		return ast, nil
	}
	dec := json.NewDecoder(bytes.NewReader(ast))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("engine: parse: decode AST for identifier decoding: %w", err)
	}
	stream := newTokenStream(sql)
	changed := false
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

func decodeIdentifierNode(sql string, stream tokenStream, node map[string]any, changed *bool) error {
	name, ok := node["name"].(string)
	if !ok {
		return nil
	}
	if quoted, _ := node["quoted"].(bool); !quoted {
		return nil
	}
	span, hasSpan := node["span"].(map[string]any)
	if !hasSpan {
		return nil // a function node: no source spelling to decode from
	}
	start, sok := jsonInt(span["start"])
	end, eok := jsonInt(span["end"])
	var raw string
	if sok && eok {
		if b0, b1, ok := stream.byteRange(start, end); ok {
			raw = sql[b0:b1]
		}
	}
	if !strings.Contains(raw, "\\") {
		if strings.Contains(name, "\\") && !isQuotedSpelling(raw) {
			return fmt.Errorf("engine: parse: quoted identifier %q has no verifiable source spelling", name)
		}
		return nil
	}
	decoded, ok := decodeQuotedIdentifier(raw)
	if !ok {
		return fmt.Errorf("engine: parse: quoted identifier %s cannot be decoded as ClickHouse does", raw)
	}
	if decoded != name {
		node["name"] = decoded
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
