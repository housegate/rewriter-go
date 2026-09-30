package engine

import (
	"fmt"
	"strings"
)

// DBLevelKind classifies a database-level statement.
type DBLevelKind int

const (
	DBNone DBLevelKind = iota // not a USE/SHOW statement
	DBUse                     // USE <db>
	DBShow                    // SHOW <what> [FROM/IN <db>] [[NOT] (I)LIKE '<pat>']
)

// DBLevelInfo is the extracted structure of a USE/SHOW statement.
type DBLevelInfo struct {
	Kind              DBLevelKind
	ShowWhat          string // SHOW: "TABLES"/"DATABASES"/"CLUSTERS"/... (uppercased); "" otherwise
	ShowWhatNext      string // SHOW: the token after ShowWhat, uppercased (ROLES in SHOW CURRENT ROLES); "" when none
	ShowExtended      bool   // SHOW carries the optional EXTENDED prefix (SHOW [EXTENDED] [FULL] COLUMNS ...)
	ShowFull          bool   // SHOW carries the optional FULL prefix
	ShowTemporary     bool   // SHOW carries the optional TEMPORARY prefix
	ShowTable         string // COLUMNS/INDEX family: the table named by the FIRST FROM/IN clause; "" otherwise
	ShowTableResolved bool   // the COLUMNS/INDEX family table target reduced to a static identifier
	HasTableClause    bool   // the COLUMNS/INDEX family carries an explicit table clause, resolvable or not
	DB                string // semantic USE db, or SHOW's FROM/IN db; "" when absent
	HasDBClause       bool   // SHOW carries an explicit FROM/IN clause, even when its target is not a static name
	DBResolved        bool   // the explicit SHOW FROM/IN target was resolved to DB
	HasLike           bool
	Like              string // LIKE pattern (logical/unescaped: 'O''Brien%' → O'Brien%)
	// LikeRaw is the pattern's source lexeme, quotes and escapes verbatim
	// (`'d\_%'`, `'O''Brien%'`). The decoded Like loses `\_` and `\%`, which
	// ClickHouse reads as a literal underscore / percent, so a handler that
	// re-emits the pattern must carry LikeRaw. "" when no string follows.
	LikeRaw             string
	LikeNot             bool // NOT (I)LIKE
	LikeCaseInsensitive bool // ILIKE
	// Trailing reports a token after the modelled head — `USE <db>`, or
	// `SHOW [EXTENDED] [FULL] [TEMPORARY] <kind> [{FROM|IN} <db>]` followed at
	// most by one `[NOT] (I)LIKE '<pattern>'` — other than a closing
	// semicolon: WHERE, LIMIT, FORMAT, SETTINGS, INTO OUTFILE or junk. A
	// handler that synthesizes SQL from these fields would drop it.
	Trailing bool
	// ShowTableMultiPart reports a COLUMNS/INDEX family table clause of three
	// or more dotted parts. ClickHouse reads it as DB = the first part and
	// ShowTable = the last part, which is what DB / ShowTable then hold.
	ShowTableMultiPart bool
}

// ParseDBLevel extracts USE/SHOW structure from the clickhouse Tokenize stream.
// Returns Kind==DBNone for anything that isn't a leading USE/SHOW. Robust to the
// forms polyglot's generic parser rejects (NOT LIKE / NOT ILIKE / IN).
//
// STRING-token quoting: the clickhouse tokenizer UNESCAPES the LIKE pattern.
// A pattern written with a doubled single quote (LIKE 'O'+'+'Brien%') yields a
// STRING token whose text is the logical value O'Brien% — the doubled quote is
// collapsed and the surrounding quotes are stripped (verified against the live
// engine). We therefore store the LIKE pattern as-is (logical value); the
// handlers re-escape it when emitting synthetic SQL.
func ParseDBLevel(e Engine, sql string) (DBLevelInfo, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return DBLevelInfo{}, err
	}
	if len(toks) == 0 {
		return DBLevelInfo{}, nil
	}
	head := strings.ToUpper(toks[0].Text)

	switch head {
	case "USE":
		info := DBLevelInfo{Kind: DBUse}
		if len(toks) >= 2 && isNameToken(toks[1].TokenType) {
			info.DB = toks[1].Text
		}
		info.Trailing = tokensRemain(toks, 2)
		return info, nil
	case "SHOW":
		info := DBLevelInfo{Kind: DBShow}
		i := 1
		// ClickHouse permits these SHOW prefixes only in this order. Keep their
		// presence so policy can identify the real kind/target while exact
		// pass-through paths preserve their server-side presentation semantics.
		if i < len(toks) && isUnquotedDBKeyword(toks[i], "EXTENDED") {
			info.ShowExtended = true
			i++
		}
		if i < len(toks) && isUnquotedDBKeyword(toks[i], "FULL") {
			info.ShowFull = true
			i++
		}
		if i < len(toks) && isUnquotedDBKeyword(toks[i], "TEMPORARY") {
			info.ShowTemporary = true
			i++
		}
		if i < len(toks) {
			// The word after the optional prefixes is the kind discriminator.
			// Depending on the word it lexes as a dedicated keyword
			// (CREATE/CLUSTER/SETTINGS/TABLE) or as VAR
			// (TABLES/DATABASES/GRANTS/DICTIONARIES/...), so capture it
			// regardless of token type. This lets the handler distinguish SHOW CREATE
			// (a separate ClickHouse AST → not SHOW_TABLES) from the
			// ASTShowTablesQuery family (TABLES/CLUSTER/SETTINGS/...).
			info.ShowWhat = strings.ToUpper(toks[i].Text)
			i++
			if i < len(toks) {
				info.ShowWhatNext = strings.ToUpper(toks[i].Text)
			}
		}
		// FROM/IN is a database clause only in this bounded grammar prefix,
		// immediately after the SHOW kind. Never keep scanning for IN: later IN
		// tokens can belong to a WHERE predicate and must not overwrite the real
		// execution database.
		if isShowTableTargetKind(info.ShowWhat) {
			// ClickHouse: SHOW [EXTENDED] [FULL] COLUMNS {FROM|IN} <table>
			// [{FROM|IN} <database>], and the same shape for INDEX / INDEXES /
			// KEYS. The FIRST clause is the table; the database is either the
			// optional SECOND clause or the qualifier of a `<database>.<table>`
			// first clause. Binding the table into DB -- what this parser did for
			// every SHOW kind -- is what let this family address hg_safe.
			i = parseShowTableThenDatabase(e, sql, toks, i, &info)
		} else if i < len(toks) && (toks[i].TokenType == "FROM" || toks[i].TokenType == "IN") {
			info.HasDBClause = true
			i++
			if i < len(toks) {
				// The SHOW grammar proves that this token is in database-object
				// position. Let the parser decide whether its exact source span is
				// an identifier: ClickHouse admits dedicated keyword tokens and
				// leading-digit bare names here, while an Identifier parameter must
				// remain explicitly unresolved for SI fail-closed policy.
				name, ok := parsedIdentifierAt(e, sql, toks[i])
				if ok && name != "" {
					info.DB = name
					info.DBResolved = true
					i++
				}
			}
		}
		info.Trailing = tokensRemain(toks, afterLikeClause(toks, i))
		for i < len(toks) {
			tt := toks[i].TokenType
			switch {
			case tt == "NOT":
				info.LikeNot = true
			case tt == "LIKE" || tt == "I_LIKE":
				info.HasLike = true
				info.LikeCaseInsensitive = tt == "I_LIKE"
				if i+1 < len(toks) && toks[i+1].TokenType == "STRING" {
					info.Like = toks[i+1].Text
					if s, en := toks[i+1].Span.Start, toks[i+1].Span.End; 0 <= s && s < en && en <= len(sql) {
						info.LikeRaw = sql[s:en]
					}
				}
				return info, nil
			case tt == "WHERE" || tt == "LIMIT" || tt == "SETTINGS" || tt == "FORMAT" ||
				tt == "INTO" || tt == "PARALLEL":
				return info, nil
			}
			i++
		}
		return info, nil
	default:
		return DBLevelInfo{Kind: DBNone}, nil
	}
}

// isShowTableTargetKind reports the ClickHouse SHOW variants whose first
// FROM/IN clause names a TABLE, with the database carried by an optional
// second clause. Every other SHOW kind's single clause names a database.
// FIELDS and INDICES are ClickHouse's synonyms for COLUMNS and INDEX; they
// parse identically here, and omitting them left the table bound as the
// database while the C++ engine gated them as the pair.
func isShowTableTargetKind(kind string) bool {
	switch kind {
	case "COLUMNS", "FIELDS", "INDEX", "INDEXES", "INDICES", "KEYS":
		return true
	default:
		return false
	}
}

// isShowTailToken reports the keywords that end the SHOW target grammar. The
// set mirrors ParseDBLevel's shared tail loop so a database clause is never
// searched for past the point where ClickHouse stops accepting one.
func isShowTailToken(tokenType string) bool {
	switch tokenType {
	case "NOT", "LIKE", "I_LIKE", "WHERE", "LIMIT", "SETTINGS", "FORMAT", "INTO", "PARALLEL":
		return true
	default:
		return false
	}
}

// parseShowTableThenDatabase consumes `{FROM|IN} <table> [{FROM|IN} <database>]`
// for the COLUMNS/INDEX family and returns the index at which the shared
// LIKE/WHERE/LIMIT tail resumes. Identifier authority stays with the parser:
// every name goes through parsedIdentifierAt, so keyword-spelled and
// leading-digit names behave exactly as they do in the database-only families.
func parseShowTableThenDatabase(e Engine, sql string, toks []rawToken, i int, info *DBLevelInfo) int {
	if i >= len(toks) || (toks[i].TokenType != "FROM" && toks[i].TokenType != "IN") {
		return i
	}
	info.HasTableClause = true
	i++
	tableConsumed := false
	if i < len(toks) {
		if name, ok := parsedIdentifierAt(e, sql, toks[i]); ok && name != "" {
			if i+2 < len(toks) && toks[i+1].TokenType == "DOT" {
				if table, ok := parsedIdentifierAt(e, sql, toks[i+2]); ok && table != "" {
					info.DB, info.DBResolved, info.HasDBClause = name, true, true
					info.ShowTable, info.ShowTableResolved = table, true
					i += 3
					tableConsumed = true
					// ClickHouse's SHOW COLUMNS / INDEX parsers read a compound
					// target of three or more parts as name_parts[0] (the
					// database) and shortName(), the LAST part (the table):
					// measured on 26.2 and 25.8, `SHOW COLUMNS FROM
					// system.tables.processes` lists system.processes. Model that
					// reading and flag it, so the handler can refuse it.
					for i+1 < len(toks) && toks[i].TokenType == "DOT" {
						info.ShowTableMultiPart = true
						part, ok := parsedIdentifierAt(e, sql, toks[i+1])
						if !ok || part == "" {
							info.ShowTableResolved = false
							break
						}
						info.ShowTable = part
						i += 2
					}
				}
			} else {
				info.ShowTable, info.ShowTableResolved = name, true
				i++
				tableConsumed = true
			}
		}
	}
	if !tableConsumed {
		// A target the parser cannot reduce to a name can still span several
		// tokens (a query parameter lexes as `{ name : Type }`). Skip them to
		// find the optional database clause, but never past the tail keywords,
		// so an IN inside a predicate can never be mistaken for that clause.
		for i < len(toks) && !isShowTailToken(toks[i].TokenType) &&
			toks[i].TokenType != "FROM" && toks[i].TokenType != "IN" {
			i++
		}
	}
	if i >= len(toks) || (toks[i].TokenType != "FROM" && toks[i].TokenType != "IN") {
		return i
	}
	// An explicit database clause is authoritative over a qualifier carried by
	// the table clause, and an explicit-but-unresolvable one must not leave that
	// qualifier standing: ClickHouse executes against the clause, so policy has
	// to see an unresolved target rather than a stale resolved one.
	info.HasDBClause = true
	info.DB, info.DBResolved = "", false
	i++
	if i < len(toks) {
		if name, ok := parsedIdentifierAt(e, sql, toks[i]); ok && name != "" {
			info.DB = name
			info.DBResolved = true
			i++
		}
	}
	return i
}

func isUnquotedDBKeyword(tok rawToken, keyword string) bool {
	return tok.TokenType != "QUOTED_IDENTIFIER" && tok.TokenType != "STRING" &&
		strings.EqualFold(tok.Text, keyword)
}

// isNameToken reports whether a token type names an identifier (a db/table/
// show-kind word). The clickhouse tokenizer emits VAR for bare identifiers and
// QUOTED_IDENTIFIER for backtick-quoted ones; IDENTIFIER is included defensively
// for parity with the other dialects' tokenizers.
func isNameToken(tt string) bool {
	return tt == "VAR" || tt == "QUOTED_IDENTIFIER" || tt == "IDENTIFIER"
}

// DatabaseTarget reads the db name + IF [NOT] EXISTS flags of a create_database /
// drop_database node. Errors if the AST is neither.
func DatabaseTarget(ast AST) (db string, ifNotExists, ifExists bool, err error) {
	kind, body, _, err := bodyOf(ast)
	if err != nil {
		return "", false, false, err
	}
	if body == nil || (kind != NodeCreateDB && kind != NodeDropDB) {
		return "", false, false, fmt.Errorf("engine: not a create/drop database node (%q)", kind)
	}
	db = identName(body["name"])
	ifNotExists, _ = body["if_not_exists"].(bool)
	ifExists, _ = body["if_exists"].(bool)
	return db, ifNotExists, ifExists, nil
}

// SpliceShowTable replaces the table name of a SHOW COLUMNS / INDEX family
// statement's first FROM / IN clause — which must be a single unqualified
// name token — with replacement (already quoted), leaving every other byte of
// sql unchanged (spec 2026-09-26 R7).
func SpliceShowTable(e Engine, sql, replacement string) (string, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return "", err
	}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].TokenType != "FROM" && toks[i].TokenType != "IN" {
			continue
		}
		name := toks[i+1]
		if !isNameTok(name.TokenType) || (i+2 < len(toks) && toks[i+2].TokenType == "DOT") {
			return "", fmt.Errorf("engine: SHOW table clause is not a single unqualified name")
		}
		return sql[:name.Span.Start] + replacement + sql[name.Span.End:], nil
	}
	return "", fmt.Errorf("engine: SHOW statement has no table clause")
}

// showForwardedVerbatim reports the SHOW kinds the dispatcher forwards as
// written (every kind except TABLES and DATABASES, which are rewritten into a
// synthetic SELECT, and CREATE, which has its own handler): the COLUMNS /
// INDEX family, DICTIONARIES, CLUSTERS, MERGES, SETTINGS, PROCESSLIST, … and
// any kind the dispatcher does not recognise.
func showForwardedVerbatim(info DBLevelInfo) bool {
	if info.Kind != DBShow {
		return false
	}
	switch info.ShowWhat {
	case "TABLES", "DATABASES", "CREATE":
		return false
	}
	return true
}

// showBodyStart returns the index of a verbatim-forwarded SHOW statement's
// first top-level trailing clause keyword (WHERE, LIKE, ILIKE, LIMIT, OFFSET,
// SETTINGS, FORMAT, INTO) after the SHOW kind, or len(toks) when it has none.
func showBodyStart(toks []rawToken) int {
	depth := 0
	for i := 2; i < len(toks); i++ {
		switch toks[i].TokenType {
		case "L_PAREN":
			depth++
			continue
		case "R_PAREN":
			depth--
			continue
		}
		if depth != 0 || !opaqueKeyword(toks[i]) {
			continue
		}
		switch strings.ToUpper(toks[i].Text) {
		case "WHERE", "LIKE", "ILIKE", "LIMIT", "OFFSET", "SETTINGS", "FORMAT", "INTO":
			return i
		}
	}
	return len(toks)
}

// ShowBodyIsUngoverned reports whether a verbatim-forwarded SHOW statement
// carries SQL the rewriter does not rewrite (spec 2026-09-26 R7): in its
// trailing clauses after the SHOW target (WHERE / LIKE / ILIKE / LIMIT / …),
// a SELECT or WITH keyword, a name followed by "(" (a function call — table
// functions and lookups included; a keyword-tokenized call such as IF( is not
// a name), an IN-family occurrence whose operand region is not literal-only
// (OpaqueInRefusedAt), an Identifier parameter or a quoted dotted
// name. The target itself (a database may be named `select`) is not scanned. Literals, plain identifiers and operators pass (`LIMIT 5`). A
// tokenizer failure is ungoverned.
func ShowBodyIsUngoverned(e Engine, info DBLevelInfo, sql string) bool {
	if !showForwardedVerbatim(info) {
		return false
	}
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return true
	}
	for i := showBodyStart(toks); i < len(toks); i++ {
		tok := toks[i]
		if isNameTok(tok.TokenType) && i+1 < len(toks) && toks[i+1].TokenType == "L_PAREN" {
			return true
		}
		if opaqueKeyword(tok) && (strings.EqualFold(tok.Text, "SELECT") || strings.EqualFold(tok.Text, "WITH")) {
			return true
		}
		if tok.TokenType == "QUOTED_IDENTIFIER" && strings.Contains(tok.Text, ".") {
			return true
		}
		if tok.TokenType == "L_BRACE" {
			return true
		}
		if OpaqueInRefusedAt(toks, i) {
			return true
		}
	}
	return false
}

// ShowBodyQualifiedNames returns every `db.table` run in a verbatim-forwarded
// SHOW statement's trailing clauses (the target itself is collected from
// ParseDBLevel).
func ShowBodyQualifiedNames(e Engine, info DBLevelInfo, sql string) []TableTarget {
	if !showForwardedVerbatim(info) {
		return nil
	}
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return nil
	}
	return qualifiedNameRuns(toks, showBodyStart(toks))
}

// ShowBodyDatabases returns the qualifier of every `name.name` run in a
// verbatim-forwarded SHOW statement's trailing clauses, for the T3 protected
// check.
func ShowBodyDatabases(e Engine, info DBLevelInfo, sql string) []string {
	var dbs []string
	for _, tt := range ShowBodyQualifiedNames(e, info, sql) {
		dbs = append(dbs, tt.DB)
	}
	return dbs
}

// tokensRemain reports a token other than a semicolon at or after toks[from].
func tokensRemain(toks []rawToken, from int) bool {
	for i := from; i < len(toks); i++ {
		if toks[i].TokenType != "SEMICOLON" {
			return true
		}
	}
	return false
}

// afterLikeClause returns the index after one `[NOT] (I)LIKE '<pattern>'`
// clause starting at toks[i], or i when none starts there.
func afterLikeClause(toks []rawToken, i int) int {
	j := i
	if j < len(toks) && toks[j].TokenType == "NOT" {
		j++
	}
	if j+1 < len(toks) && (toks[j].TokenType == "LIKE" || toks[j].TokenType == "I_LIKE") && toks[j+1].TokenType == "STRING" {
		return j + 2
	}
	return i
}
