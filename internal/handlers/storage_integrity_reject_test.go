package handlers

import (
	"testing"

	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

func siRejectSelection() nameresolve.Selection {
	return nameresolve.Selection{
		Mode: nameresolve.ModeDynamic,
		Dynamic: &pb.RewriteTableDynamicArgs{
			DatabaseMap:            map[string]string{"db1": "phys"},
			KnownPhysicalDatabases: []string{"phys"},
			Delim:                  "_",
			StorageIntegrity: &pb.StorageIntegrityArgs{
				Tables: map[string]*pb.StorageIntegrityArgs_Table{
					"db1.t": {SafeTable: "hg_safe.db1__t", UnsafeTable: "hg_unsafe.db1__t"},
				},
				ReadMode:            pb.StorageIntegrityArgs_READ_MODE_SAFE,
				ReservedRowIdColumn: "_hg_row_id",
				ContractVersion:     pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1,
			},
		},
	}
}

func TestAnnotateStorageIntegrityReject_ProvenD2Targets(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct{ name, sql, want string }{
		{"physical table", "SYSTEM START MERGES hg_unsafe.db1__t",
			"storage-integrity physical table hg_unsafe.db1__t is not directly addressable"},
		{"escaped backtick physical table", "SYSTEM START MERGES `hg\\x5Funsafe`.`db1__t`",
			"storage-integrity physical table hg_unsafe.db1__t is not directly addressable"},
		{"escaped double quoted physical table", "SYSTEM START MERGES \"hg\\x5Funsafe\".\"db1__t\"",
			"storage-integrity physical table hg_unsafe.db1__t is not directly addressable"},
		{"quote doubled physical table", "SYSTEM START MERGES hg_safe.`x``y`",
			"storage-integrity physical table hg_safe.x`y is not directly addressable"},
		{"escaped apostrophe physical table", "SYSTEM START MERGES hg_safe.`x\\'y`",
			"storage-integrity physical table hg_safe.x'y is not directly addressable"},
		{"escaped backtick physical table", "SYSTEM START MERGES hg_safe.`x\\`y`",
			"storage-integrity physical table hg_safe.x`y is not directly addressable"},
		{"escaped backslash physical table", "SYSTEM START MERGES hg_safe.`x\\\\y`",
			"storage-integrity physical table hg_safe.x\\y is not directly addressable"},
		{"logical table", "CHECK TABLE db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane"},
		{"physical database", "TRUNCATE DATABASE hg_safe",
			"storage-integrity physical database hg_safe is not directly addressable"},
		{"physical database after IF EMPTY", "TRUNCATE DATABASE IF EMPTY hg_safe",
			"storage-integrity physical database hg_safe is not directly addressable"},
		{"truncate-all physical database after IF EXISTS", "TRUNCATE ALL TABLES FROM IF EXISTS hg_unsafe",
			"storage-integrity physical database hg_unsafe is not directly addressable"},
		{"later dictionary list target", "DROP DICTIONARY other.u, hg_safe.db1__t",
			"storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		{"dictionary literal target", "SYSTEM RELOAD DICTIONARY 'hg_safe.db1__t'",
			"storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		{"cluster skipped before physical target", "SYSTEM STOP PULLING REPLICATION LOG ON CLUSTER hg_safe hg_unsafe.db1__t",
			"storage-integrity physical table hg_unsafe.db1__t is not directly addressable"},
		{"later flush log physical target is not masked", "SYSTEM FLUSH LOGS other.u, hg_safe.db1__t",
			"storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		{"opaque flush target does not mask later physical target", "SYSTEM FLUSH LOGS other.u, {target:Identifier}, hg_safe.db1__t",
			"storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		{"leading opaque flush target does not mask later physical target", "SYSTEM FLUSH LOGS {target:Identifier}, hg_safe.db1__t",
			"storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		{"later async insert queue logical target is not masked", "SYSTEM FLUSH ASYNC INSERT QUEUE other.u, db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane"},
		{"keyword physical table target", "SYSTEM START MERGES hg_safe.select",
			"storage-integrity physical table hg_safe.select is not directly addressable"},
		{"keyword physical target keeps list position", "SYSTEM FLUSH LOGS other.u, hg_safe.select, db1.t",
			"storage-integrity physical table hg_safe.select is not directly addressable"},
		{"live view to physical target precedes source", "CREATE LIVE VIEW other.v TO hg_safe.sink AS SELECT * FROM other.u",
			"storage-integrity physical table hg_safe.sink is not directly addressable"},
		{"physical table precedes logical table", "CREATE LIVE VIEW other.v AS SELECT * FROM hg_unsafe.x JOIN db1.t ON 1",
			"storage-integrity physical table hg_unsafe.x is not directly addressable"},
		{"logical table precedes physical table", "CREATE LIVE VIEW other.v AS SELECT * FROM db1.t JOIN hg_safe.x ON 1",
			"storage-integrity table db1.t accepts writes only through the signed statement lane"},
		{"decoy cannot mask later logical table", "CREATE LIVE VIEW other.v AS SELECT hg_safe, count(hg_unsafe) FROM other.u JOIN db1.t ON 1",
			"storage-integrity table db1.t accepts writes only through the signed statement lane"},
		{"string decoy cannot reorder physical source", "CREATE LIVE VIEW other.v AS SELECT 'db1.t' AS decoy FROM hg_unsafe.x JOIN db1.t ON 1",
			"storage-integrity physical table hg_unsafe.x is not directly addressable"},
		{"column decoy cannot reorder physical source", "CREATE LIVE VIEW other.v AS SELECT db1.t FROM hg_unsafe.x JOIN db1.t AS db1 ON 1",
			"storage-integrity physical table hg_unsafe.x is not directly addressable"},
		{"partial table function names physical namespace", "CREATE LIVE VIEW other.v AS SELECT * FROM remote('host', 'hg_safe', concat('t'))",
			"storage-integrity physical database hg_safe is not directly addressable"},
		{"quoted table function identifier is decoded", "CREATE LIVE VIEW other.v AS SELECT * FROM remote('host', `hg\\x5Fsafe`, 'db1__t')",
			"storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		{"set source order keeps physical left first", "CREATE LIVE VIEW other.v AS SELECT * FROM hg_unsafe.x UNION ALL SELECT * FROM db1.t",
			"storage-integrity physical table hg_unsafe.x is not directly addressable"},
		{"test view set fake time keeps its physical target", "SYSTEM TEST VIEW hg_safe.v SET FAKE TIME '2025-01-02 03:04:05'",
			"storage-integrity physical table hg_safe.v is not directly addressable"},
		{"test view unset fake time keeps its logical target", "SYSTEM TEST VIEW db1.t UNSET FAKE TIME",
			"storage-integrity table db1.t accepts writes only through the signed statement lane"},
		{"CASE condition source precedes result and else", "CREATE LIVE VIEW other.v AS SELECT CASE WHEN EXISTS(SELECT 1 FROM hg_unsafe.x) THEN (SELECT 1 FROM db1.t) ELSE (SELECT 1 FROM hg_safe.y) END",
			"storage-integrity physical table hg_unsafe.x is not directly addressable"},
		{"reserved row id has no Task2 precedence", "CREATE LIVE VIEW other.v AS SELECT _hg_row_id FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_UnsupportedStatement, Message: "original"}
			AnnotateStorageIntegrityReject(e, resp, tc.sql, siRejectSelection())
			if resp.GetMessage() != tc.want {
				t.Fatalf("message = %q, want %q", resp.GetMessage(), tc.want)
			}
		})
	}
}

func TestAnnotateStorageIntegrityReject_UnprovenNamesKeepCallerMessage(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct{ name, sql string }{
		{"nothing storage integrity", "SYSTEM RELOAD CONFIG"},
		{"comments only", "SYSTEM RELOAD CONFIG /* hg_safe.db1__t */ -- hg_unsafe.db1__t\n"},
		{"bare column", "CREATE LIVE VIEW other.v AS SELECT hg_safe FROM other.u"},
		{"function argument", "CREATE LIVE VIEW other.v AS SELECT count(hg_unsafe) FROM other.u"},
		{"output alias", "CREATE LIVE VIEW other.v AS SELECT x AS hg_safe FROM other.u"},
		{"table alias", "CREATE LIVE VIEW other.v AS SELECT x FROM other.u AS hg_safe"},
		{"alias qualified column", "CREATE LIVE VIEW other.v AS SELECT hg_safe.x FROM other.u AS hg_safe"},
		{"settings key", "OPTIMIZE TABLE other.u FINAL SETTINGS hg_safe=1"},
		{"cluster name", "SYSTEM RELOAD CONFIG ON CLUSTER hg_safe"},
		{"invalid SYSTEM cluster parameter", "SYSTEM START MERGES ON CLUSTER {cluster:Identifier} hg_safe.db1__t"},
		{"invalid SYSTEM cluster parameter after target", "SYSTEM START MERGES hg_safe.db1__t ON CLUSTER {cluster:Identifier}"},
		{"malformed SYSTEM table-list suffix", "SYSTEM FLUSH LOGS hg_safe.db1__t garbage"},
		{"malformed SYSTEM table-list trailing comma", "SYSTEM FLUSH LOGS hg_safe.db1__t,"},
		{"SYSTEM view controls have no cluster branch", "SYSTEM REFRESH VIEW ON CLUSTER hg_safe hg_unsafe.db1__t"},
		{"partition identifier", "CHECK TABLE other.u PARTITION hg_safe"},
		{"qualified identifier parameter cannot invent bare database", "SYSTEM START MERGES hg_safe.{target:Identifier}"},
		{"drop replica zkpath", "SYSTEM DROP REPLICA 'hg_safe' FROM ZKPATH '/clickhouse/hg_unsafe.db1__t'"},
		{"semantic literal escape is not decoded again", `CREATE LIVE VIEW other.v AS SELECT * FROM remote('host', 'hg\\x5Fsafe', 'db1__t')`},
		{"reserved row id only", "CREATE LIVE VIEW other.v AS SELECT _hg_row_id"},
		{"malformed quoted identifier", "SYSTEM START MERGES hg_safe.`x\\`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_UnsupportedStatement, Message: "original"}
			AnnotateStorageIntegrityReject(e, resp, tc.sql, siRejectSelection())
			if resp.GetMessage() != "original" {
				t.Fatalf("message = %q, want caller message to remain", resp.GetMessage())
			}
		})
	}
}

func TestAnnotateStorageIntegrityReject_ProvenPrefixPrecedesOpaqueSystemTarget(t *testing.T) {
	e := newEngine(t)
	for _, sql := range []string{
		"SYSTEM FLUSH LOGS hg_safe.db1__t, {target:Identifier}",
		"SYSTEM FLUSH ASYNC INSERT QUEUE hg_safe.db1__t, {target:Identifier}",
	} {
		resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_UnsupportedStatement, Message: "original"}
		AnnotateStorageIntegrityReject(e, resp, sql, siRejectSelection())
		want := "storage-integrity physical table hg_safe.db1__t is not directly addressable"
		if resp.GetMessage() != want {
			t.Fatalf("%s message = %q, want %q", sql, resp.GetMessage(), want)
		}
	}
}

func TestAnnotateStorageIntegrityReject_LiveViewInTableOperands(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct{ name, sql, want string }{
		{"infix logical", "CREATE LIVE VIEW other.v AS SELECT id IN db1.t FROM other.u",
			"storage-integrity table db1.t accepts writes only through the signed statement lane"},
		{"GLOBAL IN physical", "CREATE LIVE VIEW other.v AS SELECT id GLOBAL IN hg_safe.x FROM db1.t",
			"storage-integrity physical table hg_safe.x is not directly addressable"},
		{"callable in physical", "CREATE LIVE VIEW other.v AS SELECT in(id, hg_unsafe.x) FROM db1.t",
			"storage-integrity physical table hg_unsafe.x is not directly addressable"},
		{"ordinary scalar qualifier is a decoy", "CREATE LIVE VIEW other.v AS SELECT equals(id, hg_safe.x) FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane"},
		{"in-scope CTE is not an IN table", "CREATE LIVE VIEW other.v AS WITH t AS (SELECT * FROM other.u) SELECT id IN t FROM other.base",
			"original"},
		{"partially opaque IN target does not mask later physical source", "CREATE LIVE VIEW other.v AS SELECT id IN hg_safe.{target:Identifier} FROM hg_unsafe.x",
			"storage-integrity physical table hg_unsafe.x is not directly addressable"},
		{"fully opaque callable IN target does not mask later logical source", "CREATE LIVE VIEW other.v AS SELECT in(id, {target:Identifier}) FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_UnsupportedStatement, Message: "original"}
			AnnotateStorageIntegrityReject(e, resp, tc.sql, siRejectSelection())
			if resp.GetMessage() != tc.want {
				t.Fatalf("message = %q, want %q", resp.GetMessage(), tc.want)
			}
		})
	}
}

func TestAnnotateStorageIntegrityReject_LiveViewVerifierRegressions(t *testing.T) {
	e := newEngine(t)
	sel := siRejectSelection()
	sel.Dynamic.UpstreamLogicalDatabaseInContext = "db1"
	for _, tc := range []struct{ name, sql, want string }{
		{"recursive CTE self reference is not a physical current-database table",
			"CREATE LIVE VIEW other.v AS WITH RECURSIVE t AS (SELECT * FROM t) SELECT * FROM t", "original"},
		{"output alias is not an IN table",
			"CREATE LIVE VIEW other.v AS SELECT tuple(1,2) AS t, 1 IN t", "original"},
		{"FROM alias is not an IN table",
			"CREATE LIVE VIEW other.v AS SELECT id IN t FROM other.u AS t", "original"},
		{"window PARTITION source keeps precedence over ORDER source",
			"CREATE LIVE VIEW other.v AS SELECT sum(x) OVER (PARTITION BY (SELECT 1 FROM hg_unsafe.db1__x) ORDER BY (SELECT 1 FROM db1.t))",
			"storage-integrity physical table hg_unsafe.db1__x is not directly addressable"},
		{"qualified opaque source does not hide later physical source",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.{target:Identifier} JOIN hg_unsafe.db1__x ON 1",
			"storage-integrity physical table hg_unsafe.db1__x is not directly addressable"},
		{"parameterized alias does not suppress later logical source",
			"CREATE LIVE VIEW other.v AS SELECT * FROM other.u AS {alias:Identifier} JOIN db1.t ON 1",
			"storage-integrity table db1.t accepts writes only through the signed statement lane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_UnsupportedStatement, Message: "original"}
			AnnotateStorageIntegrityReject(e, resp, tc.sql, sel)
			if resp.GetMessage() != tc.want {
				t.Fatalf("message = %q, want %q", resp.GetMessage(), tc.want)
			}
		})
	}
}

func TestAnnotateStorageIntegrityReject_LiveViewOnClusterGrammar(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want string
	}{
		{
			"TO keyword cluster preserves physical destination",
			"CREATE LIVE VIEW other.v ON CLUSTER to TO hg_safe.sink AS SELECT * FROM other.u",
			"storage-integrity physical table hg_safe.sink is not directly addressable",
		},
		{
			"AS keyword cluster preserves logical source",
			"CREATE LIVE VIEW other.v ON CLUSTER as AS SELECT * FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane",
		},
		{
			"quoted dotted cluster preserves target ordering",
			"CREATE LIVE VIEW other.v ON CLUSTER `cluster.to` TO hg_safe.sink AS SELECT * FROM db1.t",
			"storage-integrity physical table hg_safe.sink is not directly addressable",
		},
		{
			"identifier parameter cluster invalidates pinned grammar",
			"CREATE LIVE VIEW hg_safe.v ON CLUSTER {cluster:Identifier} TO hg_unsafe.sink AS SELECT * FROM db1.t",
			"original",
		},
		{
			"invalid UUID does not expose an apparent physical target",
			"CREATE LIVE VIEW hg_safe.v UUID 'not-a-uuid' AS SELECT * FROM db1.t",
			"original",
		},
		{
			"malformed property declaration does not expose an apparent physical target",
			"CREATE LIVE VIEW hg_safe.v (nonsense) AS SELECT * FROM db1.t",
			"original",
		},
		{
			"AS column name does not hide logical source",
			"CREATE LIVE VIEW other.v (AS UInt64) AS SELECT * FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane",
		},
		{
			"opaque TO target does not erase earlier physical primary",
			"CREATE LIVE VIEW hg_safe.v TO {sink:Identifier} AS SELECT * FROM hg_unsafe.x",
			"storage-integrity physical table hg_safe.v is not directly addressable",
		},
		{
			"opaque TO target does not hide later physical source",
			"CREATE LIVE VIEW other.v TO {sink:Identifier} AS SELECT * FROM hg_unsafe.x",
			"storage-integrity physical table hg_unsafe.x is not directly addressable",
		},
		{
			"opaque qualified TO target does not fabricate its prefix",
			"CREATE LIVE VIEW other.v TO hg_safe.{sink:Identifier} AS SELECT * FROM hg_unsafe.x",
			"storage-integrity physical table hg_unsafe.x is not directly addressable",
		},
		{
			"UUID preserves primary before destination and source",
			"CREATE LIVE VIEW hg_safe.v UUID '01234567-89ab-cdef-0123-456789abcdef' TO hg_unsafe.sink AS SELECT * FROM db1.t",
			"storage-integrity physical table hg_safe.v is not directly addressable",
		},
		{
			"SQL SECURITY DEFINER preamble preserves logical source",
			"CREATE SQL SECURITY DEFINER LIVE VIEW other.v AS SELECT * FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane",
		},
		{
			"SQL SECURITY INVOKER preamble preserves logical source",
			"CREATE SQL SECURITY INVOKER LIVE VIEW other.v AS SELECT * FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane",
		},
		{
			"bare AS definer before live view is consumed",
			"CREATE DEFINER=AS LIVE VIEW other.v AS SELECT * FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane",
		},
		{
			"bare TO definer after target is consumed",
			"CREATE LIVE VIEW other.v DEFINER=TO AS SELECT * FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane",
		},
		{
			"unquoted definer user and host before live view are consumed",
			"CREATE DEFINER=user@host LIVE VIEW other.v AS SELECT * FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane",
		},
		{
			"unquoted definer user and host after target are consumed",
			"CREATE LIVE VIEW other.v DEFINER=AS@TO SQL SECURITY INVOKER AS SELECT * FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane",
		},
		{
			"security after column preamble preserves logical source",
			"CREATE LIVE VIEW other.v (x UInt64) SQL SECURITY DEFINER AS SELECT * FROM db1.t",
			"storage-integrity table db1.t accepts writes only through the signed statement lane",
		},
		{
			"definer spelling is not an SI object",
			"CREATE DEFINER=hg_safe LIVE VIEW other.v AS SELECT * FROM other.u",
			"original",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_UnsupportedStatement, Message: "original"}
			AnnotateStorageIntegrityReject(e, resp, tc.sql, siRejectSelection())
			if resp.GetMessage() != tc.want {
				t.Fatalf("message = %q, want %q", resp.GetMessage(), tc.want)
			}
		})
	}
}

func TestAnnotateStorageIntegrityReject_PreservesExistingClassification(t *testing.T) {
	e := newEngine(t)

	t.Run("success is never touched", func(t *testing.T) {
		resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_Success, Message: "success"}
		AnnotateStorageIntegrityReject(e, resp, "SYSTEM START MERGES hg_unsafe.db1__t", siRejectSelection())
		if resp.GetMessage() != "success" {
			t.Fatalf("message = %q, want the untouched success message", resp.GetMessage())
		}
	})

	t.Run("existing SI classification wins", func(t *testing.T) {
		resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_UnsupportedStatement, Message: "handler message",
			OriginalAccessedTables: []*pb.AccessedTable{{OriginalDatabase: "db1", OriginalTable: "t", IsStorageIntegrity: true}}}
		AnnotateStorageIntegrityReject(e, resp, "CREATE LIVE VIEW other.v AS SELECT * FROM hg_unsafe.x", siRejectSelection())
		if resp.GetMessage() != "handler message" {
			t.Fatalf("message = %q, want the handler's own SI message", resp.GetMessage())
		}
	})

	t.Run("bare table uses physical database context", func(t *testing.T) {
		sel := siRejectSelection()
		sel.Dynamic.UpstreamLogicalDatabaseInContext = "hg_safe"
		resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_UnsupportedStatement, Message: "original"}
		AnnotateStorageIntegrityReject(e, resp, "CHECK TABLE db1__t", sel)
		if resp.GetMessage() != "storage-integrity physical table hg_safe.db1__t is not directly addressable" {
			t.Fatalf("message = %q", resp.GetMessage())
		}
	})

	t.Run("unresolved table function cannot fall into current logical database", func(t *testing.T) {
		sel := siRejectSelection()
		sel.Dynamic.UpstreamLogicalDatabaseInContext = "db1"
		resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_UnsupportedStatement, Message: "original"}
		AnnotateStorageIntegrityReject(e, resp,
			"CREATE LIVE VIEW other.v AS SELECT * FROM remote('host', concat('other'), 't')", sel)
		if resp.GetMessage() != "original" {
			t.Fatalf("message = %q, want unresolved namespace to keep caller message", resp.GetMessage())
		}
	})

	t.Run("complete start and stop listen forms are targetless with colliding SI tables", func(t *testing.T) {
		sel := siRejectSelection()
		sel.Dynamic.UpstreamLogicalDatabaseInContext = "db1"
		sel.Dynamic.StorageIntegrity.Tables["db1.TCP"] = &pb.StorageIntegrityArgs_Table{
			SafeTable: "hg_safe.db1__TCP", UnsafeTable: "hg_unsafe.db1__TCP",
		}
		sel.Dynamic.StorageIntegrity.Tables["db1.HTTP"] = &pb.StorageIntegrityArgs_Table{
			SafeTable: "hg_safe.db1__HTTP", UnsafeTable: "hg_unsafe.db1__HTTP",
		}
		for _, sql := range []string{
			"SYSTEM START LISTEN TCP",
			"SYSTEM STOP LISTEN QUERIES ALL EXCEPT TCP, HTTP",
			"SYSTEM START LISTEN CUSTOM 'hg_safe'",
		} {
			resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_UnsupportedStatement, Message: "original"}
			AnnotateStorageIntegrityReject(e, resp, sql, sel)
			if resp.GetMessage() != "original" {
				t.Fatalf("%s message = %q, want caller message", sql, resp.GetMessage())
			}
		}
	})

	t.Run("only a source-role table function is classified", func(t *testing.T) {
		sel := siRejectSelection()
		sel.Dynamic.UpstreamLogicalDatabaseInContext = "db1"
		for _, tc := range []struct{ sql, want string }{
			{"CREATE LIVE VIEW other.v AS SELECT merge('t')", "original"},
			{"CREATE LIVE VIEW other.v AS SELECT * FROM merge('t')",
				"storage-integrity table db1.t accepts writes only through the signed statement lane"},
			{"CREATE LIVE VIEW other.v AS SELECT remote('host', 'hg_safe', 'db1__t') FROM other.u", "original"},
			{"CREATE LIVE VIEW other.v AS SELECT * FROM remote('host', 'hg_safe', 'db1__t')",
				"storage-integrity physical table hg_safe.db1__t is not directly addressable"},
		} {
			resp := &pb.RewriteSQLResponse{Code: pb.RewriteCode_UnsupportedStatement, Message: "original"}
			AnnotateStorageIntegrityReject(e, resp, tc.sql, sel)
			if resp.GetMessage() != tc.want {
				t.Fatalf("%s message = %q, want %q", tc.sql, resp.GetMessage(), tc.want)
			}
		}
	})
}
