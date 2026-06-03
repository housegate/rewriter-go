// Package engine is the ONLY code that talks to polyglot. It is the seam a
// future WASM/wazero backend would replace; nothing outside this package may
// import the polyglot SDK.
package engine

import "encoding/json"

// AST is polyglot's JSON AST. We decode only the nodes we mutate.
type AST = json.RawMessage

// Engine wraps the polyglot SQL engine, pinned to the ClickHouse dialect.
type Engine interface {
	ParseOne(sql string) (AST, error)
	Generate(ast AST) (string, error)
	RenameTables(ast AST, mapping map[string]string) (AST, error)
	QualifyTables(ast AST, db string) (AST, error)
	Tokenize(sql string) (AST, error)
	// DiffSQL compares two SQL strings semantically (polyglot parses both and
	// diffs the ASTs). Returns the raw diff JSON; harness code interprets it.
	DiffSQL(sql1, sql2 string) (AST, error)
	Close() error
}
