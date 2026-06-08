package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/handlers"
)

type remoteTableJSON struct {
	Addr, Database, Table, User, Password string
}
type selectCase struct {
	Name                 string            `json:"name"`
	SQL                  string            `json:"sql"`
	Dynamic              *dynamicJSON      `json:"dynamic"`
	Static               *staticJSON       `json:"static"`
	CTE                  map[string]string `json:"cte"`
	LimitForce           *int32            `json:"limit_force"`
	Offset               *int32            `json:"offset"`
	Settings             map[string]int32  `json:"settings"`
	WantCode             string            `json:"want_code"`
	WantTableRewrites    map[string]string `json:"want_table_rewrites"`
	WantFailedCteAliases []string          `json:"want_failed_cte_aliases"`
	WantAccessed         []accessedJSON    `json:"want_accessed"`
	WantSQL              string            `json:"want_sql"`
}
type dynamicJSON struct {
	DatabaseMap            map[string]string `json:"database_map"`
	KnownPhysicalDatabases []string          `json:"known_physical_databases"`
	UpstreamLogical        string            `json:"upstream_logical_database_in_context"`
	Delim                  string            `json:"delim"`
}
type tableWithDBJSON struct {
	Database string `json:"database"`
	Table    string `json:"table"`
}
type staticJSON struct {
	TableMap             map[string]string            `json:"table_map"`
	RemoteTableMap       map[string]remoteTableJSON   `json:"remote_table_map"`
	TableWithDatabaseMap map[string]tableWithDBJSON   `json:"table_with_database_map"`
}
type accessedJSON struct {
	OriginalDatabase string `json:"original_database"`
	OriginalTable    string `json:"original_table"`
	LogicalDatabase  string `json:"logical_database"`
	PhysicalDatabase string `json:"physical_database"`
	IsRemote         bool   `json:"is_remote"`
}

func (c selectCase) options() []*pb.RewriteOption {
	var opts []*pb.RewriteOption
	if c.CTE != nil {
		m := map[string]*pb.RewriteCommonTableExprArgs_CommonTableExpr{}
		for alias, sql := range c.CTE {
			m[alias] = &pb.RewriteCommonTableExprArgs_CommonTableExpr{Alias: alias, Sql: sql}
		}
		opts = append(opts, &pb.RewriteOption{Op: pb.RewriteOp_CommonTableExprRewrite,
			Value: &pb.RewriteOption_CommonTableExprArgs{CommonTableExprArgs: &pb.RewriteCommonTableExprArgs{CteMap: m}}})
	}
	switch {
	case c.Dynamic != nil:
		opts = append(opts, &pb.RewriteOption{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: &pb.RewriteTableDynamicArgs{
				DatabaseMap: c.Dynamic.DatabaseMap, KnownPhysicalDatabases: c.Dynamic.KnownPhysicalDatabases,
				UpstreamLogicalDatabaseInContext: c.Dynamic.UpstreamLogical, Delim: c.Dynamic.Delim}}}})
	case c.Static != nil:
		sa := &pb.RewriteTableStaticArgs{TableMap: c.Static.TableMap}
		if c.Static.RemoteTableMap != nil {
			sa.RemoteTableMap = map[string]*pb.RewriteTableStaticArgs_RemoteTable{}
			for k, r := range c.Static.RemoteTableMap {
				sa.RemoteTableMap[k] = &pb.RewriteTableStaticArgs_RemoteTable{Addr: r.Addr, Database: r.Database, Table: r.Table, User: r.User, Password: r.Password}
			}
		}
		if c.Static.TableWithDatabaseMap != nil {
			sa.TableWithDatabaseMap = map[string]*pb.RewriteTableStaticArgs_TableWithDatabase{}
			for k, v := range c.Static.TableWithDatabaseMap {
				sa.TableWithDatabaseMap[k] = &pb.RewriteTableStaticArgs_TableWithDatabase{Database: v.Database, Table: v.Table}
			}
		}
		opts = append(opts, &pb.RewriteOption{Op: pb.RewriteOp_TableNameRewrite,
			Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{StaticArgs: sa}}})
	}
	if c.LimitForce != nil {
		opts = append(opts, &pb.RewriteOption{Op: pb.RewriteOp_LimitRewrite,
			Value: &pb.RewriteOption_LimitArgs{LimitArgs: &pb.RewriteLimitArgs{Value: &pb.RewriteLimitArgs_ForceLimit{ForceLimit: *c.LimitForce}}}})
	}
	if c.Offset != nil {
		opts = append(opts, &pb.RewriteOption{Op: pb.RewriteOp_OffsetRewrite,
			Value: &pb.RewriteOption_OffsetArgs{OffsetArgs: &pb.RewriteOffsetArgs{Offset: *c.Offset}}})
	}
	if c.Settings != nil {
		var ss []*pb.RewriteSettingsArgs_Setting
		keys := make([]string, 0, len(c.Settings))
		for k := range c.Settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ss = append(ss, &pb.RewriteSettingsArgs_Setting{Key: k, Value: &pb.RewriteSettingsArgs_Setting_IntValue{IntValue: c.Settings[k]}})
		}
		opts = append(opts, &pb.RewriteOption{Op: pb.RewriteOp_SettingsRewrite,
			Value: &pb.RewriteOption_SettingsArgs{SettingsArgs: &pb.RewriteSettingsArgs{Settings: ss}}})
	}
	return opts
}

var codeByName = map[string]pb.RewriteCode{
	"Success":              pb.RewriteCode_Success,
	"SyntaxError":          pb.RewriteCode_SyntaxError,
	"UnsupportedStatement": pb.RewriteCode_UnsupportedStatement,
	"InvalidRewriteRequest": pb.RewriteCode_InvalidRewriteRequest,
}

func loadCases(t *testing.T) []selectCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "select_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []selectCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestSelectGolden(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	oracle, _ := DialOracle() // nil when REWRITER_ORACLE_ADDR unset
	defer oracle.Close()

	for _, c := range loadCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			ast, err := e.ParseOne(c.SQL)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			resp, err := handlers.RewriteSelect(e, ast, c.options())
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if c.WantCode != "" && resp.GetCode() != codeByName[c.WantCode] {
				t.Errorf("code = %v, want %s", resp.GetCode(), c.WantCode)
			}
			if c.WantTableRewrites != nil && !eqStrMap(resp.GetTableRewrites(), c.WantTableRewrites) {
				t.Errorf("table_rewrites = %v, want %v", resp.GetTableRewrites(), c.WantTableRewrites)
			}
			if c.WantFailedCteAliases != nil && !eqStrSlice(resp.GetFailedCteAliases(), c.WantFailedCteAliases) {
				t.Errorf("failed_cte_aliases = %v, want %v", resp.GetFailedCteAliases(), c.WantFailedCteAliases)
			}
			if c.WantAccessed != nil {
				checkAccessed(t, resp.GetOriginalAccessedTables(), c.WantAccessed)
			}
			if c.WantSQL != "" && resp.GetSqlAfterRewrite() != c.WantSQL {
				t.Errorf("sql:\n got %q\nwant %q", resp.GetSqlAfterRewrite(), c.WantSQL)
			}
			// Live oracle differential (only when REWRITER_ORACLE_ADDR is set).
			if oracle != nil {
				want, oerr := oracle.Rewrite(c.SQL, c.options())
				if oerr != nil {
					t.Fatalf("oracle: %v", oerr)
				}
				if d := Compare(resp, want, semanticSQLEq(e)); !d.Equal() {
					t.Errorf("oracle divergence: %v", d.Mismatches)
				}
			}
		})
	}
}

// semanticSQLEq compares two SQL strings by normalizing both through polyglot
// (parse+generate), so cosmetic differences (backticks vs double-quotes, spacing)
// collapse while genuine structural differences remain.
func semanticSQLEq(e engine.Engine) SemanticEq {
	return func(a, b string) (bool, error) {
		na, err := normalizeSQLGolden(e, a)
		if err != nil {
			return false, err
		}
		nb, err := normalizeSQLGolden(e, b)
		if err != nil {
			return false, err
		}
		return na == nb, nil
	}
}

func normalizeSQLGolden(e engine.Engine, sql string) (string, error) {
	ast, err := e.ParseOne(sql)
	if err != nil {
		return "", err
	}
	return e.Generate(ast)
}

func eqStrMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
func eqStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func checkAccessed(t *testing.T, got []*pb.AccessedTable, want []accessedJSON) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("accessed len = %d, want %d (%+v)", len(got), len(want), got)
		return
	}
	for i, w := range want {
		g := got[i]
		if g.GetOriginalDatabase() != w.OriginalDatabase || g.GetOriginalTable() != w.OriginalTable ||
			g.GetLogicalDatabase() != w.LogicalDatabase || g.GetPhysicalDatabase() != w.PhysicalDatabase ||
			g.GetIsRemote() != w.IsRemote {
			t.Errorf("accessed[%d] = %+v, want %+v", i, g, w)
		}
	}
}
