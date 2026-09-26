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
// the statement: a subquery (a SELECT or WITH keyword), an IN / NOT IN /
// GLOBAL IN / GLOBAL NOT IN operand or callable IN-family argument that is an
// identifier, a quoted identifier, a parameter, or a parenthesis opening one
// of those (a table operand), an Identifier parameter anywhere, or one of the
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
	if opaqueProjectionBody(toks) {
		return opaqueProjectionIsUngoverned(toks)
	}
	for i, tok := range toks {
		if OpaqueInTableAt(toks, i) {
			return true
		}
		if !opaqueKeyword(tok) {
			continue
		}
		switch strings.ToUpper(tok.Text) {
		case "SELECT":
			return true
		case "WITH":
			if i+1 < len(toks) && strings.EqualFold(toks[i+1].Text, "NAME") {
				continue // ALTER … FREEZE WITH NAME 'x'
			}
			return true
		}
	}
	for _, segment := range opaqueActionSegments(toks) {
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

// OpaqueInTableAt is the one IN rule every opaque-text scanner applies at
// toks[i] (spec 2026-09-26 R2 / R7, residual round 3). It covers every
// spelling:
//
//   - the infix keyword IN (NOT IN / GLOBAL [NOT] IN end in the same token)
//     whose operand is an identifier, quoted identifier, parameter, or any
//     number of opening parentheses followed by one of those;
//   - the callable form — the IN keyword immediately followed by "(" (`in(`,
//     `IN(`: bare `in` lexes as the keyword) as well as every IN-family
//     function name (in, notIn, globalIn, globalNotIn, nullIn, notNullIn,
//     globalNullIn, globalNotNullIn and their IgnoreSet aliases) as a VAR or
//     QUOTED_IDENTIFIER in any case — whose SECOND top-level argument is such
//     an operand. The tokenizer has already dropped comments and whitespace.
//
// An IN keyword followed by "(" is the callable form when it starts an
// expression (the previous token does not end an operand) and the infix form
// otherwise, so `a IN (1, 2)` and `in(a, (1, 2))` both pass. A subquery
// operand counts as a table read. An unterminated call is a table operand
// (fail closed).
func OpaqueInTableAt(toks []rawToken, i int) bool {
	tok := toks[i]
	inKeyword := tok.TokenType != "STRING" && tok.TokenType != "QUOTED_IDENTIFIER" && tok.TokenType != "VAR" &&
		strings.EqualFold(tok.Text, "IN")
	if inKeyword && !(i+1 < len(toks) && toks[i+1].TokenType == "L_PAREN" && !opaqueOperandEndsBefore(toks, i)) {
		// Infix IN: its right operand.
		return opaqueInOperandIsTable(toks, i+1)
	}
	if !(inKeyword || isCallableInName(tok)) || i+1 >= len(toks) || toks[i+1].TokenType != "L_PAREN" {
		return false
	}
	groups, ok := rawCallArgGroups(toks, i+1)
	if !ok {
		return true
	}
	return len(groups) >= 2 && opaqueInOperandIsTable(groups[1], 0)
}

// opaqueOperandEndsBefore reports whether the token before toks[i] ends an
// operand — so an IN keyword at i is the infix operator, not a call: an
// identifier, literal, closing bracket, or the NOT / GLOBAL of NOT IN /
// GLOBAL IN.
func opaqueOperandEndsBefore(toks []rawToken, i int) bool {
	if i == 0 {
		return false
	}
	prev := toks[i-1]
	switch prev.TokenType {
	case "VAR", "QUOTED_IDENTIFIER", "NUMBER", "STRING", "R_PAREN", "R_BRACKET", "R_BRACE":
		return true
	}
	switch strings.ToUpper(prev.Text) {
	case "NOT", "GLOBAL", "NULL", "TRUE", "FALSE":
		return true
	}
	return false
}

// opaqueInOperandIsTable reports whether the tokens starting at i form a table
// operand: a (possibly qualified) identifier that is not itself a function
// call, a brace parameter, or any number of opening parentheses followed by one
// of those.
func opaqueInOperandIsTable(toks []rawToken, i int) bool {
	for i < len(toks) && toks[i].TokenType == "L_PAREN" {
		i++
	}
	if i >= len(toks) {
		return false
	}
	if opaqueKeyword(toks[i]) && (strings.EqualFold(toks[i].Text, "SELECT") || strings.EqualFold(toks[i].Text, "WITH")) {
		return true // a subquery operand reads a table
	}
	switch {
	case toks[i].TokenType == "L_BRACE":
		return true
	case isNameTok(toks[i].TokenType):
		if strings.EqualFold(toks[i].Text, "PARTITION") && toks[i].TokenType == "VAR" {
			return false // … IN PARTITION p
		}
		if i+1 < len(toks) && toks[i+1].TokenType == "L_PAREN" {
			return false // a function call is a value
		}
		return true
	default:
		return false
	}
}

// opaqueActionSegments splits a token stream into its top-level
// comma-separated ALTER actions.
func opaqueActionSegments(toks []rawToken) [][]rawToken {
	var out [][]rawToken
	depth, start := 0, 0
	for i, tok := range toks {
		switch tok.TokenType {
		case "L_PAREN":
			depth++
		case "R_PAREN":
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
		if OpaqueInTableAt(toks, i) {
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
// subquery), a table-operand IN or callable IN, a lookup-family call, or an
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
		if OpaqueInTableAt(toks, i) {
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
