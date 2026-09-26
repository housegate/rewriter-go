package engine

import (
	"reflect"
	"testing"
)

func TestCollectDatabaseReferences(t *testing.T) {
	e := newTestEngine(t)
	for sql, want := range map[string][]string{
		"SELECT * FROM db1.o WHERE a IN phys.`x`":                   {"db1", "phys"},
		"INSERT INTO db1.o SELECT * FROM hg_safe.db1__t":            {"db1", "hg_safe"},
		"CREATE TABLE db1.n (a UInt64) ENGINE = Merge('phys', 'x')": {"db1", "phys"},
		"SELECT * FROM merge('hg_promote', 'x')":                    {"hg_promote"},
		"USE phys":                                                  {"phys"},
		"EXISTS TABLE phys.`x`":                                     {"phys"},
		"CREATE MATERIALIZED VIEW db1.mv TO phys.`x` AS SELECT 1":   {"db1", "phys"},
		"SELECT * FROM o":                                           nil,
		"SELECT joinGet('phys.`db2.x`', 'v', 1)":                    {"phys"},
		// Review round 1 finding 1: the identifier form (unquoted, no
		// surrounding string literal) must resolve too.
		"SELECT joinGet(phys.`db2.x`, 'v', 1)": {"phys"},
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		got, err := CollectDatabaseReferences(e, ast, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", sql, got, want)
		}
	}
}
