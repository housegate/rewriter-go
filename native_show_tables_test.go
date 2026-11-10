package rewriter

import (
	"strings"
	"testing"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/handlers"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// The synthetic SHOW TABLES listing of db1 (physical phys, prefix "db1.").
const showTablesListing = "SELECT multiIf(startsWith(name, 'db1.'), substring(name, length('db1.') + 1), name) AS name" +
	" FROM (SELECT name FROM system.tables WHERE database = 'phys' AND startsWith(name, 'db1.'))"

const showFullTablesListing = "SELECT multiIf(startsWith(name, 'db1.'), substring(name, length('db1.') + 1), name) AS name, engine" +
	" FROM (SELECT name, engine FROM system.tables WHERE database = 'phys' AND startsWith(name, 'db1.'))"

// TestShowTablesLikeLimitFull pins the SHOW TABLES forms routed through the
// system.tables mapping (housegate/housegate#319): a [NOT] LIKE / ILIKE
// pattern and a LIMIT apply to the logical name, after the prefix is
// stripped, ordered by name as ClickHouse's own SHOW TABLES is, and FULL adds
// the engine column. Each rewrite was executed on ClickHouse 26.8 and 25.8
// against a native SHOW TABLES of a database holding the same tables (same
// rows, same order).
func TestShowTablesLikeLimitFull(t *testing.T) {
	wrap := func(listing, cols, tail string) string {
		return "SELECT " + cols + " FROM (" + listing + ")" + tail
	}
	var cases []tablerefCase
	for _, si := range []bool{false, true} {
		for _, c := range []struct{ sql, want string }{
			{"SHOW TABLES", showTablesListing},
			{"SHOW TABLES FROM db1", showTablesListing},
			{"SHOW TABLES LIKE 'a%'", wrap(showTablesListing, "name", " WHERE name LIKE 'a%' ORDER BY name")},
			{"SHOW TABLES FROM db1 ILIKE 'A%'", wrap(showTablesListing, "name", " WHERE name ILIKE 'A%' ORDER BY name")},
			{"SHOW TABLES IN db1 NOT LIKE 'a%'", wrap(showTablesListing, "name", " WHERE name NOT LIKE 'a%' ORDER BY name")},
			{"SHOW TABLES NOT ILIKE 'a%';", wrap(showTablesListing, "name", " WHERE name NOT ILIKE 'a%' ORDER BY name")},
			// ClickHouse keeps the backslash of `\_` in the decoded pattern;
			// re-escaped it is still a literal underscore.
			{`SHOW TABLES LIKE 'a\_%'`, wrap(showTablesListing, "name", ` WHERE name LIKE 'a\\_%' ORDER BY name`)},
			{"SHOW TABLES LIKE 'O''B%'", wrap(showTablesListing, "name", " WHERE name LIKE 'O''B%' ORDER BY name")},
			{`SHOW TABLES LIKE 'O\'B%'`, wrap(showTablesListing, "name", " WHERE name LIKE 'O''B%' ORDER BY name")},
			{"SHOW TABLES LIMIT 0", wrap(showTablesListing, "name", " ORDER BY name LIMIT 0")},
			{"SHOW TABLES FROM db1 LIKE 'a%' LIMIT 3", wrap(showTablesListing, "name", " WHERE name LIKE 'a%' ORDER BY name LIMIT 3")},
			{"SHOW FULL TABLES", wrap(showFullTablesListing, "name, engine", " ORDER BY name")},
			{"SHOW FULL TABLES FROM db1 LIKE '%b%' LIMIT 1", wrap(showFullTablesListing, "name, engine", " WHERE name LIKE '%b%' ORDER BY name LIMIT 1")},
		} {
			cases = append(cases, tablerefCase{name: c.sql, sql: c.sql, si: si, wantCode: pb.RewriteCode_Success, wantSQL: c.want})
		}
		for _, sql := range []string{
			"SHOW TEMPORARY TABLES",
			"SHOW FULL TEMPORARY TABLES",
			"SHOW EXTENDED TABLES FROM db1",
			"SHOW TABLES FROM db1 WHERE name = 'o'",
			"SHOW TABLES LIKE ''",
			"SHOW TABLES ILIKE '' LIMIT 1",
			"SHOW TABLES LIKE 'a%' FORMAT JSON",
			"SHOW TABLES LIKE 'a%' SETTINGS max_threads = 1",
			"SHOW TABLES LIKE 'a%' INTO OUTFILE '/tmp/x'",
			"SHOW TABLES LIMIT 1 FORMAT JSON",
			"SHOW TABLES LIMIT 1 OFFSET 1",
			"SHOW TABLES LIMIT 1, 2",
			"SHOW TABLES LIMIT -1",
			// Spellings ClickHouse reads as a number but the rewrite does not
			// re-render (engine-local conservative refusal).
			"SHOW TABLES LIMIT 010",
			"SHOW TABLES LIMIT 0x10",
			"SHOW TABLES LIMIT 1e1",
			"SHOW TABLES LIMIT 18446744073709551616",
			"SHOW TABLES LIMIT (SELECT count() FROM db1.o)",
		} {
			cases = append(cases, tablerefCase{name: sql, sql: sql, si: si,
				wantCode: pb.RewriteCode_UnsupportedStatement, wantMsg: engine.UnsupportedStatementMessage})
		}
	}
	runTablerefCases(t, cases)
}

// TestShowTablesLikeKeepsDatabaseRefusals pins that a LIKE / LIMIT / FULL
// tail never turns a database refusal into an answer: the protected,
// reserved and unknown databases keep the base statement's code and message.
func TestShowTablesLikeKeepsDatabaseRefusals(t *testing.T) {
	e := newEngine(t)
	for _, si := range []bool{false, true} {
		for _, base := range []string{
			"SHOW TABLES FROM phys",
			"SHOW TABLES FROM hg_safe",
			"SHOW TABLES FROM hg_unsafe",
			"SHOW TABLES FROM hg_promote",
			"SHOW TABLES FROM nope",
		} {
			for _, variant := range []string{
				base + " LIKE 'a%'",
				base + " NOT ILIKE 'a%' LIMIT 2",
				strings.Replace(base, "SHOW TABLES", "SHOW FULL TABLES", 1),
			} {
				t.Run(variant, func(t *testing.T) {
					want, err := doRewrite(e, base, tablerefOpts(si))
					if err != nil {
						t.Fatal(err)
					}
					got, err := doRewrite(e, variant, tablerefOpts(si))
					if err != nil {
						t.Fatal(err)
					}
					if want.GetCode() == pb.RewriteCode_Success {
						t.Fatalf("base %q answered Success", base)
					}
					if got.GetCode() != want.GetCode() || got.GetMessage() != want.GetMessage() {
						t.Fatalf("variant = %s (%s), base = %s (%s)", got.GetCode(), got.GetMessage(), want.GetCode(), want.GetMessage())
					}
				})
			}
		}
	}
}

// TestShowTablesLikeRemoteSource pins the remote-upstream source with a LIKE,
// LIMIT and FULL tail: the listing reads remote(...), the filter still uses
// the database_map physical name.
func TestShowTablesLikeRemoteSource(t *testing.T) {
	e := newEngine(t)
	dyn := &pb.RewriteTableDynamicArgs{
		DatabaseMap:                          map[string]string{"tenant1": "testnet"},
		LogicalDatabaseToRemoteUpstreamIndex: map[string]string{"tenant1": "up0"},
		RemoteUpstreams: map[string]*pb.RewriteTableDynamicArgs_RemoteUpstream{
			"up0": {Addr: "h:9000", User: "u", Password: "p"},
		},
	}
	sql := "SHOW FULL TABLES FROM tenant1 LIKE 'a%' LIMIT 5"
	ast, err := e.ParseOne(sql)
	if err != nil {
		t.Fatal(err)
	}
	resp, handled, err := handlers.RewriteDBLevel(e, ast, sql, []*pb.RewriteOption{tableRewriteDynamic(dyn)})
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	want := "SELECT name, engine FROM (SELECT multiIf(startsWith(name, 'tenant1.'), substring(name, length('tenant1.') + 1), name) AS name, engine" +
		" FROM (SELECT name, engine FROM remote('h:9000', system, tables, 'u', 'p') WHERE database = 'testnet' AND startsWith(name, 'tenant1.')))" +
		" WHERE name LIKE 'a%' ORDER BY name LIMIT 5"
	if resp.GetCode() != pb.RewriteCode_Success || resp.GetSqlAfterRewrite() != want {
		t.Fatalf("code=%s msg=%q\nsql =%q\nwant=%q", resp.GetCode(), resp.GetMessage(), resp.GetSqlAfterRewrite(), want)
	}
	if resp.GetDatabaseRewrites()["tenant1"] != "testnet" {
		t.Fatalf("database_rewrites=%v", resp.GetDatabaseRewrites())
	}
}
