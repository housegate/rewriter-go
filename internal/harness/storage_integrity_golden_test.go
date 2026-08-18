package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// siDynamicJSON is dblevelDynamicJSON plus the storage_integrity block.
type siDynamicJSON struct {
	DatabaseMap                          map[string]string             `json:"database_map"`
	KnownPhysicalDatabases               []string                      `json:"known_physical_databases"`
	UpstreamLogical                      string                        `json:"upstream_logical_database_in_context"`
	Delim                                string                        `json:"delim"`
	LogicalDatabaseToRemoteUpstreamIndex map[string]string             `json:"logical_database_to_remote_upstream_index"`
	RemoteUpstreams                      map[string]remoteUpstreamJSON `json:"remote_upstreams"`
	StorageIntegrity                     *siArgsJSON                   `json:"storage_integrity"`
}

type siArgsJSON struct {
	Tables              map[string]siTableJSON `json:"tables"`
	ReadMode            string                 `json:"read_mode"` // "SAFE" | "UNSAFE_LATEST"
	ReservedRowIDColumn string                 `json:"reserved_row_id_column"`
}

type siTableJSON struct {
	SafeTable           string   `json:"safe_table"`
	UnsafeTable         string   `json:"unsafe_table"`
	ExcludedUnsafeParts []string `json:"excluded_unsafe_parts"`
}

type siCase struct {
	Name                string            `json:"name"`
	SQL                 string            `json:"sql"`
	Dynamic             *siDynamicJSON    `json:"dynamic"`
	WantCode            string            `json:"want_code"`
	WantStmt            string            `json:"want_stmt"`
	WantTableRewrites   map[string]string `json:"want_table_rewrites"`
	WantAccessed        []accessedJSON    `json:"want_accessed"`
	WantSQL             string            `json:"want_sql"`
	SQLExact            bool              `json:"sql_exact"`
	WantSQLContains     []string          `json:"want_sql_contains"`
	WantSQLNotContains  []string          `json:"want_sql_not_contains"`
	WantMessageContains string            `json:"want_message_contains"`
	Reject              bool              `json:"reject"`
	AllowSQLDivergence  bool              `json:"allow_sql_divergence"`
	WantNoContractAck   bool              `json:"want_no_contract_ack"`
}

var siReadModeByName = map[string]pb.StorageIntegrityArgs_ReadMode{
	"":              pb.StorageIntegrityArgs_READ_MODE_UNSPECIFIED,
	"SAFE":          pb.StorageIntegrityArgs_READ_MODE_SAFE,
	"UNSAFE_LATEST": pb.StorageIntegrityArgs_READ_MODE_UNSAFE_LATEST,
	"INVALID_99":    pb.StorageIntegrityArgs_ReadMode(99),
}

// siStmtByName adds the statement types the shared maps do not know yet.
var siStmtByName = map[string]pb.StatementType{
	"DESCRIBE": pb.StatementType_STATEMENT_TYPE_DESCRIBE,
}

var siCodeByName = map[string]pb.RewriteCode{
	"Success":               pb.RewriteCode_Success,
	"SyntaxError":           pb.RewriteCode_SyntaxError,
	"RewriteError":          pb.RewriteCode_RewriteError,
	"UnsupportedStatement":  pb.RewriteCode_UnsupportedStatement,
	"InvalidRewriteRequest": pb.RewriteCode_InvalidRewriteRequest,
}

func siStmtType(name string) pb.StatementType {
	if s, ok := siStmtByName[name]; ok {
		return s
	}
	return phase4StmtType(name)
}

func (c siCase) options() []*pb.RewriteOption {
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
	if si := c.Dynamic.StorageIntegrity; si != nil {
		args := &pb.StorageIntegrityArgs{
			Tables:              map[string]*pb.StorageIntegrityArgs_Table{},
			ReadMode:            siReadModeByName[si.ReadMode],
			ReservedRowIdColumn: si.ReservedRowIDColumn,
			ContractVersion:     pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1,
		}
		for k, t := range si.Tables {
			args.Tables[k] = &pb.StorageIntegrityArgs_Table{
				SafeTable: t.SafeTable, UnsafeTable: t.UnsafeTable, ExcludedUnsafeParts: t.ExcludedUnsafeParts,
			}
		}
		da.StorageIntegrity = args
	}
	return []*pb.RewriteOption{{Op: pb.RewriteOp_TableNameRewrite,
		Value: &pb.RewriteOption_TableNameArgs{TableNameArgs: &pb.RewriteTableNameArgs{DynamicArgs: da}}}}
}

func loadSICases(t *testing.T) []siCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "storage_integrity_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []siCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// TestStorageIntegrityGolden is the Spec G parity gate. Cases are driven
// through the public NativeRewriter (full doRewrite dispatch) so every
// statement family is covered. Structured fields compare exactly, SQL
// semantically (exact when sql_exact), plus engine-agnostic
// want_sql_contains / want_sql_not_contains substrings that the C++ test
// applies to the identical JSON.
func TestStorageIntegrityGolden(t *testing.T) {
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

	for _, c := range loadSICases(t) {
		t.Run(c.Name, func(t *testing.T) {
			r := newWriteRewriter(e, c.options())
			res, err := r.Rewrite(context.Background(), c.SQL, "acct")
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if c.Dynamic != nil && c.Dynamic.StorageIntegrity != nil && len(c.Dynamic.StorageIntegrity.Tables) > 0 {
				want := pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1
				if c.WantNoContractAck {
					want = pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_UNSPECIFIED
				}
				if res.StorageIntegrityContractVersion != want {
					t.Errorf("storage_integrity_contract_version = %v, want %v", res.StorageIntegrityContractVersion, want)
				}
			}
			if c.WantCode != "" && res.Code != siCodeByName[c.WantCode] {
				t.Errorf("code = %v, want %s (%s)", res.Code, c.WantCode, res.Message)
			}
			if res.StatementType != siStmtType(c.WantStmt) {
				t.Errorf("statement_type = %v, want %q", res.StatementType, c.WantStmt)
			}
			if c.WantMessageContains != "" && !strings.Contains(res.Message, c.WantMessageContains) {
				t.Errorf("message = %q, want contains %q", res.Message, c.WantMessageContains)
			}
			if c.WantTableRewrites != nil && !eqStrMap(res.TableRewrites, c.WantTableRewrites) {
				t.Errorf("table_rewrites = %v, want %v", res.TableRewrites, c.WantTableRewrites)
			}
			if c.WantAccessed != nil {
				checkAccessed(t, res.OriginalAccessedTables, c.WantAccessed)
			}
			if c.Reject {
				if res.SQL != c.SQL {
					t.Errorf("reject must echo original SQL: got %q", res.SQL)
				}
			} else {
				switch {
				case c.SQLExact:
					if res.SQL != c.WantSQL {
						t.Errorf("sql (exact):\n got %q\nwant %q", res.SQL, c.WantSQL)
					}
				case c.WantSQL != "":
					if eq, err := semEq(res.SQL, c.WantSQL); err != nil || !eq {
						t.Errorf("sql (semantic):\n got %q\nwant %q (err=%v)", res.SQL, c.WantSQL, err)
					}
				}
			}
			for _, sub := range c.WantSQLContains {
				if !strings.Contains(res.SQL, sub) {
					t.Errorf("sql %q must contain %q", res.SQL, sub)
				}
			}
			for _, sub := range c.WantSQLNotContains {
				if strings.Contains(res.SQL, sub) {
					t.Errorf("sql %q must NOT contain %q", res.SQL, sub)
				}
			}
			if oracle != nil {
				want, oerr := oracle.Rewrite(c.SQL, c.options())
				if oerr != nil {
					t.Fatalf("oracle: %v", oerr)
				}
				got := pbFromResult(res)
				cmpEq := semEq
				if c.Reject || c.AllowSQLDivergence {
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
