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
	Kind                DBLevelKind
	ShowWhat            string // SHOW: "TABLES"/"DATABASES"/"CLUSTERS"/... (uppercased); "" otherwise
	ShowExtended        bool   // SHOW carries the optional EXTENDED prefix (SHOW [EXTENDED] [FULL] COLUMNS ...)
	ShowFull            bool   // SHOW carries the optional FULL prefix
	ShowTemporary       bool   // SHOW carries the optional TEMPORARY prefix
	ShowTable           string // COLUMNS/INDEX family: the table named by the FIRST FROM/IN clause; "" otherwise
	ShowTableResolved   bool   // the COLUMNS/INDEX family table target reduced to a static identifier
	HasTableClause      bool   // the COLUMNS/INDEX family carries an explicit table clause, resolvable or not
	DB                  string // semantic USE db, or SHOW's FROM/IN db; "" when absent
	HasDBClause         bool   // SHOW carries an explicit FROM/IN clause, even when its target is not a static name
	DBResolved          bool   // the explicit SHOW FROM/IN target was resolved to DB
	HasLike             bool
	Like                string // LIKE pattern (logical/unescaped: 'O''Brien%' → O'Brien%)
	LikeNot             bool   // NOT (I)LIKE
	LikeCaseInsensitive bool   // ILIKE
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
