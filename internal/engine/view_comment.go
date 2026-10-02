package engine

import "strings"

// StripViewComment recognises `CREATE [OR REPLACE] [MATERIALIZED] VIEW … AS
// (<query>) COMMENT '<text>'`, the form the Sentio driver gives every view
// (chx buildCreateViewSQL). The pinned Polyglot's create_view grammar stops
// before a trailing view COMMENT, so the whole-statement parse gate refuses
// the statement. It returns sql without the COMMENT clause and the clause's
// string literal exactly as written, so the ordinary pipeline, every gate and
// policy check included, runs on the statement without its comment and
// AppendViewComment puts the literal back on a rewritten Success. ok=false,
// with sql unchanged, for every other statement.
//
// ClickHouse reads a COMMENT '<text>' that ends a CREATE VIEW or CREATE
// MATERIALIZED VIEW as the view's comment, the same as COMMENT '<text>'
// written before AS (measured with formatQuery on 25.8, 26.2 and 26.8,
// which print it back after the body on 25.8 and before AS on 26.x, for TO and
// ENGINE materialized views, with and without a column list). A comment
// carries no name, so the policy decision on the rest of the statement is the
// decision on the whole statement. The form is recognised only when it is
// unambiguous in both directions:
//
//   - the statement starts CREATE [OR REPLACE] [MATERIALIZED] VIEW (no LIVE,
//     WINDOW, TEMPORARY or other prefix);
//   - it ends, at bracket depth 0 and before only whitespace and semicolons,
//     in the keyword COMMENT and one single-quoted string literal;
//   - the token before COMMENT closes the parenthesised view query: the
//     stripped statement parses to a create_view node with
//     query_parenthesized, whose last token is that ")". A query that is not
//     parenthesised ends in an expression, which ClickHouse can also read a
//     trailing COMMENT against (… CAST(a AS String) COMMENT 'c' is a syntax
//     error on 25.8 and 26.2), so it is left to the parse gate as before.
//
// A statement with COMMENT twice, a heredoc or "…" comment, or a FORMAT /
// SETTINGS / anything else after the literal keeps the parse gate's refusal.
func StripViewComment(e Engine, sql string) (stripped, comment string, ok bool) {
	upper := strings.ToUpper(sql)
	if !strings.HasSuffix(strings.TrimRight(sql, " \t\r\n;"), "'") ||
		!strings.Contains(upper, "COMMENT") || !strings.Contains(upper, "VIEW") {
		return sql, "", false // the statement cannot end in COMMENT '…'; skip tokenizing it
	}
	toks, err := tokenizeRaw(e, sql)
	if err != nil || len(toks) < 6 {
		return sql, "", false
	}
	n := len(toks)
	for n > 0 && toks[n-1].TokenType == "SEMICOLON" {
		n--
	}
	if n < 6 {
		return sql, "", false
	}
	lit, kw, closer := toks[n-1], toks[n-2], toks[n-3]
	if lit.TokenType != "STRING" || !strings.HasPrefix(lit.Source, "'") ||
		!strings.EqualFold(kw.Source, "COMMENT") || closer.TokenType != "R_PAREN" {
		return sql, "", false
	}
	if strings.Trim(sql[lit.Span.End:], " \t\r\n;") != "" {
		return sql, "", false // a comment or other text after the literal
	}
	if !createViewHeader(toks[:n]) {
		return sql, "", false
	}
	depth := 0
	for _, tk := range toks[:n-2] {
		switch tk.TokenType {
		case "L_PAREN", "L_BRACKET", "L_BRACE":
			depth++
		case "R_PAREN", "R_BRACKET", "R_BRACE":
			depth--
		}
	}
	if depth != 0 {
		return sql, "", false
	}
	candidate := strings.TrimRight(sql[:kw.Span.Start], " \t\r\n")
	ast, perr := e.ParseOne(candidate)
	if perr != nil {
		return sql, "", false
	}
	kind, body, _, berr := bodyOf(ast)
	if berr != nil || kind != NodeCreateView || body == nil || body["query_parenthesized"] != true {
		return sql, "", false
	}
	return candidate, sql[lit.Span.Start:lit.Span.End], true
}

// createViewHeader reports whether toks start CREATE [OR REPLACE]
// [MATERIALIZED] VIEW, every word bare.
func createViewHeader(toks []rawToken) bool {
	i := 0
	next := func(w string) bool {
		if i < len(toks) && !isQuotedLexeme(toks[i].TokenType) && strings.EqualFold(toks[i].Source, w) {
			i++
			return true
		}
		return false
	}
	if !next("CREATE") {
		return false
	}
	if next("OR") && !next("REPLACE") {
		return false
	}
	next("MATERIALIZED")
	return next("VIEW")
}

// AppendViewComment puts the COMMENT clause StripViewComment removed back on
// the rewritten statement. It returns ok=false when the rewritten statement
// does not end in the ")" that closes its parenthesised query, so the comment
// could attach to something else; the caller then refuses the statement.
func AppendViewComment(rewritten, comment string) (string, bool) {
	out := strings.TrimRight(rewritten, " \t\r\n;")
	if !strings.HasSuffix(out, ")") {
		return "", false
	}
	return out + " COMMENT " + comment, true
}
