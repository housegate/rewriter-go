package engine

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// NameRefKind identifies the grammar role in which a name was found. A bare
// table and a database are intentionally distinct: treating every identifier
// as both roles would turn columns, aliases, cluster names, and settings into
// storage-integrity false positives.
type NameRefKind uint8

const (
	NameRefTable NameRefKind = iota + 1
	NameRefDatabase
)

// NameRef is one syntactically proven table or database target. Database
// references carry DB only; table references carry Table and optionally DB.
type NameRef struct {
	Kind  NameRefKind
	DB    string
	Table string
}

// NameRefs returns only object references proven by the grammar of the opaque
// Spec-I D2 statement families. It is not a general identifier scanner.
//
// CREATE LIVE VIEW is the sole family here with an embedded read query. Its
// SELECT is parsed independently and handed to the existing AST collectors, so
// CTE aliases, table aliases, expressions, and table functions keep the same
// semantics as ordinary SELECT rewriting.
func NameRefs(e Engine, sql string) ([]NameRef, error) {
	ast, err := e.ParseOne(sql)
	if err != nil {
		return nil, err
	}
	return NameRefsFromAST(e, ast, sql)
}

// NameRefsFromAST is NameRefs with the caller's already-parsed statement. D2's
// shared finalize path uses it so structured embedded SELECTs are never parsed
// a second time; opaque command grammar still comes from the tokenizer below.
func NameRefsFromAST(e Engine, ast AST, sql string) ([]NameRef, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, nil
	}

	var refs []NameRef
	switch {
	case keywordAt(toks, 0, "SYSTEM"):
		refs = systemNameRefs(e, sql, toks)
	case keywordsAt(toks, 0, "CHECK", "ALL", "TABLES"):
		// CHECK ALL TABLES has no user-selected table target.
	case keywordsAt(toks, 0, "CHECK", "TABLE"):
		if ref, after, ok := tableRefAt(e, sql, toks, 2, false); ok &&
			checkTableTailValid(e, sql, toks, after) {
			refs = append(refs, ref)
		}
	case keywordsAt(toks, 0, "TRUNCATE", "DATABASE"):
		refs = truncateDatabaseNameRefs(e, sql, toks)
	case keywordsAt(toks, 0, "TRUNCATE", "ALL", "TABLES", "FROM"):
		refs = truncateAllTablesNameRefs(e, sql, toks, 4)
	case keywordsAt(toks, 0, "TRUNCATE", "TABLES", "FROM"):
		refs = truncateAllTablesNameRefs(e, sql, toks, 3)
	case keywordsAt(toks, 0, "ALTER", "DATABASE"):
		refs = alterDatabaseNameRefs(e, sql, toks)
	case keywordsAt(toks, 0, "DROP", "DICTIONARY"):
		refs = dropDictionaryNameRefs(e, sql, toks)
	case keywordAt(toks, 0, "CREATE") || keywordAt(toks, 0, "ATTACH"):
		refs, err = createLiveViewNameRefs(e, ast, sql, toks)
		if err != nil {
			return nil, err
		}
	}

	return dedupeNameRefs(refs), nil
}

// PrewhereTargets binds each real PREWHERE keyword to the FIRST table of the
// FROM clause of the query block that owns it. PREWHERE is a query-level
// clause evaluated against the main table, so a JOINed table does not own it —
// the same rule collectSelectLevelSampleTargets applies to SELECT-level SAMPLE.
//
// The keyword is matched by text rather than token type because the dialect
// tokenizer does not necessarily give PREWHERE its own type. keywordAt excludes
// string literals and quoted identifiers so those spellings cannot trigger
// policy.
func PrewhereTargets(e Engine, sql string) ([]TableTarget, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return nil, err
	}
	var out []TableTarget
	for i := range toks {
		if !keywordAt(toks, i, "PREWHERE") {
			continue
		}
		from := -1
		nesting := 0
		for j := i - 1; j >= 0; j-- {
			if toks[j].Text == ")" {
				nesting++
				continue
			}
			if toks[j].Text == "(" && nesting > 0 {
				nesting--
				continue
			}
			if nesting == 0 && toks[j].TokenType == "FROM" {
				from = j
				break
			}
		}
		if from < 0 {
			continue
		}
		// PREWHERE is a grammar-proven table-source position. Parse that name run
		// through the existing identifier authority instead of trusting token text:
		// Polyglot preserves ClickHouse backslash escapes (for example `\x64b1`)
		// in QUOTED_IDENTIFIER text, while tableRefAt resolves them to the semantic
		// database name (db1). Raw token text would miss an SI target and fail open.
		if ref, _, ok := tableRefAt(e, sql, toks, from+1, false); ok {
			out = append(out, TableTarget{DB: ref.DB, Table: ref.Table})
		}
	}
	return out, nil
}

type systemTargetMode uint8

const (
	systemNoTarget systemTargetMode = iota
	systemTable
	systemTableList
	systemOptionalTable
	systemStringTable
	systemView
	systemDatabase
	systemMerges
	systemBareView
	systemTestView
	systemDropReplica
	systemDropDatabaseReplica
)

type systemTargetSpec struct {
	words []string
	mode  systemTargetMode
}

// Keep longer phrases before their prefixes. This list mirrors the table/view
// branches of ClickHouse ParserSystemQuery; non-object targets (disks, models,
// caches, failpoints, etc.) are deliberately absent.
var systemTargetSpecs = []systemTargetSpec{
	{[]string{"START", "PULLING", "REPLICATION", "LOG"}, systemOptionalTable},
	{[]string{"STOP", "PULLING", "REPLICATION", "LOG"}, systemOptionalTable},
	// The actual Polyglot grammar pin is ClickHouse v26.7, where both FLUSH
	// forms accept the same optional ordered table list. The deployment runtime
	// may still be ClickHouse 25.8, where ASYNC INSERT QUEUE is not a target-list
	// suffix; D2 intentionally follows the parser that classifies this request.
	{[]string{"FLUSH", "ASYNC", "INSERT", "QUEUE"}, systemTableList},
	{[]string{"FLUSH", "LOGS"}, systemTableList},
	{[]string{"START", "VIRTUAL", "PARTS", "UPDATE"}, systemOptionalTable},
	{[]string{"STOP", "VIRTUAL", "PARTS", "UPDATE"}, systemOptionalTable},
	{[]string{"START", "REDUCE", "BLOCKING", "PARTS"}, systemOptionalTable},
	{[]string{"STOP", "REDUCE", "BLOCKING", "PARTS"}, systemOptionalTable},
	{[]string{"FLUSH", "OBJECT", "STORAGE", "QUEUE"}, systemTable},
	{[]string{"PREWARM", "PRIMARY", "INDEX", "CACHE"}, systemTable},
	{[]string{"START", "ALL", "BACKGROUND"}, systemNoTarget},
	{[]string{"STOP", "ALL", "BACKGROUND"}, systemNoTarget},
	{[]string{"PAUSE", "ALL", "BACKGROUND"}, systemNoTarget},
	{[]string{"CANCEL", "ALL", "BACKGROUND"}, systemNoTarget},
	{[]string{"REFRESH", "ALL", "BACKGROUND"}, systemNoTarget},
	{[]string{"START", "DISTRIBUTED", "SENDS"}, systemOptionalTable},
	{[]string{"STOP", "DISTRIBUTED", "SENDS"}, systemOptionalTable},
	{[]string{"START", "REPLICATED", "SENDS"}, systemOptionalTable},
	{[]string{"STOP", "REPLICATED", "SENDS"}, systemOptionalTable},
	{[]string{"START", "REPLICATION", "QUEUES"}, systemOptionalTable},
	{[]string{"STOP", "REPLICATION", "QUEUES"}, systemOptionalTable},
	{[]string{"START", "REPLICATED", "VIEW"}, systemView},
	{[]string{"STOP", "REPLICATED", "VIEW"}, systemView},
	{[]string{"RESTORE", "DATABASE", "REPLICA"}, systemDatabase},
	{[]string{"SYNC", "DATABASE", "REPLICA"}, systemDatabase},
	{[]string{"DROP", "DATABASE", "REPLICA"}, systemDropDatabaseReplica},
	{[]string{"WAIT", "LOADING", "PARTS"}, systemTable},
	{[]string{"WAIT", "QUERY", "RUNNER"}, systemTable},
	{[]string{"START", "TTL", "MERGES"}, systemOptionalTable},
	{[]string{"STOP", "TTL", "MERGES"}, systemOptionalTable},
	{[]string{"LOAD", "PRIMARY", "KEY"}, systemOptionalTable},
	{[]string{"UNLOAD", "PRIMARY", "KEY"}, systemOptionalTable},
	{[]string{"PREWARM", "MARK", "CACHE"}, systemTable},
	{[]string{"START", "VIEWS"}, systemNoTarget},
	{[]string{"STOP", "VIEWS"}, systemNoTarget},
	{[]string{"PAUSE", "VIEWS"}, systemNoTarget},
	{[]string{"START", "LISTEN"}, systemNoTarget},
	{[]string{"STOP", "LISTEN"}, systemNoTarget},
	{[]string{"RELOAD", "DICTIONARY"}, systemStringTable},
	{[]string{"UNLOAD", "DICTIONARY"}, systemStringTable},
	{[]string{"RESTART", "REPLICA"}, systemTable},
	{[]string{"RESTORE", "REPLICA"}, systemTable},
	{[]string{"SYNC", "REPLICA"}, systemTable},
	{[]string{"FLUSH", "DISTRIBUTED"}, systemTable},
	{[]string{"START", "MOVES"}, systemOptionalTable},
	{[]string{"STOP", "MOVES"}, systemOptionalTable},
	{[]string{"START", "FETCHES"}, systemOptionalTable},
	{[]string{"STOP", "FETCHES"}, systemOptionalTable},
	{[]string{"START", "CLEANUP"}, systemOptionalTable},
	{[]string{"STOP", "CLEANUP"}, systemOptionalTable},
	{[]string{"SYNC", "MERGES"}, systemOptionalTable},
	{[]string{"SCHEDULE", "MERGE"}, systemTable},
	{[]string{"START", "MERGES"}, systemMerges},
	{[]string{"STOP", "MERGES"}, systemMerges},
	{[]string{"REFRESH", "VIEW"}, systemView},
	{[]string{"WAIT", "VIEW"}, systemView},
	{[]string{"START", "VIEW"}, systemView},
	{[]string{"STOP", "VIEW"}, systemView},
	{[]string{"PAUSE", "VIEW"}, systemView},
	{[]string{"CANCEL", "VIEW"}, systemView},
	{[]string{"TEST", "VIEW"}, systemTestView},
	{[]string{"DROP", "REPLICA"}, systemDropReplica},
	// The background-view aliases accept a table immediately after the verb.
	{[]string{"START"}, systemBareView},
	{[]string{"STOP"}, systemBareView},
	{[]string{"PAUSE"}, systemBareView},
	{[]string{"CANCEL"}, systemBareView},
	{[]string{"REFRESH"}, systemBareView},
}

func systemNameRefs(e Engine, sql string, toks []rawToken) []NameRef {
	for i := 1; i+2 < len(toks); i++ {
		if keywordsAt(toks, i, "ON", "CLUSTER") {
			if _, parameter := identifierParameterEnd(toks, i+2); parameter {
				return nil
			}
		}
	}
	for _, spec := range systemTargetSpecs {
		if !keywordsAt(toks, 1, spec.words...) {
			continue
		}
		i := 1 + len(spec.words)
		switch spec.mode {
		case systemNoTarget:
			return nil
		case systemMerges:
			if keywordsAt(toks, i, "ON", "VOLUME") {
				return nil
			}
			var valid bool
			i, valid = systemClusterEnd(toks, i)
			if !valid {
				return nil
			}
			if keywordsAt(toks, i, "ON", "VOLUME") {
				return nil
			}
			if ref, after, ok := tableRefAt(e, sql, toks, i, false); ok && onlyStatementEnd(toks, after) {
				return []NameRef{ref}
			}
			return nil
		case systemTable, systemOptionalTable, systemStringTable:
			var valid bool
			i, valid = systemClusterEnd(toks, i)
			if !valid {
				return nil
			}
			if ref, after, ok := tableRefAt(e, sql, toks, i, spec.mode == systemStringTable); ok &&
				systemTargetTailValid(e, sql, spec, toks, after) {
				return []NameRef{ref}
			}
			return nil
		case systemView:
			// ParserSystemQuery's view-control branches accept exactly one view
			// target and no ON CLUSTER clause before or after it.
			if keywordsAt(toks, i, "ON", "CLUSTER") {
				return nil
			}
			if ref, after, ok := tableRefAt(e, sql, toks, i, false); ok && onlyStatementEnd(toks, after) {
				return []NameRef{ref}
			}
			return nil
		case systemTestView:
			// ClickHouse v25.8 requires exactly one target followed by either
			// SET FAKE TIME <StringLiteral> or UNSET FAKE TIME. Unlike the other
			// view-control branches, the action is part of the command grammar.
			if keywordsAt(toks, i, "ON", "CLUSTER") {
				return nil
			}
			ref, after, ok := tableRefAt(e, sql, toks, i, false)
			if !ok {
				return nil
			}
			switch {
			case keywordsAt(toks, after, "UNSET", "FAKE", "TIME") && onlyStatementEnd(toks, after+3):
				return []NameRef{ref}
			case keywordsAt(toks, after, "SET", "FAKE", "TIME") && after+3 < len(toks) &&
				validSystemFakeTime(toks[after+3]) && onlyStatementEnd(toks, after+4):
				return []NameRef{ref}
			default:
				return nil
			}
		case systemTableList:
			var valid bool
			i, valid = systemClusterEnd(toks, i)
			if !valid {
				return nil
			}
			var refs []NameRef
			for {
				ref, after, ok := tableRefAt(e, sql, toks, i, false)
				if ok {
					refs = append(refs, ref)
				} else if opaqueEnd, opaque := opaqueTableRefEnd(toks, i); opaque {
					after = opaqueEnd
				} else {
					return nil
				}
				if onlyStatementEnd(toks, after) {
					return refs
				}
				if after >= len(toks) || toks[after].TokenType != "COMMA" {
					return nil
				}
				i = after + 1
			}
		case systemDatabase:
			var valid bool
			i, valid = systemClusterEnd(toks, i)
			if !valid {
				return nil
			}
			if ref, after, ok := databaseRefAt(e, sql, toks, i); ok &&
				systemTargetTailValid(e, sql, spec, toks, after) {
				return []NameRef{ref}
			}
			return nil
		case systemBareView:
			if ref, after, ok := tableRefAt(e, sql, toks, i, false); ok && onlyStatementEnd(toks, after) {
				return []NameRef{ref}
			}
			return nil
		case systemDropReplica, systemDropDatabaseReplica:
			return systemDropReplicaRefs(e, sql, toks, i, spec.mode == systemDropDatabaseReplica)
		}
	}
	return nil
}

func systemTargetTailValid(e Engine, sql string, spec systemTargetSpec, toks []rawToken, i int) bool {
	if keywordsAt(toks, i, "ON", "CLUSTER") {
		if !systemAllowsClusterAfterTarget(spec) {
			return false
		}
		var valid bool
		i, valid = systemClusterEnd(toks, i)
		if !valid {
			return false
		}
	}
	switch {
	case equalWords(spec.words, "FLUSH", "OBJECT", "STORAGE", "QUEUE"):
		return keywordAt(toks, i, "PATH") && i+1 < len(toks) &&
			isStringLiteralToken(toks[i+1]) && onlyStatementEnd(toks, i+2)
	case equalWords(spec.words, "FLUSH", "DISTRIBUTED"):
		return onlyStatementEnd(toks, i) ||
			(keywordAt(toks, i, "SETTINGS") && systemSettingsTailValid(e, sql, toks, i+1))
	case equalWords(spec.words, "SCHEDULE", "MERGE"):
		return keywordAt(toks, i, "PARTS") && systemPartsTailValid(toks, i+1)
	case equalWords(spec.words, "SYNC", "REPLICA"):
		return systemSyncReplicaTailValid(toks, i)
	case equalWords(spec.words, "SYNC", "DATABASE", "REPLICA"):
		return onlyStatementEnd(toks, i) ||
			(keywordAt(toks, i, "STRICT") && onlyStatementEnd(toks, i+1))
	default:
		return onlyStatementEnd(toks, i)
	}
}

func systemAllowsClusterAfterTarget(spec systemTargetSpec) bool {
	for _, words := range [][]string{
		{"START", "DISTRIBUTED", "SENDS"},
		{"STOP", "DISTRIBUTED", "SENDS"},
		{"LOAD", "PRIMARY", "KEY"},
		{"UNLOAD", "PRIMARY", "KEY"},
		{"FLUSH", "OBJECT", "STORAGE", "QUEUE"},
		{"FLUSH", "DISTRIBUTED"},
		{"RESTORE", "REPLICA"},
		{"RELOAD", "DICTIONARY"},
		{"UNLOAD", "DICTIONARY"},
	} {
		if equalWords(spec.words, words...) {
			return true
		}
	}
	return false
}

func systemSyncReplicaTailValid(toks []rawToken, i int) bool {
	if keywordsAt(toks, i, "IF", "EXISTS") {
		i += 2
	}
	switch {
	case onlyStatementEnd(toks, i):
		return true
	case keywordAt(toks, i, "STRICT"):
		return onlyStatementEnd(toks, i+1)
	case keywordAt(toks, i, "PULL"):
		return onlyStatementEnd(toks, i+1)
	case keywordAt(toks, i, "LIGHTWEIGHT"):
		i++
		if onlyStatementEnd(toks, i) {
			return true
		}
		if !keywordAt(toks, i, "FROM") {
			return false
		}
		i++
		wantReplica := true
		for i < liveViewStatementEnd(toks) {
			if wantReplica {
				if !isStringLiteralToken(toks[i]) {
					return false
				}
				wantReplica = false
				i++
				continue
			}
			if toks[i].TokenType != "COMMA" {
				return false
			}
			wantReplica = true
			i++
		}
		return !wantReplica
	default:
		return false
	}
}

func equalWords(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !strings.EqualFold(got[i], want[i]) {
			return false
		}
	}
	return true
}

func systemSettingsTailValid(e Engine, sql string, toks []rawToken, start int) bool {
	return systemSettingsRangeValid(e, sql, toks, start, liveViewStatementEnd(toks))
}

func systemSettingsRangeValid(e Engine, sql string, toks []rawToken, start, end int) bool {
	if start >= end {
		return false
	}
	for i := start; ; {
		settingStart := i
		nameEnd, ok := systemSettingNameEnd(toks, i, end)
		if !ok {
			return false
		}
		valueEnd := nameEnd
		if nameEnd < end && (toks[nameEnd].TokenType == "EQ" || toks[nameEnd].Text == "=") {
			name := systemSettingName(toks, settingStart, nameEnd)
			if name == "param_" {
				return false
			}
			if strings.HasPrefix(name, "param_") {
				valueEnd, ok = systemParameterValueEnd(toks, nameEnd+1, end)
			} else {
				valueEnd, ok = systemOrdinarySettingValueEnd(e, sql, toks, nameEnd+1, end)
			}
			if !ok {
				return false
			}
		} else if !systemBoolShorthandAllowed(systemSettingName(toks, settingStart, nameEnd)) {
			// Whether `name` without `= value` is legal depends on ClickHouse's
			// pinned Settings registry, not the SQL grammar. Keep this allowlist
			// deliberately small and conservative: an unknown valid shorthand
			// merely keeps the generic reject message, while accepting a non-Bool
			// name would falsely claim an exact SYSTEM command.
			return false
		}
		if valueEnd == end {
			return true
		}
		if toks[valueEnd].TokenType != "COMMA" || valueEnd+1 >= end {
			return false
		}
		i = valueEnd + 1
	}
}

func systemSettingName(toks []rawToken, start, end int) string {
	var b strings.Builder
	for i := start; i < end; i++ {
		if toks[i].TokenType == "DOT" {
			b.WriteByte('.')
		} else {
			b.WriteString(toks[i].Text)
		}
	}
	return b.String()
}

func systemBoolShorthandAllowed(name string) bool {
	switch strings.ToLower(name) {
	case "async_insert":
		return true
	default:
		return false
	}
}

func systemSettingNameEnd(toks []rawToken, i, end int) (int, bool) {
	return systemCompoundIdentifierEnd(toks, i, end)
}

// systemCompoundIdentifierEnd mirrors the ParserCompoundIdentifier consumed by
// ParserSetQuery for param_ values. Besides ordinary dotted names, ClickHouse
// accepts JSON-path delimiters (.:, .^, and .@) and the [] shorthand that is
// legal after a non-first JSON path component. Polyglot tokenizes .: as one
// token and folds @ into the following VAR, so both native token spellings are
// handled explicitly here.
func systemCompoundIdentifierEnd(toks []rawToken, i, end int) (int, bool) {
	componentEnd, ok := systemCompoundIdentifierComponentEnd(toks, i, end, false)
	if !ok {
		return i, false
	}
	i = componentEnd
	allowArrayAddition := false // ParserCompoundIdentifier excludes [] after its first component.
	for i < end {
		if allowArrayAddition {
			for i+1 < end && toks[i].TokenType == "L_BRACKET" && toks[i+1].TokenType == "R_BRACKET" {
				i += 2
			}
		}

		next, delimited, special := systemCompoundIdentifierDelimiterEnd(toks, i, end)
		if !delimited {
			return i, true
		}
		componentEnd, ok = systemCompoundIdentifierComponentEnd(toks, next, end, special)
		if !ok {
			return i, false
		}
		i = componentEnd
		// ClickHouse checks the [] JSON-array shorthand only in the ordinary-dot
		// branch. A component introduced by .:, .^, or .@ cannot carry it.
		allowArrayAddition = !special
	}
	return i, true
}

func systemCompoundIdentifierComponentEnd(toks []rawToken, i, end int, collapsedAtDelimiter bool) (int, bool) {
	if i >= end {
		return i, false
	}
	if isIdentifierCandidate(toks[i]) {
		return i + 1, true
	}
	// Polyglot emits foo.@bar as DOT + VAR("@bar"), whereas ClickHouse's
	// parser sees the @ as part of its JSON-path delimiter.
	if collapsedAtDelimiter && toks[i].TokenType == "VAR" && strings.HasPrefix(toks[i].Text, "@") &&
		isIdentifierText(strings.TrimPrefix(toks[i].Text, "@")) {
		return i + 1, true
	}
	return i, false
}

func systemCompoundIdentifierDelimiterEnd(toks []rawToken, i, end int) (next int, ok, special bool) {
	if i >= end {
		return i, false, false
	}
	if toks[i].TokenType == "DOT_COLON" || toks[i].Text == ".:" {
		return i + 1, true, true
	}
	if toks[i].TokenType != "DOT" && toks[i].Text != "." {
		return i, false, false
	}
	if i+1 >= end {
		return i, false, false
	}
	switch {
	case toks[i+1].TokenType == "COLON" || toks[i+1].Text == ":",
		toks[i+1].TokenType == "CARET" || toks[i+1].Text == "^",
		toks[i+1].TokenType == "AT" || toks[i+1].TokenType == "D_AT" || toks[i+1].Text == "@":
		return i + 2, true, true
	case toks[i+1].TokenType == "VAR" && strings.HasPrefix(toks[i+1].Text, "@"):
		return i + 1, true, true
	default:
		return i + 1, true, false
	}
}

func systemOrdinarySettingValueEnd(e Engine, sql string, toks []rawToken, i, end int) (int, bool) {
	if i >= end {
		return i, false
	}
	if keywordAt(toks, i, "DEFAULT") {
		return i + 1, true
	}
	if substitutionEnd, ok := systemSettingSubstitutionEnd(e, sql, toks, i, end); ok {
		return substitutionEnd, true
	}
	if toks[i].Text == "disk" && i+1 < end && toks[i+1].TokenType == "L_PAREN" {
		groupEnd, ok := balancedTokenGroupEnd(toks, i+1, "L_PAREN", "R_PAREN")
		if !ok || !systemSettingExpressionProbe(e, sql, toks, i, groupEnd) {
			return i, false
		}
		return groupEnd, true
	}
	if mapEnd, ok := systemStringMapEnd(toks, i, end); ok {
		return mapEnd, true
	}
	return systemSettingScalarEnd(toks, i, end)
}

func systemSettingScalarEnd(toks []rawToken, i, end int) (int, bool) {
	if i >= end {
		return i, false
	}
	if toks[i].Text == "+" || toks[i].Text == "-" {
		if i+1 < end && (toks[i+1].TokenType == "NUMBER" || keywordAt(toks, i+1, "INF") ||
			keywordAt(toks, i+1, "INFINITY") || keywordAt(toks, i+1, "NAN")) {
			return i + 2, true
		}
		return i, false
	}
	if isStringLiteralToken(toks[i]) || toks[i].TokenType == "NUMBER" ||
		keywordAt(toks, i, "NULL") || keywordAt(toks, i, "TRUE") || keywordAt(toks, i, "FALSE") ||
		keywordAt(toks, i, "INF") || keywordAt(toks, i, "INFINITY") || keywordAt(toks, i, "NAN") {
		return i + 1, true
	}
	return i, false
}

func systemStringMapEnd(toks []rawToken, i, end int) (int, bool) {
	if i >= end || toks[i].TokenType != "L_BRACE" {
		return i, false
	}
	i++
	if i < end && toks[i].TokenType == "R_BRACE" {
		return i + 1, true
	}
	for {
		if i >= end || !isStringLiteralToken(toks[i]) || i+2 >= end ||
			toks[i+1].TokenType != "COLON" || !isStringLiteralToken(toks[i+2]) {
			return i, false
		}
		i += 3
		if i >= end {
			return i, false
		}
		if toks[i].TokenType == "R_BRACE" {
			return i + 1, true
		}
		if toks[i].TokenType != "COMMA" || i+1 >= end || toks[i+1].TokenType == "R_BRACE" {
			return i, false
		}
		i++
	}
}

func systemSettingSubstitutionEnd(e Engine, sql string, toks []rawToken, i, end int) (int, bool) {
	if i >= end || toks[i].TokenType != "L_BRACE" || i+4 >= end ||
		!isIdentifierCandidate(toks[i+1]) || toks[i+1].TokenType == "QUOTED_IDENTIFIER" ||
		toks[i+2].TokenType != "COLON" {
		return i, false
	}
	groupEnd, ok := balancedTokenGroupEnd(toks, i, "L_BRACE", "R_BRACE")
	if !ok || groupEnd <= i+4 {
		return i, false
	}
	typeStart, typeEnd := toks[i+3].Span.Start, toks[groupEnd-2].Span.End
	if typeStart < 0 || typeEnd <= typeStart || typeEnd > len(sql) {
		return i, false
	}
	_, err := e.ParseOne("SELECT CAST(NULL AS " + sql[typeStart:typeEnd] + ")")
	return groupEnd, err == nil
}

func systemSettingExpressionProbe(e Engine, sql string, toks []rawToken, start, end int) bool {
	if start >= end || end > len(toks) {
		return false
	}
	startByte, endByte := toks[start].Span.Start, toks[end-1].Span.End
	if startByte < 0 || endByte <= startByte || endByte > len(sql) {
		return false
	}
	ast, err := e.ParseOne("SELECT " + sql[startByte:endByte] + " AS __hg_system_setting_probe__")
	if err != nil {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return false
	}
	selectBody, ok := root[NodeSelect].(map[string]any)
	if !ok {
		return false
	}
	expressions, ok := selectBody["expressions"].([]any)
	if !ok || len(expressions) != 1 {
		return false
	}
	expression, ok := expressions[0].(map[string]any)
	if !ok {
		return false
	}
	alias, ok := expression["alias"].(map[string]any)
	if !ok {
		return false
	}
	aliasName, ok := alias["alias"].(map[string]any)
	if !ok || aliasName["name"] != "__hg_system_setting_probe__" {
		return false
	}
	this, ok := alias["this"].(map[string]any)
	if !ok {
		return false
	}
	function, ok := this["function"].(map[string]any)
	functionName, _ := function["name"].(string)
	return ok && functionName == "disk" &&
		!systemSettingASTContainsKey(function["args"], "alias")
}

func systemSettingASTContainsKey(node any, key string) bool {
	switch n := node.(type) {
	case map[string]any:
		if _, present := n[key]; present {
			return true
		}
		for _, child := range n {
			if systemSettingASTContainsKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range n {
			if systemSettingASTContainsKey(child, key) {
				return true
			}
		}
	}
	return false
}

func systemParameterValueEnd(toks []rawToken, i, end int) (int, bool) {
	if scalarEnd, ok := systemSettingScalarEnd(toks, i, end); ok {
		return scalarEnd, true
	}
	if nameEnd, ok := systemCompoundIdentifierEnd(toks, i, end); ok {
		return nameEnd, true
	}
	if i >= end {
		return i, false
	}
	switch toks[i].TokenType {
	case "L_BRACKET":
		return systemParameterCollectionEnd(toks, i, end, "R_BRACKET", false)
	case "L_PAREN":
		return systemParameterCollectionEnd(toks, i, end, "R_PAREN", false)
	case "L_BRACE":
		return systemParameterCollectionEnd(toks, i, end, "R_BRACE", true)
	default:
		return i, false
	}
}

func systemParameterCollectionEnd(toks []rawToken, i, end int, close string, mapEntries bool) (int, bool) {
	i++
	if i < end && toks[i].TokenType == close {
		return i + 1, true
	}
	for {
		var ok bool
		if mapEntries {
			i, ok = systemSettingScalarEnd(toks, i, end)
		} else {
			i, ok = systemParameterCollectionValueEnd(toks, i, end)
		}
		if !ok {
			return i, false
		}
		if mapEntries {
			if i >= end || toks[i].TokenType != "COLON" {
				return i, false
			}
			i, ok = systemParameterCollectionValueEnd(toks, i+1, end)
			if !ok {
				return i, false
			}
		}
		if i >= end {
			return i, false
		}
		if toks[i].TokenType == close {
			return i + 1, true
		}
		if toks[i].TokenType != "COMMA" || i+1 >= end || toks[i+1].TokenType == close {
			return i, false
		}
		i++
	}
}

func systemParameterCollectionValueEnd(toks []rawToken, i, end int) (int, bool) {
	if scalarEnd, ok := systemSettingScalarEnd(toks, i, end); ok {
		return scalarEnd, true
	}
	if i >= end {
		return i, false
	}
	switch toks[i].TokenType {
	case "L_BRACKET":
		return systemParameterCollectionEnd(toks, i, end, "R_BRACKET", false)
	case "L_PAREN":
		return systemParameterCollectionEnd(toks, i, end, "R_PAREN", false)
	case "L_BRACE":
		return systemParameterCollectionEnd(toks, i, end, "R_BRACE", true)
	default:
		return i, false
	}
}

func validSystemFakeTime(tok rawToken) bool {
	if tok.TokenType != "STRING" {
		return false
	}
	// readDateTimeText, used by ParserSystemQuery, accepts the canonical
	// second-resolution DateTime text. Staying conservative here is intentional:
	// an unrecognised valid spelling merely keeps the generic fail-closed error.
	_, err := time.ParseInLocation("2006-01-02 15:04:05", tok.Text, time.UTC)
	return err == nil
}

func checkTableTailValid(e Engine, sql string, toks []rawToken, i int) bool {
	end := liveViewStatementEnd(toks)
	if i == end {
		return true
	}
	switch {
	case keywordAt(toks, i, "SETTINGS"):
		return systemSettingsTailValid(e, sql, toks, i+1)
	case keywordAt(toks, i, "PART"):
		if i+1 >= end || !isStringLiteralToken(toks[i+1]) {
			return false
		}
		i += 2
		return i == end || (keywordAt(toks, i, "SETTINGS") && systemSettingsTailValid(e, sql, toks, i+1))
	case keywordAt(toks, i, "PARTITION"):
		for settings := i + 2; settings < end; settings++ {
			if !keywordAt(toks, settings, "SETTINGS") {
				continue
			}
			valueEnd, ok := checkPartitionValueEnd(e, sql, toks, i+1, settings)
			if ok && valueEnd == settings && systemSettingsTailValid(e, sql, toks, settings+1) {
				return true
			}
		}
		valueEnd, ok := checkPartitionValueEnd(e, sql, toks, i+1, end)
		return ok && valueEnd == end
	default:
		return false
	}
}

func checkPartitionValueEnd(e Engine, sql string, toks []rawToken, i, end int) (int, bool) {
	if i >= end {
		return i, false
	}
	if keywordAt(toks, i, "ALL") {
		return i + 1, true
	}
	if keywordAt(toks, i, "ID") {
		if i+1 < end && isStringLiteralToken(toks[i+1]) {
			return i + 2, true
		}
		if substitutionEnd, ok := systemSettingSubstitutionEnd(e, sql, toks, i+1, end); ok {
			return substitutionEnd, true
		}
		return i, false
	}
	if substitutionEnd, ok := systemSettingSubstitutionEnd(e, sql, toks, i, end); ok {
		return substitutionEnd, true
	}
	if arrayEnd, ok := checkPartitionLiteralArrayEnd(toks, i, end); ok {
		return arrayEnd, true
	}
	if scalarEnd, ok := systemSettingScalarEnd(toks, i, end); ok {
		return scalarEnd, true
	}
	if checkPartitionExpressionProbe(e, sql, toks, i, end) {
		return end, true
	}
	return i, false
}

// checkPartitionLiteralArrayEnd mirrors ParserLiteral's square-array branch:
// elements may be scalar literals or nested literal arrays, but never general
// expressions, tuple expressions, functions, or trailing commas.
func checkPartitionLiteralArrayEnd(toks []rawToken, i, end int) (int, bool) {
	if i >= end || toks[i].TokenType != "L_BRACKET" {
		return i, false
	}
	i++
	if i < end && toks[i].TokenType == "R_BRACKET" {
		return i + 1, true
	}
	for {
		var next int
		var ok bool
		if i < end && toks[i].TokenType == "L_BRACKET" {
			next, ok = checkPartitionLiteralArrayEnd(toks, i, end)
		} else {
			next, ok = systemSettingScalarEnd(toks, i, end)
		}
		if !ok {
			return i, false
		}
		i = next
		if i >= end {
			return i, false
		}
		if toks[i].TokenType == "R_BRACKET" {
			return i + 1, true
		}
		if toks[i].TokenType != "COMMA" || i+1 >= end || toks[i+1].TokenType == "R_BRACKET" {
			return i, false
		}
		i++
	}
}

func checkPartitionExpressionProbe(e Engine, sql string, toks []rawToken, start, end int) bool {
	if start >= end || end > len(toks) {
		return false
	}
	startByte, endByte := toks[start].Span.Start, toks[end-1].Span.End
	if startByte < 0 || endByte <= startByte || endByte > len(sql) {
		return false
	}
	const alias = "__hg_check_partition_probe__"
	ast, err := e.ParseOne("SELECT " + sql[startByte:endByte] + " AS " + alias)
	if err != nil {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return false
	}
	selectBody, ok := root[NodeSelect].(map[string]any)
	if !ok {
		return false
	}
	expressions, ok := selectBody["expressions"].([]any)
	if !ok || len(expressions) != 1 {
		return false
	}
	expression, ok := expressions[0].(map[string]any)
	if !ok {
		return false
	}
	aliased, ok := expression["alias"].(map[string]any)
	if !ok || identName(aliased["alias"]) != alias {
		return false
	}
	this, ok := aliased["this"].(map[string]any)
	if !ok {
		return false
	}
	// ParserPartition accepts a scalar ASTLiteral or an ASTFunction whose root
	// is tuple. Polyglot represents tuple(...) as function{name:"tuple"} and
	// parenthesized tuple syntax as a dedicated tuple node.
	if _, literal := this["literal"]; literal {
		return true
	}
	if _, tuple := this["tuple"]; tuple {
		return true
	}
	function, ok := this["function"].(map[string]any)
	name, _ := function["name"].(string)
	return ok && name == "tuple"
}

func alterDatabaseNameRefs(e Engine, sql string, toks []rawToken) []NameRef {
	ref, i, ok := databaseRefAt(e, sql, toks, 2)
	if !ok {
		return nil
	}
	if keywordsAt(toks, i, "ON", "CLUSTER") {
		var valid bool
		i, valid = systemClusterEnd(toks, i)
		if !valid {
			return nil
		}
	}
	end := liveViewStatementEnd(toks)
	if i >= end {
		return nil
	}
	parenthesized := toks[i].TokenType == "L_PAREN"
	for {
		commandEnd := end
		closeEnd := -1
		if parenthesized {
			if toks[i].TokenType != "L_PAREN" {
				return nil
			}
			var balanced bool
			closeEnd, balanced = balancedTokenGroupEnd(toks, i, "L_PAREN", "R_PAREN")
			if !balanced || closeEnd > end {
				return nil
			}
			commandEnd = closeEnd - 1
			i++
		} else if toks[i].TokenType == "L_PAREN" {
			return nil
		}
		switch {
		case keywordsAt(toks, i, "MODIFY", "COMMENT"):
			if i+2 >= commandEnd || !isStringLiteralToken(toks[i+2]) {
				return nil
			}
			i += 3
			if parenthesized && i != commandEnd {
				return nil
			}
		case keywordsAt(toks, i, "MODIFY", "SETTING"):
			if !systemSettingsRangeValid(e, sql, toks, i+2, commandEnd) {
				return nil
			}
			i = commandEnd
		default:
			return nil
		}
		if parenthesized {
			i = closeEnd
		}
		if i == end {
			return []NameRef{ref}
		}
		if toks[i].TokenType != "COMMA" || i+1 >= end {
			return nil
		}
		i++
	}
}

func systemPartsTailValid(toks []rawToken, start int) bool {
	end := liveViewStatementEnd(toks)
	if start >= end {
		return false
	}
	wantValue := true
	for i := start; i < end; i++ {
		if wantValue {
			if !isStringLiteralToken(toks[i]) {
				return false
			}
			wantValue = false
			continue
		}
		if toks[i].TokenType != "COMMA" {
			return false
		}
		wantValue = true
	}
	return !wantValue
}

func systemDropReplicaRefs(e Engine, sql string, toks []rawToken, i int, databaseOnly bool) []NameRef {
	var valid bool
	i, valid = systemClusterEnd(toks, i)
	if !valid || i >= len(toks) || !isStringLiteralToken(toks[i]) {
		return nil
	}
	i++ // required replica name
	if keywordsAt(toks, i, "FROM", "SHARD") {
		if i+2 >= len(toks) || !isStringLiteralToken(toks[i+2]) {
			return nil
		}
		i += 3
	}
	if onlyStatementEnd(toks, i) {
		return nil // valid whole-replica form, but it names no table/database
	}
	if !keywordAt(toks, i, "FROM") || i+1 >= len(toks) {
		return nil
	}
	i++
	switch {
	case keywordAt(toks, i, "DATABASE"):
		ref, after, ok := databaseRefAt(e, sql, toks, i+1)
		if !ok {
			return nil
		}
		if databaseOnly && keywordsAt(toks, after, "WITH", "TABLES") {
			after += 2
		}
		if !onlyStatementEnd(toks, after) {
			return nil
		}
		return []NameRef{ref}
	case !databaseOnly && keywordAt(toks, i, "TABLE"):
		ref, after, ok := tableRefAt(e, sql, toks, i+1, false)
		if !ok || !onlyStatementEnd(toks, after) {
			return nil
		}
		return []NameRef{ref}
	case keywordAt(toks, i, "ZKPATH"):
		if i+1 >= len(toks) || !isStringLiteralToken(toks[i+1]) || toks[i+1].Text == "" ||
			!onlyStatementEnd(toks, i+2) {
			return nil
		}
		return nil
	default:
		return nil
	}
}

func truncateDatabaseNameRefs(e Engine, sql string, toks []rawToken) []NameRef {
	i := 2
	if keywordsAt(toks, i, "IF", "EXISTS") {
		i += 2
	}
	if keywordsAt(toks, i, "IF", "EMPTY") {
		i += 2
	}
	ref, after, ok := databaseRefAt(e, sql, toks, i)
	if !ok {
		return nil
	}
	i = after
	if keywordsAt(toks, i, "ON", "CLUSTER") {
		var valid bool
		i, valid = skipOnCluster(toks, i)
		if !valid {
			return nil
		}
	}
	switch {
	case keywordAt(toks, i, "SYNC"):
		i++
	case keywordsAt(toks, i, "NO", "DELAY"):
		i += 2
	}
	if !onlyStatementEnd(toks, i) {
		return nil
	}
	return []NameRef{ref}
}

func truncateAllTablesNameRefs(e Engine, sql string, toks []rawToken, start int) []NameRef {
	i := skipIfExists(toks, start)
	ref, after, ok := databaseRefAt(e, sql, toks, i)
	if !ok {
		return nil
	}
	i = after
	negated := false
	if keywordAt(toks, i, "NOT") {
		negated = true
		i++
	}
	if keywordAt(toks, i, "LIKE") || keywordAt(toks, i, "ILIKE") {
		if i+1 >= len(toks) || !isStringLiteralToken(toks[i+1]) {
			return nil
		}
		i += 2
	} else if negated {
		return nil
	}
	if keywordsAt(toks, i, "ON", "CLUSTER") {
		var valid bool
		i, valid = skipOnCluster(toks, i)
		if !valid {
			return nil
		}
	}
	switch {
	case keywordAt(toks, i, "SYNC"):
		i++
	case keywordsAt(toks, i, "NO", "DELAY"):
		i += 2
	}
	if !onlyStatementEnd(toks, i) {
		return nil
	}
	return []NameRef{ref}
}

func dropDictionaryNameRefs(e Engine, sql string, toks []rawToken) []NameRef {
	i := skipIfExists(toks, 2)
	if keywordsAt(toks, i, "IF", "EMPTY") {
		i += 2
	}
	refs := make([]NameRef, 0, 1)
	for {
		if ref, after, ok := tableRefAt(e, sql, toks, i, false); ok {
			refs = append(refs, ref)
			i = after
		} else if after, opaque := opaqueTableRefEnd(toks, i); opaque {
			i = after
		} else {
			return nil
		}
		if i < len(toks) && toks[i].TokenType == "COMMA" {
			i++
			continue
		}
		break
	}
	if keywordsAt(toks, i, "ON", "CLUSTER") {
		var valid bool
		i, valid = skipOnCluster(toks, i)
		if !valid {
			return nil
		}
	}
	switch {
	case keywordAt(toks, i, "SYNC"):
		i++
	case keywordsAt(toks, i, "NO", "DELAY"):
		i += 2
	}
	if !onlyStatementEnd(toks, i) {
		return nil
	}
	return refs
}

type liveViewGrammar struct {
	refs     []NameRef
	queryAST AST
}

// LiveViewClass is the total, single-tokenization classification consumed by
// Native's active-storage-integrity dispatcher. Only ExactLiveView is eligible
// for D2 object attribution. MalformedLiveViewPrefix is deliberately generic:
// the grammar has not proven any target, but the structured CREATE VIEW must
// still be prevented from reaching the ordinary-view success path.
type LiveViewClass uint8

const (
	NotLiveView LiveViewClass = iota
	ExactLiveView
	MalformedLiveViewPrefix
)

// ClassifyLiveView recognizes the exact ClickHouse v25.8 LIVE VIEW grammar and
// its bounded fail-closed prefix. The malformed recovery is gated by a
// parser-proven create_view node; raw token coincidences never promote an
// unrelated statement. Opaque raw/command nodes first pass a small lexical
// prefilter which ignores quoted text and comments, so an unavailable engine
// tokenizer remains irrelevant to unrelated statements. Once selected, the
// engine tokenizer is called exactly once and every error is returned to the
// caller instead of being silently treated as "not a LIVE VIEW".
func ClassifyLiveView(e Engine, ast AST, sql string) (LiveViewClass, error) {
	kind, err := NodeKind(ast)
	if err != nil {
		return NotLiveView, err
	}
	switch kind {
	case NodeCreateView:
		// Structured CREATE VIEW nodes need the exact grammar plus bounded
		// malformed-prefix recovery below.
	case NodeRaw, NodeCommand:
		if !opaqueSQLMayBeLiveView(sql) {
			return NotLiveView, nil
		}
	default:
		return NotLiveView, nil
	}

	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return NotLiveView, err
	}
	if !keywordAt(toks, 0, "CREATE") && !keywordAt(toks, 0, "ATTACH") {
		return NotLiveView, nil
	}
	if _, ok := parseLiveViewGrammar(e, sql, toks); ok {
		return ExactLiveView, nil
	}
	if keywordsAt(toks, 1, "LIVE", "VIEW") {
		return MalformedLiveViewPrefix, nil
	}

	if kind != NodeCreateView {
		return NotLiveView, nil
	}
	after, present, trustworthy := liveViewSQLSecurityEnd(toks, 1)
	if present && trustworthy {
		switch {
		case keywordsAt(toks, after, "LIVE", "VIEW"):
			return MalformedLiveViewPrefix, nil
		case keywordAt(toks, after, "VIEW"):
			// A valid security group followed by ordinary VIEW stays ordinary;
			// the user or host itself is allowed to be named `live`.
			return NotLiveView, nil
		}
	}
	if malformedSecurityRegionHasLiveView(toks, 1) {
		return MalformedLiveViewPrefix, nil
	}
	return NotLiveView, nil
}

// opaqueSQLMayBeLiveView is deliberately smaller than the exact LIVE VIEW
// grammar. It only decides whether an opaque parser node deserves the engine
// tokenizer call above. Comments disappear between lexical tokens, quoted
// strings/identifiers remain opaque separators, and the scan stops at the
// ordinary VIEW/query boundary. This makes both of these decisions stable:
//
//	CREATE LIVE /* comment */ VIEW ...       => inspect
//	CREATE WINDOW VIEW ... AS SELECT 'LIVE VIEW' => do not inspect
//
// The security-value exclusions mirror malformedSecurityRegionHasLiveView so
// DEFINER=live VIEW stays an ordinary VIEW rather than a LIVE VIEW prefix.
func opaqueSQLMayBeLiveView(sql string) bool {
	toks := opaqueSQLPrefixTokens(sql)
	if len(toks) == 0 || (toks[0] != "CREATE" && toks[0] != "ATTACH") {
		return false
	}

	closers := make([]string, 0, 2)
	for i := 1; i < len(toks); {
		if len(closers) == 0 {
			switch {
			case toks[i] == "DEFINER":
				// ParserSQLSecurity permits keywords (including AS, VIEW,
				// SELECT, WITH, LIVE, and DEFINER) as bare user/host
				// values. Consume the bounded role clause before looking
				// for the object-kind marker.
				i = opaqueLiveViewDefinerEnd(toks, i)
				continue
			case i+1 < len(toks) && toks[i] == "SQL" && toks[i+1] == "SECURITY":
				// The mode is one lexical token. Consuming it here also
				// prevents its text (for example DEFINER) from being
				// mistaken for a following role clause.
				if i+2 >= len(toks) {
					return false
				}
				i += 3
				continue
			case i+1 < len(toks) && toks[i] == "LIVE" && toks[i+1] == "VIEW":
				return true
			}
		}

		switch toks[i] {
		case "(":
			closers = append(closers, ")")
			i++
			continue
		case "[":
			closers = append(closers, "]")
			i++
			continue
		case "{":
			closers = append(closers, "}")
			i++
			continue
		case ")", "]", "}":
			if len(closers) == 0 || closers[len(closers)-1] != toks[i] {
				return false
			}
			closers = closers[:len(closers)-1]
			i++
			continue
		}
		if len(closers) != 0 {
			i++
			continue
		}
		switch toks[i] {
		case "VIEW", "AS", "SELECT", "WITH", ";":
			return false
		}
		i++
	}
	return false
}

func opaqueLiveViewDefinerEnd(toks []string, i int) int {
	i++
	if i < len(toks) && toks[i] == "=" {
		i++
	}
	if i < len(toks) && toks[i] == "@" {
		i++
		return opaqueLiveViewSecurityAtomEnd(toks, i)
	}
	i = opaqueLiveViewSecurityAtomEnd(toks, i)
	if i < len(toks) && toks[i] == "@" {
		i = opaqueLiveViewSecurityAtomEnd(toks, i+1)
	}
	return i
}

func opaqueLiveViewSecurityAtomEnd(toks []string, i int) int {
	if i >= len(toks) {
		return i
	}
	closer := ""
	switch toks[i] {
	case "(":
		closer = ")"
	case "[":
		closer = "]"
	case "{":
		closer = "}"
	default:
		return i + 1
	}
	closers := []string{closer}
	for i++; i < len(toks); i++ {
		switch toks[i] {
		case "(":
			closers = append(closers, ")")
		case "[":
			closers = append(closers, "]")
		case "{":
			closers = append(closers, "}")
		case ")", "]", "}":
			if toks[i] != closers[len(closers)-1] {
				return i + 1
			}
			closers = closers[:len(closers)-1]
			if len(closers) == 0 {
				return i + 1
			}
		}
	}
	return i
}

// opaqueSQLPrefixTokens performs only the lexical work needed by the prefilter:
// ASCII words and structural punctuation are retained, comments are skipped,
// and every quoted form becomes one opaque token. It intentionally does not
// call Engine.Tokenize; that is the failure boundary this prefilter protects.
func opaqueSQLPrefixTokens(sql string) []string {
	const maxTokens = 512
	toks := make([]string, 0, 24)
	for i := 0; i < len(sql) && len(toks) < maxTokens; {
		switch {
		case isOpaqueSQLSpace(sql[i]):
			i++
		case sql[i] >= utf8.RuneSelf:
			r, size := utf8.DecodeRuneInString(sql[i:])
			if r == '\uFEFF' || unicode.IsSpace(r) {
				i += size
				continue
			}
			if end, ok := opaqueUnicodeQuoteEnd(sql, i); ok {
				i = end
				toks = append(toks, "<QUOTED>")
				continue
			}
			toks = append(toks, "<NONASCII>")
			i += size
		case i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-':
			i += 2
			for i < len(sql) && sql[i] != '\n' && sql[i] != '\r' {
				i++
			}
		case sql[i] == '#':
			i++
			for i < len(sql) && sql[i] != '\n' && sql[i] != '\r' {
				i++
			}
		case i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*':
			i += 2
			depth := 1
			for i < len(sql) && depth > 0 {
				switch {
				case i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*':
					depth++
					i += 2
				case i+1 < len(sql) && sql[i] == '*' && sql[i+1] == '/':
					depth--
					i += 2
				default:
					i++
				}
			}
		case sql[i] == '\'' || sql[i] == '"' || sql[i] == '`':
			quote := sql[i]
			i++
			for i < len(sql) {
				if sql[i] == '\\' && i+1 < len(sql) {
					i += 2
					continue
				}
				if sql[i] != quote {
					i++
					continue
				}
				if i+1 < len(sql) && sql[i+1] == quote {
					i += 2
					continue
				}
				i++
				break
			}
			toks = append(toks, "<QUOTED>")
		case sql[i] == '$':
			if end, ok := opaqueDollarStringEnd(sql, i); ok {
				i = end
				toks = append(toks, "<QUOTED>")
				continue
			}
			toks = append(toks, "$")
			i++
		case isOpaqueSQLWordStart(sql[i]):
			start := i
			i++
			for i < len(sql) && isOpaqueSQLWordPart(sql[i]) {
				i++
			}
			toks = append(toks, strings.ToUpper(sql[start:i]))
		default:
			toks = append(toks, sql[i:i+1])
			i++
		}
	}
	return toks
}

// opaqueUnicodeQuoteEnd consumes ClickHouse's paired Unicode quote forms:
// curly single quotes for strings and curly double quotes for identifiers.
// As with the ASCII scanner above, backslash escapes and doubled closing
// delimiters remain inside the one opaque token; an unterminated quote consumes
// the rest of the prefix conservatively.
func opaqueUnicodeQuoteEnd(sql string, start int) (int, bool) {
	opener, openerSize := utf8.DecodeRuneInString(sql[start:])
	var closer rune
	switch opener {
	case '\u2018':
		closer = '\u2019'
	case '\u201c':
		closer = '\u201d'
	default:
		return start, false
	}
	for i := start + openerSize; i < len(sql); {
		if sql[i] == '\\' && i+1 < len(sql) {
			_, escapedSize := utf8.DecodeRuneInString(sql[i+1:])
			i += 1 + escapedSize
			continue
		}
		r, size := utf8.DecodeRuneInString(sql[i:])
		if r != closer {
			i += size
			continue
		}
		if next := i + size; next < len(sql) {
			nextRune, nextSize := utf8.DecodeRuneInString(sql[next:])
			if nextRune == closer {
				i = next + nextSize
				continue
			}
		}
		return i + size, true
	}
	return len(sql), true
}

func opaqueDollarStringEnd(sql string, start int) (int, bool) {
	i := start + 1
	for i < len(sql) && (isOpaqueSQLWordPart(sql[i])) {
		i++
	}
	if i >= len(sql) || sql[i] != '$' {
		return start, false
	}
	delim := sql[start : i+1]
	rest := sql[i+1:]
	closeAt := strings.Index(rest, delim)
	if closeAt < 0 {
		return len(sql), true
	}
	return i + 1 + closeAt + len(delim), true
}

func isOpaqueSQLSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

func isOpaqueSQLWordStart(b byte) bool {
	return b == '_' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

func isOpaqueSQLWordPart(b byte) bool {
	return isOpaqueSQLWordStart(b) || b >= '0' && b <= '9'
}

func malformedSecurityRegionHasLiveView(toks []rawToken, start int) bool {
	closers := make([]string, 0, 2)
	for i := start; i < len(toks); i++ {
		switch toks[i].TokenType {
		case "L_PAREN":
			closers = append(closers, "R_PAREN")
			continue
		case "L_BRACKET":
			closers = append(closers, "R_BRACKET")
			continue
		case "L_BRACE":
			closers = append(closers, "R_BRACE")
			continue
		case "R_PAREN", "R_BRACKET", "R_BRACE":
			if len(closers) == 0 || closers[len(closers)-1] != toks[i].TokenType {
				return false
			}
			closers = closers[:len(closers)-1]
			continue
		}
		if len(closers) != 0 {
			continue
		}
		if keywordsAt(toks, i, "LIVE", "VIEW") && !liveViewIsSecurityValue(toks, i) {
			return true
		}
		// These tokens end the leading security region. Standalone VIEW is
		// deliberately checked after adjacent LIVE VIEW so the valid marker is
		// accepted while `DEFINER=live VIEW` remains ordinary CREATE VIEW.
		if keywordAt(toks, i, "VIEW") || keywordAt(toks, i, "AS") ||
			keywordAt(toks, i, "SELECT") || keywordAt(toks, i, "WITH") {
			return false
		}
	}
	return false
}

func liveViewIsSecurityValue(toks []rawToken, i int) bool {
	if i > 0 && (keywordAt(toks, i-1, "DEFINER") || toks[i-1].TokenType == "EQ" || toks[i-1].Text == "=" ||
		toks[i-1].TokenType == "D_AT" || toks[i-1].Text == "@") {
		return true
	}
	return i >= 2 && keywordsAt(toks, i-2, "SQL", "SECURITY")
}

func createLiveViewNameRefs(e Engine, _ AST, sql string, toks []rawToken) ([]NameRef, error) {
	grammar, ok := parseLiveViewGrammar(e, sql, toks)
	if !ok {
		return nil, nil
	}
	refs := grammar.refs

	// The independently parsed exact query tail is the only source authority.
	// A visitor error invalidates D2 attribution instead of falling back to a
	// lossy target-only annotation.
	sources, err := CollectEmbeddedReadSources(grammar.queryAST)
	if err != nil {
		return nil, err
	}
	return append(refs, readSourceNameRefs(e, sources)...), nil
}

// parseLiveViewGrammar mirrors pinned ClickHouse v25.8.32.4 order:
//
//	(CREATE|ATTACH) [security] LIVE VIEW [IF NOT EXISTS] target [UUID s]
//	[ON CLUSTER c] [TO target [UUID s]] [(nonempty columns)]
//	[security when absent before LIVE] AS <SelectWithUnion> [COMMENT s]
//
// Identifier parameters occupy a valid target position but do not produce a
// NameRef. They are skipped precisely and never suppress concrete targets
// before or after them. ON CLUSTER deliberately excludes parameters: the
// pinned parser uses its parameter-disabled ParserIdentifier there.
func parseLiveViewGrammar(e Engine, sql string, toks []rawToken) (liveViewGrammar, bool) {
	var grammar liveViewGrammar
	if !keywordAt(toks, 0, "CREATE") && !keywordAt(toks, 0, "ATTACH") {
		return grammar, false
	}
	i := 1
	afterSecurity, securityBefore, trustworthy := liveViewSQLSecurityEnd(toks, i)
	if securityBefore {
		if !trustworthy {
			return grammar, false
		}
		i = afterSecurity
	}
	if !keywordsAt(toks, i, "LIVE", "VIEW") {
		return grammar, false
	}
	i = skipIfNotExists(toks, i+2)

	target, afterTarget, concrete, valid := liveViewTargetAt(e, sql, toks, i)
	if !valid {
		return grammar, false
	}
	if concrete {
		grammar.refs = append(grammar.refs, target)
	}
	i = afterTarget
	if i, valid = liveViewUUIDEnd(toks, i); !valid {
		return liveViewGrammar{}, false
	}
	if keywordsAt(toks, i, "ON", "CLUSTER") {
		if i, valid = liveViewClusterEnd(toks, i); !valid {
			return liveViewGrammar{}, false
		}
	}
	if keywordAt(toks, i, "TO") {
		to, afterTO, toConcrete, targetValid := liveViewTargetAt(e, sql, toks, i+1)
		if !targetValid {
			return liveViewGrammar{}, false
		}
		if toConcrete {
			grammar.refs = append(grammar.refs, to)
		}
		i = afterTO
		if i, valid = liveViewUUIDEnd(toks, i); !valid {
			return liveViewGrammar{}, false
		}
	}
	if i < len(toks) && toks[i].TokenType == "L_PAREN" {
		if i, valid = liveViewColumnsEnd(e, sql, toks, i); !valid {
			return liveViewGrammar{}, false
		}
	}
	if securityBefore {
		if _, present, _ := liveViewSQLSecurityEnd(toks, i); present {
			return liveViewGrammar{}, false
		}
	} else if after, present, ok := liveViewSQLSecurityEnd(toks, i); present {
		if !ok {
			return liveViewGrammar{}, false
		}
		i = after
	}
	if !keywordAt(toks, i, "AS") {
		return liveViewGrammar{}, false
	}
	queryStart := i + 1
	queryEnd := liveViewStatementEnd(toks)
	if queryEnd >= queryStart+2 && keywordAt(toks, queryEnd-2, "COMMENT") && isStringLiteralToken(toks[queryEnd-1]) {
		queryEnd -= 2
	}
	if queryStart >= queryEnd {
		return liveViewGrammar{}, false
	}
	queryAST, valid := parseLiveViewQueryExact(e, sql, toks, queryStart, queryEnd)
	if !valid {
		return liveViewGrammar{}, false
	}
	grammar.queryAST = queryAST
	return grammar, true
}

func liveViewStatementEnd(toks []rawToken) int {
	i := len(toks)
	for i > 0 && (toks[i-1].TokenType == "SEMICOLON" || toks[i-1].Text == ";") {
		i--
	}
	return i
}

func liveViewTargetAt(e Engine, sql string, toks []rawToken, i int) (NameRef, int, bool, bool) {
	if ref, end, ok := tableRefAt(e, sql, toks, i, false); ok {
		return ref, end, true, true
	}
	if end, opaque := opaqueTableRefEnd(toks, i); opaque {
		return NameRef{}, end, false, true
	}
	return NameRef{}, i, false, false
}

func liveViewUUIDEnd(toks []rawToken, i int) (int, bool) {
	if !keywordAt(toks, i, "UUID") {
		return i, true
	}
	if i+1 >= len(toks) {
		return i, false
	}
	value, ok := liveViewStringLiteralValue(toks[i+1])
	if !ok || !isLiveViewUUID(strings.ToLower(value)) {
		return i, false
	}
	return i + 2, true
}

func isLiveViewUUID(value string) bool {
	if isUUID(value) {
		return true
	}
	if len(value) != 32 {
		return false
	}
	for _, r := range value {
		if !isHex(r) {
			return false
		}
	}
	return true
}

// liveViewStringLiteralValue returns the tokenizer-authoritative semantic
// value for the literal forms accepted by UUID. STRING text is already
// decoded. DOLLAR_STRING encodes a non-empty tag as "tag\x00contents" and an
// empty tag as contents, so only that tokenizer-owned separator is removed.
func liveViewStringLiteralValue(tok rawToken) (string, bool) {
	switch tok.TokenType {
	case "STRING", "HEREDOC_STRING", "HEREDOC_STRING_ALTERNATIVE":
		return tok.Text, true
	case "DOLLAR_STRING":
		if split := strings.IndexByte(tok.Text, 0); split >= 0 {
			return tok.Text[split+1:], true
		}
		return tok.Text, true
	default:
		return "", false
	}
}

func nonEmptyStringLiteral(tok rawToken) bool {
	value, ok := liveViewStringLiteralValue(tok)
	return ok && value != ""
}

// liveViewSQLSecurityEnd consumes ParserSQLSecurity's two optional pieces in
// either accepted order: DEFINER[=]user[@host] and SQL SECURITY <mode>. The
// user/host positions accept one identifier or non-empty string literal; in
// particular bare AS/TO are values here and must not become LIVE VIEW clause
// delimiters. Query parameters are forbidden by ClickHouse for this grammar.
func liveViewSQLSecurityEnd(toks []rawToken, i int) (end int, present, trustworthy bool) {
	start := i
	seenDefiner := false
	seenSecurity := false
	for {
		switch {
		case !seenDefiner && keywordAt(toks, i, "DEFINER"):
			present = true
			seenDefiner = true
			i++
			if i < len(toks) && (toks[i].TokenType == "EQ" || toks[i].Text == "=") {
				i++
			}
			var ok bool
			i, ok = liveViewSecurityUserEnd(toks, i)
			if !ok {
				return start, true, false
			}
		case !seenSecurity && keywordsAt(toks, i, "SQL", "SECURITY"):
			present = true
			seenSecurity = true
			if !keywordAt(toks, i+2, "DEFINER") && !keywordAt(toks, i+2, "INVOKER") && !keywordAt(toks, i+2, "NONE") {
				return start, true, false
			}
			i += 3
		default:
			if !present {
				return start, false, true
			}
			return i, true, true
		}
	}
}

func liveViewSecurityUserEnd(toks []rawToken, i int) (int, bool) {
	if i < 0 || i >= len(toks) {
		return i, false
	}
	if _, parameter := identifierParameterEnd(toks, i); parameter {
		return i, false
	}
	if isStringLiteralToken(toks[i]) {
		value, ok := liveViewStringLiteralValue(toks[i])
		return i + 1, ok && value != "" && !startsIdentifierQueryParameter(value)
	}
	// ParserSQLSecurity handles bare CURRENT_USER before the general
	// ParserUserNameWithHost path. It is therefore the whole user and cannot
	// take @host; leaving any following @ token unconsumed makes the surrounding
	// LIVE VIEW grammar fail exactly as ClickHouse does.
	if keywordAt(toks, i, "CURRENT_USER") {
		return i + 1, true
	}
	// A quoted identifier is one semantic username regardless of punctuation
	// inside it. An @ preserved in the tokenizer value is content, while a
	// syntactically separate @ token still introduces a host.
	if toks[i].TokenType == "QUOTED_IDENTIFIER" {
		if toks[i].Text == "" {
			return i, false
		}
		end := i + 1
		if end < len(toks) && (toks[end].TokenType == "D_AT" || toks[end].Text == "@") {
			return liveViewSecurityHostEnd(toks, end+1)
		}
		if end < len(toks) && strings.HasPrefix(toks[end].Text, "@") {
			host := strings.TrimPrefix(toks[end].Text, "@")
			return end + 1, host != "" && !strings.Contains(host, "@") && isIdentifierText(host)
		}
		return end, true
	}
	text := toks[i].Text
	if at := strings.IndexByte(text, '@'); at >= 0 {
		if strings.EqualFold(text[:at], "CURRENT_USER") {
			return i, false
		}
		if strings.Count(text, "@") != 1 || at == 0 || !isIdentifierText(text[:at]) {
			return i, false
		}
		if host := text[at+1:]; host != "" {
			return i + 1, isIdentifierText(host)
		}
		return liveViewSecurityHostEnd(toks, i+1)
	}
	if !isIdentifierCandidate(toks[i]) {
		return i, false
	}
	end := i + 1
	if end < len(toks) && (toks[end].TokenType == "D_AT" || toks[end].Text == "@") {
		return liveViewSecurityHostEnd(toks, end+1)
	}
	return end, true
}

// startsIdentifierQueryParameter mirrors ParserUserNameWithHost's special
// mistake guard for a quoted username. ClickHouse tokenizes the decoded string
// and rejects when ParserIdentifier(true) can consume an Identifier query
// parameter at byte zero; it deliberately does not require end-of-input, so a
// suffix remains a rejection while leading space or a spaced/lowercase type is
// ordinary string content.
func startsIdentifierQueryParameter(value string) bool {
	if len(value) < len("{x:Identifier}") || value[0] != '{' {
		return false
	}
	colon := strings.IndexByte(value, ':')
	if colon <= 1 || !strings.HasPrefix(value[colon:], ":Identifier}") {
		return false
	}
	return isIdentifierText(value[1:colon])
}

func liveViewSecurityHostEnd(toks []rawToken, i int) (int, bool) {
	if i < 0 || i >= len(toks) {
		return i, false
	}
	if isStringLiteralToken(toks[i]) {
		return i + 1, nonEmptyStringLiteral(toks[i])
	}
	if isIdentifierCandidate(toks[i]) {
		return i + 1, true
	}
	return i, false
}

func isIdentifierText(text string) bool {
	return isIdentifierCandidate(rawToken{Text: text, TokenType: "VAR"})
}

// liveViewClusterEnd consumes CREATE LIVE VIEW's ON CLUSTER value before the
// raw adapter looks for target-level TO/AS. ClickHouse v25.8 accepts exactly
// one non-empty parameter-disabled ParserIdentifier or string literal here, so
// a BareWord may itself be TO/AS and a quoted identifier may contain dots.
// Identifier query parameters make this whole LIVE VIEW shape unproven.
func liveViewClusterEnd(toks []rawToken, i int) (int, bool) {
	if !keywordsAt(toks, i, "ON", "CLUSTER") {
		return i, true
	}
	i += 2
	if _, parameter := identifierParameterEnd(toks, i); parameter {
		return i, false
	}
	if i >= len(toks) {
		return i, false
	}
	if isStringLiteralToken(toks[i]) {
		return i + 1, nonEmptyStringLiteral(toks[i])
	}
	if !isIdentifierCandidate(toks[i]) {
		return i, false
	}
	return i + 1, true
}

// liveViewColumnsEnd validates the exact parenthesized token span through the
// existing ClickHouse parser. Polyglot represents a missing column type as
// no_type; ClickHouse permits that only when a default/materialized/alias
// expression supplies the type. Requiring the synthetic ENGINE property also
// proves the parser consumed through the end of the declaration list.
func liveViewColumnsEnd(e Engine, sql string, toks []rawToken, i int) (int, bool) {
	end, ok := balancedTokenGroupEnd(toks, i, "L_PAREN", "R_PAREN")
	if !ok || end == i+2 || toks[end-2].TokenType == "COMMA" {
		return i, false
	}
	startByte, endByte := toks[i].Span.Start, toks[end-1].Span.End
	if startByte < 0 || endByte <= startByte || endByte > len(sql) {
		return i, false
	}
	probe, err := e.ParseOne("CREATE TABLE __hg_live_view_columns_probe__ " + sql[startByte:endByte] + " ENGINE=Memory")
	if err != nil || !validLiveViewColumnsProbe(probe) {
		return i, false
	}
	return end, true
}

func validLiveViewColumnsProbe(ast AST) bool {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return false
	}
	body, ok := root[NodeCreateTable].(map[string]any)
	if !ok {
		return false
	}
	name, ok := body["name"].(map[string]any)
	if !ok || name["schema"] != nil || name["catalog"] != nil {
		return false
	}
	identifier, ok := name["name"].(map[string]any)
	if !ok || identifier["name"] != "__hg_live_view_columns_probe__" {
		return false
	}
	columns, columnsOK := body["columns"].([]any)
	if !columnsOK {
		return false
	}
	hasDeclaration := len(columns) > 0
	for _, raw := range columns {
		column, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		noType, _ := column["no_type"].(bool)
		if noType {
			hasSupplyingExpression := false
			for _, key := range []string{"default", "materialized_expr", "alias_expr", "ephemeral"} {
				hasSupplyingExpression = hasSupplyingExpression || column[key] != nil
			}
			if !hasSupplyingExpression {
				return false
			}
		}
	}
	properties, ok := body["properties"].([]any)
	if !ok || len(properties) != 1 {
		return false
	}
	property, ok := properties[0].(map[string]any)
	if !ok {
		return false
	}
	engineProperty, ok := property["engine_property"].(map[string]any)
	if !ok {
		return false
	}
	engineThis, ok := engineProperty["this"].(map[string]any)
	if !ok {
		return false
	}
	engineIdentifier, ok := engineThis["identifier"].(map[string]any)
	if !ok || engineIdentifier["name"] != "Memory" {
		return false
	}
	for _, key := range []string{"constraints", "indexes", "projections", "foreign_keys"} {
		if raw, present := body[key]; present && raw != nil {
			values, ok := raw.([]any)
			if !ok {
				return false
			}
			hasDeclaration = hasDeclaration || len(values) > 0
		}
	}
	if body["primary_key"] != nil {
		hasDeclaration = true
	}
	return hasDeclaration
}

const liveViewQueryProbeAlias = "__hg_live_view_query_probe__"

// parseLiveViewQueryExact rejects suffixes silently ignored by ParseOne. The
// query is also parsed inside a synthetic subquery whose closing parenthesis
// and sentinel alias can exist in the AST only when the inner SelectWithUnion
// consumed its complete input. This retains genuine SELECT tails such as
// SETTINGS and functions ending in ')' while preventing post-AS security,
// FORMAT, or garbage from being mistaken for valid LIVE VIEW grammar.
func parseLiveViewQueryExact(e Engine, sql string, toks []rawToken, start, end int) (AST, bool) {
	ranges, parallel, valid := liveViewParallelQueryRanges(toks, start, end)
	if !valid {
		return nil, false
	}
	if parallel {
		parts := make([]json.RawMessage, 0, len(ranges))
		for _, queryRange := range ranges {
			ast, ok := parseLiveViewSingleQueryExact(e, sql, toks, queryRange[0], queryRange[1])
			if !ok {
				return nil, false
			}
			parts = append(parts, json.RawMessage(ast))
		}
		combined, err := json.Marshal(parts)
		return AST(combined), err == nil
	}
	return parseLiveViewSingleQueryExact(e, sql, toks, start, end)
}

// liveViewParallelQueryRanges splits ClickHouse's top-level
// `<select> PARALLEL WITH <select>` chain without treating nested expression
// tokens as delimiters. Each member is then independently proven by the same
// exact SelectWithUnion wrapper below, and the resulting AST roots are walked
// as one ordered array.
func liveViewParallelQueryRanges(toks []rawToken, start, end int) ([][2]int, bool, bool) {
	if start < 0 || end <= start || end > len(toks) {
		return nil, false, false
	}
	ranges := make([][2]int, 0, 2)
	segmentStart := start
	closers := make([]string, 0, 2)
	for i := start; i < end; i++ {
		switch toks[i].TokenType {
		case "L_PAREN":
			closers = append(closers, "R_PAREN")
			continue
		case "L_BRACKET":
			closers = append(closers, "R_BRACKET")
			continue
		case "L_BRACE":
			closers = append(closers, "R_BRACE")
			continue
		case "R_PAREN", "R_BRACKET", "R_BRACE":
			if len(closers) == 0 || closers[len(closers)-1] != toks[i].TokenType {
				return nil, false, false
			}
			closers = closers[:len(closers)-1]
			continue
		}
		if len(closers) == 0 &&
			liveViewKeywordPairIsDelimiter(toks, start, i, "PARALLEL", "WITH") &&
			liveViewQueryMemberStartsAt(toks, i+2, end) {
			if segmentStart == i || i+2 >= end {
				return nil, true, false
			}
			ranges = append(ranges, [2]int{segmentStart, i})
			segmentStart = i + 2
			i++
		}
	}
	if len(closers) != 0 {
		return nil, len(ranges) > 0, false
	}
	if len(ranges) == 0 {
		return nil, false, true
	}
	ranges = append(ranges, [2]int{segmentStart, end})
	return ranges, true, true
}

func liveViewQueryMemberStartsAt(toks []rawToken, i, end int) bool {
	if i < 0 || i >= end {
		return false
	}
	return keywordAt(toks, i, "SELECT") || keywordAt(toks, i, "WITH") || keywordAt(toks, i, "FROM")
}

func parseLiveViewSingleQueryExact(e Engine, sql string, toks []rawToken, start, end int) (AST, bool) {
	if start < 0 || end <= start || end > len(toks) {
		return nil, false
	}
	startByte, endByte := toks[start].Span.Start, toks[end-1].Span.End
	if startByte < 0 || endByte <= startByte || endByte > len(sql) {
		return nil, false
	}
	query := sql[startByte:endByte]
	if ast, _, ok := parseLiveViewQueryCandidate(e, query); ok {
		return ast, true
	}

	// Polyglot's pinned SELECT parser rejects an Identifier parameter in explicit
	// and implicit alias positions even though ClickHouse accepts those aliases.
	// A narrow probe substitution lets the existing AST visitor retain later
	// proven sources. Every substitute is then required to occur only in an AST
	// alias field and is marked opaque; CAST/type/query parameters cannot be
	// promoted accidentally by this compatibility seam.
	adapted, candidates, ok := adaptLiveViewOpaqueAliases(sql, toks, start, end)
	if !ok {
		return nil, false
	}
	probeAST, probeWrapper, ok := parseLiveViewQueryCandidate(e, adapted)
	if !ok {
		return nil, false
	}
	probeRoles := liveViewAliasSentinels(probeAST, candidates)
	wrapperRoles := liveViewAliasSentinels(probeWrapper, candidates)
	aliases := make(map[string]bool)
	for sentinel := range probeRoles {
		if wrapperRoles[sentinel] {
			aliases[sentinel] = true
		}
	}
	adapted, ok = renderLiveViewOpaqueAliases(sql, toks, start, end, candidates, aliases)
	if !ok {
		return nil, false
	}
	ast, wrapper, ok := parseLiveViewQueryCandidate(e, adapted)
	if !ok {
		return nil, false
	}
	ast, astOK := markLiveViewOpaqueAliases(ast, aliases)
	wrapper, wrapperOK := markLiveViewOpaqueAliases(wrapper, aliases)
	if !astOK || !wrapperOK || !liveViewQueryWrapperComplete(wrapper) {
		return nil, false
	}
	return ast, true
}

func parseLiveViewQueryCandidate(e Engine, query string) (AST, AST, bool) {
	ast, err := e.ParseOne(query)
	if err != nil || !isSelectWithUnionKind(ast) {
		return nil, nil, false
	}
	wrapper, err := e.ParseOne("SELECT * FROM (" + query + ") AS " + liveViewQueryProbeAlias)
	if err != nil || !liveViewQueryWrapperComplete(wrapper) {
		return nil, nil, false
	}
	return ast, wrapper, true
}

type liveViewOpaqueAliasCandidate struct {
	sentinel string
	start    int
	end      int
}

func adaptLiveViewOpaqueAliases(sql string, toks []rawToken, start, end int) (string, []liveViewOpaqueAliasCandidate, bool) {
	startByte, endByte := toks[start].Span.Start, toks[end-1].Span.End
	if startByte < 0 || endByte <= startByte || endByte > len(sql) {
		return "", nil, false
	}
	var candidates []liveViewOpaqueAliasCandidate
	used := make(map[string]bool)
	for i := start; i < end; i++ {
		parameterStart := i
		parameterEnd, parameter := identifierParameterEnd(toks, parameterStart)
		if keywordAt(toks, i, "AS") {
			parameterStart = i + 1
			parameterEnd, parameter = identifierParameterEnd(toks, parameterStart)
		} else if parameter {
			parameter = isImplicitAliasParameter(toks, start, parameterStart, parameterEnd, end)
		}
		if !parameter || parameterEnd > end {
			continue
		}
		parameterStartByte, parameterEndByte := toks[parameterStart].Span.Start, toks[parameterEnd-1].Span.End
		if parameterStartByte < startByte || parameterEndByte <= parameterStartByte || parameterEndByte > endByte {
			return "", nil, false
		}
		sentinel := "__hg_live_view_opaque_alias_" + strconv.Itoa(len(candidates)) + "__"
		for strings.Contains(sql, sentinel) || used[sentinel] {
			sentinel += "_"
		}
		used[sentinel] = true
		candidates = append(candidates, liveViewOpaqueAliasCandidate{
			sentinel: sentinel,
			start:    parameterStartByte,
			end:      parameterEndByte,
		})
		i = parameterEnd - 1
	}
	all := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		all[candidate.sentinel] = true
	}
	adapted, ok := renderLiveViewOpaqueAliases(sql, toks, start, end, candidates, all)
	return adapted, candidates, ok
}

func renderLiveViewOpaqueAliases(
	sql string,
	toks []rawToken,
	start, end int,
	candidates []liveViewOpaqueAliasCandidate,
	selected map[string]bool,
) (string, bool) {
	startByte, endByte := toks[start].Span.Start, toks[end-1].Span.End
	if startByte < 0 || endByte <= startByte || endByte > len(sql) {
		return "", false
	}
	type replacement struct {
		start int
		end   int
		text  string
	}
	replacements := make([]replacement, 0, len(candidates)+1)
	for _, candidate := range candidates {
		if selected[candidate.sentinel] {
			replacements = append(replacements, replacement{
				start: candidate.start,
				end:   candidate.end,
				text:  candidate.sentinel,
			})
		}
	}
	// ClickHouse 25.8 accepts ONLY JOIN as the legacy spelling of ANTI LEFT
	// JOIN, while the pinned Polyglot parser silently truncates it. Normalize
	// exactly that lexical pair inside the same source-span renderer used for
	// opaque aliases; the exact wrapper then proves the full adapted query.
	for i := start; i+1 < end; i++ {
		switch {
		case liveViewLocalJoinModifierAt(toks, start, i, end):
			replacements = append(replacements, replacement{
				start: toks[i].Span.Start,
				end:   toks[i].Span.End,
				text:  "",
			})
		case liveViewKeywordPairIsDelimiter(toks, start, i, "ONLY", "JOIN"):
			replacements = append(replacements, replacement{
				start: toks[i].Span.Start,
				end:   toks[i+1].Span.End,
				text:  "ANTI LEFT JOIN",
			})
			i++
		case keywordsAt(toks, i, "ONLY", "JOIN") && liveViewKeywordStartsBareTable(toks, start, i):
			// The pinned Polyglot parser treats bare ONLY as the legacy join
			// modifier even where ClickHouse's grammar first consumes a table
			// name. Quote only the probe token so its semantic object name is
			// retained without changing the original SQL.
			replacements = append(replacements, replacement{
				start: toks[i].Span.Start,
				end:   toks[i].Span.End,
				text:  "`" + sql[toks[i].Span.Start:toks[i].Span.End] + "`",
			})
		}
	}
	if len(replacements) == 0 {
		return "", false
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start < replacements[j].start })

	var out strings.Builder
	cursor := startByte
	for _, replacement := range replacements {
		if replacement.start < cursor || replacement.end <= replacement.start || replacement.end > endByte {
			return "", false
		}
		out.WriteString(sql[cursor:replacement.start])
		out.WriteString(replacement.text)
		cursor = replacement.end
	}
	out.WriteString(sql[cursor:endByte])
	return out.String(), true
}

// liveViewKeywordPairIsDelimiter distinguishes compatibility syntax from an
// unquoted identifier that happens to use the same keyword. ClickHouse accepts
// ONLY and PARALLEL as table components after FROM/JOIN/DOT and as explicit
// aliases after AS; a comma introduces the same table-name position. Rewriting
// or splitting those pairs would fabricate a different object or query.
func liveViewKeywordPairIsDelimiter(toks []rawToken, start, i int, words ...string) bool {
	if !keywordsAt(toks, i, words...) || i <= start {
		return false
	}
	previous := toks[i-1]
	if previous.TokenType == "DOT" || previous.TokenType == "COMMA" {
		return false
	}
	for _, word := range []string{"FROM", "JOIN", "AS"} {
		if keywordAt(toks, i-1, word) {
			return false
		}
	}
	return true
}

func liveViewKeywordStartsBareTable(toks []rawToken, start, i int) bool {
	if i <= start {
		return false
	}
	if toks[i-1].TokenType == "COMMA" {
		return true
	}
	return keywordAt(toks, i-1, "FROM") || keywordAt(toks, i-1, "JOIN")
}

func liveViewLocalJoinModifierAt(toks []rawToken, start, i, end int) bool {
	if !keywordAt(toks, i, "LOCAL") || i <= start {
		return false
	}
	previous := toks[i-1]
	if previous.TokenType == "DOT" || previous.TokenType == "COMMA" {
		return false
	}
	for _, word := range []string{"FROM", "JOIN", "AS"} {
		if keywordAt(toks, i-1, word) {
			return false
		}
	}
	for j := i + 1; j < end && j <= i+3; j++ {
		if keywordAt(toks, j, "JOIN") {
			return true
		}
		allowed := false
		for _, word := range []string{"ANY", "ALL", "ASOF", "SEMI", "ANTI", "PASTE", "INNER", "LEFT", "RIGHT", "FULL", "CROSS"} {
			if keywordAt(toks, j, word) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return false
}

func liveViewAliasSentinels(ast AST, candidates []liveViewOpaqueAliasCandidate) map[string]bool {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil
	}
	known := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		known[candidate.sentinel] = true
	}
	seen := make(map[string]bool, len(candidates))
	aliasOnly := make(map[string]bool, len(candidates))
	for sentinel := range known {
		aliasOnly[sentinel] = true
	}
	var visit func(any, string)
	visit = func(node any, field string) {
		switch n := node.(type) {
		case map[string]any:
			if name, _ := n["name"].(string); known[name] {
				seen[name] = true
				if field != "alias" && field != "column_aliases" {
					aliasOnly[name] = false
				}
			}
			for key, child := range n {
				visit(child, key)
			}
		case []any:
			for _, child := range n {
				visit(child, field)
			}
		}
	}
	visit(root, "")
	result := make(map[string]bool)
	for sentinel := range known {
		if seen[sentinel] && aliasOnly[sentinel] {
			result[sentinel] = true
		}
	}
	return result
}

func isImplicitAliasParameter(toks []rawToken, start, parameterStart, parameterEnd, end int) bool {
	if parameterStart <= start || parameterEnd > end {
		return false
	}
	previous := toks[parameterStart-1]
	if previous.TokenType == "DOT" || previous.TokenType == "COLON" || previous.TokenType == "L_BRACE" ||
		previous.TokenType == "COMMA" {
		return false
	}
	if parameterEnd == end {
		return true
	}
	next := toks[parameterEnd]
	if next.TokenType == "COMMA" || next.TokenType == "L_PAREN" || next.TokenType == "R_PAREN" ||
		next.TokenType == "SEMICOLON" || next.Text == ";" {
		return true
	}
	for _, boundary := range []string{
		"FROM", "JOIN", "GLOBAL", "LOCAL", "ANY", "ALL", "ASOF", "SEMI", "ANTI", "PASTE",
		"INNER", "LEFT", "RIGHT", "FULL", "CROSS", "ARRAY", "ONLY", "ON", "USING", "WITH",
		"FINAL", "SAMPLE", "PREWHERE", "WHERE", "GROUP", "HAVING", "WINDOW", "QUALIFY", "ORDER", "LIMIT", "OFFSET", "SETTINGS",
		"UNION", "INTERSECT", "EXCEPT", "PARALLEL",
	} {
		if keywordAt(toks, parameterEnd, boundary) {
			return true
		}
	}
	return false
}

func markLiveViewOpaqueAliases(ast AST, sentinels map[string]bool) (AST, bool) {
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, false
	}
	seen := make(map[string]bool, len(sentinels))
	var visit func(any, string) bool
	visit = func(node any, field string) bool {
		switch n := node.(type) {
		case map[string]any:
			if name, _ := n["name"].(string); sentinels[name] {
				if field != "alias" && field != "column_aliases" {
					return false
				}
				n[opaqueIdentifierParameterKey] = true
				seen[name] = true
			}
			for key, child := range n {
				if !visit(child, key) {
					return false
				}
			}
		case []any:
			for _, child := range n {
				if !visit(child, field) {
					return false
				}
			}
		}
		return true
	}
	if !visit(root, "") || len(seen) != len(sentinels) {
		return nil, false
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return nil, false
	}
	return AST(encoded), true
}

func liveViewQueryWrapperComplete(ast AST) bool {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return false
	}
	selectBody, ok := root[NodeSelect].(map[string]any)
	if !ok {
		return false
	}
	from, ok := selectBody["from"].(map[string]any)
	if !ok {
		return false
	}
	expressions, ok := from["expressions"].([]any)
	if !ok || len(expressions) != 1 {
		return false
	}
	expression, ok := expressions[0].(map[string]any)
	if !ok {
		return false
	}
	subquery, ok := expression["subquery"].(map[string]any)
	if !ok || subquery["this"] == nil {
		return false
	}
	alias, ok := subquery["alias"].(map[string]any)
	if !ok {
		return false
	}
	name, ok := alias["name"].(string)
	return ok && name == liveViewQueryProbeAlias
}

func isSelectWithUnionKind(ast AST) bool {
	kind, err := NodeKind(ast)
	if err == nil && (kind == NodeSelect || kind == NodeUnion || kind == NodeIntersect || kind == NodeExcept) {
		return true
	}
	// Parenthesized SelectWithUnion parses as one top-level subquery wrapper.
	// Keep the wrapper (the ordered read visitor already understands it), but
	// require its body to be a complete select/set root and forbid alias/limit
	// decorations that are not part of the AS query itself.
	var root map[string]any
	if json.Unmarshal(ast, &root) != nil || len(root) != 1 {
		return false
	}
	subquery, ok := root["subquery"].(map[string]any)
	if !ok || !cteBodyIsReadQuery(subquery["this"]) {
		return false
	}
	return subquery["alias"] == nil && subquery["order_by"] == nil &&
		subquery["limit"] == nil && subquery["offset"] == nil
}

func balancedTokenGroupEnd(toks []rawToken, i int, open, close string) (int, bool) {
	if i < 0 || i >= len(toks) || toks[i].TokenType != open {
		return i, false
	}
	depth := 0
	for ; i < len(toks); i++ {
		switch toks[i].TokenType {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i + 1, true
			}
			if depth < 0 {
				return i, false
			}
		}
	}
	return i, false
}

func readSourceNameRefs(e Engine, sources []ReadSourceRef) []NameRef {
	refs := make([]NameRef, 0, len(sources))
	for _, source := range sources {
		db, dbOK := readSourceName(e, source.Target.DB, source.databaseIdentifier)
		table, tableOK := readSourceName(e, source.Target.Table, source.tableIdentifier)
		switch source.Kind {
		case ReadSourceTable:
			if tableOK && table != "" && (source.Target.DB == "" || dbOK) {
				refs = append(refs, NameRef{Kind: NameRefTable, DB: db, Table: table})
			}
		case ReadSourceTableFunction, ReadSourceInTable:
			switch {
			case source.Resolved && dbOK && db != "" && tableOK && table != "":
				refs = append(refs, NameRef{Kind: NameRefTable, DB: db, Table: table})
			case source.UsesCurrentDatabase && tableOK && table != "":
				refs = append(refs, NameRef{Kind: NameRefTable, Table: table})
			case !source.Resolved && dbOK && db != "":
				// A known database with a dynamic table still proves a namespace.
				refs = append(refs, NameRef{Kind: NameRefDatabase, DB: db})
			}
		}
	}
	return refs
}

func readSourceName(e Engine, name string, identifier bool) (string, bool) {
	if !identifier {
		return name, true // parser literal values are already semantic
	}
	return decodedASTIdentifier(e, name)
}

func tableRefAt(e Engine, sql string, toks []rawToken, i int, allowString bool) (NameRef, int, bool) {
	if i >= len(toks) {
		return NameRef{}, i, false
	}
	if _, parameter := identifierParameterEnd(toks, i); parameter {
		return NameRef{}, i, false
	}
	if i+2 < len(toks) && toks[i+1].TokenType == "DOT" {
		if _, parameter := identifierParameterEnd(toks, i+2); parameter {
			// A concrete prefix plus an opaque Identifier parameter is still one
			// opaque object. Never invent the prefix as a bare table/database.
			return NameRef{}, i, false
		}
	}
	if allowString && isStringLiteralToken(toks[i]) {
		name, ok := liveViewStringLiteralValue(toks[i]) // tokenizer-authoritative semantic value
		if !ok || name == "" {
			return NameRef{}, i, false
		}
		if dot := strings.IndexByte(name, '.'); dot >= 0 {
			if dot == 0 || dot == len(name)-1 {
				return NameRef{}, i, false
			}
			return NameRef{Kind: NameRefTable, DB: name[:dot], Table: name[dot+1:]}, i + 1, true
		}
		return NameRef{Kind: NameRefTable, Table: name}, i + 1, true
	}
	end, ok := nameRunEnd(toks, i)
	if !ok {
		return NameRef{}, i, false
	}
	startByte, endByte := toks[i].Span.Start, toks[end-1].Span.End
	if startByte < 0 || endByte <= startByte || endByte > len(sql) {
		return NameRef{}, i, false
	}
	ast, err := e.ParseOne("SELECT * FROM " + sql[startByte:endByte])
	if err == nil {
		tables, collectErr := CollectSelectTables(ast)
		if collectErr == nil && len(tables) == 1 && tables[0].Table != "" {
			db, dbOK := decodedASTIdentifier(e, tables[0].DB)
			name, nameOK := decodedASTIdentifier(e, tables[0].Table)
			if dbOK && nameOK && name != "" {
				return NameRef{Kind: NameRefTable, DB: db, Table: name}, end, true
			}
		}
	}

	// A keyword in the first component can make the synthetic FROM ambiguous
	// even though ClickHouse's ParserIdentifier accepts it after a dot. Parse
	// each grammar-proven component behind a neutral database name and retain
	// only the parser-produced identifier values.
	first, ok := parsedIdentifierAt(e, sql, toks[i])
	if !ok {
		return NameRef{}, i, false
	}
	if end == i+1 {
		return NameRef{Kind: NameRefTable, Table: first}, end, true
	}
	second, ok := parsedIdentifierAt(e, sql, toks[i+2])
	if !ok {
		return NameRef{}, i, false
	}
	return NameRef{Kind: NameRefTable, DB: first, Table: second}, end, true
}

// opaqueTableRefEnd consumes a valid one- or two-part table target containing
// at least one {x:Identifier} component. The target is syntactically known but
// has no concrete NameRef; callers continue after it so later proven objects
// retain their source order.
func opaqueTableRefEnd(toks []rawToken, i int) (int, bool) {
	if firstEnd, firstOpaque := identifierParameterEnd(toks, i); firstOpaque {
		if firstEnd < len(toks) && toks[firstEnd].TokenType == "DOT" {
			if secondEnd, secondOpaque := identifierParameterEnd(toks, firstEnd+1); secondOpaque {
				return secondEnd, true
			}
			if firstEnd+1 < len(toks) && isIdentifierCandidate(toks[firstEnd+1]) {
				return firstEnd + 2, true
			}
			return i, false
		}
		return firstEnd, true
	}
	if i+2 < len(toks) && isIdentifierCandidate(toks[i]) && toks[i+1].TokenType == "DOT" {
		if secondEnd, secondOpaque := identifierParameterEnd(toks, i+2); secondOpaque {
			return secondEnd, true
		}
	}
	return i, false
}

func databaseRefAt(e Engine, sql string, toks []rawToken, i int) (NameRef, int, bool) {
	if i >= len(toks) || !isIdentifierCandidate(toks[i]) {
		return NameRef{}, i, false
	}
	startByte, endByte := toks[i].Span.Start, toks[i].Span.End
	if startByte < 0 || endByte <= startByte || endByte > len(sql) {
		return NameRef{}, i, false
	}
	const probe = "__storage_integrity_name_probe"
	ast, err := e.ParseOne("SELECT * FROM " + sql[startByte:endByte] + "." + probe)
	if err == nil {
		tables, collectErr := CollectSelectTables(ast)
		if collectErr == nil && len(tables) == 1 && tables[0].DB != "" && tables[0].Table == probe {
			if name, ok := decodedASTIdentifier(e, tables[0].DB); ok && name != "" {
				return NameRef{Kind: NameRefDatabase, DB: name}, i + 1, true
			}
		}
	}
	name, ok := parsedIdentifierAt(e, sql, toks[i])
	if !ok {
		return NameRef{}, i, false
	}
	return NameRef{Kind: NameRefDatabase, DB: name}, i + 1, true
}

func parsedIdentifierAt(e Engine, sql string, tok rawToken) (string, bool) {
	startByte, endByte := tok.Span.Start, tok.Span.End
	if startByte < 0 || endByte <= startByte || endByte > len(sql) {
		return "", false
	}
	const probeDB = "__storage_integrity_name_probe"
	ast, err := e.ParseOne("SELECT * FROM " + probeDB + "." + sql[startByte:endByte])
	if err != nil {
		return "", false
	}
	tables, err := CollectSelectTables(ast)
	if err != nil || len(tables) != 1 || tables[0].DB != probeDB || tables[0].Table == "" {
		return "", false
	}
	return decodedASTIdentifier(e, tables[0].Table)
}

// decodedASTIdentifier keeps parsed identifier values authoritative. Polyglot
// already resolves identifier quote-doubling in the AST. Its ClickHouse table
// AST currently preserves backslash escapes, so the narrow escaped case is
// decoded through a parsed string literal. ClassifyLiveView thereby retains its
// one-tokenization contract instead of silently depending on a second lexer run.
func decodedASTIdentifier(e Engine, name string) (string, bool) {
	if name == "" {
		return "", true
	}
	if !strings.ContainsRune(name, '\\') {
		return name, true
	}
	// Preserve every backslash escape for the engine tokenizer. Escape only an
	// unescaped apostrophe that would otherwise terminate this synthetic string;
	// this is transport quoting, not a second implementation of ClickHouse's
	// identifier escape rules.
	var literal strings.Builder
	literal.Grow(len(name) + 2)
	literal.WriteByte('\'')
	backslashes := 0
	for i := 0; i < len(name); i++ {
		if name[i] == '\'' && backslashes%2 == 0 {
			literal.WriteByte('\\')
		}
		literal.WriteByte(name[i])
		if name[i] == '\\' {
			backslashes++
		} else {
			backslashes = 0
		}
	}
	literal.WriteByte('\'')
	ast, err := e.ParseOne("SELECT " + literal.String())
	if err != nil {
		return "", false
	}
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return "", false
	}
	selectBody, ok := root[NodeSelect].(map[string]any)
	if !ok {
		return "", false
	}
	expressions, ok := selectBody["expressions"].([]any)
	if !ok || len(expressions) != 1 {
		return "", false
	}
	expression, ok := expressions[0].(map[string]any)
	if !ok {
		return "", false
	}
	stringLiteral, ok := expression["literal"].(map[string]any)
	if !ok || stringLiteral["literal_type"] != "string" {
		return "", false
	}
	decoded, _ := stringLiteral["value"].(string)
	return decoded, decoded != ""
}

// SemanticIdentifier resolves parser-preserved ClickHouse identifier escapes
// to the name ClickHouse executes. Polyglot already resolves ordinary quoting
// but currently preserves backslash escapes in AST identifier fields.
func SemanticIdentifier(e Engine, name string) (string, bool) {
	return decodedASTIdentifier(e, name)
}

// SemanticTableTarget resolves parser-preserved ClickHouse identifier escapes
// in a TableTarget collected from the AST. Polyglot already resolves ordinary
// quoting but deliberately preserves backslash escapes in identifier names;
// storage-integrity policy must compare the semantic names ClickHouse executes
// (for example `\x64b1`.t is db1.t), not those preserved spellings.
func SemanticTableTarget(e Engine, target TableTarget) (TableTarget, bool) {
	db, dbOK := SemanticIdentifier(e, target.DB)
	table, tableOK := SemanticIdentifier(e, target.Table)
	if !dbOK || !tableOK || table == "" {
		return TableTarget{}, false
	}
	target.DB = db
	target.Table = table
	return target, true
}

func nameRunEnd(toks []rawToken, i int) (int, bool) {
	if i < 0 || i >= len(toks) || !isIdentifierCandidate(toks[i]) {
		return i, false
	}
	if i+2 < len(toks) && toks[i+1].TokenType == "DOT" && isIdentifierCandidate(toks[i+2]) {
		return i + 3, true
	}
	return i + 1, true
}

// isIdentifierCandidate admits any lexical BareWord, including tokens that the
// tokenizer labels as keywords. The surrounding command grammar establishes
// the object position; tableRefAt/databaseRefAt then make the parser the final
// authority. Restricting candidates to VAR would incorrectly reject valid
// ClickHouse names such as hg_safe.select.
func isIdentifierCandidate(tok rawToken) bool {
	if tok.TokenType == "QUOTED_IDENTIFIER" {
		return true
	}
	if isStringLiteralToken(tok) || tok.Text == "" {
		return false
	}
	for i, r := range tok.Text {
		isLetter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		isDigit := r >= '0' && r <= '9'
		if r == '_' || isLetter || i > 0 && isDigit {
			continue
		}
		return false
	}
	return true
}

func onlyStatementEnd(toks []rawToken, i int) bool {
	for ; i < len(toks); i++ {
		if toks[i].TokenType != "SEMICOLON" && toks[i].Text != ";" {
			return false
		}
	}
	return true
}

func skipIfExists(toks []rawToken, i int) int {
	if keywordsAt(toks, i, "IF", "EXISTS") {
		return i + 2
	}
	return i
}

func skipIfNotExists(toks []rawToken, i int) int {
	if keywordsAt(toks, i, "IF", "NOT", "EXISTS") {
		return i + 3
	}
	return i
}

func skipOnCluster(toks []rawToken, i int) (int, bool) {
	if !keywordsAt(toks, i, "ON", "CLUSTER") {
		return i, false
	}
	if end, ok := identifierParameterEnd(toks, i+2); ok {
		// Cluster is a non-object grammar role. Its Identifier parameter can be
		// skipped structurally without trying to turn the dynamic value into a
		// NameRef, allowing the real table target after it to retain D2 ordering.
		return end, true
	}
	if i+2 >= len(toks) {
		return i, false
	}
	if isStringLiteralToken(toks[i+2]) {
		return i + 3, nonEmptyStringLiteral(toks[i+2])
	}
	if !isIdentifierCandidate(toks[i+2]) {
		return i, false
	}
	return i + 3, true
}

// systemClusterEnd consumes ParserSystemQuery's concrete ON CLUSTER spelling.
// Unlike general DDL grammar, the pinned SYSTEM grammar does not accept an
// Identifier query parameter for the cluster position.
func systemClusterEnd(toks []rawToken, i int) (int, bool) {
	if !keywordsAt(toks, i, "ON", "CLUSTER") {
		return i, true
	}
	if _, parameter := identifierParameterEnd(toks, i+2); parameter {
		return i, false
	}
	if i+2 >= len(toks) {
		return i, false
	}
	if isStringLiteralToken(toks[i+2]) {
		return i + 3, nonEmptyStringLiteral(toks[i+2])
	}
	if !isIdentifierCandidate(toks[i+2]) {
		return i, false
	}
	return i + 3, true
}

func identifierParameterEnd(toks []rawToken, i int) (int, bool) {
	if i < 0 || i+4 >= len(toks) {
		return i, false
	}
	if toks[i].TokenType != "L_BRACE" || !isIdentifierCandidate(toks[i+1]) ||
		toks[i+2].TokenType != "COLON" || toks[i+3].Text != "Identifier" ||
		toks[i+4].TokenType != "R_BRACE" {
		return i, false
	}
	return i + 5, true
}

func keywordAt(toks []rawToken, i int, word string) bool {
	if i < 0 || i >= len(toks) {
		return false
	}
	if isStringLiteralToken(toks[i]) {
		return false
	}
	switch toks[i].TokenType {
	case "QUOTED_IDENTIFIER", "NUMBER":
		return false
	}
	return strings.EqualFold(toks[i].Text, word)
}

func isStringLiteralToken(tok rawToken) bool {
	switch tok.TokenType {
	case "STRING", "DOLLAR_STRING", "HEREDOC_STRING", "HEREDOC_STRING_ALTERNATIVE":
		return true
	default:
		return false
	}
}

func keywordsAt(toks []rawToken, i int, words ...string) bool {
	if i < 0 || i+len(words) > len(toks) {
		return false
	}
	for j, word := range words {
		if !keywordAt(toks, i+j, word) {
			return false
		}
	}
	return true
}

func dedupeNameRefs(refs []NameRef) []NameRef {
	if len(refs) < 2 {
		return refs
	}
	seen := make(map[NameRef]bool, len(refs))
	out := make([]NameRef, 0, len(refs))
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}
