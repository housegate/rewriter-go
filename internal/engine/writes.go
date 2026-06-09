package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WriteRole identifies the function a table reference plays inside a write
// statement (the kind tells you the statement; the role tells you which slot
// within it). Both InspectWrite and RewriteWriteTargets visit slots by role.
type WriteRole string

const (
	RoleCreate      WriteRole = "create"       // CREATE TABLE/VIEW target
	RoleCloneSource WriteRole = "clone_source" // CREATE TABLE ... CLONE <src>
	RoleViewTo      WriteRole = "view_to"      // CREATE MATERIALIZED VIEW ... TO <tbl>
	RoleDrop        WriteRole = "drop"         // DROP TABLE/VIEW target
	RoleTruncate    WriteRole = "truncate"     // TRUNCATE TABLE target
	RoleAlter       WriteRole = "alter"        // ALTER TABLE target
	RoleInsert      WriteRole = "insert"       // INSERT INTO target
	RoleUpdate      WriteRole = "update"       // UPDATE target
	RoleDelete      WriteRole = "delete"       // DELETE FROM target
)

// WriteSlot is one rewriteable table reference inside a write statement.
type WriteSlot struct {
	Role   WriteRole
	Target TableTarget
}

// CommandSub sub-classifies a `command` node (filled by later tasks; defined
// here so the shared WriteInfo struct is stable across the write task series).
type CommandSub string

const (
	CmdNone        CommandSub = ""
	CmdRename      CommandSub = "rename"
	CmdExchange    CommandSub = "exchange"
	CmdAlterUpdate CommandSub = "alter_update"
	CmdBareReject  CommandSub = "bare_reject"
)

// WriteInfo is the read view of a write statement. Task 2 fills the simple
// single-target kinds (Kind, Slots, IfExists, Multi, Materialized). The remaining
// fields are populated by later tasks (3-6) — notably IfNotExists by Task 3
// (create_table) / Task 4 (create_view); they are declared now so those tasks
// extend this type without redeclaring it.
type WriteInfo struct {
	Kind        string      // node-kind token (NodeDropTable, NodeUpdate, ...)
	Slots       []WriteSlot // every rewriteable table ref, in document order
	IfExists    bool        // IF EXISTS present
	IfNotExists bool        // IF NOT EXISTS present

	Multi           bool // DROP TABLE with >1 name
	CrossTable      bool // statement spans multiple physical tables (later tasks)
	Materialized    bool // MATERIALIZED view
	AsTableFunction bool // CREATE ... AS <table function> (later tasks)
	MissingTable    bool // expected table ref absent (later tasks)
	IsView          bool // CREATE/DROP VIEW (later tasks)
	HasViewBody     bool // view definition carries a SELECT body (later tasks)

	// Task 13 reject-guard parity discriminators.
	TruncateTarget string // truncate node `target` ("Table"/"Database"); "" when not a truncate
	IsDictionary   bool   // CREATE DICTIONARY (create_table.table_modifier == "DICTIONARY")

	Sub        CommandSub    // command sub-classification (later tasks)
	RawTargets []TableTarget // raw targets parsed from a command's SQL (later tasks)
}

// setTableRef sets a table node's name (always) and schema (only when newDB is
// non-empty, so an empty newDB preserves the existing schema). Unlike SELECT
// rewriting, writes never add a back-alias to the rewritten table.
func setTableRef(tbl map[string]any, newDB, newTable string) {
	tbl["name"] = ident(newTable)
	if newDB != "" {
		tbl["schema"] = ident(newDB)
	}
}

// writeSlots is the single visitor driving BOTH read (InspectWrite) and mutate
// (RewriteWriteTargets) paths. For the given node kind it locates each
// table-ref node in document order and calls visit(role, tbl). visit may mutate
// tbl in place (Go maps are references). Kinds not handled here yield no slots.
func writeSlots(kind string, body map[string]any, visit func(role WriteRole, tbl map[string]any)) {
	tblOf := func(v any) (map[string]any, bool) { m, ok := v.(map[string]any); return m, ok }
	switch kind {
	case NodeDropTable:
		if names, ok := body["names"].([]any); ok && len(names) > 0 {
			if tbl, ok := tblOf(names[0]); ok {
				visit(RoleDrop, tbl)
			}
		}
	case NodeDropView:
		if tbl, ok := tblOf(body["name"]); ok {
			visit(RoleDrop, tbl)
		}
	case NodeTruncate:
		if tbl, ok := tblOf(body["table"]); ok {
			visit(RoleTruncate, tbl)
		}
	case NodeUpdate:
		if tbl, ok := tblOf(body["table"]); ok {
			visit(RoleUpdate, tbl)
		}
	case NodeDelete:
		if tbl, ok := tblOf(body["table"]); ok {
			visit(RoleDelete, tbl)
		}
	case NodeCreateTable:
		if tbl, ok := tblOf(body["name"]); ok {
			visit(RoleCreate, tbl)
		}
		if tbl, ok := tblOf(body["clone_source"]); ok {
			visit(RoleCloneSource, tbl)
		}
	case NodeAlterTable:
		if tbl, ok := tblOf(body["name"]); ok {
			visit(RoleAlter, tbl)
		}
	case NodeInsert:
		if tbl, ok := tblOf(body["table"]); ok {
			visit(RoleInsert, tbl)
		}
	case NodeCreateView:
		if tbl, ok := tblOf(body["name"]); ok {
			visit(RoleCreate, tbl)
		}
		if tbl, ok := tblOf(body["to_table"]); ok {
			visit(RoleViewTo, tbl)
		}
	}
}

// bodyOf decodes a write AST into its kind (the single top-level key), the body
// object under that key, and the root map (for re-encoding after mutation).
func bodyOf(ast AST) (kind string, body map[string]any, root map[string]any, err error) {
	if err = json.Unmarshal(ast, &root); err != nil {
		return "", nil, nil, fmt.Errorf("engine: decode write: %w", err)
	}
	if len(root) != 1 {
		return "", nil, nil, fmt.Errorf("engine: expected one top-level key, got %d", len(root))
	}
	for k, v := range root {
		kind = k
		body, _ = v.(map[string]any)
	}
	return kind, body, root, nil
}

// ExistenceClause reads the AST's IF [NOT] EXISTS clause and reports which form
// is present. IfNotExists is set for the CREATE family (CREATE TABLE / DATABASE /
// VIEW carrying `if_not_exists`); IfExists for the DROP/TRUNCATE family (DROP
// TABLE / VIEW / DATABASE and TRUNCATE carrying `if_exists`). Any other kind (and
// a kind without the flag) yields (false, false, nil). This mirrors the flags
// WriteInfo.IfExists/IfNotExists and DatabaseTarget already read, surfaced as a
// single AST-driven helper so native.go can stamp existence_clause on EVERY
// response (it must survive rejects — the proto contract requires it accurate on a
// non-Success response; only a SyntaxError, which never parses, leaves it
// UNSPECIFIED).
func ExistenceClause(ast AST) (ifNotExists, ifExists bool, err error) {
	kind, body, _, err := bodyOf(ast)
	if err != nil {
		return false, false, err
	}
	if body == nil {
		return false, false, nil
	}
	switch kind {
	case NodeCreateTable, NodeCreateDB, NodeCreateView:
		inx, _ := body["if_not_exists"].(bool)
		return inx, false, nil
	case NodeDropTable, NodeDropView, NodeDropDB, NodeTruncate:
		ix, _ := body["if_exists"].(bool)
		return false, ix, nil
	default:
		return false, false, nil
	}
}

// InspectWrite returns the read view of a write statement. Task 2 fills the
// simple single-target kinds' flags and slots; later tasks extend the switch.
func InspectWrite(ast AST) (WriteInfo, error) {
	kind, body, _, err := bodyOf(ast)
	if err != nil {
		return WriteInfo{}, err
	}
	info := WriteInfo{Kind: kind}
	if body == nil {
		return info, nil
	}
	switch kind {
	case NodeDropTable:
		if names, ok := body["names"].([]any); ok && len(names) > 1 {
			info.Multi = true
		}
		info.IfExists, _ = body["if_exists"].(bool)
	case NodeDropView:
		info.IfExists, _ = body["if_exists"].(bool)
		info.Materialized, _ = body["materialized"].(bool)
	case NodeTruncate:
		info.IfExists, _ = body["if_exists"].(bool)
		// `target` discriminates TRUNCATE TABLE ("Table") from TRUNCATE DATABASE
		// ("Database"). VERIFIED via probe. Note: TRUNCATE VIEW / TRUNCATE ALL
		// TABLES FROM both still carry target=="Table" (polyglot mis-parses the
		// keyword as the table name and LOSES the real object), so the handler
		// must additionally scan the raw SQL — target alone catches only DATABASE.
		info.TruncateTarget, _ = body["target"].(string)
	case NodeCreateTable:
		info.IfNotExists, _ = body["if_not_exists"].(bool)
		// CREATE DICTIONARY folds into a create_table node discriminated by
		// table_modifier=="DICTIONARY" (VERIFIED via probe — exact field+value).
		// C++ guards on ASTCreateQuery::is_dictionary (writes.cc:270-272).
		tm, _ := body["table_modifier"].(string)
		info.IsDictionary = tm == "DICTIONARY"
		// `CREATE TABLE x AS table_function(...)`. The Phase-2 plan assumed a
		// top-level `as_table_function` key (mirroring the C++ DB AST field
		// ASTCreateQuery::as_table_function, writes.cc:346). Polyglot does NOT
		// expose that key; instead it reuses the `clone_source` slot but with an
		// empty name and a nested `identifier_func.function` (verified for
		// `AS remote(...)` and `AS numbers(...)`). So the table-function form is
		// `clone_source` present AND carrying `identifier_func`. A plain
		// `AS db2.src` clone source has no `identifier_func` and is left as a
		// rewriteable RoleCloneSource slot instead.
		info.AsTableFunction = cloneSourceIsTableFunction(body)
	case NodeAlterTable:
		info.IfExists, _ = body["if_exists"].(bool)
		info.CrossTable = alterHasCrossTableRef(body)
	case NodeInsert:
		// `INSERT INTO FUNCTION remote(...)`. The C++ guard is
		// insert_query->table_function (writes.cc:504). The Phase-2 plan assumed a
		// top-level `insert.table_function` key; Polyglot does NOT expose that.
		// VERIFIED shape: the function lives under `function_target`
		// ({"function":{…}}), and `table` is STILL present but with an EMPTY name
		// ("name":""). So AsTableFunction keys off `function_target`. Because the
		// empty-named table is a present map, the table-absence MissingTable check
		// below does NOT double-fire on the FUNCTION form — but if it ever did, the
		// handler (Task 6) rejects AsTableFunction first, surfacing the FUNCTION
		// message ahead of any missing-table message.
		if body["function_target"] != nil {
			info.AsTableFunction = true
		}
		// MissingTable guards an INSERT whose target table node is absent
		// altogether. No INSERT shape Polyglot emits drops `table` (plain, VALUES,
		// SELECT, FORMAT, and INTO FUNCTION all carry a `table` map — FUNCTION just
		// empty-names it), so this stays false in practice; it is a defensive guard
		// only, kept symmetric with the C++ port.
		if _, ok := body["table"].(map[string]any); !ok {
			info.MissingTable = true
		}
	case NodeCreateView:
		info.IsView = true
		info.Materialized, _ = body["materialized"].(bool)
		info.IfNotExists, _ = body["if_not_exists"].(bool)
		if _, ok := body["query"].(map[string]any); ok {
			info.HasViewBody = true
		}
	case NodeCommand:
		// Tier-C: RENAME TABLE / EXCHANGE TABLES / ALTER…UPDATE (and the bare
		// rejects) parse to an opaque command node {"this":"<raw sql>"} with no
		// structured table refs. Sub-classify by leading keyword(s) so the
		// dispatcher (Tasks 7-10) routes to the raw splice path or passes the
		// statement through to later phases.
		raw, _ := body["this"].(string)
		info.Sub = classifyWriteCommand(raw)
	}
	writeSlots(kind, body, func(role WriteRole, tbl map[string]any) {
		info.Slots = append(info.Slots, WriteSlot{Role: role, Target: decodeTableTarget(tbl)})
	})
	// A table-function target leaves an empty-name placeholder slot: CREATE TABLE x
	// AS table_function(...) under clone_source, INSERT INTO FUNCTION(...) under
	// table. Neither is a rewriteable table — drop every empty-name slot so callers
	// see only real targets (0 for INSERT…FUNCTION, 1 for CREATE…AS function). The
	// handler rejects AsTableFunction before rewriting anyway, but keeping Slots
	// clean prevents any naive iterate-and-rewrite from corrupting the AST.
	if info.AsTableFunction {
		kept := info.Slots[:0]
		for _, s := range info.Slots {
			if s.Target.Table != "" {
				kept = append(kept, s)
			}
		}
		info.Slots = kept
	}
	return info, nil
}

// RewriteWriteTargets visits every rewriteable table ref and applies the
// decision returned by decide. Only ActionRename is honored — writes are never
// rewritten to remote() (ActionRemote/ActionSkip leave the node untouched).
func RewriteWriteTargets(ast AST, decide func(WriteSlot) TableDecision) (AST, error) {
	kind, body, root, err := bodyOf(ast)
	if err != nil {
		return nil, err
	}
	if body != nil {
		writeSlots(kind, body, func(role WriteRole, tbl map[string]any) {
			d := decide(WriteSlot{Role: role, Target: decodeTableTarget(tbl)})
			if d.Action == ActionRename {
				setTableRef(tbl, d.NewDB, d.NewTable)
			}
		})
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode write: %w", err)
	}
	return AST(out), nil
}

// ExtractViewBody returns the view's embedded body as a standalone {"select":…}
// AST (the value of create_view.query), or ok=false when absent. The returned AST
// is an independent copy safe to rewrite before SetViewBody splices it back.
//
// Empirically verified: polyglot renders create_view.query as exactly
// {"select":{…}} for both CREATE VIEW and CREATE MATERIALIZED VIEW (… TO …),
// so the returned AST is directly consumable by CollectSelectTables /
// RewriteSelectTables, whose top-level key is "select".
func ExtractViewBody(ast AST) (AST, bool, error) {
	_, body, _, err := bodyOf(ast)
	if err != nil {
		return nil, false, err
	}
	q, ok := body["query"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	b, err := json.Marshal(q)
	if err != nil {
		return nil, false, fmt.Errorf("engine: encode view body: %w", err)
	}
	return AST(b), true, nil
}

// SetViewBody replaces create_view.query with the given {"select":…} body AST and
// re-encodes the whole statement. It errors on a non-view kind so a caller cannot
// silently splice a body onto the wrong node.
func SetViewBody(ast AST, body AST) (AST, error) {
	kind, b, root, err := bodyOf(ast)
	if err != nil {
		return nil, err
	}
	if kind != NodeCreateView {
		return nil, fmt.Errorf("engine: SetViewBody on non-view kind %q", kind)
	}
	var bodyNode map[string]any
	if err := json.Unmarshal(body, &bodyNode); err != nil {
		return nil, fmt.Errorf("engine: decode view body: %w", err)
	}
	b["query"] = bodyNode
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("engine: encode view: %w", err)
	}
	return AST(out), nil
}

// GenerateInsert generates the rewritten INSERT. Generate() reproduces VALUES
// tuples (semantically — `(1,'a')` becomes `(1, 'a')`) and keeps `INSERT INTO
// FUNCTION …`, but it DROPS a `FORMAT <fmt> <payload>` tail (Polyglot folds the
// FORMAT clause into insert.query as {"command":{"this":"FORMAT <fmt>"}} and the
// inline data payload is not modeled at all). So when the original carried a
// FORMAT payload we splice the ORIGINAL bytes back, mirroring C++ ASTInsertQuery's
// data/end splice (writes.cc:520-535). Boundary = the byte `end` of the
// format-name token (the identifier right after FORMAT).
func GenerateInsert(e Engine, originalSQL string, rewritten AST) (string, error) {
	prelude, err := e.Generate(rewritten)
	if err != nil {
		return "", err
	}
	// Only splice when the AST confirms a FORMAT *data clause* (insert.query is a
	// {"command":{"this":"FORMAT <fmt>"}}). This gate is load-bearing: the
	// tokenizer emits token_type=="FORMAT" for FORMAT used as a plain identifier
	// too (e.g. `INSERT INTO t (x) SELECT FORMAT FROM s`, where insert.query is a
	// select), so a token-only scan would false-splice and corrupt a normal
	// INSERT…SELECT. VALUES (query=nil) and INSERT…SELECT (query=select) never gate in.
	if !insertHasFormatClause(rewritten) {
		return prelude, nil
	}
	tail, ok, err := insertFormatTail(e, originalSQL)
	if err != nil {
		return "", err
	}
	if !ok {
		return prelude, nil
	}
	return prelude + tail, nil
}

// insertHasFormatClause reports whether an INSERT AST carries a FORMAT data clause
// — i.e. insert.query == {"command":{"this":"FORMAT <fmt>"}}. Verified shapes:
// VALUES → query nil; INSERT…SELECT (incl. a column literally named FORMAT) →
// query is a {"select":…}; only the data-FORMAT form folds into a command node.
func insertHasFormatClause(ast AST) bool {
	_, body, _, err := bodyOf(ast)
	if err != nil || body == nil {
		return false
	}
	q, ok := body["query"].(map[string]any)
	if !ok {
		return false
	}
	cmd, ok := q["command"].(map[string]any)
	if !ok {
		return false
	}
	this, _ := cmd["this"].(string)
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(this)), "FORMAT")
}

// insertFormatTail tokenizes originalSQL and returns the original substring from
// the end of the format-name token to EOF (the payload tail, with its leading
// whitespace/newline). ok=false when there is no FORMAT clause. Callers MUST first
// confirm a FORMAT data clause via insertHasFormatClause — this scan keys off the
// LAST FORMAT token (the data clause always trails any FORMAT-as-identifier and
// the payload runs to EOF), so combined with the AST gate it cannot mis-target.
//
// Boundary note (VERIFIED against Polyglot's tokenizer): the token after FORMAT is
// the format NAME (e.g. JSONEachRow / CSV); its span.end is the byte offset where
// the inline payload begins. For `INSERT INTO db.t FORMAT JSONEachRow {"x":1}` the
// name token ends at byte 35, so originalSQL[35:] = ` {"x":1}` (leading space
// preserved → valid SQL). The payload itself tokenizes as a single VAR whose span
// is collapsed to EOF, so we deliberately key off the format-NAME token's end.
func insertFormatTail(e Engine, originalSQL string) (string, bool, error) {
	toksAST, err := e.Tokenize(originalSQL)
	if err != nil {
		return "", false, err
	}
	var toks []struct {
		TokenType string `json:"token_type"`
		Span      struct {
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"span"`
	}
	if err := json.Unmarshal(toksAST, &toks); err != nil {
		return "", false, fmt.Errorf("engine: decode tokens: %w", err)
	}
	boundary, found := -1, false
	for i, tk := range toks {
		if tk.TokenType == "FORMAT" && i+1 < len(toks) {
			boundary, found = toks[i+1].Span.End, true // keep scanning → last FORMAT wins
		}
	}
	if found && boundary >= 0 && boundary <= len(originalSQL) {
		return originalSQL[boundary:], true, nil
	}
	return "", false, nil
}

// cloneSourceIsTableFunction reports whether a create_table's `clone_source`
// slot is actually a table function (`AS remote(...)`, `AS numbers(...)`) rather
// than a plain table source (`AS db2.src`). Polyglot renders the table-function
// form as a clone_source whose `identifier_func` is populated. Mirrors the
// intent of C++ ASTCreateQuery::as_table_function (writes.cc:346), which Polyglot
// folds into clone_source instead of a dedicated field.
func cloneSourceIsTableFunction(body map[string]any) bool {
	cs, ok := body["clone_source"].(map[string]any)
	if !ok {
		return false
	}
	return cs["identifier_func"] != nil
}

// alterHasCrossTableRef reports whether any ALTER action references a second
// table (ATTACH/REPLACE PARTITION FROM <table>, MOVE PARTITION TO TABLE). Mirrors
// C++ alterHasCrossTableRef (writes.cc:160-168), which inspects each command's
// structured from_table/to_table fields. (FETCH PARTITION FROM '<zk-path>' is NOT
// cross-table — its FROM is a ZooKeeper path; C++ accepts it. See rawActionIsCrossTable.)
//
// Polyglot models these two ways: some forms (ATTACH PART|PARTITION ... FROM,
// MOVE PART|PARTITION ... TO TABLE) arrive as {"Raw":{"sql":"…"}} actions detected
// by rawActionIsCrossTable; others (e.g. REPLACE PARTITION ... FROM) arrive
// STRUCTURED as {"ReplacePartition":{"source":{"table":…}}}. We catch both so the
// Task-7 reject is faithful to the C++ test that rejects REPLACE PARTITION ... FROM
// (rewriter_test.cc:2389).
func alterHasCrossTableRef(body map[string]any) bool {
	actions, ok := body["actions"].([]any)
	if !ok {
		return false
	}
	for _, a := range actions {
		am, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if raw, ok := am["Raw"].(map[string]any); ok {
			sql, _ := raw["sql"].(string)
			if rawActionIsCrossTable(sql) {
				return true
			}
			continue
		}
		// Structured (non-Raw) action: any action variant carrying a nested
		// table reference (e.g. ReplacePartition's `source.table`) is a
		// cross-table ref. Scan the single variant payload for such a ref.
		for _, v := range am {
			if vm, ok := v.(map[string]any); ok && structuredActionRefsTable(vm) {
				return true
			}
		}
	}
	return false
}

// rawActionIsCrossTable matches the cross-table ALTER partition forms by leading
// keyword over a Raw action's SQL, mirroring which forms C++ populates
// from_table/to_table for (ASTAlterQuery.h:202-219):
//   - ATTACH/REPLACE PART|PARTITION ... FROM <table>  → from_table (cross-table)
//   - MOVE         PART|PARTITION ... TO TABLE <table> → to_table   (cross-table)
//
// The leading-keyword gate (rather than a bare "PARTITION"+"FROM" scan) handles
// both granularities — PART and PARTITION — and crucially EXCLUDES:
//   - FETCH PARTITION ... FROM '<zk-path>': FETCH's FROM is a ZooKeeper path, not
//     a table; C++ leaves from_table empty and ACCEPTS it (no FETCH reject).
//   - MOVE PARTITION ... TO DISK/VOLUME: single-table, no TO TABLE.
func rawActionIsCrossTable(sql string) bool {
	u := strings.ToUpper(strings.TrimSpace(sql))
	if strings.HasPrefix(u, "ATTACH ") || strings.HasPrefix(u, "REPLACE ") {
		return strings.Contains(u, " FROM ")
	}
	if strings.HasPrefix(u, "MOVE ") {
		return strings.Contains(u, " TO TABLE ")
	}
	return false
}

// structuredActionRefsTable reports whether a structured ALTER action payload
// references a second table via a `source` or `destination` object holding a
// `table` node (the shape Polyglot uses for REPLACE/MOVE PARTITION between
// tables). Known structured cross-table variant as of this writing:
// ReplacePartition (source.table). The check is deliberately broad (any
// source/destination.table) — erring toward CrossTable=true is safe for a reject
// gate. If Polyglot exposes a NEW structured variant whose source/destination
// table is NOT a cross-table ref (so C++ would accept it), tighten this to an
// allow-list of cross-table variant keys to avoid over-rejecting.
func structuredActionRefsTable(payload map[string]any) bool {
	for _, key := range []string{"source", "destination"} {
		ref, ok := payload[key].(map[string]any)
		if !ok {
			continue
		}
		if _, ok := ref["table"].(map[string]any); ok {
			return true
		}
	}
	return false
}

// classifyWriteCommand sub-classifies a raw `command` SQL string by leading
// keyword(s). It rejects ONLY the table/dictionary/database DDL that C++
// writes.cc rejects via a structured AST guard; everything else returns CmdNone
// so the dispatcher passes it through (handled=false) to later phases / the
// SELECT handler — matching C++, which returns NotAWrite for any kind it doesn't
// recognize.
//
// CRITICAL — access-entity DDL parity: ALTER USER / DROP USER / DROP ROLE /
// DROP QUOTA / DROP ROW POLICY / DROP SETTINGS PROFILE / DROP NAMED COLLECTION
// all flatten to a `command` node, but in C++ they are a SEPARATE AST type that
// writes.cc never matches → they fall through to SELECT → Success. So they MUST
// classify as CmdNone (pass-through), NOT a reject. That is why the broad "ALTER "
// and "DROP " reject prefixes are gone: "DROP " is narrowed to "DROP DICTIONARY"
// (the only command-node DROP C++ rejects), and there is no bare "ALTER " reject
// at all (the only command-node ALTER C++ rejects, ALTER non-TABLE, lands in a
// `raw` node — handled by the handler's dispatchRawReject, not here). No
// access-entity form begins with RENAME / EXCHANGE / DETACH / OPTIMIZE-family, so
// the retained reject prefixes never catch one (there is no RENAME USER, no
// EXCHANGE USER; DETACH is only table/view/dictionary). Verified via probe.
func classifyWriteCommand(sql string) CommandSub {
	u := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	// Accepted table forms FIRST — the reject prefixes below (RENAME / EXCHANGE)
	// would otherwise swallow them.
	case strings.HasPrefix(u, "RENAME TABLE"):
		return CmdRename
	case strings.HasPrefix(u, "RENAME "): // RENAME DATABASE / RENAME DICTIONARY → reject (writes.cc:564-572)
		return CmdBareReject
	case strings.HasPrefix(u, "EXCHANGE TABLE"): // EXCHANGE TABLES
		return CmdExchange
	case strings.HasPrefix(u, "EXCHANGE "): // EXCHANGE DICTIONARIES → reject
		return CmdBareReject
	case strings.HasPrefix(u, "ALTER TABLE") && containsWord(u, "UPDATE"):
		return CmdAlterUpdate
	case strings.HasPrefix(u, "DETACH"): // DETACH TABLE/VIEW → reject (writes.cc:383-385)
		return CmdBareReject
	case strings.HasPrefix(u, "DROP DICTIONARY"): // ONLY DICTIONARY → reject (writes.cc:387-389).
		// DROP TABLE/VIEW/DATABASE are STRUCTURED nodes (never here); DROP
		// USER/ROLE/QUOTA/ROW POLICY/SETTINGS PROFILE/NAMED COLLECTION are
		// access-entity DDL C++ passes through → fall to CmdNone default below.
		return CmdBareReject
	case strings.HasPrefix(u, "OPTIMIZE"), strings.HasPrefix(u, "UNDROP"),
		strings.HasPrefix(u, "MOVE"), strings.HasPrefix(u, "BACKUP"),
		strings.HasPrefix(u, "RESTORE"), strings.HasPrefix(u, "KILL"):
		return CmdBareReject
	default:
		// USE / SHOW / SHOW CREATE / GRANT / REVOKE / EXISTS + ALL access-entity
		// ALTER/DROP/CREATE command-node forms (ALTER USER, DROP USER/ROLE/QUOTA/…)
		// — not a write this phase rejects → pass through (Phase 3/4 / SELECT).
		return CmdNone
	}
}

// containsWord reports whether upper-cased haystack contains word as a
// whitespace-delimited token (avoids matching UPDATE inside an identifier).
func containsWord(haystack, word string) bool {
	for _, f := range strings.Fields(haystack) {
		if f == word {
			return true
		}
	}
	return false
}

// rawToken is one lexer token from Engine.Tokenize. Empirically (probed against
// Polyglot's ClickHouse tokenizer): plain identifiers are token_type=="VAR";
// backtick-quoted identifiers are token_type=="QUOTED_IDENTIFIER" with the
// backticks STRIPPED from text (Text=="weird.name" for `weird.name`) while span
// still covers the full quoted run; the keywords TABLE/TO/AND/UPDATE/WHERE/RENAME/
// ALTER and punctuation DOT/COMMA/EQ are their own token types. NOTE: EXCHANGE and
// TABLES are emitted as VAR (not dedicated keywords), so the TABLE/TABLES boundary
// is found by case-insensitive TEXT match, not token-type.
type rawToken struct {
	TokenType string `json:"token_type"`
	Text      string `json:"text"`
	Span      struct {
		Start int `json:"start"`
		End   int `json:"end"`
	} `json:"span"`
}

// tokenizeRaw runs the engine lexer over sql and decodes the token stream.
func tokenizeRaw(e Engine, sql string) ([]rawToken, error) {
	toksAST, err := e.Tokenize(sql)
	if err != nil {
		return nil, err
	}
	var toks []rawToken
	if err := json.Unmarshal(toksAST, &toks); err != nil {
		return nil, fmt.Errorf("engine: decode tokens: %w", err)
	}
	return toks, nil
}

// tableRefSpan is one [db.]table name-run located in the raw token stream, with
// the byte span [Start,End) it occupies in the ORIGINAL SQL (for splicing).
type tableRefSpan struct {
	Target TableTarget
	Start  int
	End    int
}

// isNameTok reports whether a token type denotes an identifier name-run element.
// Plain names lex as VAR; backtick-quoted names lex as QUOTED_IDENTIFIER (with
// the backticks stripped from .Text — see rawToken). Both can sit in a db/table
// position.
func isNameTok(tt string) bool {
	return tt == "VAR" || tt == "QUOTED_IDENTIFIER"
}

// scanTableRefs extracts every [db.]table name-run in a table-name position for
// the command sub-kind:
//
//	rename/exchange: every name-run after the TABLE/TABLES keyword is a table
//	alter_update:    only the single name-run immediately after TABLE
//
// A name-run is name (DOT name)?. Non-name tokens (TO/AND/COMMA/keywords)
// separate runs. The TABLE/TABLES boundary is matched on token TEXT (EXCHANGE
// TABLES both lex as VAR, so a token-type gate would miss it). For a
// QUOTED_IDENTIFIER the .Text already has its backticks stripped, so the Target
// carries the unquoted identifier — matching the handler's unquoted rewrite key.
func scanTableRefs(toks []rawToken, sub CommandSub) []tableRefSpan {
	start := 0
	for i, tk := range toks {
		if strings.EqualFold(tk.Text, "TABLE") || strings.EqualFold(tk.Text, "TABLES") {
			start = i + 1
			break
		}
	}
	var out []tableRefSpan
	i := start
	for i < len(toks) {
		if !isNameTok(toks[i].TokenType) {
			i++
			continue
		}
		var span tableRefSpan
		if i+2 < len(toks) && toks[i+1].TokenType == "DOT" && isNameTok(toks[i+2].TokenType) {
			span.Target = TableTarget{DB: toks[i].Text, Table: toks[i+2].Text}
			span.Start, span.End = toks[i].Span.Start, toks[i+2].Span.End
			i += 3
		} else {
			span.Target = TableTarget{Table: toks[i].Text}
			span.Start, span.End = toks[i].Span.Start, toks[i].Span.End
			i++
		}
		out = append(out, span)
		if sub == CmdAlterUpdate {
			break
		}
	}
	return out
}

// RawTableRefs returns the table refs in a tier-C raw command (read-only), in
// document order, together with the command sub-kind. For a non-rewriteable
// command (anything other than rename/exchange/alter_update) it returns no refs
// and the classified sub-kind, so the handler can decide pass-through vs reject.
func RawTableRefs(e Engine, ast AST) ([]TableTarget, CommandSub, error) {
	_, body, _, err := bodyOf(ast)
	if err != nil {
		return nil, CmdNone, err
	}
	raw, _ := body["this"].(string)
	sub := classifyWriteCommand(raw)
	if sub != CmdRename && sub != CmdExchange && sub != CmdAlterUpdate {
		return nil, sub, nil
	}
	toks, err := tokenizeRaw(e, raw)
	if err != nil {
		return nil, sub, err
	}
	spans := scanTableRefs(toks, sub)
	out := make([]TableTarget, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.Target)
	}
	return out, sub, nil
}

// SpliceRawTables rewrites table-name spans of a tier-C raw command. rewrites
// maps qualify(origDB,origTable) → new qualified name (the caller is expected to
// pre-quote dotted/dynamic names via QuoteQualified). Spans are replaced
// right-to-left so earlier byte offsets stay valid. A ref absent from the map is
// left untouched.
func SpliceRawTables(e Engine, originalSQL string, rewrites map[string]string) (string, error) {
	sub := classifyWriteCommand(originalSQL)
	// Only the tier-C table-bearing commands have a table-name grammar to splice.
	// Guard symmetric with RawTableRefs so a misuse on a non-rewriteable command
	// (e.g. "USE db") can't accidentally splice a same-named identifier.
	if sub != CmdRename && sub != CmdExchange && sub != CmdAlterUpdate {
		return originalSQL, nil
	}
	toks, err := tokenizeRaw(e, originalSQL)
	if err != nil {
		return "", err
	}
	spans := scanTableRefs(toks, sub)
	out := originalSQL
	for i := len(spans) - 1; i >= 0; i-- {
		s := spans[i]
		nv, ok := rewrites[qualifyTT(s.Target)]
		if !ok {
			continue
		}
		out = out[:s.Start] + nv + out[s.End:]
	}
	return out, nil
}

// qualifyTT builds "db.table" (or bare "table") — the rewrites map key used by
// SpliceRawTables. Mirrors qualify() in the handler layer; both build the key
// from UNQUOTED identifier text.
func qualifyTT(tt TableTarget) string {
	if tt.DB == "" {
		return tt.Table
	}
	return tt.DB + "." + tt.Table
}

// QuoteQualified renders "db.table" with each segment backtick-quoted only when
// needsQuoting (so a dotted dynamic table name like `tenant1.events` splices as a
// single quoted identifier rather than re-parsing as a multi-part name).
func QuoteQualified(db, table string) string {
	q := func(s string) string {
		if needsQuoting(s) {
			return "`" + s + "`"
		}
		return s
	}
	if db == "" {
		return q(table)
	}
	return q(db) + "." + q(table)
}
