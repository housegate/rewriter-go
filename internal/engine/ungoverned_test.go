package engine

import "testing"

func TestOpaqueTextIsUngoverned(t *testing.T) {
	e := newTestEngine(t)
	for text, want := range map[string]bool{
		"DELETE WHERE a = 1":                                                        false,
		"DELETE WHERE a IN (1, 2)":                                                  false,
		"DELETE WHERE a IN tuple(1, 2)":                                             false,
		"DELETE IN PARTITION tuple() WHERE a = 1":                                   false,
		"MODIFY TTL d + INTERVAL 1 DAY":                                             false,
		"MODIFY COLUMN b UInt8 DEFAULT 2":                                           false,
		"MOVE PARTITION tuple() TO DISK 'd'":                                        false,
		"ATTACH PART 'x'":                                                           false,
		"FREEZE WITH NAME 'x'":                                                      false,
		"MODIFY COMMENT 'select from with'":                                         false,
		"ADD PROJECTION p(SELECT a ORDER BY b)":                                     false,
		"DELETE WHERE a IN(SELECT 1)":                                               true,
		"DELETE WHERE a IN db1.p":                                                   true,
		"DELETE WHERE a IN p":                                                       true,
		`DELETE WHERE a IN "db2.x"`:                                                 true,
		"DELETE WHERE a NOT IN ((db1.p))":                                           true,
		"DELETE WHERE a GLOBAL IN db1.p":                                            true,
		"DELETE WHERE in(a, db1.p)":                                                 true,
		"DELETE WHERE a IN {p:Identifier}":                                          true,
		"MODIFY QUERY SELECT * FROM db1.p":                                          true,
		"MODIFY COLUMN b UInt8 DEFAULT(SELECT 1)":                                   true,
		"MODIFY TTL d DELETE WHERE a IN db1.v":                                      true,
		"FETCH PARTITION tuple() FROM '/x'":                                         true,
		"FETCH PART 'p' FROM '/x'":                                                  true,
		"ATTACH PARTITION tuple() FROM db1.p":                                       true,
		"REPLACE PARTITION tuple() FROM db1.p":                                      true,
		"MOVE PARTITION tuple() TO TABLE db1.p":                                     true,
		"ADD PROJECTION p(SELECT a FROM db1.p)":                                     true,
		"ADD PROJECTION p(SELECT a WHERE a IN (SELECT 1))":                          true,
		"ALTER TABLE db1.o UPDATE b = 1 WHERE 1, DELETE WHERE a IN db1.p":           true,
		"ALTER TABLE db1.o UPDATE b = 1 WHERE 1, FETCH PARTITION tuple() FROM '/x'": true,
	} {
		if got := OpaqueTextIsUngoverned(e, text); got != want {
			t.Errorf("OpaqueTextIsUngoverned(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestOpaqueTextDatabases(t *testing.T) {
	e := newTestEngine(t)
	dbs, ok := OpaqueTextDatabases(e, `ALTER TABLE db1.o UPDATE b = (SELECT max(a) FROM hg_promote.x) WHERE a IN phys."db2.x"`)
	if !ok {
		t.Fatal("tokenize failed")
	}
	want := []string{"db1", "hg_promote", "phys"}
	if len(dbs) != len(want) {
		t.Fatalf("dbs = %v, want %v", dbs, want)
	}
	for i := range want {
		if dbs[i] != want[i] {
			t.Fatalf("dbs = %v, want %v", dbs, want)
		}
	}
}

func TestExpressionPositionHasReads(t *testing.T) {
	e := newTestEngine(t)
	for sql, want := range map[string]bool{
		"UPDATE db1.o SET b = 1 WHERE a = 1":                                                   false,
		"UPDATE db1.o SET b = 1 WHERE a IN db1.p":                                              true,
		"DELETE FROM db1.o WHERE a IN (SELECT a FROM p)":                                       true,
		"INSERT INTO db1.o VALUES (1)":                                                         false,
		"INSERT INTO db1.o VALUES ((SELECT 1 FROM db1.p))":                                     true,
		"INSERT INTO db1.o SELECT * FROM db1.p":                                                false,
		"CREATE TABLE db1.n ENGINE = Memory AS SELECT * FROM db1.p":                            false,
		"CREATE TABLE db1.n (a UInt64 DEFAULT 1) ENGINE = MergeTree ORDER BY a":                false,
		"CREATE TABLE db1.n (a UInt64 DEFAULT (SELECT 1 FROM db1.p)) ENGINE = Memory":          true,
		"CREATE TABLE db1.n (a UInt64) ENGINE = MergeTree ORDER BY a IN db1.p":                 true,
		"CREATE MATERIALIZED VIEW db1.mv ENGINE = MergeTree ORDER BY a AS SELECT * FROM db1.o": false,
		"ALTER TABLE db1.o ADD COLUMN c UInt8 DEFAULT a IN db1.p":                              true,
		"ALTER TABLE db1.o REPLACE PARTITION tuple() FROM db1.p":                               true,
		"SELECT * FROM db1.o WHERE a IN db1.p":                                                 false,
	} {
		ast, err := e.ParseOne(sql)
		if err != nil {
			t.Fatalf("%s: parse: %v", sql, err)
		}
		got, err := ExpressionPositionHasReads(ast)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if got != want {
			t.Errorf("ExpressionPositionHasReads(%s) = %v, want %v", sql, got, want)
		}
	}
}
