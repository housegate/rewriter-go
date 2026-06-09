package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
)

type granteeJSON struct {
	Name          string `json:"name"`
	IsCurrentUser bool   `json:"is_current_user"`
}

type privilegeDeltaJSON struct {
	Action           string        `json:"action"` // "GRANT" / "REVOKE"
	Scope            string        `json:"scope"`  // "TABLE" / "DATABASE"
	OriginalDatabase string        `json:"original_database"`
	LogicalDatabase  string        `json:"logical_database"`
	PhysicalDatabase string        `json:"physical_database"`
	OriginalTable    string        `json:"original_table"`
	PhysicalTable    string        `json:"physical_table"`
	Privileges       []string      `json:"privileges"`
	Grantees         []granteeJSON `json:"grantees"`
	GrantOption      bool          `json:"grant_option"`
}

// phase4StmtByName carries the Phase-4 statement types that wantStmtType's
// underlying maps (writes/db-level) don't already register, so the Phase-4 driver
// can resolve EXISTS / SHOW CREATE / GRANT / REVOKE before falling back.
var phase4StmtByName = map[string]pb.StatementType{
	"EXISTS_TABLE":      pb.StatementType_STATEMENT_TYPE_EXISTS_TABLE,
	"SHOW_CREATE_TABLE": pb.StatementType_STATEMENT_TYPE_SHOW_CREATE_TABLE,
	"GRANT":             pb.StatementType_STATEMENT_TYPE_GRANT,
	"REVOKE":            pb.StatementType_STATEMENT_TYPE_REVOKE,
}

func phase4StmtType(name string) pb.StatementType {
	if s, ok := phase4StmtByName[name]; ok {
		return s
	}
	return wantStmtType(name)
}

// phase4Case mirrors dblevelCase, swapping database_rewrites for the table-side
// (want_table_rewrites + want_accessed, for EXISTS/SHOW CREATE) and adding
// want_privileges_deltas (for GRANT/REVOKE). SQL is compared semantically by
// default; EXISTS/SHOW CREATE outputs re-parse to a `command` blob (semantic ≈
// exact), so want_sql must match C++ formatAst.
type phase4Case struct {
	Name              string               `json:"name"`
	SQL               string               `json:"sql"`
	Dynamic           *dblevelDynamicJSON  `json:"dynamic"`
	WantCode          string               `json:"want_code"`
	WantStmt          string               `json:"want_stmt"`
	WantTableRewrites map[string]string    `json:"want_table_rewrites"`
	WantAccessed      []accessedJSON       `json:"want_accessed"`
	WantDeltas        []privilegeDeltaJSON `json:"want_privileges_deltas"`
	WantSQL           string               `json:"want_sql"`
	SQLExact          bool                 `json:"sql_exact"`
	SQLContains       []string             `json:"sql_contains"`
	Reject            bool                 `json:"reject"`
}

func (c phase4Case) options() []*pb.RewriteOption {
	if c.Dynamic == nil {
		return nil
	}
	da := &pb.RewriteTableDynamicArgs{
		DatabaseMap:                          c.Dynamic.DatabaseMap,
		KnownPhysicalDatabases:               c.Dynamic.KnownPhysicalDatabases,
		UpstreamLogicalDatabaseInContext:     c.Dynamic.UpstreamLogical,
		Delim:                                c.Dynamic.Delim,
		LogicalDatabaseToRemoteUpstreamIndex: c.Dynamic.LogicalDatabaseToRemoteUpstreamIndex,
	}
	if c.Dynamic.RemoteUpstreams != nil {
		da.RemoteUpstreams = map[string]*pb.RewriteTableDynamicArgs_RemoteUpstream{}
		for k, u := range c.Dynamic.RemoteUpstreams {
			da.RemoteUpstreams[k] = &pb.RewriteTableDynamicArgs_RemoteUpstream{Addr: u.Addr, User: u.User, Password: u.Password}
		}
	}
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: da}}}}
}

func (c phase4Case) wantDeltas() []*pb.PrivilegeDelta {
	if c.WantDeltas == nil {
		return nil
	}
	out := make([]*pb.PrivilegeDelta, 0, len(c.WantDeltas))
	for _, d := range c.WantDeltas {
		pd := &pb.PrivilegeDelta{
			Action:           map[string]pb.PrivilegeDelta_Action{"GRANT": pb.PrivilegeDelta_ACTION_GRANT, "REVOKE": pb.PrivilegeDelta_ACTION_REVOKE}[d.Action],
			Scope:            map[string]pb.PrivilegeDelta_Scope{"TABLE": pb.PrivilegeDelta_SCOPE_TABLE, "DATABASE": pb.PrivilegeDelta_SCOPE_DATABASE}[d.Scope],
			OriginalDatabase: d.OriginalDatabase, LogicalDatabase: d.LogicalDatabase, PhysicalDatabase: d.PhysicalDatabase,
			OriginalTable: d.OriginalTable, PhysicalTable: d.PhysicalTable,
			Privileges: d.Privileges, GrantOption: d.GrantOption,
		}
		for _, g := range d.Grantees {
			pd.Grantees = append(pd.Grantees, &pb.PrivilegeDelta_Grantee{Name: g.Name, IsCurrentUser: g.IsCurrentUser})
		}
		out = append(out, pd)
	}
	return out
}

func loadPhase4Cases(t *testing.T) []phase4Case {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "phase4_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []phase4Case
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// TestPhase4Golden is the Phase-4 parity gate for EXISTS / SHOW CREATE / GRANT /
// REVOKE. Structured fields (code / statement_type / table_rewrites /
// original_accessed_tables / privileges_deltas) are compared EXACTLY;
// sql_after_rewrite SEMANTICALLY. want_* were frozen from the native rewriter's
// real output (verified by the handler unit tests); the REWRITER_ORACLE_ADDR
// differential is the TRUE gate.
func TestPhase4Golden(t *testing.T) {
	if os.Getenv("POLYGLOT_SQL_FFI_PATH") == "" {
		t.Skip("needs engine")
	}
	e, err := engine.NewPolyglot("")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	oracle, _ := DialOracle()
	defer oracle.Close()
	semEq := semanticSQLEq(e)

	for _, c := range loadPhase4Cases(t) {
		t.Run(c.Name, func(t *testing.T) {
			r := newWriteRewriter(e, c.options())
			res, err := r.Rewrite(context.Background(), c.SQL, "acct")
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if c.WantCode != "" && res.Code != codeByName[c.WantCode] {
				t.Errorf("code = %v, want %s (%s)", res.Code, c.WantCode, res.Message)
			}
			if c.WantStmt != "" && res.StatementType != phase4StmtType(c.WantStmt) {
				t.Errorf("statement_type = %v, want %s", res.StatementType, c.WantStmt)
			}
			if c.WantTableRewrites != nil && !eqStrMap(res.TableRewrites, c.WantTableRewrites) {
				t.Errorf("table_rewrites = %v, want %v", res.TableRewrites, c.WantTableRewrites)
			}
			if c.WantAccessed != nil {
				checkAccessed(t, res.OriginalAccessedTables, c.WantAccessed)
			}
			if c.WantDeltas != nil && !privilegeDeltasEqual(res.PrivilegesDeltas, c.wantDeltas()) {
				t.Errorf("privileges_deltas = %+v, want %+v", res.PrivilegesDeltas, c.wantDeltas())
			}
			checkPhase4SQL(t, c, res.SQL, semEq)

			if oracle != nil {
				want, oerr := oracle.Rewrite(c.SQL, c.options())
				if oerr != nil {
					t.Fatalf("oracle: %v", oerr)
				}
				got := pbFromResult(res)
				cmpEq := semEq
				if c.Reject {
					got.SqlAfterRewrite = want.GetSqlAfterRewrite()
					if got.SqlAfterRewrite == "" {
						cmpEq = nil
					}
				}
				if d := Compare(got, want, cmpEq); !d.Equal() {
					t.Errorf("oracle divergence: %v", d.Mismatches)
				}
			}
		})
	}
}

func checkPhase4SQL(t *testing.T, c phase4Case, got string, semEq SemanticEq) {
	t.Helper()
	switch {
	case len(c.SQLContains) > 0:
		for _, sub := range c.SQLContains {
			if !strings.Contains(got, sub) {
				t.Errorf("sql:\n got %q\nwant contains %q", got, sub)
			}
		}
	case c.SQLExact:
		if got != c.WantSQL {
			t.Errorf("sql (exact):\n got %q\nwant %q", got, c.WantSQL)
		}
	case c.WantSQL != "":
		eq, err := semEq(got, c.WantSQL)
		if err != nil {
			t.Errorf("sql (semantic-error): %v\n got %q\nwant %q", err, got, c.WantSQL)
		} else if !eq {
			t.Errorf("sql (semantic):\n got %q\nwant %q", got, c.WantSQL)
		}
	}
}
