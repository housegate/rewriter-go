package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// streamedValuesStandIn is the format name parsed in place of a streamed
// `FORMAT Values`. It is a format Polyglot reads as a plain FORMAT <name>
// clause and, like "Values", six bytes long, so every span in the stand-in
// statement is the span of the same token in the client's SQL.
const streamedValuesStandIn = "Native"

// streamedValuesSQL reports whether sql is an INSERT whose last token is the
// format name of a trailing `FORMAT Values` clause (any case), and if so
// returns the statement with that name replaced by streamedValuesStandIn and
// the client's spelling of the name.
//
// clickhouse-client sends `INSERT … FORMAT Values` whose rows come from stdin
// as the statement text alone and streams the rows as Data packets, so nothing
// follows the format name. The pinned Polyglot requires a row after
// `FORMAT Values` and refuses the statement, while it accepts the same
// statement with any other format name (parser.rs, the ClickHouse FORMAT
// branches of the INSERT source). The tokenizer drops comments, so a comment
// after the name still matches here; CheckParsedInFull refuses it.
func streamedValuesSQL(e Engine, sql string) (standIn, spelling string, ok bool) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return "", "", false
	}
	n := len(toks)
	if n < 3 || toks[0].TokenType != "INSERT" ||
		toks[n-2].TokenType != "FORMAT" || toks[n-1].TokenType != "VALUES" {
		return "", "", false
	}
	name := toks[n-1]
	spelling = sql[name.Span.Start:name.Span.End]
	if len(spelling) != len(streamedValuesStandIn) || !strings.EqualFold(spelling, "values") {
		return "", "", false
	}
	return sql[:name.Span.Start] + streamedValuesStandIn + sql[name.Span.End:], spelling, true
}

// restoreStreamedValuesFormat puts the client's format name back into the
// FORMAT data clause of an INSERT parsed from streamedValuesSQL's stand-in.
// Polyglot folds the clause into insert.query as
// {"command":{"this":"FORMAT <name>"}}; the generated SQL then names the
// format the client sent.
func restoreStreamedValuesFormat(ast AST, spelling string) (AST, error) {
	dec := json.NewDecoder(bytes.NewReader(ast))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("engine: parse: decode streamed FORMAT Values: %w", err)
	}
	ins, _ := root[NodeInsert].(map[string]any)
	q, _ := ins["query"].(map[string]any)
	cmd, _ := q["command"].(map[string]any)
	if want := "FORMAT " + streamedValuesStandIn; cmd == nil || cmd["this"] != want {
		return nil, fmt.Errorf("engine: parse: streamed FORMAT Values stand-in did not parse to a FORMAT %s clause", streamedValuesStandIn)
	}
	cmd["this"] = "FORMAT " + spelling
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: parse: encode streamed FORMAT Values: %w", err)
	}
	return AST(out), nil
}

// isClickHouseInsertWhitespace reports whether s is empty or only the
// whitespace ClickHouse skips between tokens.
func isClickHouseInsertWhitespace(s string) bool {
	return strings.Trim(s, " \t\n\r\f\v") == ""
}
