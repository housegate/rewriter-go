package engine

import (
	"encoding/json"
	"fmt"
)

// Node-kind tokens = the single top-level JSON key polyglot emits (see spec §3.1).
// Confirmed against internal/engine/testdata/ast-shapes/ (Task 3).
const (
	NodeSelect      = "select"
	NodeInsert      = "insert"
	NodeCreateTable = "create_table"
	NodeDropTable   = "drop_table"
	NodeAlterTable  = "alter_table"
	NodeCreateDB    = "create_database"
	NodeDropDB      = "drop_database"
	NodeTruncate    = "truncate"
	NodeDelete      = "delete"
	NodeCreateView  = "create_view"
	NodeDropView    = "drop_view"
	NodeUpdate      = "update"
	NodeCommand     = "command" // USE/GRANT/REVOKE/RENAME/SHOW*/EXISTS — raw SQL in .this
)

// NodeKind returns a node's kind: the single top-level key of the AST object.
func NodeKind(ast AST) (string, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(ast, &head); err != nil {
		return "", fmt.Errorf("engine: decode AST head: %w", err)
	}
	if len(head) != 1 {
		return "", fmt.Errorf("engine: expected exactly one top-level key, got %d", len(head))
	}
	for k := range head {
		return k, nil
	}
	return "", fmt.Errorf("engine: empty AST object")
}

// CommandSQL returns the raw SQL held in a `command` node (command.this).
// Errors if the node is not a command.
func CommandSQL(ast AST) (string, error) {
	var head struct {
		Command *struct {
			This string `json:"this"`
		} `json:"command"`
	}
	if err := json.Unmarshal(ast, &head); err != nil {
		return "", fmt.Errorf("engine: decode command: %w", err)
	}
	if head.Command == nil {
		return "", fmt.Errorf("engine: AST is not a command node")
	}
	return head.Command.This, nil
}
