package engine

import (
	"encoding/json"
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
	DB                  string // USE db, or SHOW's FROM/IN db; "" when absent
	HasLike             bool
	Like                string // LIKE pattern (logical/unescaped: 'O''Brien%' → O'Brien%)
	LikeNot             bool   // NOT (I)LIKE
	LikeCaseInsensitive bool   // ILIKE
}

type dbToken struct {
	TokenType string `json:"token_type"`
	Text      string `json:"text"`
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
	toksAST, err := e.Tokenize(sql)
	if err != nil {
		return DBLevelInfo{}, err
	}
	var toks []dbToken
	if err := json.Unmarshal(toksAST, &toks); err != nil {
		return DBLevelInfo{}, fmt.Errorf("engine: decode tokens: %w", err)
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
		if i < len(toks) && isNameToken(toks[i].TokenType) {
			info.ShowWhat = strings.ToUpper(toks[i].Text)
			i++
		}
		for i < len(toks) {
			tt := toks[i].TokenType
			switch {
			case tt == "FROM" || tt == "IN":
				if i+1 < len(toks) && isNameToken(toks[i+1].TokenType) {
					info.DB = toks[i+1].Text
					i += 2
					continue
				}
			case tt == "NOT":
				info.LikeNot = true
			case tt == "LIKE" || tt == "I_LIKE":
				info.HasLike = true
				info.LikeCaseInsensitive = tt == "I_LIKE"
				if i+1 < len(toks) && toks[i+1].TokenType == "STRING" {
					info.Like = toks[i+1].Text
					i += 2
					continue
				}
			}
			i++
		}
		return info, nil
	default:
		return DBLevelInfo{Kind: DBNone}, nil
	}
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
