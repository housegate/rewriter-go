package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MetadataAccessDisabledMessage is the refusal of a statement that reads
// ClickHouse metadata under RewriteSQLRequest.refuse_metadata_access. Same
// text as rewriter-grpc (handlers/metadata_access.h) and as upstream
// sql-rewriter answered its static-mode caller.
const MetadataAccessDisabledMessage = "metadata access is disabled"

// isMetadataDatabase reports whether a decoded database name is `system` or
// `information_schema`, compared case-insensitively. ClickHouse database
// names are case-sensitive, so `SYSTEM` is not the metadata database; the
// comparison errs toward refusing, as rewriter-grpc's does.
func isMetadataDatabase(db string) bool {
	db = strings.ReplaceAll(db, "`", "")
	return strings.EqualFold(db, systemDatabaseName) || strings.EqualFold(db, "information_schema")
}

// ReadsMetadata reports whether a statement reads ClickHouse metadata, the
// rule RewriteSQLRequest.refuse_metadata_access opts into (mirrors
// rewriter-grpc's readsMetadata):
//   - a reference to the `system` / `information_schema` database in any
//     position CollectObjectRefs reads (tables, targets, IN operands, table
//     function and engine arguments, string lookups, command text);
//   - every SHOW form backed by a system table or access storage (SHOW
//     CREATE and SHOW COLUMNS / INDEX are checked by target instead), and
//     DESCRIBE FILESYSTEM CACHE;
//   - a table function, table engine or CLICKHOUSE dictionary source whose
//     database is not a static name outside those databases (the session
//     database included), except the external-only forms of jdbc / odbc /
//     mongodb;
//   - a string lookup whose name argument is not static, and a
//     hasColumnInTable remote-host form;
//   - a version() call and the deployment-identity calls and bare names
//     (FirstDeploymentIdentityName).
//
// ast is the parse of sql. A non-nil error means the statement could not be
// read in full; callers refuse it.
func ReadsMetadata(e Engine, ast AST, sql string) (bool, error) {
	kind, err := NodeKind(ast)
	if err != nil {
		return false, err
	}
	if kind == NodeCommand {
		if refused, err := metadataCommand(e, sql); err != nil || refused {
			return refused, err
		}
	}
	// USE / SHOW … FROM, and the `… DATABASE <name>` statements (CREATE,
	// DROP, SHOW CREATE, EXISTS, …), name a bare database, which no qualified
	// run carries.
	if info, err := ParseDBLevel(e, sql); err == nil && info.Kind != DBNone && isMetadataDatabase(info.DB) {
		return true, nil
	}
	if refused, err := databaseKeywordTarget(e, sql); err != nil || refused {
		return refused, err
	}
	refs, err := CollectObjectRefs(e, ast, sql)
	if err != nil {
		return false, err
	}
	for _, ref := range refs {
		if isMetadataDatabase(ref.DB) {
			return true, nil
		}
	}
	if kind != NodeCommand {
		var root any
		if err := json.Unmarshal(ast, &root); err != nil {
			return false, fmt.Errorf("engine: decode: %w", err)
		}
		found := false
		check := func(_ map[string]any, d namespaceRefDetail) {
			if !found && namespaceMayReachMetadata(d.ref) {
				found = true
			}
		}
		if err := walkStatementObjects(root, readSourceScope{}, readSourceVisitor{
			table:     func(_, _ map[string]any, _ TableTarget) {},
			inTable:   check,
			namespace: check,
		}); err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
		for _, o := range collectStringLookupOccurrences(root) {
			if o.call.Arg == "" {
				return true, nil
			}
			if strings.HasPrefix(strings.ToLower(o.call.Function), "hascolumnintable") && len(o.args) != 3 {
				return true, nil
			}
		}
	}
	if found, err := hasVersionCall(e, sql); err != nil || found {
		return found, err
	}
	_, found, err := FirstDeploymentIdentityName(e, ast, sql)
	return found, err
}

// namespaceMayReachMetadata is the namespace half of ReadsMetadata: an IN
// table operand is checked by CollectObjectRefs alone; a table function,
// table engine or dictionary source must name a static database outside the
// metadata databases.
func namespaceMayReachMetadata(ref NamespaceRef) bool {
	if isMetadataDatabase(ref.Target.DB) {
		return true
	}
	if ref.Source == NamespaceRefInTable {
		return false
	}
	switch strings.ToLower(ref.Name) {
	case "jdbc", "odbc", "mongodb":
		// The short / URI forms name an external table only; the
		// (database, table) forms were checked above.
		return false
	}
	return ref.UsesCurrentDatabase || (ref.Target.DB == "" && !ref.Resolved)
}

// metadataCommand reports a SHOW form that reads a system table or access
// storage, DESCRIBE FILESYSTEM CACHE, and a USE of a metadata database.
// SHOW CREATE … and SHOW COLUMNS /
// INDEX name a table, which CollectObjectRefs checks.
func metadataCommand(e Engine, sql string) (bool, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return false, err
	}
	var words []string
	for _, tk := range toks {
		if len(words) == 3 {
			break
		}
		words = append(words, strings.ToUpper(tk.Text))
	}
	if len(words) == 0 {
		return false, nil
	}
	switch words[0] {
	case "SHOW":
		if len(words) < 2 {
			return true, nil
		}
		if words[1] == "CREATE" {
			// SHOW CREATE [TEMPORARY] TABLE / VIEW / DICTIONARY / DATABASE
			// name a target; SHOW CREATE USER / ROLE / QUOTA / … read access
			// storage.
			if len(words) < 3 {
				return true, nil
			}
			switch words[2] {
			case "TABLE", "TEMPORARY", "VIEW", "DICTIONARY", "DATABASE":
				return false, nil
			}
			// `SHOW CREATE t` is SHOW CREATE TABLE t; an access-entity kind
			// is refused.
			return isAccessEntityKind(words[2]), nil
		}
		if isShowTableTargetKind(words[1]) {
			return false, nil
		}
		return true, nil
	case "DESCRIBE", "DESC":
		return len(words) >= 2 && words[1] == "FILESYSTEM", nil
	case "USE":
		// ParseDBLevel drops a keyword-spelled target (`USE system`);
		// sourceNameValue reads it as ClickHouse does. A target it cannot
		// read statically is refused.
		if len(toks) < 2 {
			return false, nil
		}
		db, ok := sourceNameValue(sql, toks[1])
		return !ok || isMetadataDatabase(db), nil
	}
	return false, nil
}

func isAccessEntityKind(word string) bool {
	switch word {
	case "USER", "USERS", "ROLE", "ROLES", "QUOTA", "QUOTAS", "PROFILE", "PROFILES",
		"SETTINGS", "ROW", "POLICY", "POLICIES", "MASKING":
		return true
	}
	return false
}

// hasVersionCall reports a version() call in sql: a bare word or a quoted
// identifier spelling version, case-insensitively, directly followed by "("
// and not after "." (the call shape FirstDeploymentIdentityCall reads). The
// dynamic-mode identity list leaves version() alone because HouseGate's
// ServerHello already carries it; a static-mode caller gets no hello.
func hasVersionCall(e Engine, sql string) (bool, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return false, err
	}
	for i := 0; i+1 < len(toks); i++ {
		tk := toks[i]
		if toks[i+1].TokenType != "L_PAREN" || tk.Source == "" {
			continue
		}
		if i > 0 && toks[i-1].TokenType == "DOT" {
			continue
		}
		if i > 0 && !isQuotedLexeme(toks[i-1].TokenType) && tableNameIntroducers[strings.ToLower(toks[i-1].Source)] {
			continue
		}
		name := tk.Source
		switch {
		case isQuotedLexeme(tk.TokenType) && isQuotedIdentifier(tk.Source):
			unquoted, ok := clickhouseUnquote(tk.Source)
			if !ok {
				unquoted = tk.Text
			}
			name = unquoted
		case !isQuotedLexeme(tk.TokenType) && isWordStart(tk.Source):
		default:
			continue
		}
		if strings.EqualFold(name, "version") {
			return true, nil
		}
	}
	return false, nil
}

// databaseKeywordTarget reports a statement whose head names a metadata
// database after the DATABASE keyword (`DROP DATABASE system`, `SHOW CREATE
// DATABASE system`, `CREATE DATABASE IF NOT EXISTS system`, …): the keyword
// within the first four tokens, then an optional IF [NOT] EXISTS, then the
// name, read as ClickHouse reads it (sourceNameValue).
func databaseKeywordTarget(e Engine, sql string) (bool, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return false, err
	}
	for i := 0; i < len(toks) && i < 4; i++ {
		if !strings.EqualFold(toks[i].Text, "DATABASE") {
			continue
		}
		j := i + 1
		for j < len(toks) && (strings.EqualFold(toks[j].Text, "IF") || strings.EqualFold(toks[j].Text, "NOT") ||
			strings.EqualFold(toks[j].Text, "EXISTS")) {
			j++
		}
		if j >= len(toks) {
			return false, nil
		}
		db, ok := sourceNameValue(sql, toks[j])
		return ok && isMetadataDatabase(db), nil
	}
	return false, nil
}
