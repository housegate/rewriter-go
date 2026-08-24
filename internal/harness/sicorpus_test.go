package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// SICorpusPathEnv overrides which corpus file the harness loads. Used by the
// contract meta-test to validate a candidate file before it is published to
// rewriter-grpc.
const SICorpusPathEnv = "SI_CORPUS_PATH"

// UpdateGoldenEnv, set to exactly "1", switches golden tests in this repo from
// comparing to regenerating. Named after the repo's existing env-var
// convention (see OracleAddrEnv in oracle.go).
const UpdateGoldenEnv = "UPDATE_GOLDEN"

// SICorpusPath resolves the storage-integrity corpus file.
func SICorpusPath() string {
	if p := os.Getenv(SICorpusPathEnv); p != "" {
		return p
	}
	return filepath.Join("testdata", "storage_integrity_cases.json")
}

// SITable is one logical storage-integrity table's physical mapping.
type SITable struct {
	SafeTable           string   `json:"safe_table"`
	UnsafeTable         string   `json:"unsafe_table"`
	ExcludedUnsafeParts []string `json:"excluded_unsafe_parts,omitempty"`
}

// SIArgs is the storage_integrity block of a case's dynamic args.
type SIArgs struct {
	Tables              map[string]SITable `json:"tables"`
	ReadMode            string             `json:"read_mode,omitempty"` // "" | "SAFE" | "UNSAFE_LATEST" | "INVALID_99"
	ReservedRowIDColumn string             `json:"reserved_row_id_column,omitempty"`
}

// SIDynamic is the dynamic-args block of a case.
type SIDynamic struct {
	DatabaseMap                          map[string]string             `json:"database_map,omitempty"`
	KnownPhysicalDatabases               []string                      `json:"known_physical_databases,omitempty"`
	UpstreamLogical                      string                        `json:"upstream_logical_database_in_context,omitempty"`
	UpstreamPhysical                     string                        `json:"upstream_physical_database_in_context,omitempty"`
	Delim                                string                        `json:"delim,omitempty"`
	LogicalDatabaseToRemoteUpstreamIndex map[string]string             `json:"logical_database_to_remote_upstream_index,omitempty"`
	RemoteUpstreams                      map[string]remoteUpstreamJSON `json:"remote_upstreams,omitempty"`
	StorageIntegrity                     *SIArgs                       `json:"storage_integrity,omitempty"`
}

// SICase is the frozen corpus schema. Every key is listed here; the loader
// rejects any other key, which is what deletes `sql_exact` as a concept.
//
// Contract (Spec J D3), enforced by ValidateSICorpus:
//   - every case carries an explicit, known want_code;
//   - a reject case carries want_code != "Success" and a want_message_contains
//     substring, and pins no SQL;
//   - a success case pins SQL exactly -- one want_sql when the engines agree,
//     or allow_sql_divergence plus both want_sql_go and want_sql_cpp when they
//     legitimately differ;
//   - want_sql_contains is an additional assertion only, and may not contain a
//     substring that is already present in the input SQL.
type SICase struct {
	Name                string            `json:"name"`
	SQL                 string            `json:"sql"`
	Dynamic             *SIDynamic        `json:"dynamic,omitempty"`
	WantCode            string            `json:"want_code,omitempty"`
	WantStmt            string            `json:"want_stmt,omitempty"`
	WantSQL             string            `json:"want_sql,omitempty"`
	WantSQLGo           string            `json:"want_sql_go,omitempty"`
	WantSQLCPP          string            `json:"want_sql_cpp,omitempty"`
	WantSQLContains     []string          `json:"want_sql_contains,omitempty"`
	WantSQLNotContains  []string          `json:"want_sql_not_contains,omitempty"`
	WantMessageContains string            `json:"want_message_contains,omitempty"`
	WantTableRewrites   map[string]string `json:"want_table_rewrites,omitempty"`
	WantAccessed        []accessedJSON    `json:"want_accessed,omitempty"`
	Reject              bool              `json:"reject,omitempty"`
	AllowSQLDivergence  bool              `json:"allow_sql_divergence,omitempty"`
	WantNoContractAck   bool              `json:"want_no_contract_ack,omitempty"`
}

// LoadSICorpus reads exactly one JSON value and strictly decodes the corpus.
// DisallowUnknownFields freezes the schema: a stray or deleted key (notably
// `sql_exact`) is a load error, not a silently ignored field.
func LoadSICorpus(t *testing.T) []SICase {
	t.Helper()
	path := SICorpusPath()
	cases, err := loadSICorpusFile(path)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return cases
}

func loadSICorpusFile(path string) ([]SICase, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var cases []SICase
	if err := dec.Decode(&cases); err != nil {
		return nil, err
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("corpus must contain exactly one JSON value")
		}
		return nil, fmt.Errorf("decode content after corpus JSON value: %w", err)
	}
	return cases, nil
}

var siKnownCodes = map[string]pb.RewriteCode{
	"Success":               pb.RewriteCode_Success,
	"SyntaxError":           pb.RewriteCode_SyntaxError,
	"RewriteError":          pb.RewriteCode_RewriteError,
	"UnsupportedStatement":  pb.RewriteCode_UnsupportedStatement,
	"InvalidRewriteRequest": pb.RewriteCode_InvalidRewriteRequest,
}

// ValidateSICorpus returns one string per contract violation. An empty slice
// means the corpus satisfies Spec J D3. The rule ids are stable so the C++
// mirror can emit the same text.
func ValidateSICorpus(cases []SICase) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range cases {
		add := func(rule, detail string) {
			out = append(out, fmt.Sprintf("%s: %s: %s", c.Name, rule, detail))
		}
		if strings.TrimSpace(c.Name) == "" {
			out = append(out, "<unnamed>: R1: name must be non-empty")
			continue
		}
		if seen[c.Name] {
			add("R1", "duplicate case name")
		}
		seen[c.Name] = true
		if strings.TrimSpace(c.SQL) == "" {
			add("R2", "sql must be non-empty")
		}
		if c.WantCode == "" {
			add("R6", "want_code must be non-empty")
		} else if _, ok := siKnownCodes[c.WantCode]; !ok {
			add("R6", "unknown want_code "+c.WantCode)
		}
		isReject := c.WantCode != "" && c.WantCode != "Success"
		isSuccess := c.WantCode == "Success"
		switch {
		case isReject:
			if !c.Reject {
				add("R3", "want_code != Success must set reject: true")
			}
			if c.WantMessageContains == "" {
				add("R3", "a reject case must carry want_message_contains")
			}
			if c.AllowSQLDivergence {
				add("R3", "a reject case forbids allow_sql_divergence")
			}
			if c.WantSQL != "" || c.WantSQLGo != "" || c.WantSQLCPP != "" ||
				len(c.WantSQLContains) > 0 || len(c.WantSQLNotContains) > 0 {
				add("R3", "a reject case must not pin SQL (it echoes the input)")
			}
		case isSuccess:
			if c.Reject {
				add("R3", "reject: true requires want_code != Success")
			}
			if c.AllowSQLDivergence {
				if c.WantSQLGo == "" || c.WantSQLCPP == "" {
					add("R3", "allow_sql_divergence requires both want_sql_go and want_sql_cpp")
				}
				if c.WantSQL != "" {
					add("R3", "allow_sql_divergence forbids want_sql; pin each engine separately")
				}
			} else {
				if c.WantSQL == "" {
					add("R3", "a success case must carry want_sql")
				}
				if c.WantSQLGo != "" || c.WantSQLCPP != "" {
					add("R3", "per-engine pins require allow_sql_divergence: true")
				}
			}
		default:
			if c.Reject {
				add("R3", "reject: true requires an explicit non-Success want_code")
			}
		}
		if isSuccess {
			for _, sub := range c.WantSQLContains {
				if sub == "" {
					add("R4", "want_sql_contains entry must be non-empty")
					continue
				}
				if siContainsEquivalent(c.SQL, sub) {
					add("R4", fmt.Sprintf("vacuous want_sql_contains %q: already a substring of the input SQL, so a no-op rewriter passes", sub))
				}
			}
			for _, sub := range c.WantSQLNotContains {
				if sub == "" {
					add("R5", "want_sql_not_contains entry must be non-empty")
				}
			}
		}
	}
	return out
}

func siContainsEquivalent(sql, sub string) bool {
	return strings.Contains(sql, sub) ||
		strings.Contains(NormalizeSIIdentifierQuotes(sql), NormalizeSIIdentifierQuotes(sub))
}

// The shared corpus must stay byte-identical to
// rewriter-grpc/tests/testdata/storage_integrity_cases.json. This repo locally
// pins its own exact bytes, catching accidental edits and stale local pins.
// Intentional updates require paired PRs, an explicit byte-for-byte cmp, and a
// recorded SHA-256 in each PR description.
const (
	SICorpusFingerprint uint64 = 9742307615402485274
	SICorpusBytes       int    = 184892
	SICorpusCases       int    = 180
)

// siCorpusFingerprint is FNV-1a/64 over the exact file bytes, mirrored by
// si_corpus::Fingerprint in rewriter-grpc.
func siCorpusFingerprint(raw []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, b := range raw {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return h
}

// legacySICase is the pre-migration shape, decoded leniently so
// LegacyCoverageReport can read a corpus that still carries `sql_exact`.
type legacySICase struct {
	Name            string   `json:"name"`
	SQL             string   `json:"sql"`
	WantSQL         string   `json:"want_sql"`
	SQLExact        bool     `json:"sql_exact"`
	Reject          bool     `json:"reject"`
	WantSQLContains []string `json:"want_sql_contains"`
}

// LegacyCoverageReport quantifies what the pre-migration corpus actually
// asserted, so Spec J §5's acceptance sentence is checkable rather than
// anecdotal. It returns the non-reject cases whose want_sql_contains entries
// are all already present in the input SQL (vacuous), and the cases carrying a
// want_sql that the pre-fix C++ runner never compared because it gated that
// single comparison on sql_exact (dead).
func LegacyCoverageReport(raw []byte) (vacuous, deadWantSQL []string, err error) {
	var cases []legacySICase
	if err := json.Unmarshal(raw, &cases); err != nil {
		return nil, nil, fmt.Errorf("decode legacy corpus: %w", err)
	}
	for _, c := range cases {
		if !c.Reject && len(c.WantSQLContains) > 0 {
			allPresent := true
			for _, sub := range c.WantSQLContains {
				if !siContainsEquivalent(c.SQL, sub) {
					allPresent = false
					break
				}
			}
			if allPresent {
				vacuous = append(vacuous, c.Name)
			}
		}
		if c.WantSQL != "" && !c.SQLExact {
			deadWantSQL = append(deadWantSQL, c.Name)
		}
	}
	return vacuous, deadWantSQL, nil
}
