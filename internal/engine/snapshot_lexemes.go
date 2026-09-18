package engine

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Polyglot retains identifier spans but numeric nodes carry only text. Match the
// complete lexical numeric stream to numeric AST leaves in SQL grammar order,
// before normalization, so a parser that rounds/drops/reorders a token refuses.
// This is fidelity validation, never statement admission: the typed closed walk
// independently admits every node and every semantic field.
func bindSnapshotLexemes(root snapshotObject, raw AST, sql string) error {
	var tokens []struct {
		Type string                   `json:"token_type"`
		Text string                   `json:"text"`
		Span struct{ Start, End int } `json:"span"`
	}
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return snapshotStatement
	}
	for i, t := range tokens {
		if t.Type == "PLUS" {
			j := i + 1
			for j < len(tokens) && tokens[j].Type == "L_PAREN" {
				j++
			}
			if j >= len(tokens) || tokens[j].Type != "NUMBER" {
				return snapshotSyntax
			}
		}
		if t.Type == "NUMBER" {
			signs := 0
			for j := i - 1; j >= 0; j-- {
				typ := tokens[j].Type
				if typ == "PLUS" || typ == "DASH" {
					signs++
					continue
				}
				if typ == "L_PAREN" {
					continue
				}
				break
			}
			if signs > 1 {
				return snapshotLiteral
			}
		}
	}
	numbers := []string{}
	for _, t := range tokens {
		if t.Span.Start < 0 || t.Span.End < t.Span.Start || t.Span.End > len(sql) {
			return snapshotLiteral
		}
		if t.Type == "NUMBER" {
			lex := sql[t.Span.Start:t.Span.End]
			if lex != t.Text {
				return snapshotLiteral
			}
			numbers = append(numbers, lex)
		}
		switch t.Type {
		case "PLACEHOLDER", "PARAMETER", "L_BRACE":
			return snapshotParameter
		}
	}
	leaves := []string{}
	var walk func(any)
	walk = func(raw any) {
		switch x := raw.(type) {
		case []any:
			for _, v := range x {
				walk(v)
			}
		case map[string]any:
			if l := sm(x["literal"]); l != nil && l["literal_type"] == "number" {
				leaves = append(leaves, ss(l["value"]))
				return
			}
			// This order covers both WITH placements, SELECT projection before FROM,
			// UNION branches, and left-to-right expression/function argument evaluation.
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.SliceStable(keys, func(i, j int) bool {
				a, b := snapshotLexicalRank(keys[i]), snapshotLexicalRank(keys[j])
				if a != b {
					return a < b
				}
				return keys[i] < keys[j]
			})
			for _, k := range keys {
				if k == "span" || strings.HasSuffix(k, "comments") {
					continue
				}
				walk(x[k])
			}
		}
	}
	walk(root)
	if len(leaves) != len(numbers) {
		return snapshotLiteral
	}
	for i := range leaves {
		if leaves[i] != numbers[i] {
			return snapshotLiteral
		}
	}
	return nil
}
func snapshotLexicalRank(k string) int {
	switch k {
	case "with":
		return 0
	case "ctes":
		return 1
	case "table":
		return 2
	case "columns":
		return 3
	case "left":
		return 4
	case "this":
		return 5
	case "expressions":
		return 6
	case "args":
		return 7
	case "from":
		return 8
	case "joins":
		return 9
	case "on":
		return 10
	case "where_clause":
		return 11
	case "query":
		return 12
	case "right":
		return 13
	}
	return 20
}

func snapshotNormalizeIdentifiers(e Engine, root snapshotObject) {
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, v := range x {
				walk(v)
			}
		case map[string]any:
			if x["quoted"] == true {
				if name, ok := x["name"].(string); ok {
					if _, function := x["args"]; !function && sid(name)["quoted"] == false {
						raw, err := e.ParseOne("SELECT " + name)
						if err == nil {
							var ast snapshotObject
							if json.Unmarshal(raw, &ast) == nil {
								sel := sm(ast["select"])
								exprs := sa(sel["expressions"])
								if len(exprs) == 1 {
									col := sm(sm(exprs[0])["column"])
									if col["table"] == nil && ss(sm(col["name"])["name"]) == name {
										x["quoted"] = false
									}
								}
							}
						}

					}
				}
			}
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(root)
}

// The pinned generator spells an ALL inner join as INNER ALL JOIN. Canonical
// snapshot SQL uses ALL INNER JOIN. This post-validation formatter acts only
// on lexer-classified keyword tokens, never on strings or identifiers.
func generateSnapshot(e Engine, ast AST) (string, error) {
	sql, err := e.Generate(ast)
	if err != nil {
		return "", err
	}
	raw, err := e.Tokenize(sql)
	if err != nil {
		return "", err
	}
	var ts []struct {
		Text string                   `json:"text"`
		Type string                   `json:"token_type"`
		Span struct{ Start, End int } `json:"span"`
	}
	if err = json.Unmarshal(raw, &ts); err != nil {
		return "", err
	}
	for i := len(ts) - 1; i >= 0; i-- {
		if ts[i].Type == "QUOTED_IDENTIFIER" {
			text := strings.ReplaceAll(ts[i].Text, "\\", "\\\\")
			text = strings.ReplaceAll(text, "`", "\\`")
			sql = sql[:ts[i].Span.Start] + "`" + text + "`" + sql[ts[i].Span.End:]
			continue
		}
		if i+2 < len(ts) && ts[i].Type == "INNER" && ts[i+1].Type == "ALL" && ts[i+2].Type == "JOIN" {
			start, end := ts[i].Span.Start, ts[i+1].Span.End
			sql = sql[:start] + "ALL INNER" + sql[end:]
		}
	}
	return sql, nil
}

// Materialize the original AST once in lexical order, including unused CTEs.
// Referencing a CTE repeatedly must never consume additional pool entries.
func materializeSnapshotAST(root snapshotObject, opts SnapshotOptions) (int, error) {
	count := 0
	var walk func(any) error
	walk = func(raw any) error {
		switch x := raw.(type) {
		case []any:
			for _, v := range x {
				if err := walk(v); err != nil {
					return err
				}
			}
		case map[string]any:
			name := ""
			if r, ok := x["rand"]; ok {
				name = "rand"
				if r != nil {
					if snapshotFields(sm(r), "", "seed lower upper") != nil {
						return snapshotVolatile
					}
				}
			} else if f := sm(x["function"]); f != nil {
				switch strings.ToLower(ss(f["name"])) {
				case "rand32", "rand64":
					name = strings.ToLower(ss(f["name"]))
					if snapshotFields(f, "name", "args distinct trailing_comments use_bracket_syntax no_parens quoted") != nil {
						return snapshotVolatile
					}
				}
			}
			if name != "" {
				if !opts.Materialize {
					return snapshotResidual
				}
				if count >= len(opts.Random) {
					return snapshotMaterialization
				}
				v := opts.Random[count]
				count++
				if name != "rand64" && v > 1<<32-1 {
					return snapshotMaterialization
				}
				replaceMap(x, snapshotNumber(strconv.FormatUint(v, 10)))
				return nil
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				a, b := snapshotLexicalRank(keys[i]), snapshotLexicalRank(keys[j])
				if a != b {
					return a < b
				}
				return keys[i] < keys[j]
			})
			for _, k := range keys {
				if k == "span" || strings.HasSuffix(k, "comments") {
					continue
				}
				if err := walk(x[k]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err := walk(root)
	return count, err
}
