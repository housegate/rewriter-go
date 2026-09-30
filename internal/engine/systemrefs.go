package engine

import (
	"encoding/json"
	"strings"
)

// systemDatabaseName is ClickHouse's `system` database (nameresolve.SystemDatabase;
// the engine package does not import nameresolve).
const systemDatabaseName = "system"

// ObjectRef is one caller-input reference to a table-like object, as
// CollectObjectRefs reports it.
type ObjectRef struct {
	// DB is the database after ClickHouse's identifier decoding; "" for an
	// unqualified name, which ClickHouse resolves in the session database.
	DB string
	// Table is the object name after ClickHouse's identifier decoding, or,
	// for a pattern carrier (merge() / ENGINE = Merge), the pattern text as
	// the statement spells it. "" when the position names a database but no
	// static object.
	Table string
	// Exact reports that Table is an object name that may be compared with
	// a name list. A pattern or an unknown name is never exact.
	Exact bool
}

// ClickHouseIdentifierValue returns the name ClickHouse reads for an
// identifier whose text a parser or tokenizer has already unquoted. Polyglot
// decodes some escapes differently from ClickHouse (it keeps `\N`, which
// ClickHouse drops, and reads `\_` as `_`), so any backslash sequence left in
// the text is decoded again with ClickHouse's rules (clickhouseUnquote). A
// second decode can only turn a name that already carried a backslash into
// another name; used for a refusal check it errs toward refusing. Text that
// clickhouseUnquote cannot read (a trailing backslash) is returned as is.
func ClickHouseIdentifierValue(name string) string {
	if !strings.Contains(name, `\`) {
		return name
	}
	v, ok := clickhouseUnquote("`" + strings.ReplaceAll(name, "`", "``") + "`")
	if !ok {
		return name
	}
	return v
}

// IsSystemDatabase reports whether a decoded database name, or the session's
// logical database for an unqualified name (db == ""), is ClickHouse's
// `system` database. ClickHouse database names are case-sensitive (measured
// on 26.2: SYSTEM.processes is UNKNOWN_DATABASE).
func IsSystemDatabase(db, contextDB string) bool {
	if db == "" {
		return contextDB == systemDatabaseName
	}
	return db == systemDatabaseName
}

// CollectObjectRefs returns every caller-input reference to a table-like
// object, in every position the table-reference policy governs: FROM / JOIN
// / subquery / CTE / UNION arm / view() and view / materialized-view bodies /
// INSERT … SELECT and CTAS sources / IN operands (every form) / column and
// ALTER-action expressions, write and DDL targets, table-function and
// table-engine arguments, string-lookup arguments (joinGet / dictGet /
// hasColumnInTable families), opaque ALTER, CREATE VIEW column-list and
// INSERT query text, and, for a command node (DESCRIBE, EXISTS, SHOW CREATE,
// SHOW COLUMNS / INDEX, RENAME, ALTER … UPDATE, GRANT, …), every qualified
// `<db>.<name>` run in its text plus its unqualified target. Names are
// decoded the way ClickHouse's ParserIdentifier reads them. Only caller input
// is read: SQL the rewriter emits is never passed here, so the rewriter's own
// system.tables / system.columns synthesis is not a reference. A qualified
// run in command or opaque text can also be a qualified column; callers use
// the result only to refuse.
//
// The order is deterministic and is part of the cross-engine contract,
// because the first refused reference names the refusal. It is collector
// order, not strict document order:
//   - structured statement: the statement's own write / DDL targets (in
//     AllWriteTargets order), then every read source and carrier in walker
//     order (a CTE body before the main query, the select list before FROM,
//     FROM / JOIN before WHERE / IN, a FROM subquery before a later JOIN),
//     then the qualified runs of the opaque texts (ALTER actions, view
//     column-list items, the INSERT query text), then the string-lookup
//     arguments in walk order;
//   - command node: the qualified runs of its text in text order, then the
//     SHOW COLUMNS / INDEX target, then the unqualified DESCRIBE / EXISTS /
//     SHOW CREATE target.
func CollectObjectRefs(e Engine, ast AST, sql string) ([]ObjectRef, error) {
	var out []ObjectRef
	addTarget := func(tt TableTarget) {
		if tt.Table == "" {
			return
		}
		out = append(out, ObjectRef{DB: ClickHouseIdentifierValue(tt.DB), Table: ClickHouseIdentifierValue(tt.Table), Exact: true})
	}
	kind, err := NodeKind(ast)
	if err != nil {
		return nil, err
	}
	if kind == NodeCommand {
		refs, ok := qualifiedSourceRuns(e, sql)
		if !ok {
			return nil, errTokenizeObjectRefs
		}
		out = append(out, refs...)
		if info, err := ParseDBLevel(e, sql); err == nil && info.Kind == DBShow &&
			isShowTableTargetKind(info.ShowWhat) && info.HasTableClause && info.ShowTableResolved {
			// SHOW COLUMNS FROM t [{FROM|IN} db]: the two-clause form is not
			// a qualified run, and an unqualified t resolves in the session
			// database.
			addTarget(TableTarget{DB: info.DB, Table: info.ShowTable})
		}
		if t, err := ParseObjectTarget(e, sql); err == nil && t.Verb != VerbNone &&
			t.ObjType != "DATABASE" && t.Shape == ObjectTargetName && t.DB == "" {
			addTarget(TableTarget{Table: t.Table})
		}
		return out, nil
	}
	var root any
	if err := json.Unmarshal(ast, &root); err != nil {
		return nil, err
	}
	targets, err := AllWriteTargets(e, ast)
	if err != nil {
		return nil, err
	}
	for _, tt := range targets {
		addTarget(tt)
	}
	namespace := func(_ map[string]any, d namespaceRefDetail) {
		ref := d.ref
		if ref.Target.DB == "" && !ref.UsesCurrentDatabase {
			if ref.Source == NamespaceRefInTable {
				// An unqualified IN operand is a table in the session database.
				addTarget(ref.Target)
			}
			return
		}
		table := ref.Target.Table
		exact := ref.Resolved && table != "" && !strings.EqualFold(ref.Name, "merge")
		if exact {
			table = ClickHouseIdentifierValue(table)
		}
		out = append(out, ObjectRef{DB: ClickHouseIdentifierValue(ref.Target.DB), Table: table, Exact: exact})
	}
	if err := walkStatementObjects(root, readSourceScope{}, readSourceVisitor{
		table:     func(_, _ map[string]any, tt TableTarget) { addTarget(tt) },
		inTable:   namespace,
		namespace: namespace,
	}); err != nil {
		return nil, err
	}
	texts, err := OpaqueStatementTexts(e, ast, sql)
	if err != nil {
		return nil, err
	}
	if text, ok, ierr := OpaqueInsertQueryText(ast); ierr != nil {
		return nil, ierr
	} else if ok {
		texts = append(texts, text)
	}
	for _, text := range texts {
		refs, ok := qualifiedSourceRuns(e, text)
		if !ok {
			return nil, errTokenizeObjectRefs
		}
		out = append(out, refs...)
	}
	for _, o := range collectStringLookupOccurrences(root) {
		call := o.call
		if call.DB != "" || call.Table != "" {
			addTarget(TableTarget{DB: call.DB, Table: call.Table})
			continue
		}
		if dot := strings.IndexByte(call.Arg, '.'); dot > 0 {
			addTarget(TableTarget{DB: unwrapQuoted(call.Arg[:dot]), Table: unwrapQuoted(call.Arg[dot+1:])})
		}
	}
	return out, nil
}

type objectRefsError string

func (e objectRefsError) Error() string { return string(e) }

const errTokenizeObjectRefs = objectRefsError("engine: tokenize object references")

// qualifiedSourceRuns scans text with the engine tokenizer and returns every
// `<db> . <name>` run whose first part is not itself the table half of a
// longer run, with each part read from its source lexeme the way ClickHouse
// reads it: a bare word as spelled (a keyword token such as SYSTEM included:
// the tokenizer types it by spelling, ClickHouse reads it as a name here), a
// quoted identifier decoded by clickhouseUnquote, a “…” identifier verbatim.
// Whitespace and comments between the parts do not matter, as for
// ClickHouse. A `<db> .` followed by anything that is not a name
// (`system.*`) is a reference to no static object. For a run of three or
// more parts, `<first> . <last>` (ClickHouse's SHOW COLUMNS / INDEX reading)
// is reported before `<first> . <second>` (an expression's reading). ok=false
// means the text could not be tokenized.
func qualifiedSourceRuns(e Engine, text string) ([]ObjectRef, bool) {
	toks, err := tokenizeRaw(e, text)
	if err != nil {
		return nil, false
	}
	var out []ObjectRef
	for i := 0; i+1 < len(toks); i++ {
		if toks[i+1].TokenType != "DOT" {
			continue
		}
		if i > 0 && toks[i-1].TokenType == "DOT" {
			continue
		}
		db, ok := sourceNameValue(text, toks[i])
		if !ok {
			continue
		}
		ref := ObjectRef{DB: db}
		if i+2 < len(toks) {
			if table, ok := sourceNameValue(text, toks[i+2]); ok {
				ref.Table, ref.Exact = table, true
			} else {
				ref.Table = toks[i+2].Text
			}
		}
		// A run of three or more parts: ClickHouse's SHOW COLUMNS / INDEX
		// read it as (first part, LAST part); an expression reads it as
		// (first, second) plus a column. Report the (first, last) reading
		// first, then (first, second), so both are checked; a caller that
		// only refuses errs toward refusing.
		if ref.Exact {
			j := i + 2
			for j+2 < len(toks) && toks[j+1].TokenType == "DOT" {
				last, ok := sourceNameValue(text, toks[j+2])
				if !ok {
					out = append(out, ObjectRef{DB: db, Table: toks[j+2].Text})
					break
				}
				j += 2
				if j+1 >= len(toks) || toks[j+1].TokenType != "DOT" {
					out = append(out, ObjectRef{DB: db, Table: last, Exact: true})
				}
			}
		}
		out = append(out, ref)
	}
	return out, true
}

// sourceNameValue reads one token as a name from its source bytes, the way
// ClickHouse's lexer reads an identifier: a bare word ([A-Za-z_][A-Za-z0-9_$]*)
// as spelled, a backtick / double-quoted identifier decoded with ClickHouse's
// rules, or a Unicode “…” identifier with its body verbatim. Anything else (a
// string, a number, an operator) is not a name.
func sourceNameValue(text string, tok rawToken) (string, bool) {
	start, end := tok.Span.Start, tok.Span.End
	if start < 0 || end > len(text) || start >= end {
		return "", false
	}
	raw := text[start:end]
	switch raw[0] {
	case '`', '"':
		return clickhouseUnquote(raw)
	}
	if strings.HasPrefix(raw, "“") {
		// ClickHouse's lexer also reads “…” as a quoted identifier, with the
		// body verbatim (no escapes); ‘…’ is a string, not a name.
		return unicodeQuoted(raw, "“", "”")
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		letter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !letter && (i == 0 || !((c >= '0' && c <= '9') || c == '$')) {
			return "", false
		}
	}
	return raw, true
}
