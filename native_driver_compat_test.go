package rewriter

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/housegate/rewriter-proto/gen/pb"
	"google.golang.org/protobuf/proto"
)

// driverDynamic is the request HouseGate sends for a Sentio indexer driver
// session (NetworkV1): the driver connects with the physical database as its
// ClientHello database, so the session carries no logical context, and its
// one logical database maps to the physical one. With si set, one Active
// storage-integrity table of another tenant is configured, as on an
// SI-enabled HouseGate.
func driverDynamic(si bool) *pb.RewriteTableDynamicArgs {
	phys := "devnet2"
	dyn := &pb.RewriteTableDynamicArgs{
		DatabaseMap:                       map[string]string{"hw1kzqe3_0": "devnet2"},
		KnownPhysicalDatabases:            []string{"devnet2"},
		ProtectedDatabases:                []string{"devnet2", "hg_safe", "hg_unsafe", "hg_promote"},
		UpstreamLogicalDatabaseInContext:  "",
		UpstreamPhysicalDatabaseInContext: &phys,
		Delim:                             "",
	}
	if si {
		dyn.StorageIntegrity = &pb.StorageIntegrityArgs{
			Tables: map[string]*pb.StorageIntegrityArgs_Table{
				"si_db.events": {SafeTable: "hg_safe.si_db__events", UnsafeTable: "hg_unsafe.si_db__events"},
			},
			ReadMode:            pb.StorageIntegrityArgs_READ_MODE_SAFE,
			ReservedRowIdColumn: "_hg_row_id",
			ContractVersion:     pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V2,
			ReservedDatabases:   []string{"hg_safe", "hg_unsafe", "hg_promote"},
		}
	}
	return dyn
}

type driverStatement struct {
	ID     int    `json:"id"`
	Shape  string `json:"shape"`
	Expect string `json:"expect"`
	Origin string `json:"origin"`
	SQL    string `json:"sql"`
}

func loadDriverStatements(t *testing.T) []driverStatement {
	t.Helper()
	raw, err := os.ReadFile("testdata/driver_statements.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Statements []driverStatement `json:"statements"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Statements
}

// externalFilterTable is the clickhouse-go external table the driver's
// entity list uses for an id IN-filter above SENTIO_DEFAULT_ENTITY_HUGE_ID_SET_SIZE.
var externalFilterTable = regexp.MustCompile("`(filter_[0-9a-f]+)`")

// TestDriverStatementCompat runs every statement the Sentio indexer driver
// sends (the appendix of the 2026-10-02 driver/engine compatibility report,
// testdata/driver_statements.json) through the engine with the driver-session
// request, with storage integrity off and on. Every driver statement must be
// answered Success, except:
//
//   - the 44 entity-list statements that read the id set from a clickhouse-go
//     external table (IN (SELECT s FROM `filter_<hex>`)): the session has no
//     logical context and the external table arrives after the Query packet,
//     so the unqualified name is refused (a known, deferred driver change);
//   - the 4 statements the driver does not send today (cluster-aware DDL and
//     a raw '\_' literal clickhouse-go never produces), whose answers are
//     pinned below.
//
// The 970 driver statements include the shapes this branch fixed:
// startsWith (the patch-part probe before every lightweight delete), CREATE
// [OR REPLACE] VIEW / MATERIALIZED VIEW … AS (…) COMMENT '…' (every entity
// schema) and the system.clusters probe's `cluster not like 'all-%'`.
func TestDriverStatementCompat(t *testing.T) {
	e := newEngine(t)
	stmts := loadDriverStatements(t)
	if len(stmts) != 974 {
		t.Fatalf("driver statements = %d, want 974", len(stmts))
	}
	counts := map[string]int{}
	for _, s := range stmts {
		counts[s.Expect]++
	}
	if counts["success"] != 926 || counts["external_filter"] != 44 || counts["na"] != 4 {
		t.Fatalf("expectation counts = %v", counts)
	}
	// The not-reachable statements, by appendix id: code and message with
	// SI off, then with SI on.
	type answer struct {
		code pb.RewriteCode
		msg  string
	}
	na := map[int][2]answer{
		971: {{pb.RewriteCode_UnsupportedStatement, "table engine ReplicatedMergeTree is not accepted"},
			{pb.RewriteCode_UnsupportedStatement, "table engine ReplicatedMergeTree is not accepted"}},
		972: {{pb.RewriteCode_UnsupportedStatement, "statement is not supported"},
			{pb.RewriteCode_UnsupportedStatement, StorageIntegrityUnmodelledMessage}},
		973: {{pb.RewriteCode_Success, "success"}, {pb.RewriteCode_Success, "success"}},
		// The raw literal '\_' is unescaped by Polyglot to '_', which the
		// mid-statement drop gate refuses (clickhouse-go always sends '\\_').
		974: {{pb.RewriteCode_UnsupportedStatement, "statement is not supported"},
			{pb.RewriteCode_UnsupportedStatement, "statement is not supported"}},
	}
	for _, si := range []bool{false, true} {
		opts := []*pb.RewriteOption{tableRewriteDynamic(driverDynamic(si))}
		for _, s := range stmts {
			t.Run(fmt.Sprintf("si=%v/%d", si, s.ID), func(t *testing.T) {
				resp, err := doRewrite(e, s.SQL, opts)
				if err != nil {
					t.Fatalf("doRewrite: %v", err)
				}
				switch s.Expect {
				case "success":
					if resp.GetCode() != pb.RewriteCode_Success {
						t.Fatalf("%s: code = %s (%s), want Success\nsql: %s", s.Shape, resp.GetCode(), resp.GetMessage(), s.SQL)
					}
				case "external_filter":
					m := externalFilterTable.FindStringSubmatch(s.SQL)
					if m == nil {
						t.Fatalf("no external filter table in %s", s.SQL)
					}
					want := fmt.Sprintf("unqualified table %q does not resolve through the session's logical database", m[1])
					if resp.GetCode() != pb.RewriteCode_InvalidRewriteRequest || resp.GetMessage() != want {
						t.Fatalf("code = %s (%s), want InvalidRewriteRequest (%s)", resp.GetCode(), resp.GetMessage(), want)
					}
				case "na":
					a, ok := na[s.ID]
					if !ok {
						t.Fatalf("no pinned answer for n/a statement %d", s.ID)
					}
					want := a[0]
					if si {
						want = a[1]
					}
					if resp.GetCode() != want.code || resp.GetMessage() != want.msg {
						t.Fatalf("code = %s (%s), want %s (%s)", resp.GetCode(), resp.GetMessage(), want.code, want.msg)
					}
				default:
					t.Fatalf("unknown expectation %q", s.Expect)
				}
			})
		}
	}
}

// TestDriverStatementCompatFixedShapes pins the exact SQL HouseGate forwards
// for the shapes this branch fixed, in the driver session: the call keeps
// its ClickHouse spelling, the view keeps its COMMENT after the rewritten
// body, and the probe keeps reading the cluster column.
func TestDriverStatementCompatFixedShapes(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		opts := []*pb.RewriteOption{tableRewriteDynamic(driverDynamic(si))}
		for _, tc := range []struct {
			sql, want string
		}{
			{"SELECT count(), sum(data_uncompressed_bytes) FROM system.parts WHERE database = 'devnet2' AND table = 'hw1kzqe3_0.counter_total' AND active AND startsWith(name, 'patch-')",
				"SELECT count(), sum(data_uncompressed_bytes) FROM system.parts WHERE database = 'devnet2' AND table = 'hw1kzqe3_0.counter_total' AND active AND startsWith(name, 'patch-')"},
			{"CREATE OR REPLACE VIEW `hw1kzqe3_0`.`v` (`id` String COMMENT 'c1') AS (SELECT `id` FROM `hw1kzqe3_0`.`entity_A`) COMMENT 'view c'",
				"CREATE OR REPLACE VIEW devnet2.\"hw1kzqe3_0.v\" (\"id\" String COMMENT 'c1') AS (SELECT \"id\" FROM devnet2.\"hw1kzqe3_0.entity_A\" \"hw1kzqe3_0.entity_A\") COMMENT 'view c'"},
			{"CREATE MATERIALIZED VIEW `hw1kzqe3_0`.`mv` TO `hw1kzqe3_0`.`t` (`id` String COMMENT 'c1') AS (SELECT * FROM `hw1kzqe3_0`.`s`) COMMENT 'c'",
				"CREATE MATERIALIZED VIEW devnet2.\"hw1kzqe3_0.mv\" TO devnet2.\"hw1kzqe3_0.t\" (\"id\" String COMMENT 'c1') AS (SELECT * FROM devnet2.\"hw1kzqe3_0.s\" \"hw1kzqe3_0.s\") COMMENT 'c'"},
			{"SELECT cluster FROM (SELECT cluster, count(*) AS rs, SUM(host_address = '127.0.0.1') AS cl FROM system.clusters WHERE cluster not like 'all-%' GROUP BY cluster) WHERE cl > 0 AND rs > 1",
				"SELECT cluster FROM (SELECT cluster, count(*) AS rs, SUM(host_address = '127.0.0.1') AS cl FROM system.clusters WHERE NOT cluster LIKE 'all-%' GROUP BY cluster) WHERE cl > 0 AND rs > 1"},
		} {
			t.Run(fmt.Sprintf("si=%v/%s", si, strings.SplitN(tc.sql, " ", 4)[2]), func(t *testing.T) {
				resp, err := doRewrite(e, tc.sql, opts)
				if err != nil {
					t.Fatal(err)
				}
				if resp.GetCode() != pb.RewriteCode_Success || resp.GetSqlAfterRewrite() != tc.want {
					t.Fatalf("code = %s (%s)\nsql  = %s\nwant = %s", resp.GetCode(), resp.GetMessage(), resp.GetSqlAfterRewrite(), tc.want)
				}
			})
		}
	}
}

// TestRestoredFunctionArgumentsAreGoverned pins that a restored function call
// (restoreFunctionSpellings turns a typed node into a plain call) governs its
// arguments like any other call: every read in an argument gets the answer
// the same read gets inside lower(…), in every SI state.
func TestRestoredFunctionArgumentsAreGoverned(t *testing.T) {
	e := newEngine(t)
	calls := []string{"startsWith(%s, 'x')", "toTypeName(%s)", "CHAR_LENGTH(%s)", "instr(%s, 'x')",
		"locate('x', %s)", "trim(%s, 'x')", "match(%s, 'x')", "toStartOfDay(%s)"}
	args := []string{"(SELECT a FROM phys.x)", "(SELECT a FROM db1.t)", "(SELECT a FROM hg_safe.db1__t)",
		"(SELECT a FROM other.secret)", "(SELECT a FROM o2)", "(SELECT a FROM system.processes)",
		"(SELECT a FROM {p:Identifier})", "joinGet('other.j', 'v', 1)"}
	for _, si := range []bool{false, true} {
		for _, c := range calls {
			for _, a := range args {
				sql := "SELECT " + strings.Replace(c, "%s", a, 1) + " FROM db1.o"
				base := "SELECT lower(" + a + ") FROM db1.o"
				t.Run(sql, func(t *testing.T) {
					got, err := doRewrite(e, sql, tablerefOpts(si))
					if err != nil {
						t.Fatal(err)
					}
					want, err := doRewrite(e, base, tablerefOpts(si))
					if err != nil {
						t.Fatal(err)
					}
					if got.GetCode() != want.GetCode() || got.GetMessage() != want.GetMessage() ||
						!proto.Equal(&pb.RewriteSQLResponse{OriginalAccessedTables: got.GetOriginalAccessedTables(), TableRewrites: got.GetTableRewrites()},
							&pb.RewriteSQLResponse{OriginalAccessedTables: want.GetOriginalAccessedTables(), TableRewrites: want.GetTableRewrites()}) {
						t.Fatalf("\n got %s (%s) %v\nwant %s (%s) %v", got.GetCode(), got.GetMessage(), got.GetOriginalAccessedTables(),
							want.GetCode(), want.GetMessage(), want.GetOriginalAccessedTables())
					}
				})
			}
		}
	}
}
