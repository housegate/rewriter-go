package engine

import "strings"

// ObjectVerb classifies an EXISTS / SHOW CREATE / DESCRIBE statement. They are
// "<verb> [TEMPORARY] [<object-type>] [db.]name" in the ClickHouse grammar and
// all reach us as an opaque `command` node, so a shared tokenize-based
// extractor recovers their structure.
type ObjectVerb int

const (
	VerbNone       ObjectVerb = iota // not an EXISTS / SHOW CREATE / DESCRIBE statement
	VerbExists                       // EXISTS …
	VerbShowCreate                   // SHOW CREATE …
	VerbDescribe                     // DESCRIBE | DESC [TABLE] …
)

// ObjectTarget is the extracted structure of an EXISTS / SHOW CREATE / DESCRIBE statement.
type ObjectTarget struct {
	Verb      ObjectVerb
	Temporary bool
	ObjType   string // "TABLE" (default) / "DATABASE" / "VIEW" / "DICTIONARY"
	DB        string // "" when the name was bare
	Table     string
}

// ParseObjectTarget extracts EXISTS / SHOW CREATE / DESCRIBE structure from the clickhouse
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
	case "DESCRIBE", "DESC":
		out.Verb, i = VerbDescribe, 1
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

// parseObjectTargetFunctionCallFromTokens detects an EXISTS / SHOW CREATE /
// DESCRIBE statement whose target is a function call rather than a plain
// [db.]name (spec 2026-09-26 T5, Task 7 fix round 1 finding 3).
// ParseObjectTarget's own name-run extraction stops at the name token and
// silently drops a following "(...)", so e.g. `DESCRIBE TABLE mysql('h',
// 'default', 'u', 'x', 'y')` reports Table="mysql" and (having matched none
// of RewriteDescribe's SI/reject conditions) passes the whole statement
// through unchanged: neither the T5 table-function allowlist nor the
// protected-database check ever sees it. ok=false for anything that is not
// this exact shape (a bare name, one of the three verbs not present at all,
// or an unterminated call).
//
// Takes an already-tokenized stream — the caller (CollectCommandTextFindings,
// Task 7 fix round 2) tokenizes the command text exactly once and shares it
// with the raw-text string-lookup scan, rather than this function tokenizing
// again on its own: doing so unconditionally (as an earlier revision did, via
// a cheap keyword-prefix pre-check on the raw text before tokenizing) both
// duplicated work and was defeated by a leading comment (fix round 2 finding
// 3 — the pre-check ran on the ORIGINAL text, comment included, before any
// tokenizing that would have stripped it).
//
// argDatabases lists, for each top-level call argument that decodes as a
// single string literal or a (possibly db.name-qualified) identifier, the
// database half a protected-database check should consult: the whole
// literal/identifier text when it names no '.', or the text before the
// first '.' when it does (mirroring stringLookupDatabase's own convention).
// An argument of any other shape (nested call, number, expression, ...)
// contributes nothing — it is not a namespace-bearing candidate this policy
// classifies.
func parseObjectTargetFunctionCallFromTokens(toks []rawToken) (verb ObjectVerb, name string, argDatabases []string, ok bool) {
	if len(toks) == 0 {
		return VerbNone, "", nil, false
	}
	i := 0
	switch strings.ToUpper(toks[0].Text) {
	case "EXISTS":
		verb, i = VerbExists, 1
	case "SHOW":
		if len(toks) < 2 || !strings.EqualFold(toks[1].Text, "CREATE") {
			return VerbNone, "", nil, false
		}
		verb, i = VerbShowCreate, 2
	case "DESCRIBE", "DESC":
		verb, i = VerbDescribe, 1
	default:
		return VerbNone, "", nil, false
	}
	if i < len(toks) && strings.EqualFold(toks[i].Text, "TEMPORARY") {
		i++
	}
	if i < len(toks) {
		switch strings.ToUpper(toks[i].Text) {
		case "TABLE", "DATABASE", "VIEW", "DICTIONARY":
			i++
		}
	}
	// The function-name position accepts any identifier-shaped token TEXT,
	// not just isNameTok's VAR/QUOTED_IDENTIFIER: several of the exact names
	// T5 must classify here are lexer keywords in this tokenizer, not plain
	// VAR tokens — measured directly, "merge" (used by both a table function
	// and a table engine) tokenizes as token_type "MERGE", so `DESCRIBE TABLE
	// merge('hg_safe', 'db1__t')` was silently falling through unclassified
	// before this check was widened.
	if i >= len(toks) || !looksLikeBareIdentifierText(toks[i].Text) {
		return VerbNone, "", nil, false
	}
	name = toks[i].Text
	i++
	if i >= len(toks) || toks[i].TokenType != "L_PAREN" {
		return VerbNone, "", nil, false // a plain [db.]name target, not a function call
	}
	argDatabases, ok = objectTargetCallArgDatabases(toks, i)
	if !ok {
		return VerbNone, "", nil, false
	}
	return verb, name, argDatabases, true
}

// looksLikeBareIdentifierText reports whether s is shaped like an unquoted
// SQL identifier — a letter or underscore, then letters/digits/underscores —
// regardless of which token type the tokenizer assigned it. Some table
// function/engine names this policy must classify (e.g. "merge") are lexer
// keywords here, not plain VAR tokens; isNameTok's TokenType check misses
// them, but their Text is still an ordinary identifier spelling.
func looksLikeBareIdentifierText(s string) bool {
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			continue
		case r >= '0' && r <= '9' && i > 0:
			continue
		default:
			return false
		}
	}
	return s != ""
}

// objectTargetCallArgDatabases scans the token stream from a call's opening
// "(" (at toks[openIdx]) to its matching ")", splitting on top-level commas
// (paren depth 0 relative to the call) and reporting each argument's
// database-check candidate per ParseObjectTargetFunctionCall's doc comment.
// ok=false means the call never closes (malformed input).
func objectTargetCallArgDatabases(toks []rawToken, openIdx int) (out []string, ok bool) {
	depth := 0
	groupStart := openIdx + 1
	addGroup := func(group []rawToken) {
		switch {
		case len(group) == 1 && group[0].TokenType == "STRING":
			out = append(out, firstDotSegment(group[0].Text))
		case len(group) == 1 && isNameTok(group[0].TokenType):
			out = append(out, group[0].Text)
		case len(group) == 3 && isNameTok(group[0].TokenType) && group[1].TokenType == "DOT" && isNameTok(group[2].TokenType):
			out = append(out, group[0].Text)
		}
	}
	for i := openIdx + 1; i < len(toks); i++ {
		switch toks[i].TokenType {
		case "L_PAREN":
			depth++
		case "R_PAREN":
			if depth == 0 {
				addGroup(toks[groupStart:i])
				return out, true
			}
			depth--
		case "COMMA":
			if depth == 0 {
				addGroup(toks[groupStart:i])
				groupStart = i + 1
			}
		}
	}
	return nil, false
}

// firstDotSegment returns s up to (not including) its first '.', or s
// unchanged when it names no '.'.
func firstDotSegment(s string) string {
	if idx := strings.IndexByte(s, '.'); idx >= 0 {
		return s[:idx]
	}
	return s
}

// IsSessionSettingAssignment reports whether text (the raw SQL of a
// `command` node) is a session settings assignment: SET <name> = … (spec
// 2026-09-26 T7, Task 7 fix round 1 finding 4). Tokenized rather than matched
// against a literal "SET " prefix so any whitespace between SET and the
// setting name qualifies — a tab, not just one ASCII space. SET ROLE … and
// SET DEFAULT ROLE … TO … are access-management statements this repo does
// not model, not settings assignments, and must not qualify even though they
// share the SET keyword: the token right after SET must be an identifier
// that is neither ROLE nor DEFAULT, and the token after THAT must be "=".
func IsSessionSettingAssignment(e Engine, text string) bool {
	toks, err := tokenizeRaw(e, text)
	if err != nil || len(toks) < 3 {
		return false
	}
	if !strings.EqualFold(toks[0].Text, "SET") {
		return false
	}
	if !isNameTok(toks[1].TokenType) {
		return false
	}
	if strings.EqualFold(toks[1].Text, "ROLE") || strings.EqualFold(toks[1].Text, "DEFAULT") {
		return false
	}
	return toks[2].TokenType == "EQ" || toks[2].Text == "="
}
