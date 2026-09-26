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
		// Command kinds.
		"RENAME TABLE phys.x TO db1.z":                     {"phys", "db1"},
		"DESCRIBE TABLE hg_safe.x":                         {"hg_safe"},
		"SHOW TABLES FROM phys":                            {"phys"},
		"ALTER TABLE db1.o UPDATE b = 1 WHERE a IN phys.x": {"db1", "phys"},
		// RHS forms and parenthesised / nested IN operands (spec 2026-09-26 R1).
		"SELECT * FROM db1.o WHERE (a, b) IN phys.x":  {"db1", "phys"},
		"SELECT * FROM db1.o WHERE a IN (phys.x)":     {"db1", "phys"},
		"SELECT * FROM db1.o WHERE a IN ((phys.x))":   {"db1", "phys"},
		"SELECT * FROM db1.o WHERE in(a, ((phys.x)))": {"db1", "phys"},
		"SELECT * FROM db1.o WHERE a IN `db2.x`":      {"db1"},
		// Mutation and column-expression positions (R2).
		"ALTER TABLE db1.o DELETE WHERE a IN (SELECT a FROM hg_promote.x)":                       {"db1", "hg_promote"},
		"DELETE FROM db1.o WHERE a IN (SELECT a FROM phys.x)":                                    {"db1", "phys"},
		"CREATE TABLE db1.n (a UInt64 DEFAULT (SELECT max(a) FROM hg_unsafe.x)) ENGINE = Memory": {"db1", "hg_unsafe"},
		"ALTER TABLE db1.o REPLACE PARTITION tuple() FROM phys.x":                                {"db1", "phys"},
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

func TestCollectDatabaseReferenceSets_Blind(t *testing.T) {
	e := newTestEngine(t)
	for sql, want := range map[string][]string{
		"SELECT * FROM db1.o WHERE a IN hg_safe.x":        nil,
		"SELECT * FROM db1.o WHERE a IN (hg_safe.x)":      {"hg_safe"},
		"SELECT * FROM db1.o WHERE a IN ((hg_safe.x))":    {"hg_safe"},
		"SELECT joinGet('hg_safe.x', 'v', 1)":             {"hg_safe"},
		"SELECT hasColumnInTable('hg_safe', 'x', 'c')":    {"hg_safe"},
		"SELECT * FROM hg_safe.x":                         nil,
		"SELECT * FROM db1.o WHERE a IN ('hg_safe.x', 1)": nil,
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		_, blind, err := CollectDatabaseReferenceSets(e, ast, sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if !reflect.DeepEqual(blind, want) {
			t.Errorf("%s: blind %v want %v", sql, blind, want)
		}
	}
}
