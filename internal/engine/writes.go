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
	case NodeCreateTable:
		info.IfNotExists, _ = body["if_not_exists"].(bool)
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
	}
	writeSlots(kind, body, func(role WriteRole, tbl map[string]any) {
		info.Slots = append(info.Slots, WriteSlot{Role: role, Target: decodeTableTarget(tbl)})
	})
	// CREATE TABLE x AS table_function(...) reuses the clone_source slot for the
	// function node, whose decoded Target has an empty name — it is NOT a
	// rewriteable table. Drop it so callers see only the real create target.
	// (The handler rejects AsTableFunction before rewriting anyway, but keeping
	// Slots clean prevents any naive iterate-and-rewrite from corrupting the AST.)
	if info.AsTableFunction {
		kept := info.Slots[:0]
		for _, s := range info.Slots {
			if s.Role != RoleCloneSource {
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
