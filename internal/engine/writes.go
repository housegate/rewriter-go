package engine

import (
	"encoding/json"
	"fmt"
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
	}
	writeSlots(kind, body, func(role WriteRole, tbl map[string]any) {
		info.Slots = append(info.Slots, WriteSlot{Role: role, Target: decodeTableTarget(tbl)})
	})
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
