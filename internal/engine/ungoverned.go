package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// walkGenericExpression walks an expression-bearing subtree whose exact shape
// the ordered walker does not model field by field: a CREATE TABLE / CREATE
// VIEW column definition (DEFAULT / MATERIALIZED / ALIAS / EPHEMERAL / TTL /
// CODEC), a constraint (CHECK, INDEX … TYPE), a non-engine storage property
// (PARTITION BY, ORDER BY, PRIMARY KEY, SAMPLE BY, TTL), or a structured
// ALTER action. Every read-bearing node (a read query, an IN node, a function
// call, a table payload) is handed to the ordinary walker, so T2 / T3 / T5 and
// every collector see it exactly as they would in a SELECT; any other node is
// descended generically (spec 2026-09-26 R2).
func walkGenericExpression(node any, scope readSourceScope, visitor readSourceVisitor) error {
	switch n := node.(type) {
	case []any:
		for _, child := range n {
			if err := walkGenericExpression(child, scope, visitor); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if opaqueSubquery(n) {
			return nil
		}
		if handled, err := walkReadQuery(n, scope, visitor); handled {
			return err
		}
		if in, ok := n["in"].(map[string]any); ok && len(n) == 1 {
			return walkInExpression(in, scope, visitor)
		}
		if function, ok := n["function"].(map[string]any); ok && len(n) == 1 {
			return walkFunctionExpression(function, scope, visitor)
		}
		if table, ok := n["table"].(map[string]any); ok && isTableRefPayload(table) {
			emitTableSource(n, table, scope, visitor)
			return nil
		}
		for _, key := range sortedMapKeys(n) {
			if err := walkGenericExpression(n[key], scope, visitor); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

// ExpressionPositionHasReads reports whether a read source — a table, an
// IN-table operand, a table function or a namespace carrier — occurs in a
// position no rewrite pipeline reaches (spec 2026-09-26 R2): a structured
// UPDATE / DELETE statement's assignments, predicate and other clauses, an
// INSERT's VALUES expressions, a CREATE TABLE / CREATE VIEW column,
// constraint or non-engine storage property, and a structured ALTER action.
// Only an INSERT … SELECT / CTAS / CREATE VIEW body is rewritten, so a read
// anywhere else would be forwarded unrewritten and unreported; the caller
// refuses the statement instead.
func ExpressionPositionHasReads(ast AST) (bool, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return false, fmt.Errorf("engine: decode expression positions: %w", err)
	}
	found := false
	visitor := readSourceVisitor{
		table:          func(_, _ map[string]any, _ TableTarget) { found = true },
		function:       func(_ map[string]any, _ namespaceRefDetail) { found = true },
		inTable:        func(_ map[string]any, _ namespaceRefDetail) { found = true },
		namespace:      func(_ map[string]any, _ namespaceRefDetail) { found = true },
		parameter:      func(map[string]any) { found = true },
		sourceFunction: func(string) { found = true },
	}
	scope := readSourceScope{}
	var err error
	switch {
	case statementMap(root, NodeUpdate) != nil:
		err = walkUpdateObjects(statementMap(root, NodeUpdate), scope, visitor)
	case statementMap(root, NodeDelete) != nil:
		err = walkDeleteObjects(statementMap(root, NodeDelete), scope, visitor)
	case statementMap(root, NodeInsert) != nil:
		err = walkGenericExpression(statementMap(root, NodeInsert)["values"], scope, visitor)
	case statementMap(root, NodeCreateTable) != nil:
		body := statementMap(root, NodeCreateTable)
		err = walkGenericExpression([]any{body["columns"], body["constraints"],
			nonEngineProperties(body["properties"]), nonEngineProperties(body["post_table_properties"])}, scope, visitor)
	case statementMap(root, NodeCreateView) != nil:
		body := statementMap(root, NodeCreateView)
		err = walkGenericExpression([]any{body["columns"], nonEngineProperties(body["table_properties"])}, scope, visitor)
	case statementMap(root, NodeAlterTable) != nil:
		body := statementMap(root, NodeAlterTable)
		err = walkGenericExpression([]any{body["actions"], body["partition"]}, scope, visitor)
	}
	return found, err
}

// nonEngineProperties drops the engine and dictionary-source properties (their
// arguments are governed by the T5 allowlist and the namespace policy) and
// keeps every other storage property for walkGenericExpression.
func nonEngineProperties(node any) []any {
	props, _ := node.([]any)
	var out []any
	for _, p := range props {
		pm, _ := p.(map[string]any)
		if pm == nil {
			continue
		}
		if _, ok := pm["engine_property"]; ok {
			continue
		}
		if _, ok := pm["dict_property"]; ok {
			continue
		}
		out = append(out, pm)
	}
	return out
}

// OpaqueAlterTexts returns the opaque ALTER text polyglot does not structure
// (spec 2026-09-26 R2): the whole text of an `ALTER TABLE … UPDATE` / multi-
// command command node, and every {"Raw":{"sql":…}} action of a structured
// ALTER TABLE (DELETE, MODIFY TTL, MODIFY COLUMN, MODIFY QUERY, FETCH /
// ATTACH / MOVE PARTITION, …). Every other statement returns nothing.
func OpaqueAlterTexts(ast AST) ([]string, error) {
	kind, body, _, err := bodyOf(ast)
	if err != nil {
		return nil, err
	}
	switch kind {
	case NodeCommand:
		raw, _ := body["this"].(string)
		if classifyWriteCommand(raw) == CmdAlterUpdate {
			return []string{raw}, nil
		}
		return nil, nil
	case NodeAlterTable:
		var texts []string
		actions, _ := body["actions"].([]any)
		for _, a := range actions {
			am, _ := a.(map[string]any)
			if raw, ok := am["Raw"].(map[string]any); ok {
				if sql, ok := raw["sql"].(string); ok {
					texts = append(texts, sql)
				}
			}
		}
		return texts, nil
	default:
		return nil, nil
	}
}

// OpaqueTextQualifiedNames returns, in token order, every `db.table` run in
// an opaque text (the table half of a longer run is not a qualifier).
// ok=false means the text could not be tokenized.
func OpaqueTextQualifiedNames(e Engine, text string) (names []TableTarget, ok bool) {
	toks, err := tokenizeRaw(e, text)
	if err != nil {
		return nil, false
	}
	return qualifiedNameRuns(toks, 0), true
}

func qualifiedNameRuns(toks []rawToken, from int) []TableTarget {
	var out []TableTarget
	for i := from; i+2 < len(toks); i++ {
		if isNameTok(toks[i].TokenType) && toks[i+1].TokenType == "DOT" && isNameTok(toks[i+2].TokenType) {
			if i > 0 && toks[i-1].TokenType == "DOT" {
				continue
			}
			out = append(out, TableTarget{DB: toks[i].Text, Table: toks[i+2].Text})
		}
	}
	return out
}

// OpaqueTextDatabases returns, in token order, the qualifier of every
// `name.name` run in an opaque ALTER text, so the T3 protected-database check
// covers a name the structured walker cannot see. ok=false means the text
// could not be tokenized.
func OpaqueTextDatabases(e Engine, text string) (dbs []string, ok bool) {
	toks, err := tokenizeRaw(e, text)
	if err != nil {
		return nil, false
	}
	for i := 0; i+2 < len(toks); i++ {
		if isNameTok(toks[i].TokenType) && toks[i+1].TokenType == "DOT" && isNameTok(toks[i+2].TokenType) {
			if i > 0 && toks[i-1].TokenType == "DOT" {
				continue // the table half of a longer run, never a qualifier
			}
			dbs = append(dbs, toks[i].Text)
		}
	}
	return dbs, true
}

// OpaqueTextIsUngoverned reports whether an opaque ALTER text carries a read
// the rewriter cannot govern (spec 2026-09-26 R2), so the caller must refuse
// the statement: a subquery (a SELECT or WITH keyword), an IN-family
// occurrence whose operand region is not literal-only (OpaqueInRefusedAt), an
// Identifier parameter anywhere, or one of the
// cross-table partition actions (FETCH PARTITION|PART, ATTACH / REPLACE
// PARTITION|PART … FROM, MOVE PARTITION|PART … TO TABLE). ALTER TABLE …
// MODIFY QUERY always carries a SELECT, so it is always refused (spec
// 2026-09-26 R3: the materialized-view body is not rewritten in place). A
// tokenizer failure is reported as ungoverned (fail closed).
func OpaqueTextIsUngoverned(e Engine, text string) bool {
	toks, err := tokenizeRaw(e, text)
	if err != nil {
		return true
	}
	if tokensHoldIdentifierParameter(toks) {
		return true
	}
	// Each top-level action is judged on its own, so a caller may pass the
	// Raw actions of one statement joined with ", " (residual round 5: polyglot
	// splits a Raw action at a comma inside a bracket group, and the joined
	// text makes the group whole again) without one action's shape — an ADD
	// PROJECTION body — changing how another is scanned.
	for _, segment := range opaqueActionSegments(toks) {
		if opaqueProjectionBody(segment) {
			if opaqueProjectionIsUngoverned(segment) {
				return true
			}
			continue
		}
		for i, tok := range segment {
			if OpaqueInRefusedAt(segment, i) {
				return true
			}
			if !opaqueKeyword(tok) {
				continue
			}
			switch strings.ToUpper(tok.Text) {
			case "SELECT":
				return true
			case "WITH":
				if i+1 < len(segment) && strings.EqualFold(segment[i+1].Text, "NAME") {
					continue // ALTER … FREEZE WITH NAME 'x'
				}
				return true
			}
		}
		if opaqueCrossTableAction(segment) {
			return true
		}
	}
	return false
}

// opaqueKeyword reports a token that can be SQL grammar: anything but a string
// literal or a quoted identifier (whose text is data, not syntax).
func opaqueKeyword(tok rawToken) bool {
	return tok.TokenType != "STRING" && tok.TokenType != "QUOTED_IDENTIFIER"
}

func isCallableInName(tok rawToken) bool {
	// A quoted name (`in`, "notIn") calls the same function as the bare
	// spelling: ClickHouse resolves this family case-insensitively either way.
	if tok.TokenType != "VAR" && tok.TokenType != "QUOTED_IDENTIFIER" {
		return false
	}
	_, ok := canonicalCallableInName(strings.ToLower(tok.Text))
	return ok
}

// OpaqueInRefusedAt is the one IN rule every opaque-text scanner applies at
// toks[i] (spec 2026-09-26 R2 / R7, residual round 4: the operand-region
// rule). An IN-family occurrence is the IN keyword (alone or after NOT /
// GLOBAL / GLOBAL NOT, and the keyword-lexed callable `in(`) or an IN-family
// function name (in, notIn, globalIn, globalNotIn, nullIn, notNullIn,
// globalNullIn, globalNotNullIn and their IgnoreSet aliases) as a VAR or
// QUOTED_IDENTIFIER in any case. Its operand region is the bracket group
// ("(" … ")" or "[" … "]") that immediately follows it — the tokenizer has
// already dropped comments and whitespace — or, when no bracket follows, the
// single next token; a `tuple(` / `array(` literal constructor contributes its
// argument group. The occurrence is refused unless every token of the region is
// a NUMBER, STRING, NULL, TRUE, FALSE, comma, sign or bracket. Nothing about
// the token before the occurrence matters: there is no infix / callable
// classification, so `a IN (1, 2)`, `in(42, (1, 2))` and `in(42, [1, 2])`
// pass while `in(a, (1, 2))` and `a IN (1, b)` are refused. A missing or
// unterminated region is refused (fail closed); a region nested in another
// is covered by the outer one. The one exception is the IN PARTITION clause
// keyword (unquoted PARTITION after IN), which ClickHouse never parses as an
// IN operand: measured on 26.8, `a IN partition` is a syntax error in every
// expression position.
func OpaqueInRefusedAt(toks []rawToken, i int) bool {
	tok := toks[i]
	inKeyword := tok.TokenType == "IN"
	if !inKeyword && !isCallableInName(tok) {
		return false
	}
	next := i + 1
	if next >= len(toks) {
		return true
	}
	if inKeyword && opaqueKeyword(toks[next]) && strings.EqualFold(toks[next].Text, "PARTITION") {
		return false // … IN PARTITION p: the partition clause, not an IN operand
	}
	region, ok := opaqueInOperandRegion(toks, next)
	if !ok {
		return true
	}
	for _, t := range region {
		if !opaqueInLiteralToken(t) {
			return true
		}
	}
	return false
}

// opaqueInOperandRegion returns the operand region starting at toks[i]: any
// run of signs, then the bracket group opening after it, the argument group
// of a `tuple(` / `array(` literal constructor, or the single token. ok=false
// for a sign run with nothing after it or an unterminated group.
func opaqueInOperandRegion(toks []rawToken, i int) (region []rawToken, ok bool) {
	// A sign is never a region by itself (residual round 5: ClickHouse drops a
	// unary plus, so `IN +t` reads table t): a maximal run of signs extends to
	// the group or token after it, and the whole run is literal-checked.
	start := i
	for i < len(toks) && (toks[i].TokenType == "PLUS" || toks[i].TokenType == "DASH") {
		i++
	}
	if i >= len(toks) {
		return nil, false
	}
	signs := toks[start:i]
	if toks[i].TokenType == "VAR" && (strings.EqualFold(toks[i].Text, "tuple") || strings.EqualFold(toks[i].Text, "array")) &&
		i+1 < len(toks) && toks[i+1].TokenType == "L_PAREN" {
		i++
	}
	switch toks[i].TokenType {
	case "L_PAREN", "L_BRACKET":
	case "R_PAREN", "R_BRACKET", "COMMA":
		return nil, false // no operand at all (`IN +)`): fail closed
	default:
		return append(append([]rawToken{}, signs...), toks[i]), true
	}
	depth := 0
	for j := i; j < len(toks); j++ {
		switch toks[j].TokenType {
		case "L_PAREN", "L_BRACKET":
			depth++
		case "R_PAREN", "R_BRACKET":
			depth--
			if depth == 0 {
				return append(append([]rawToken{}, signs...), toks[i:j+1]...), true
			}
		}
	}
	return nil, false
}

// opaqueInLiteralToken reports a token an IN operand region may hold: a
// literal, a comma, a sign or a bracket.
func opaqueInLiteralToken(tok rawToken) bool {
	switch tok.TokenType {
	case "NUMBER", "STRING", "NULL", "TRUE", "FALSE", "COMMA", "DASH", "PLUS",
		"L_PAREN", "R_PAREN", "L_BRACKET", "R_BRACKET":
		return true
	}
	return false
}

// opaqueActionSegments splits a token stream into its top-level
// comma-separated ALTER actions (commas inside parentheses or brackets do not
// split). An ALTER … UPDATE tail also splits between its assignments, which
// every per-segment rule tolerates.
func opaqueActionSegments(toks []rawToken) [][]rawToken {
	var out [][]rawToken
	depth, start := 0, 0
	for i, tok := range toks {
		switch tok.TokenType {
		case "L_PAREN", "L_BRACKET":
			depth++
		case "R_PAREN", "R_BRACKET":
			if depth > 0 {
				depth--
			}
		case "COMMA":
			if depth == 0 {
				out = append(out, toks[start:i])
				start = i + 1
			}
		}
	}
	return append(out, toks[start:])
}

// opaqueCrossTableAction matches the partition actions that copy or move data
// between tables or from a Keeper path.
func opaqueCrossTableAction(segment []rawToken) bool {
	word := func(i int, want ...string) bool {
		if i >= len(segment) || !opaqueKeyword(segment[i]) {
			return false
		}
		for _, w := range want {
			if strings.EqualFold(segment[i].Text, w) {
				return true
			}
		}
		return false
	}
	for i := range segment {
		if !word(i+1, "PARTITION", "PART") {
			continue
		}
		switch {
		case word(i, "FETCH"):
			return true
		case word(i, "ATTACH", "REPLACE"):
			for j := i + 2; j < len(segment); j++ {
				if word(j, "FROM") {
					return true
				}
			}
		case word(i, "MOVE"):
			for j := i + 2; j+1 < len(segment); j++ {
				if word(j, "TO") && word(j+1, "TABLE") {
					return true
				}
			}
		}
	}
	return false
}

// opaqueProjectionBody reports an `ADD PROJECTION [IF NOT EXISTS] name (SELECT
// …)` action: ClickHouse's projection grammar has no FROM clause, so its
// SELECT reads only the altered table itself and is not a subquery.
func opaqueProjectionBody(toks []rawToken) bool {
	return len(toks) >= 3 && strings.EqualFold(toks[0].Text, "ADD") && strings.EqualFold(toks[1].Text, "PROJECTION")
}

// opaqueProjectionIsUngoverned applies the subquery / table-operand rules to a
// projection body, admitting exactly one SELECT keyword and no FROM / JOIN.
func opaqueProjectionIsUngoverned(toks []rawToken) bool {
	selects := 0
	for i, tok := range toks {
		if OpaqueInRefusedAt(toks, i) {
			return true
		}
		if !opaqueKeyword(tok) {
			continue
		}
		switch strings.ToUpper(tok.Text) {
		case "SELECT":
			selects++
		case "WITH", "FROM", "JOIN":
			return true
		}
	}
	return selects > 1
}

// CreateViewHasRefresh reports a CREATE [OR REPLACE] [MATERIALIZED] VIEW
// whose header (the depth-0 tokens before its body's first SELECT / WITH)
// carries a REFRESH token anywhere other than the view's own name or the TO
// target's name. The pinned polyglot drops a refreshable view's REFRESH …
// [APPEND] TO clause (and a bare REFRESH TO silently loses its TO target)
// when it regenerates the statement, so dynamic mode refuses it rather than
// forwarding a different statement (spec 2026-09-26 R12). A tokenizer
// failure reports true (fail closed).
func CreateViewHasRefresh(e Engine, sql string) bool {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return true
	}
	word := func(i int, w string) bool {
		return i < len(toks) && opaqueKeyword(toks[i]) && strings.EqualFold(toks[i].Text, w)
	}
	names := map[int]bool{}
	// A name token may lex as a keyword (`refresh` itself does).
	nameTok := func(tok rawToken) bool { return isNameTok(tok.TokenType) || mutationProbeKeywordToken(tok) }
	nameRun := func(i int) int {
		if i < len(toks) && nameTok(toks[i]) {
			names[i] = true
			if i+2 < len(toks) && toks[i+1].TokenType == "DOT" && nameTok(toks[i+2]) {
				names[i+2] = true
				return i + 3
			}
			return i + 1
		}
		return i
	}
	i := 0
	if word(i, "CREATE") {
		i++
	}
	if word(i, "OR") && word(i+1, "REPLACE") {
		i += 2
	}
	if word(i, "MATERIALIZED") {
		i++
	}
	if word(i, "VIEW") {
		i++
	}
	if word(i, "IF") && word(i+1, "NOT") && word(i+2, "EXISTS") {
		i += 3
	}
	i = nameRun(i)
	depth := 0
	for ; i < len(toks); i++ {
		switch toks[i].TokenType {
		case "L_PAREN":
			depth++
			continue
		case "R_PAREN":
			depth--
			continue
		}
		if depth != 0 {
			continue
		}
		if word(i, "SELECT") || word(i, "WITH") {
			return false
		}
		if word(i, "TO") {
			i = nameRun(i+1) - 1
			continue
		}
		if !names[i] && word(i, "REFRESH") {
			return true
		}
	}
	return false
}

// InsertFromInfile reports an INSERT … FROM INFILE statement: the pinned
// polyglot parses it as INSERT … SELECT * FROM INFILE, which would read a
// table named INFILE, so dynamic mode refuses it (spec 2026-09-26 R12). A
// tokenizer failure reports true (fail closed).
func InsertFromInfile(e Engine, sql string) bool {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return true
	}
	return headerHasWords(toks, "FROM", "INFILE")
}

// headerHasWords reports whether the depth-0 tokens before the first SELECT /
// WITH keyword contain words as a consecutive keyword sequence.
func headerHasWords(toks []rawToken, words ...string) bool {
	depth := 0
	for i, tok := range toks {
		switch tok.TokenType {
		case "L_PAREN":
			depth++
			continue
		case "R_PAREN":
			depth--
			continue
		}
		if depth != 0 || !opaqueKeyword(tok) {
			continue
		}
		if strings.EqualFold(tok.Text, "SELECT") || strings.EqualFold(tok.Text, "WITH") {
			return false
		}
		match := i+len(words) <= len(toks)
		for j := 0; match && j < len(words); j++ {
			match = opaqueKeyword(toks[i+j]) && strings.EqualFold(toks[i+j].Text, words[j])
		}
		if match {
			return true
		}
	}
	return false
}

// OpaqueInsertQueryText returns the text of an INSERT whose query polyglot
// could not structure — measured: an INSERT with a column list followed by a
// SETTINGS clause (`INSERT INTO t (a) SETTINGS … SELECT …`) or FORMAT becomes
// {"query":{"command":{"this":"SETTINGS … SELECT …"}}}, so the SELECT is
// never rewritten. ok=false for every other statement.
func OpaqueInsertQueryText(ast AST) (string, bool, error) {
	kind, body, _, err := bodyOf(ast)
	if err != nil || kind != NodeInsert || body == nil {
		return "", false, err
	}
	query, _ := body["query"].(map[string]any)
	command, _ := query["command"].(map[string]any)
	text, ok := command["this"].(string)
	return text, ok, nil
}

// OpaqueInsertQueryIsUngoverned reports whether an opaque INSERT query text
// (OpaqueInsertQueryText) can read a table the rewriter does not see (spec
// 2026-09-26 R5 / R2): a FROM / JOIN / WITH keyword, more than one SELECT (a
// subquery), an IN-family occurrence whose operand region is not
// literal-only (OpaqueInRefusedAt), a lookup-family call, or an
// Identifier parameter. `SETTINGS … SELECT 1`, `SETTINGS … VALUES (…)` and
// `FORMAT …` pass. A tokenizer failure is ungoverned.
func OpaqueInsertQueryIsUngoverned(e Engine, text string) bool {
	toks, err := tokenizeRaw(e, text)
	if err != nil {
		return true
	}
	if tokensHoldIdentifierParameter(toks) {
		return true
	}
	selects := 0
	for i, tok := range toks {
		if OpaqueInRefusedAt(toks, i) {
			return true
		}
		if tok.TokenType != "STRING" && IsStringLookup(tok.Text) && i+1 < len(toks) && toks[i+1].TokenType == "L_PAREN" {
			return true
		}
		if !opaqueKeyword(tok) {
			continue
		}
		switch strings.ToUpper(tok.Text) {
		case "FROM", "JOIN", "WITH":
			return true
		case "SELECT":
			selects++
		}
	}
	return selects > 1
}
