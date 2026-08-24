package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-proto/gen/pb"
)

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

func (c SICase) options() []*pb.RewriteOption {
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
	if c.Dynamic.UpstreamPhysical != "" {
		da.UpstreamPhysicalDatabaseInContext = &c.Dynamic.UpstreamPhysical
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

// TestStorageIntegrityGolden is the Spec G parity gate. Cases are driven
// through the public NativeRewriter (full doRewrite dispatch) so every
// statement family is covered. Structured fields compare exactly, while
// UPDATE_GOLDEN writes normalized exact SQL pins. Engine-agnostic
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
	update := os.Getenv(UpdateGoldenEnv) == "1"
	if update && oracle == nil {
		t.Fatalf("%s=1 requires %s to point at a running rewriter-grpc so want_sql_cpp can be regenerated",
			UpdateGoldenEnv, OracleAddrEnv)
	}
	cases := LoadSICorpus(t)

	for i := range cases {
		c := &cases[i]
		t.Run(c.Name, func(t *testing.T) {
			r := newWriteRewriter(e, c.options())
			res, err := r.Rewrite(context.Background(), c.SQL, "acct")
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if update {
				if c.Reject {
					return // reject cases echo the input; nothing to pin
				}
				want, oerr := oracle.Rewrite(c.SQL, c.options())
				if oerr != nil {
					t.Fatalf("oracle: %v", oerr)
				}
				goSQL := NormalizeSIIdentifierQuotes(res.SQL)
				cppSQL := NormalizeSIIdentifierQuotes(want.GetSqlAfterRewrite())
				c.WantSQL, c.WantSQLGo, c.WantSQLCPP = "", "", ""
				if goSQL == cppSQL {
					c.AllowSQLDivergence = false
					c.WantSQL = goSQL
				} else {
					c.AllowSQLDivergence = true
					c.WantSQLGo, c.WantSQLCPP = goSQL, cppSQL
				}
				return
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
			} else if c.WantSQL != "" {
				if eq, err := semEq(res.SQL, c.WantSQL); err != nil || !eq {
					t.Errorf("sql (semantic):\n got %q\nwant %q (err=%v)", res.SQL, c.WantSQL, err)
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
	if update {
		if t.Failed() {
			t.Fatal("regeneration failed; corpus was not rewritten")
		}
		writeSICorpus(t, cases)
	}
}

// writeSICorpus rewrites the corpus file deterministically.
//
// SetEscapeHTML(false) is mandatory: the corpus contains SQL such as
// `WHERE a > 1`, and the default encoder would escape every `>`, producing a
// 145 KB diff of pure escaping noise.
func writeSICorpus(t *testing.T, cases []SICase) {
	t.Helper()
	path := SICorpusPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var documents []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &documents); err != nil {
		t.Fatalf("decode %s for update: %v", path, err)
	}
	if len(documents) != len(cases) {
		t.Fatalf("update %s: decoded %d raw cases, generated %d cases", path, len(documents), len(cases))
	}
	for i := range cases {
		var name string
		if err := json.Unmarshal(documents[i]["name"], &name); err != nil {
			t.Fatalf("update %s case %d name: %v", path, i, err)
		}
		if name != cases[i].Name {
			t.Fatalf("update %s case %d order changed: raw name %q, generated name %q",
				path, i, name, cases[i].Name)
		}

		// Preserve the raw JSON for every non-mutable field. Re-encoding the
		// SICase structs directly would collapse explicit empty maps and add
		// zero-valued fields from nested shared structs, hiding unrelated
		// corpus changes inside the regeneration diff.
		for _, key := range []string{
			"want_sql", "want_sql_go", "want_sql_cpp", "allow_sql_divergence", "sql_exact",
		} {
			delete(documents[i], key)
		}
		putString := func(key, value string) {
			if value == "" {
				return
			}
			var encoded bytes.Buffer
			stringEncoder := json.NewEncoder(&encoded)
			stringEncoder.SetEscapeHTML(false)
			if err := stringEncoder.Encode(value); err != nil {
				t.Fatalf("encode %s.%s: %v", name, key, err)
			}
			documents[i][key] = bytes.TrimSpace(encoded.Bytes())
		}
		putString("want_sql", cases[i].WantSQL)
		putString("want_sql_go", cases[i].WantSQLGo)
		putString("want_sql_cpp", cases[i].WantSQLCPP)
		if cases[i].AllowSQLDivergence {
			documents[i]["allow_sql_divergence"] = json.RawMessage("true")
		}
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(documents); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("rewrote %s: %d cases, %d bytes", path, len(cases), out.Len())
}

func TestWriteSICorpusPreservesNonMutableFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "storage_integrity_cases.json")
	raw := []byte(`[
  {
    "name": "agree",
    "sql": "SELECT a FROM db.t WHERE a > 1",
    "dynamic": {"database_map": {}},
    "want_code": "Success",
    "want_stmt": "",
    "want_sql": "old",
    "sql_exact": true,
    "want_accessed": [{"original_database": "", "is_remote": false}]
  },
  {
    "name": "diverge",
    "sql": "SELECT b FROM db.t",
    "want_code": "Success",
    "want_sql": "stale",
    "allow_sql_divergence": false
  }
]`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SICorpusPathEnv, path)

	cases := []SICase{
		{Name: "agree", WantSQL: `SELECT a FROM phys."db.t" WHERE a > 1`},
		{
			Name: "diverge", AllowSQLDivergence: true,
			WantSQLGo: `SELECT b FROM phys."db.t"`, WantSQLCPP: `SELECT b FROM phys."db.t" AS "db.t"`,
		},
	}
	var before []map[string]any
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	stripMutable := func(documents []map[string]any) {
		for _, document := range documents {
			for _, key := range []string{
				"want_sql", "want_sql_go", "want_sql_cpp", "allow_sql_divergence", "sql_exact",
			} {
				delete(document, key)
			}
		}
	}
	stripMutable(before)

	writeSICorpus(t, cases)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(first, []byte(`\u003e`)) {
		t.Fatalf("writer escaped SQL comparison operator: %s", first)
	}
	var after []map[string]any
	if err := json.Unmarshal(first, &after); err != nil {
		t.Fatal(err)
	}
	stripMutable(after)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("non-mutable corpus fields changed:\n before=%#v\n  after=%#v", before, after)
	}
	var pins []map[string]json.RawMessage
	if err := json.Unmarshal(first, &pins); err != nil {
		t.Fatal(err)
	}
	if _, ok := pins[0]["sql_exact"]; ok {
		t.Fatal("sql_exact survived regeneration")
	}
	if _, ok := pins[0]["allow_sql_divergence"]; ok {
		t.Fatal("agreeing case retained allow_sql_divergence")
	}
	if got := string(pins[0]["want_sql"]); got != `"SELECT a FROM phys.\"db.t\" WHERE a > 1"` {
		t.Fatalf("agreeing pin = %s", got)
	}
	if string(pins[1]["allow_sql_divergence"]) != "true" ||
		len(pins[1]["want_sql_go"]) == 0 || len(pins[1]["want_sql_cpp"]) == 0 {
		t.Fatalf("divergent pins = %v", pins[1])
	}

	writeSICorpus(t, cases)
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("writer output is not deterministic")
	}
}
