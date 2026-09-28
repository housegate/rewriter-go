package engine

import (
	"os"
	"testing"
)

func TestClassifyTableFunction(t *testing.T) {
	cases := []struct {
		name string
		want TableFunctionClass
	}{
		{"Merge", TableFunctionRefused},
		{"MERGETREEPARTS", TableFunctionRefused}, // refused by the "mergetree" prefix, not an exact list entry
		{"numbers", TableFunctionDataOnly},
		{"s3Cluster", TableFunctionExternal},
		{"frob", TableFunctionUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyTableFunction(c.name); got != c.want {
				t.Fatalf("ClassifyTableFunction(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestTableEngineAllowed(t *testing.T) {
	cases := []struct {
		name     string
		argCount int
		want     bool
	}{
		{"MergeTree", 0, true},
		{"ReplicatedMergeTree", 0, true},
		{"ReplicatedMergeTree", 2, false},
		{"Buffer", 0, false},
		{"Frob", 0, false},
		{"memory", 0, false},
		{"mergetree", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TableEngineAllowed(c.name, c.argCount); got != c.want {
				t.Fatalf("TableEngineAllowed(%q, %d) = %v, want %v", c.name, c.argCount, got, c.want)
			}
		})
	}
}

func TestClassifyTableEngine(t *testing.T) {
	for _, c := range []struct {
		name     string
		argCount int
		want     TableEngineClass
	}{
		{"Memory", 0, TableEngineAllowedClass},
		{"memory", 0, TableEngineUnknown},
		{"MEMORY", 0, TableEngineUnknown},
		{"replicatedMergeTree", 0, TableEngineUnknown},
		{"ReplicatedMergeTree", 1, TableEngineRefused},
		{"Merge", 2, TableEngineRefused},
		{"Frob", 0, TableEngineRefused},
	} {
		if got := ClassifyTableEngine(c.name, c.argCount); got != c.want {
			t.Errorf("ClassifyTableEngine(%q, %d) = %v, want %v", c.name, c.argCount, got, c.want)
		}
	}
}

func TestRefusedTableSetting(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"disk", true},
		{"storage_policy", true},
		{"max_threads", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RefusedTableSetting(c.name); got != c.want {
				t.Fatalf("RefusedTableSetting(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

// TestCollectSourceFunctionNamesAndCreateTableStorage parses the CREATE and
// ALTER samples from Step 1 and checks the (name, argCount, settings) the T5
// preflight consumes.
func TestCollectSourceFunctionNamesAndCreateTableStorage(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := NewPolyglot("")
	if err != nil {
		t.Skipf("engine unavailable: %v", err)
	}
	defer e.Close()

	t.Run("source function name in FROM position", func(t *testing.T) {
		ast, err := e.ParseOne("SELECT * FROM merge('db1', 'o')")
		if err != nil {
			t.Fatal(err)
		}
		names, err := CollectSourceFunctionNames(ast)
		if err != nil {
			t.Fatal(err)
		}
		if len(names) != 1 || names[0] != "merge" {
			t.Fatalf("names = %v, want [merge]", names)
		}
	})

	t.Run("create table engine with order by and settings", func(t *testing.T) {
		ast, err := e.ParseOne("CREATE TABLE db1.n (a UInt64) ENGINE = MergeTree ORDER BY a SETTINGS storage_policy = 's3'")
		if err != nil {
			t.Fatal(err)
		}
		name, argc, settings, ok, err := CreateTableStorage(e, ast)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || name != "MergeTree" || argc != 0 || len(settings) != 1 || settings[0] != "storage_policy" {
			t.Fatalf("got (name=%q argc=%d settings=%v ok=%v)", name, argc, settings, ok)
		}
	})

	t.Run("create table engine with constructor arguments", func(t *testing.T) {
		ast, err := e.ParseOne("CREATE TABLE db1.n (a UInt64) ENGINE = ReplicatedMergeTree('/clickhouse/tables/x', 'r1')")
		if err != nil {
			t.Fatal(err)
		}
		name, argc, settings, ok, err := CreateTableStorage(e, ast)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || name != "ReplicatedMergeTree" || argc != 2 || len(settings) != 0 {
			t.Fatalf("got (name=%q argc=%d settings=%v ok=%v)", name, argc, settings, ok)
		}
	})

	t.Run("create table bare engine, no settings", func(t *testing.T) {
		ast, err := e.ParseOne("CREATE TABLE db1.n (a UInt64) ENGINE = Memory")
		if err != nil {
			t.Fatal(err)
		}
		name, argc, settings, ok, err := CreateTableStorage(e, ast)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || name != "Memory" || argc != 0 || len(settings) != 0 {
			t.Fatalf("got (name=%q argc=%d settings=%v ok=%v)", name, argc, settings, ok)
		}
	})

	t.Run("alter table modify setting", func(t *testing.T) {
		ast, err := e.ParseOne("ALTER TABLE db1.o MODIFY SETTING disk = 'd'")
		if err != nil {
			t.Fatal(err)
		}
		name, argc, settings, ok, err := CreateTableStorage(e, ast)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || name != "" || argc != 0 || len(settings) != 1 || settings[0] != "disk" {
			t.Fatalf("got (name=%q argc=%d settings=%v ok=%v)", name, argc, settings, ok)
		}
	})

	t.Run("select is neither shape", func(t *testing.T) {
		ast, err := e.ParseOne("SELECT 1")
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, ok, err := CreateTableStorage(e, ast)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatalf("ok = true, want false for a plain SELECT")
		}
	})
}
