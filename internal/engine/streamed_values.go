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

// valuesRowsKeyword returns the index in toks of the VALUES token that starts
// the inline rows of an INSERT whose rows Polyglot parsed into insert.values
// (the VALUES keyword, or the format name of FORMAT Values), or -1. It is the
// last VALUES token outside brackets: the rows themselves sit inside brackets,
// with only commas and a semicolon between them, while a table or column named
// values comes before them, and a quoted `Values` is not a VALUES token.
func valuesRowsKeyword(ast AST, toks []rawToken) int {
	_, body, _, err := bodyOf(ast)
	if err != nil || body == nil || body["query"] != nil {
		return -1
	}
	if rows, _ := body["values"].([]any); len(rows) == 0 {
		return -1
	}
	depth, last := 0, -1
	for i, tok := range toks {
		switch {
		case isBracketToken(tok.TokenType) && !isClosingBracket(tok.TokenType):
			depth++
		case isClosingBracket(tok.TokenType):
			depth--
		case depth == 0 && tok.TokenType == "VALUES":
			last = i
		}
	}
	return last
}

// inlineFormatValues returns the index in toks of the FORMAT keyword of an
// INSERT … FORMAT Values <rows> whose rows Polyglot parsed into insert.values,
// or -1. ClickHouse runs such a statement exactly like INSERT … VALUES <rows>
// (measured on 25.8 and 26.8, any case of the format name), and Polyglot
// parses it into the same AST, so it is checked and rewritten as the VALUES
// form, which rewriter-grpc generates too (housegate/rewriter-go#75).
//
// The FORMAT keyword is the token right before the VALUES token where the
// AST's rows begin (valuesRowsKeyword).
func inlineFormatValues(ast AST, toks []rawToken) int {
	if v := valuesRowsKeyword(ast, toks); v > 0 && toks[v-1].TokenType == "FORMAT" {
		return v - 1
	}
	return -1
}

// plainValuesRows returns the index in toks of the '(' that opens the first
// row of a plain INSERT … VALUES <rows> whose rows Polyglot parsed into
// insert.values, or -1 (housegate/rewriter-go#77).
func plainValuesRows(ast AST, toks []rawToken) int {
	v := valuesRowsKeyword(ast, toks)
	if v < 0 || (v > 0 && toks[v-1].TokenType == "FORMAT") ||
		v+1 >= len(toks) || toks[v+1].TokenType != "L_PAREN" {
		return -1
	}
	return v + 1
}

// opaqueInsertTextStart returns the byte offset in sql of the SETTINGS keyword
// that follows the column list of an INSERT Polyglot keeps as one opaque
// command text (`INSERT … (cols) SETTINGS … VALUES <rows>`, `… SELECT …`,
// `… FORMAT <name> [data]`), and the offset where that text ends: the end of
// sql, or the end of the format name of a FORMAT clause other than Values,
// whose data is not SQL. ok is false for any other statement. The keyword is
// the first SETTINGS token outside brackets after the column list's ')'.
//
// rewriter-grpc refuses a comment anywhere in that text in dynamic mode
// (`opaqueTextIsUngoverned`: a comment can hide a read from the comment-free
// lexical scan), while Polyglot's tokenizer, and so the R2 / R5 scan over the
// text (OpaqueInsertQueryIsUngoverned), skips it (housegate/rewriter-go#84).
// CheckParsedInFull admits only whitespace between the tokens of the text, in
// every mode, as it does for Values data (valuesDataStart); rewriter-grpc's
// static and no-rewrite modes forward the text verbatim instead.
func opaqueInsertTextStart(sql string, ast AST, toks []rawToken) (start, end int, ok bool) {
	_, body, _, err := bodyOf(ast)
	if err != nil || body == nil {
		return 0, 0, false
	}
	q, _ := body["query"].(map[string]any)
	cmd, _ := q["command"].(map[string]any)
	this, _ := cmd["this"].(string)
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(this)), "SETTINGS") {
		return 0, 0, false
	}
	depth, columns, start := 0, false, -1
	for i, tok := range toks {
		switch {
		case isBracketToken(tok.TokenType) && !isClosingBracket(tok.TokenType):
			depth++
		case isClosingBracket(tok.TokenType):
			depth--
			columns = columns || depth == 0
		case depth != 0 || !columns:
		case start < 0:
			if tok.TokenType == "SETTINGS" {
				start = tok.Span.Start
			}
		case tok.TokenType == "FORMAT" && i+1 < len(toks) && toks[i+1].TokenType != "VALUES":
			// The data after any other format name is not SQL; the
			// tokenizer does not split it.
			return start, toks[i+1].Span.End, true
		}
	}
	if start < 0 {
		return 0, 0, false
	}
	return start, len(sql), true
}

// valuesDataStart returns the byte offset in sql from which ClickHouse reads
// an INSERT's inline rows as Values data, and a label for messages, or ok
// false when the statement has no inline rows. After `FORMAT Values` the data
// starts right after the format name; after VALUES ClickHouse's parser first
// skips comments, and the data starts at the first row's '('.
//
// Polyglot drops a comment in the data, or regenerates it, while ClickHouse
// reads it as row text: 25.8 refuses one after a row as unparsable data (26.8
// accepts most, but not a trailing block comment), and rewriter-grpc refuses
// every comment in the rows in dynamic mode (housegate/rewriter-go#75, #77).
// The opaque `(cols) SETTINGS …` form is checked from its SETTINGS keyword on
// instead (opaqueInsertTextStart), which covers its rows too.
// CheckParsedInFull therefore admits only whitespace between the data's
// tokens, in every mode. rewriter-grpc's static and no-rewrite modes forward
// the rows verbatim instead, comment included, so those modes differ: this
// engine refuses the statement there.
func valuesDataStart(ast AST, toks []rawToken) (start int, what string, ok bool) {
	if i := inlineFormatValues(ast, toks); i >= 0 {
		name := toks[i+1]
		return name.Span.End, "FORMAT " + name.Source + " data", true
	}
	if i := plainValuesRows(ast, toks); i >= 0 {
		return toks[i].Span.Start, "VALUES rows", true
	}
	return 0, "", false
}

// valuesDataTextError returns an error wrapping ErrNotParsedInFull when the
// text of sql from offset start holds anything but whitespace between or after
// its tokens, that is, a comment.
func valuesDataTextError(sql string, toks []rawToken, start int, what string) error {
	end := start
	for _, tok := range toks {
		if tok.Span.Start < end {
			continue
		}
		if !isClickHouseInsertWhitespace(sql[end:tok.Span.Start]) {
			return fmt.Errorf("%w: comment in %s: %q", ErrNotParsedInFull, what, excerpt(sql[end:]))
		}
		end = tok.Span.End
	}
	if !isClickHouseInsertWhitespace(sql[end:]) {
		return fmt.Errorf("%w: comment in %s: %q", ErrNotParsedInFull, what, excerpt(sql[end:]))
	}
	return nil
}
