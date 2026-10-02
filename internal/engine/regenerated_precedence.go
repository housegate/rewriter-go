package engine

import (
	"bytes"
	"encoding/json"
)

// This file holds CheckRegenerated's precedence helpers: where a respelling
// that changes an operator's precedence class cannot regroup its operands.

// An operand the precedence rules accept is a single token (a name, a
// literal, NULL / TRUE / FALSE), a dotted name (t.a) or a {name:Type}
// parameter: both ClickHouse and Polyglot bind each of them tighter than any
// operator.
func isAtomicToken(tk rawToken) bool {
	switch tk.TokenType {
	case "VAR", "QUOTED_IDENTIFIER", "NUMBER", "HEX_NUMBER", "NULL", "TRUE", "FALSE":
		return true
	case "IDENTIFIER":
		return isBinaryLiteral(tk.Text)
	}
	return isQuotedLexeme(tk.TokenType)
}

// operandBefore returns the index where the operand ending at toks[j] starts,
// or -1 when that operand is not one the precedence rules accept.
func operandBefore(toks []rawToken, j int) int {
	if j < 0 {
		return -1
	}
	switch {
	case toks[j].TokenType == "R_BRACE":
		for k := j - 1; k >= 0; k-- {
			switch t := toks[k].TokenType; {
			case t == "L_BRACE":
				return k
			case isOpenBracket(t) || isCloseBracket(t):
				return -1
			}
		}
		return -1
	case isNameTok(toks[j].TokenType):
		for j >= 2 && toks[j-1].TokenType == "DOT" && isNameTok(toks[j-2].TokenType) {
			j -= 2
		}
		return j
	case isAtomicToken(toks[j]):
		return j
	}
	return -1
}

// operandAfter returns the index just past the operand starting at toks[k],
// or -1 when that operand is not one the precedence rules accept.
func operandAfter(toks []rawToken, k int) int {
	if k >= len(toks) {
		return -1
	}
	switch {
	case toks[k].TokenType == "L_BRACE":
		for j := k + 1; j < len(toks); j++ {
			switch t := toks[j].TokenType; {
			case t == "R_BRACE":
				return j + 1
			case isOpenBracket(t) || isCloseBracket(t):
				return -1
			}
		}
		return -1
	case isNameTok(toks[k].TokenType):
		for k+2 < len(toks) && toks[k+1].TokenType == "DOT" && isNameTok(toks[k+2].TokenType) {
			k += 2
		}
		return k + 1
	case isAtomicToken(toks[k]):
		return k + 1
	}
	return -1
}

func isOpenBracket(t string) bool  { return t == "L_PAREN" || t == "L_BRACKET" || t == "L_BRACE" }
func isCloseBracket(t string) bool { return t == "R_PAREN" || t == "R_BRACKET" || t == "R_BRACE" }

// opensExpression and closesExpression are the tokens that may border an
// isolated operator application: clause keywords, commas, brackets, and
// AND / OR, which bind looser than every operator the rules respell, in both
// spellings (measured with formatQuery on 26.2 and 26.7.5.10: a <=> b AND c,
// c OR a <=> b OR d, a NOT LIKE b AND c, c AND a NOT LIKE b, a <=> b AS x,
// WHEN / THEN / ELSE, WHERE and ORDER BY … DESC borders). An AND borders only
// when isLogicalAnd proves it logical: the AND of x BETWEEN y AND z is not a
// border, since x BETWEEN 1 AND a <=> b respelled is (x BETWEEN 1 AND a) <=> b.
var (
	opensExpression = map[string]bool{
		"L_PAREN": true, "COMMA": true, "SELECT": true, "DISTINCT": true, "WHERE": true, "PREWHERE": true,
		"HAVING": true, "WHEN": true, "THEN": true, "ELSE": true, "BY": true, "AND": true, "OR": true,
	}
	closesExpression = map[string]bool{
		"R_PAREN": true, "COMMA": true, "SEMICOLON": true, "FROM": true, "WHERE": true, "PREWHERE": true,
		"GROUP": true, "ORDER": true, "HAVING": true, "LIMIT": true, "SETTINGS": true, "FORMAT": true,
		"AS": true, "WHEN": true, "THEN": true, "ELSE": true, "END": true, "AND": true, "OR": true,
		"ASC": true, "DESC": true, "UNION": true,
	}
)

// isolated reports whether the operator toks[first..last] applies to two
// accepted operands and is bordered on both sides, so no neighbouring
// operator can regroup it in either spelling.
func isolated(toks []rawToken, first, last int) bool {
	start, end := operandBefore(toks, first-1), operandAfter(toks, last+1)
	if start < 0 || end < 0 {
		return false
	}
	if end < len(toks) && !closesExpression[toks[end].TokenType] {
		return false
	}
	if start == 0 {
		return true
	}
	b := toks[start-1].TokenType
	return opensExpression[b] && (b != "AND" || isLogicalAnd(toks, start-1))
}

// isLogicalAnd reports whether the AND at toks[j] is proven to be a logical
// AND rather than the AND of a BETWEEN. The scan runs back over the AND's left
// operand chain to its border, taking each balanced bracket pair and each
// balanced CASE … END as one unit (x BETWEEN CASE WHEN c THEN 1 END AND
// a <=> b respelled is (x BETWEEN … AND a) <=> b), and then pairs the chain's
// BETWEENs and ANDs from the left: each BETWEEN owns the next AND. It answers
// false, so the caller refuses, when the AND is a BETWEEN's, when the chain
// holds a BETWEEN inside another BETWEEN's bounds, when the brackets do not
// balance, and when the chain holds any token andChainTokens does not
// classify.
func isLogicalAnd(toks []rawToken, j int) bool {
	var open []string // the closing tokens still to be matched, innermost last
	var chain []int   // the chain's own BETWEEN and AND tokens, right to left
	k := j - 1
scan:
	for ; k >= 0; k-- {
		t := toks[k].TokenType
		switch {
		case isCloseBracket(t) || t == "END":
			open = append(open, t)
		case isOpenBracket(t) || t == "CASE":
			if len(open) == 0 {
				break scan // the chain starts inside this bracket or CASE
			}
			if unitCloser(t) != open[len(open)-1] {
				return false
			}
			open = open[:len(open)-1]
		case len(open) > 0:
			// inside a unit
		case andBorders[t]:
			break scan
		case t == "BETWEEN" || t == "AND":
			chain = append(chain, k)
		case !andChainToken(toks, k):
			return false
		}
	}
	if len(open) > 0 {
		return false
	}
	owned := false // a BETWEEN before this point still owns the next AND
	for i := len(chain) - 1; i >= 0; i-- {
		if toks[chain[i]].TokenType == "BETWEEN" {
			if owned {
				return false
			}
			owned = true
		} else {
			owned = false
		}
	}
	return !owned
}

// unitCloser is the token that closes an opening bracket or CASE.
func unitCloser(open string) string {
	if open == "CASE" {
		return "END"
	}
	return closerOf(open)
}

// andBorders are the tokens after which an AND's left operand chain begins,
// besides an unmatched opening bracket or CASE: a BETWEEN before one of them
// cannot own an AND after it. ON starts a join condition and -> a lambda
// body; OR binds looser than the AND.
var andBorders = map[string]bool{
	"COMMA": true, "SELECT": true, "WHERE": true, "PREWHERE": true, "HAVING": true, "WHEN": true,
	"THEN": true, "ELSE": true, "BY": true, "OR": true, "ON": true, "ARROW": true,
}

// andChainOperators are the operator tokens an AND's left operand chain may
// hold besides BETWEEN and AND: each binds tighter than AND in ClickHouse and
// in Polyglot, and none of them owns an AND.
var andChainOperators = map[string]bool{
	"DOT": true, "EQ": true, "NEQ": true, "LT": true, "GT": true, "LTE": true, "GTE": true,
	"NULLSAFE_EQ": true, "PLUS": true, "DASH": true, "STAR": true, "SLASH": true, "PERCENT": true,
	"D_PIPE": true, "NOT": true, "LIKE": true, "I_LIKE": true, "R_LIKE": true, "IN": true, "IS": true,
	"D_COLON": true, "INTERVAL": true, "CAST": true, "DATE": true, "EXISTS": true,
}

// andChainToken reports whether isLogicalAnd classifies toks[k], outside any
// bracket or CASE: an operand, an operator in andChainOperators, the DISTINCT
// of SELECT DISTINCT, or the DISTINCT FROM of IS [NOT] DISTINCT FROM.
func andChainToken(toks []rawToken, k int) bool {
	tk := toks[k]
	prev := ""
	if k > 0 {
		prev = toks[k-1].TokenType
	}
	switch tk.TokenType {
	case "DISTINCT":
		return prev == "SELECT" || prev == "IS" || prev == "NOT"
	case "FROM":
		return prev == "DISTINCT" && k >= 2 && (toks[k-2].TokenType == "IS" || toks[k-2].TokenType == "NOT")
	case "IDENTIFIER":
		return true
	}
	return isAtomicToken(tk) || andChainOperators[tk.TokenType]
}

// The ternary a ? b : c binds looser than every other operator except the
// lambda arrow, in ClickHouse and in Polyglot alike. Measured with
// formatQuery on 26.2 and 26.7.5.10 against Polyglot's IF(…): each operator
// in ternaryOperators on either side of ? and :, NOT, unary minus, t.a, a[1],
// f(b), (c) and {p:T} operands, and each border in ternaryOpens /
// ternaryCloses. AND / OR are not borders here: they bind tighter than the
// ternary.
var (
	ternaryOperators = map[string]bool{
		"EQ": true, "NEQ": true, "LT": true, "GT": true, "LTE": true, "GTE": true, "PLUS": true, "DASH": true,
		"STAR": true, "SLASH": true, "PERCENT": true, "D_PIPE": true, "AND": true, "OR": true, "NOT": true,
		"LIKE": true, "I_LIKE": true, "IN": true, "BETWEEN": true, "IS": true, "DOT": true,
	}
	// ternaryElseRejects are the operators ClickHouse rejects at the top of
	// the else branch (measured with formatQuery on 26.2: a ? b : c OR d,
	// a ? b : c AND d OR e and a ? b : NOT c OR d are syntax errors, while
	// AND, NOT, [NOT] BETWEEN, [NOT] IN, [NOT] [I]LIKE, IS [NOT] NULL, =, ==,
	// !=, <>, <, >, <=, >=, <=>, ||, arithmetic, DIV, MOD and REGEXP are read
	// as the else branch).
	ternaryElseRejects = map[string]bool{"OR": true}
	ternaryOpens       = map[string]bool{
		"COMMA": true, "SELECT": true, "DISTINCT": true, "WHERE": true, "PREWHERE": true, "HAVING": true,
		"WHEN": true, "THEN": true, "ELSE": true, "BY": true, "ARROW": true,
	}
	ternaryCloses = map[string]bool{
		"COMMA": true, "SEMICOLON": true, "FROM": true, "WHERE": true, "PREWHERE": true, "GROUP": true,
		"ORDER": true, "HAVING": true, "LIMIT": true, "SETTINGS": true, "FORMAT": true, "AS": true,
		"WHEN": true, "THEN": true, "ELSE": true, "END": true, "ASC": true, "DESC": true, "UNION": true,
	}
)

// ternaryColon returns the index of the : of the ternary whose ? is toks[q]
// when Polyglot's IF(a, b, c) regroups nothing, and -1 otherwise. The ternary
// must run from a border in ternaryOpens (or an opening bracket, or the start)
// to one in ternaryCloses (or a closing bracket, or the end). At its own
// bracket depth it may hold only operands and ternaryOperators, one ? and one
// :, each branch non-empty and ending in an operand, no operand directly
// after another (an implicit alias, INTERVAL 1 DAY, x DIV y), and no
// ternaryElseRejects operator after the :.
func ternaryColon(toks []rawToken, q int) int {
	start := 0 // the first token of the ternary
	for k, depth := q-1, 0; k >= 0; k-- {
		t := toks[k].TokenType
		if isCloseBracket(t) {
			depth++
		} else if isOpenBracket(t) {
			if depth == 0 {
				start = k + 1
				break
			}
			depth--
		} else if depth == 0 && ternaryOpens[t] {
			start = k + 1
			break
		}
	}
	end := len(toks) // one past the last token of the ternary
	for k, depth := q+1, 0; k < len(toks); k++ {
		t := toks[k].TokenType
		if isOpenBracket(t) {
			depth++
		} else if isCloseBracket(t) {
			if depth == 0 {
				end = k
				break
			}
			depth--
		} else if depth == 0 && ternaryCloses[t] {
			end = k
			break
		}
	}
	colon, afterOperand := -1, false
	for k, depth := start, 0; k < end; k++ {
		tk := toks[k]
		t := tk.TokenType
		switch {
		case isOpenBracket(t):
			if depth == 0 && afterOperand && t != "L_BRACKET" && !(t == "L_PAREN" && isNameTok(toks[k-1].TokenType)) {
				return -1 // (a)(b), 1(b), a {p:T}: not a call or a subscript
			}
			depth++
		case isCloseBracket(t):
			depth--
			if depth == 0 {
				afterOperand = true
			}
		case depth > 0:
		case k == q || (t == "COLON" && colon < 0 && k > q):
			if !afterOperand {
				return -1 // an empty or unfinished branch
			}
			if k != q {
				colon = k
			}
			afterOperand = false
		case isAtomicToken(tk):
			if afterOperand {
				return -1
			}
			afterOperand = true
		case colon >= 0 && ternaryElseRejects[t]:
			return -1 // ClickHouse rejects the statement; IF(a, b, c OR d) would not
		case ternaryOperators[t]:
			afterOperand = false
		default:
			return -1
		}
	}
	if colon < 0 || !afterOperand {
		return -1
	}
	return colon
}

// keywordNameTokens are the keyword token types the pinned tokenizer gives a
// bare word that ClickHouse also reads as a plain identifier in an operand
// position. Measured on ClickHouse 26.8 for every single-word entry of
// system.keywords whose Polyglot token type is not VAR, as the left and the
// right operand of NOT LIKE in sixteen positions (WHERE, AND, OR, PREWHERE,
// HAVING, WHEN, a SELECT item, a parenthesis, a function argument, ORDER BY
// … DESC, after DISTINCT, before AS): wherever ClickHouse accepts
// `k NOT LIKE 'x'` and Polyglot parses it in full, ClickHouse reads k as an
// identifier there and reads Polyglot's `NOT k LIKE 'x'` as NOT (k LIKE 'x').
// Left out: ALL and THEN, which ClickHouse rejects in some of those positions
// (so a respelling could turn a rejected statement into an accepted one),
// INTERVAL, which Polyglot regenerates as another expression, EXISTS, which
// Polyglot does not read as a column, and BETWEEN, IN, ILIKE, REGEXP and TOP,
// which the spelling comparison reads as the operator (NOT between is spelled
// NOT BETWEEN) or as LIMIT (TOP), so the respelling cannot match anyway.
var keywordNameTokens = map[string]bool{
	"ADD": true, "AFTER": true, "ALTER": true, "AND": true, "ANTI": true, "ANY": true, "APPLY": true,
	"AS": true, "ASC": true, "AS_OF": true, "AUTO_INCREMENT": true, "BY": true,
	"CASCADE": true, "CHECK": true, "CLUSTER": true, "COLLATE": true, "COLUMN": true, "COMMENT": true,
	"COMMIT": true, "CONSTRAINT": true, "COPY": true, "CREATE": true, "CROSS": true, "CUBE": true,
	"CURRENT": true, "DATABASE": true, "DATE": true, "DEFAULT": true, "DELETE": true, "DESC": true,
	"DESCRIBE": true, "DISTINCT": true, "DROP": true, "ELSE": true, "END": true, "ESCAPE": true,
	"EXCEPT": true, "EXECUTE": true, "FETCH": true, "FILTER": true, "FINAL": true,
	"FIRST": true, "FOLLOWING": true, "FOR": true, "FOREIGN_KEY": true, "FORMAT": true, "FROM": true,
	"FULL": true, "FUNCTION": true, "GRANT": true, "GROUPS": true, "HAVING": true,
	"INDEX": true, "INNER": true, "INTERSECT": true, "JOIN": true, "KEY": true,
	"KILL": true, "LANGUAGE": true, "LAST": true, "LEFT": true, "LIMIT": true, "LOCAL": true,
	"MATCH": true, "MATERIALIZED": true, "NATURAL": true, "NEXT": true, "NULLS": true, "OFFSET": true,
	"ON": true, "ONLY": true, "OR": true, "OUTER": true, "OVER": true, "PARTITION": true, "PLACING": true,
	"PRECEDING": true, "PREPARE": true, "PREWHERE": true, "PRIMARY_KEY": true, "QUALIFY": true,
	"RANGE": true, "RECURSIVE": true, "REFERENCES": true, "REFRESH": true, "RENAME": true,
	"REPLACE": true, "RESTRICT": true, "RETURNS": true, "REVOKE": true, "RIGHT": true, "ROLLBACK": true,
	"ROLLUP": true, "ROW": true, "ROWS": true, "SAMPLE": true, "SELECT": true,
	"SEMI": true, "SET": true, "SETTINGS": true, "SHOW": true, "SOME": true, "SYSTEM": true,
	"TABLE": true, "TEMPORARY": true, "TIMESTAMP": true, "TO": true, "TRANSACTION": true,
	"TRIGGER": true, "TRUNCATE": true, "TYPE": true, "UNBOUNDED": true, "UNION": true, "UNIQUE": true,
	"UPDATE": true, "USE": true, "VALUES": true, "VIEW": true, "WHEN": true, "WHERE": true,
	"WINDOW": true, "WITH": true,
}

// keywordColumnsAsNames returns toks with every bare keyword token that the
// statement's AST reads as (part of) a column reference retyped VAR, so the
// precedence helpers treat it as the single-token operand it is: the pinned
// tokenizer types a bare `cluster` CLUSTER, and without this
// `cluster NOT LIKE 'all-%'` (the Sentio driver's cluster probe) could not
// take the measured NOT LIKE respelling. Only a token whose type is in
// keywordNameTokens and whose span is exactly the span of an unquoted
// identifier inside a column node is retyped; a keyword Polyglot reads as
// anything else (a clause, an operator, DATE '…' as a typed literal) keeps
// its type, and the precedence helpers keep refusing it. The input slice is
// not modified.
func keywordColumnsAsNames(sql string, toks []rawToken, ast AST) []rawToken {
	candidate := false
	for _, tk := range toks {
		if keywordNameTokens[tk.TokenType] && tk.Source != "" && isWordStart(tk.Source) {
			candidate = true
			break
		}
	}
	if !candidate {
		return toks
	}
	dec := json.NewDecoder(bytes.NewReader(ast))
	dec.UseNumber()
	var root any
	if dec.Decode(&root) != nil {
		return toks
	}
	stream := newTokenStream(sql)
	spans := map[[2]int]bool{}
	addName := func(n any) {
		id, _ := n.(map[string]any)
		if id == nil || id["quoted"] != false {
			return
		}
		span, _ := id["span"].(map[string]any)
		start, sok := jsonInt(span["start"])
		end, eok := jsonInt(span["end"])
		if !sok || !eok {
			return
		}
		if b0, b1, ok := stream.byteRange(start, end); ok {
			spans[[2]int{b0, b1}] = true
		}
	}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if col, ok := v["column"].(map[string]any); ok && len(v) == 1 {
				addName(col["name"])
				addName(col["table"])
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(root)
	if len(spans) == 0 {
		return toks
	}
	out := toks
	copied := false
	for i, tk := range toks {
		if !keywordNameTokens[tk.TokenType] || !spans[[2]int{tk.Span.Start, tk.Span.End}] || !isWordStart(tk.Source) {
			continue
		}
		if !copied {
			out = append([]rawToken(nil), toks...)
			copied = true
		}
		out[i].TokenType = "VAR"
	}
	return out
}
