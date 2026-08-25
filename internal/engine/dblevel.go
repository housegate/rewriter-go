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
	ShowFull            bool   // SHOW carries the optional FULL prefix
	ShowTemporary       bool   // SHOW carries the optional TEMPORARY prefix
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
		if i < len(toks) && (toks[i].TokenType == "FROM" || toks[i].TokenType == "IN") {
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
