package engine

import "testing"

func TestTablePositionParameter(t *testing.T) {
	e := newTestEngine(t)
	for sql, want := range map[string]bool{
		"SELECT * FROM {p:Identifier}":                                  true,
		"SELECT * FROM db1.{p:Identifier}":                              true,
		"SELECT * FROM {d:Identifier}.t":                                true,
		"SELECT * FROM db1.o WHERE a IN {p:Identifier}":                 true,
		"SELECT * FROM db1.o WHERE in(a, db1.{p:Identifier})":           true,
		"INSERT INTO db1.{p:Identifier} VALUES (1)":                     true,
		"CREATE MATERIALIZED VIEW db1.mv TO {p:Identifier} AS SELECT 1": true,
		"SELECT {c:Identifier} FROM db1.o":                              false,
		"SELECT * FROM db1.o WHERE a = {v:UInt64}":                      false,
		"SELECT * FROM db1.o WHERE a IN (1, 2)":                         false,
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("%s: parse: %v", sql, err)
		}
		got, err := TablePositionParameter(ast)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if got != want {
			t.Errorf("TablePositionParameter(%s) = %v, want %v", sql, got, want)
		}
	}
}

func TestIdentifierParameterInText(t *testing.T) {
	for sql, want := range map[string]bool{
		"EXISTS TABLE db1.{p:Identifier}":        true,
		"RENAME TABLE {d:Identifier}.t TO db1.z": true,
		"USE {d : Identifier}":                   false, // inner whitespace is not a parameter spelling ClickHouse accepts
		"EXISTS TABLE db1.t":                     false,
		"SYSTEM RELOAD CONFIG":                   false,
		"SELECT '{p:Identifier}'":                false, // inside a string literal
	} {
		if got := IdentifierParameterInText(sql); got != want {
			t.Errorf("IdentifierParameterInText(%s) = %v, want %v", sql, got, want)
		}
	}
}
