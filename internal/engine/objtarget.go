package engine

import "strings"

// ObjectVerb classifies an EXISTS / SHOW CREATE statement. Both are
// "<verb> [TEMPORARY] [<object-type>] [db.]name" in the ClickHouse grammar and
// both reach us as an opaque `command` node, so a shared tokenize-based
// extractor recovers their structure.
type ObjectVerb int

const (
	VerbNone       ObjectVerb = iota // not an EXISTS / SHOW CREATE statement
	VerbExists                       // EXISTS …
	VerbShowCreate                   // SHOW CREATE …
)

// ObjectTarget is the extracted structure of an EXISTS / SHOW CREATE statement.
type ObjectTarget struct {
	Verb      ObjectVerb
	Temporary bool
	ObjType   string // "TABLE" (default) / "DATABASE" / "VIEW" / "DICTIONARY"
	DB        string // "" when the name was bare
	Table     string
}

// ParseObjectTarget extracts EXISTS / SHOW CREATE structure from the clickhouse
// Tokenize stream. Returns Verb==VerbNone for anything else. EXISTS does not parse
// structurally under ANY polyglot dialect, and bare `SHOW CREATE t` (no TABLE
// keyword) mis-parses under the generic dialect, so the tokenizer is the only
// faithful source for both (verified against the live engine).
//
// Grammar recovered: <verb> [TEMPORARY] [TABLE|DATABASE|VIEW|DICTIONARY] <name-run>
// where <name-run> is `name` or `db DOT name`. A missing object-type keyword
// defaults to TABLE (ClickHouse's `EXISTS t` / `SHOW CREATE t` ≡ … TABLE t).
// Backtick-quoted names lex as QUOTED_IDENTIFIER with the backticks stripped from
// .Text, so DB/Table carry the unquoted identifier (matching the rewrite key).
func ParseObjectTarget(e Engine, sql string) (ObjectTarget, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return ObjectTarget{}, err
	}
	if len(toks) == 0 {
		return ObjectTarget{}, nil
	}
	var out ObjectTarget
	i := 0
	switch strings.ToUpper(toks[0].Text) {
	case "EXISTS":
		out.Verb, i = VerbExists, 1
	case "SHOW":
		if len(toks) < 2 || !strings.EqualFold(toks[1].Text, "CREATE") {
			return ObjectTarget{}, nil // SHOW <other> is a db-level statement, not ours
		}
		out.Verb, i = VerbShowCreate, 2
	default:
		return ObjectTarget{}, nil
	}
	if i < len(toks) && strings.EqualFold(toks[i].Text, "TEMPORARY") {
		out.Temporary = true
		i++
	}
	out.ObjType = "TABLE"
	if i < len(toks) {
		switch strings.ToUpper(toks[i].Text) {
		case "TABLE", "DATABASE", "VIEW", "DICTIONARY":
			out.ObjType = strings.ToUpper(toks[i].Text)
			i++
		}
	}
	// Name-run: `db DOT name` or `name`.
	if i < len(toks) && isNameTok(toks[i].TokenType) {
		if i+2 < len(toks) && toks[i+1].TokenType == "DOT" && isNameTok(toks[i+2].TokenType) {
			out.DB, out.Table = toks[i].Text, toks[i+2].Text
		} else {
			out.Table = toks[i].Text
		}
	}
	return out, nil
}
