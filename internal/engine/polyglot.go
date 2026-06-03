package engine

import (
	"encoding/json"
	"fmt"

	polyglot "github.com/tobilg/polyglot/packages/go"
)

const dialect = "clickhouse"

type polyglotEngine struct {
	c *polyglot.Client
}

// NewPolyglot loads the FFI lib. If libPath is "", OpenDefault is used
// (honours POLYGLOT_SQL_FFI_PATH and local build dirs).
func NewPolyglot(libPath string) (Engine, error) {
	var (
		c   *polyglot.Client
		err error
	)
	if libPath == "" {
		c, err = polyglot.OpenDefault()
	} else {
		c, err = polyglot.Open(libPath)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: open polyglot: %w", err)
	}
	return &polyglotEngine{c: c}, nil
}

func (e *polyglotEngine) ParseOne(sql string) (AST, error) {
	ast, err := e.c.ParseOne(sql, dialect)
	if err != nil {
		return nil, fmt.Errorf("engine: parse: %w", err)
	}
	return AST(ast), nil
}

// Generate takes a single-statement AST (from ParseOne / RenameTables /
// QualifyTables — all produce single objects). The polyglot Generate call
// expects an array, so we wrap and unwrap.
func (e *polyglotEngine) Generate(ast AST) (string, error) {
	out, err := e.c.Generate(wrapArray(ast), dialect)
	if err != nil {
		return "", fmt.Errorf("engine: generate: %w", err)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("engine: generate returned no statements")
	}
	return out[0], nil
}

// RenameTables applies the table-name mapping. The polyglot SDK expects and
// returns an array; we wrap the input and unwrap element [0] on the way out.
func (e *polyglotEngine) RenameTables(ast AST, mapping map[string]string) (AST, error) {
	out, err := e.c.RenameTables(wrapArray(ast), mapping, polyglot.RenameTablesOptions{})
	if err != nil {
		return nil, fmt.Errorf("engine: rename tables: %w", err)
	}
	elem, err := unwrapArray(out)
	if err != nil {
		return nil, fmt.Errorf("engine: rename tables: %w", err)
	}
	return elem, nil
}

// QualifyTables qualifies unqualified table references with the given database.
// Same wrap/unwrap as RenameTables.
func (e *polyglotEngine) QualifyTables(ast AST, db string) (AST, error) {
	out, err := e.c.QualifyTables(wrapArray(ast), polyglot.QualifyTablesOptions{Dialect: dialect, DB: db})
	if err != nil {
		return nil, fmt.Errorf("engine: qualify tables: %w", err)
	}
	elem, err := unwrapArray(out)
	if err != nil {
		return nil, fmt.Errorf("engine: qualify tables: %w", err)
	}
	return elem, nil
}

func (e *polyglotEngine) Tokenize(sql string) (AST, error) {
	out, err := e.c.Tokenize(sql, dialect)
	if err != nil {
		return nil, fmt.Errorf("engine: tokenize: %w", err)
	}
	return AST(out), nil
}

func (e *polyglotEngine) DiffSQL(sql1, sql2 string) (AST, error) {
	out, err := e.c.Diff(sql1, sql2, dialect)
	if err != nil {
		return nil, fmt.Errorf("engine: diff: %w", err)
	}
	return AST(out), nil
}

func (e *polyglotEngine) Close() error { return e.c.Close() }

// wrapArray wraps a single-statement AST object in a JSON array, as required
// by polyglot operations that take a statement list (Generate, RenameTables,
// QualifyTables). ParseOne returns a bare object; Parse returns an array.
func wrapArray(ast AST) json.RawMessage {
	wrapped := make([]byte, 0, len(ast)+2)
	wrapped = append(wrapped, '[')
	wrapped = append(wrapped, ast...)
	wrapped = append(wrapped, ']')
	return json.RawMessage(wrapped)
}

// unwrapArray extracts element [0] from a single-element JSON array returned
// by RenameTables / QualifyTables, restoring it to the single-statement shape
// that the rest of the Engine interface contract uses.
func unwrapArray(arr json.RawMessage) (AST, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(arr, &elems); err != nil {
		return nil, fmt.Errorf("decode array response: %w", err)
	}
	if len(elems) == 0 {
		return nil, fmt.Errorf("array response was empty")
	}
	return AST(elems[0]), nil
}
