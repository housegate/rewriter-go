package nameresolve

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

func testEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", "''")
}

func TestSystemTableTenantFiltered(t *testing.T) {
	for _, table := range []string{"tables", "columns", "data_skipping_indices", "projections", "parts", "mutations", "databases"} {
		if !SystemTableTenantFiltered(table) {
			t.Errorf("system.%s should be tenant-filtered", table)
		}
		if !SystemTableAllowed(table) {
			t.Errorf("system.%s should stay on the allowlist", table)
		}
	}
	for _, table := range []string{"clusters", "one", "numbers", "processes", "completions", "parts_columns", "Tables", ""} {
		if SystemTableTenantFiltered(table) {
			t.Errorf("system.%q should not be tenant-filtered", table)
		}
	}
}

func TestSystemTableTenantFilterSQL(t *testing.T) {
	one := &pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"db1": "phys"}, Delim: "_"}
	several := &pb.RewriteTableDynamicArgs{
		DatabaseMap:    map[string]string{"zz": "phys", "db1": "phys", "aa": "other", "": "phys", "nophys": ""},
		Delim:          "-",
		ExtraArguments: []string{"r0"},
	}
	quoted := &pb.RewriteTableDynamicArgs{DatabaseMap: map[string]string{"o'k": `p\h'ys`}}
	for _, tc := range []struct {
		table string
		args  *pb.RewriteTableDynamicArgs
		want  string
	}{
		{"tables", one, "SELECT * FROM system.tables WHERE database = 'phys' AND startsWith(name, 'db1.')"},
		{"columns", one, "SELECT * FROM system.columns WHERE database = 'phys' AND startsWith(table, 'db1.')"},
		{"data_skipping_indices", one, "SELECT * FROM system.data_skipping_indices WHERE database = 'phys' AND startsWith(table, 'db1.')"},
		{"projections", one, "SELECT * FROM system.projections WHERE database = 'phys' AND startsWith(table, 'db1.')"},
		{"parts", one, "SELECT * FROM system.parts WHERE database = 'phys' AND startsWith(table, 'db1.')"},
		{"mutations", one, "SELECT * FROM system.mutations WHERE database = 'phys' AND startsWith(table, 'db1.')"},
		{"databases", one, "SELECT * FROM system.databases WHERE name IN ('phys')"},
		// (physical, logical) order; empty logicals and physicals are skipped;
		// the prefix carries the delimiter and extra arguments.
		{"tables", several, "SELECT * FROM system.tables WHERE (database = 'other' AND startsWith(name, 'aa-r0.')) OR " +
			"(database = 'phys' AND startsWith(name, 'db1-r0.')) OR (database = 'phys' AND startsWith(name, 'zz-r0.'))"},
		{"databases", several, "SELECT * FROM system.databases WHERE name IN ('other', 'phys')"},
		{"tables", quoted, `SELECT * FROM system.tables WHERE database = 'p\\h''ys' AND startsWith(name, 'o''k.')`},
		// No mapped database: nothing is shown.
		{"tables", &pb.RewriteTableDynamicArgs{}, "SELECT * FROM system.tables WHERE 0"},
		{"databases", nil, "SELECT * FROM system.databases WHERE 0"},
	} {
		if got := SystemTableTenantFilterSQL(tc.table, tc.args, testEscape); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.table, got, tc.want)
		}
	}
}
