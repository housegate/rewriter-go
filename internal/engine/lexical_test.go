package engine

import (
	"reflect"
	"strings"
	"testing"
)

func TestNameRefs_D2CommandTargets(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{"system start merges", "SYSTEM START MERGES hg_unsafe.db1__t", tableRef("hg_unsafe", "db1__t")},
		{"system stop merges", "SYSTEM STOP MERGES hg_safe.db1__t", tableRef("hg_safe", "db1__t")},
		{"system restart replica", "SYSTEM RESTART REPLICA hg_unsafe.db1__t", tableRef("hg_unsafe", "db1__t")},
		{"system sync replica", "SYSTEM SYNC REPLICA db1.t", tableRef("db1", "t")},
		{"check table", "CHECK TABLE db1.t", tableRef("db1", "t")},
		{"check all tables has no target", "CHECK ALL TABLES", nil},
		{"truncate database", "TRUNCATE DATABASE hg_safe", databaseRef("hg_safe")},
		{"truncate database if empty", "TRUNCATE DATABASE IF EMPTY hg_safe", databaseRef("hg_safe")},
		{"truncate all tables from", "TRUNCATE ALL TABLES FROM hg_unsafe", databaseRef("hg_unsafe")},
		{"truncate all tables from if exists", "TRUNCATE ALL TABLES FROM IF EXISTS hg_unsafe", databaseRef("hg_unsafe")},
		{"alter database", "ALTER DATABASE hg_safe MODIFY COMMENT 'x'", databaseRef("hg_safe")},
		{"drop dictionary", "DROP DICTIONARY hg_safe.d", tableRef("hg_safe", "d")},
		{"drop dictionary ordered list", "DROP DICTIONARY other.u, hg_safe.d",
			append(tableRef("other", "u"), tableRef("hg_safe", "d")...)},
		{"create live view", "CREATE LIVE VIEW other.v AS SELECT * FROM db1.t", append(tableRef("other", "v"), tableRef("db1", "t")...)},
		{"create live view to target", "CREATE LIVE VIEW other.v TO hg_safe.sink AS SELECT * FROM other.u",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "sink")...), tableRef("other", "u")...)},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestNameRefs_SystemTableAndViewGrammar(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{"refresh view", "SYSTEM REFRESH VIEW db1.t", tableRef("db1", "t")},
		{"wait view", "SYSTEM WAIT VIEW db1.t", tableRef("db1", "t")},
		{"test view set fake time", "SYSTEM TEST VIEW hg_safe.v SET FAKE TIME '2025-01-02 03:04:05'", tableRef("hg_safe", "v")},
		{"test view unset fake time", "SYSTEM TEST VIEW db1.t UNSET FAKE TIME", tableRef("db1", "t")},
		{"test view requires fake time action", "SYSTEM TEST VIEW hg_safe.v", nil},
		{"test view set requires string time", "SYSTEM TEST VIEW hg_safe.v SET FAKE TIME 1", nil},
		{"test view set requires parseable time", "SYSTEM TEST VIEW hg_safe.v SET FAKE TIME 'not-a-time'", nil},
		{"test view rejects trailing suffix", "SYSTEM TEST VIEW hg_safe.v UNSET FAKE TIME extra", nil},
		{"wait loading parts", "SYSTEM WAIT LOADING PARTS db1.t", tableRef("db1", "t")},
		{"wait query runner", "SYSTEM WAIT QUERY RUNNER db1.t", tableRef("db1", "t")},
		{"load primary key", "SYSTEM LOAD PRIMARY KEY db1.t", tableRef("db1", "t")},
		{"unload primary key", "SYSTEM UNLOAD PRIMARY KEY db1.t", tableRef("db1", "t")},
		{"start pulling replication log", "SYSTEM START PULLING REPLICATION LOG db1.t", tableRef("db1", "t")},
		{"stop pulling replication log", "SYSTEM STOP PULLING REPLICATION LOG db1.t", tableRef("db1", "t")},
		{"start distributed sends", "SYSTEM START DISTRIBUTED SENDS db1.t", tableRef("db1", "t")},
		{"flush distributed", "SYSTEM FLUSH DISTRIBUTED db1.t SETTINGS max_threads=1", tableRef("db1", "t")},
		{"flush logs single target", "SYSTEM FLUSH LOGS hg_safe.db1__t", tableRef("hg_safe", "db1__t")},
		{"flush logs preserves ordered targets", "SYSTEM FLUSH LOGS other.u, hg_safe.db1__t",
			append(tableRef("other", "u"), tableRef("hg_safe", "db1__t")...)},
		{"flush logs malformed suffix loses partial attribution", "SYSTEM FLUSH LOGS hg_safe.db1__t garbage", nil},
		{"flush logs trailing comma loses partial attribution", "SYSTEM FLUSH LOGS hg_safe.db1__t,", nil},
		// Polyglot is pinned to ClickHouse v26.7, whose ASYNC INSERT QUEUE
		// suffix shares FLUSH LOGS' optional table-list grammar. ClickHouse
		// runtime 25.8 does not supply that suffix, so these are parser-pin tests.
		{"flush async insert queue single target", "SYSTEM FLUSH ASYNC INSERT QUEUE hg_safe.db1__t", tableRef("hg_safe", "db1__t")},
		{"flush async insert queue preserves ordered targets", "SYSTEM FLUSH ASYNC INSERT QUEUE other.u, db1.t",
			append(tableRef("other", "u"), tableRef("db1", "t")...)},
		{"flush async insert queue target absent", "SYSTEM FLUSH ASYNC INSERT QUEUE", nil},
		{"keyword table identifier", "SYSTEM START MERGES hg_safe.select", tableRef("hg_safe", "select")},
		{"keyword database identifier", "SYSTEM START MERGES select.t", tableRef("select", "t")},
		{"flush logs keyword target preserves full order", "SYSTEM FLUSH LOGS other.u, hg_safe.select, db1.t",
			append(append(tableRef("other", "u"), tableRef("hg_safe", "select")...), tableRef("db1", "t")...)},
		{"identifier parameter is opaque", "SYSTEM START MERGES {target:Identifier}", nil},
		{"identifier parameter component cannot invent bare database", "SYSTEM START MERGES hg_safe.{target:Identifier}", nil},
		{"identifier parameter retains proven list prefix", "SYSTEM FLUSH LOGS hg_safe.db1__t, {target:Identifier}", tableRef("hg_safe", "db1__t")},
		{"async identifier parameter retains proven list prefix", "SYSTEM FLUSH ASYNC INSERT QUEUE hg_safe.db1__t, {target:Identifier}", tableRef("hg_safe", "db1__t")},
		{"identifier parameter does not suppress a later proven target", "SYSTEM FLUSH LOGS other.u, {target:Identifier}, hg_safe.db1__t",
			append(tableRef("other", "u"), tableRef("hg_safe", "db1__t")...)},
		{"leading identifier parameter does not suppress a later proven target", "SYSTEM FLUSH LOGS {target:Identifier}, hg_safe.db1__t",
			tableRef("hg_safe", "db1__t")},
		{"qualified identifier parameter does not suppress a later proven target", "SYSTEM FLUSH LOGS other.u, hg_safe.{target:Identifier}, hg_unsafe.db1__t",
			append(tableRef("other", "u"), tableRef("hg_unsafe", "db1__t")...)},
		{"identifier parameter type is case sensitive", "SYSTEM FLUSH LOGS {target:identifier}, hg_safe.db1__t", nil},
		{"schedule merge", "SYSTEM SCHEDULE MERGE db1.t PARTS 'p1'", tableRef("db1", "t")},
		{"dictionary identifier", "SYSTEM RELOAD DICTIONARY hg_safe.db1__t", tableRef("hg_safe", "db1__t")},
		{"dictionary string is grammar target", "SYSTEM UNLOAD DICTIONARY 'hg_safe.db1__t'", tableRef("hg_safe", "db1__t")},
		{"dictionary tagged dollar string is decoded", "SYSTEM RELOAD DICTIONARY $tag$hg_safe.t$tag$", tableRef("hg_safe", "t")},
		{"dictionary empty tagged dollar string has no target", "SYSTEM RELOAD DICTIONARY $tag$$tag$", nil},
		{"sync database replica", "SYSTEM SYNC DATABASE REPLICA hg_safe", databaseRef("hg_safe")},
		{"drop replica from table", "SYSTEM DROP REPLICA 'r1' FROM TABLE db1.t", tableRef("db1", "t")},
		{"drop replica from database", "SYSTEM DROP DATABASE REPLICA 'r1' FROM DATABASE hg_safe", databaseRef("hg_safe")},
		{"optional distributed target absent", "SYSTEM START DISTRIBUTED SENDS", nil},
		{"optional primary key target absent", "SYSTEM LOAD PRIMARY KEY", nil},
		{"all views has no target", "SYSTEM START VIEWS", nil},
		{"all background has no target", "SYSTEM STOP ALL BACKGROUND", nil},
		{"start listen server type has no table target", "SYSTEM START LISTEN TCP", nil},
		{"stop listen server types have no table target", "SYSTEM STOP LISTEN QUERIES ALL EXCEPT TCP, HTTP", nil},
		{"custom listen name has no table target", "SYSTEM START LISTEN CUSTOM 'hg_safe'", nil},
		{"merges on volume is not a table", "SYSTEM START MERGES ON VOLUME hg_safe.volume", nil},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestNameRefs_CompleteListenFormsParseAndRemainTargetless(t *testing.T) {
	e := newTestEngine(t)
	for _, sql := range []string{
		"SYSTEM START LISTEN TCP",
		"SYSTEM STOP LISTEN QUERIES ALL EXCEPT TCP, HTTP",
		"SYSTEM START LISTEN CUSTOM 'hg_safe'",
	} {
		if _, err := e.ParseOne(sql); err != nil {
			t.Fatalf("ParseOne(%q): %v", sql, err)
		}
		assertNameRefs(t, e, sql, nil)
	}
}

func TestNameRefs_IdentifierParametersFailConservatively(t *testing.T) {
	e := newTestEngine(t)
	for _, sql := range []string{
		"SYSTEM START MERGES {target:Identifier}",
		"SYSTEM START MERGES hg_safe.{target:Identifier}",
	} {
		if _, err := e.ParseOne(sql); err != nil {
			t.Fatalf("ParseOne(%q): %v", sql, err)
		}
		assertNameRefs(t, e, sql, nil)
	}
	assertNameRefs(t, e,
		"SYSTEM FLUSH LOGS other.u, {target:Identifier}, hg_safe.db1__t",
		append(tableRef("other", "u"), tableRef("hg_safe", "db1__t")...),
	)
	assertNameRefs(t, e,
		"SYSTEM START MERGES ON CLUSTER {cluster:Identifier} hg_safe.db1__t",
		nil,
	)
}

func TestNameRefs_ObjectRolesExcludeDecoys(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{"cluster before target", "SYSTEM START MERGES ON CLUSTER hg_safe other.u", tableRef("other", "u")},
		{"empty cluster before target is invalid", "SYSTEM START MERGES ON CLUSTER '' hg_safe.t", nil},
		{"empty tagged dollar cluster before target is invalid", "SYSTEM START MERGES ON CLUSTER $tag$$tag$ hg_safe.t", nil},
		{"identifier parameter cluster is invalid in SYSTEM grammar", "SYSTEM START MERGES ON CLUSTER {cluster:Identifier} hg_safe.db1__t", nil},
		{"identifier parameter cluster after target is invalid in SYSTEM grammar", "SYSTEM START MERGES hg_safe.db1__t ON CLUSTER {cluster:Identifier}", nil},
		{"cluster after target", "SYSTEM START DISTRIBUTED SENDS other.u ON CLUSTER hg_safe", tableRef("other", "u")},
		{"no target cluster", "SYSTEM START DISTRIBUTED SENDS ON CLUSTER hg_safe", nil},
		{"refresh view has no on cluster branch", "SYSTEM REFRESH VIEW ON CLUSTER hg_safe db1.t", nil},
		{"wait view has no on cluster branch", "SYSTEM WAIT VIEW ON CLUSTER hg_safe db1.t", nil},
		{"start view has no on cluster branch", "SYSTEM START VIEW ON CLUSTER hg_safe db1.t", nil},
		{"unknown command cluster", "SYSTEM RELOAD CONFIG ON CLUSTER hg_safe", nil},
		{"partition literal", "CHECK TABLE other.u PARTITION 'hg_safe'", tableRef("other", "u")},
		{"partition tuple", "CHECK TABLE other.u PARTITION (1, 'hg_safe')", tableRef("other", "u")},
		{"partition tuple function", "CHECK TABLE other.u PARTITION tuple(1, 'x')", tableRef("other", "u")},
		{"partition empty tuple function", "CHECK TABLE other.u PARTITION tuple()", tableRef("other", "u")},
		{"partition expression tuple", "CHECK TABLE other.u PARTITION (1+2, toUInt64(3))", tableRef("other", "u")},
		{"partition empty literal array", "CHECK TABLE other.u PARTITION []", tableRef("other", "u")},
		{"partition literal array", "CHECK TABLE other.u PARTITION [1,'x']", tableRef("other", "u")},
		{"partition nested literal array", "CHECK TABLE other.u PARTITION [[1],[2]]", tableRef("other", "u")},
		{"partition mixed scalar literal array", "CHECK TABLE other.u PARTITION [NULL,true,-1]", tableRef("other", "u")},
		{"partition literal array rejects expression", "CHECK TABLE other.u PARTITION [1+2]", nil},
		{"partition literal array rejects function", "CHECK TABLE other.u PARTITION [toUInt64(1)]", nil},
		{"partition literal array rejects tuple", "CHECK TABLE other.u PARTITION [(1,2)]", nil},
		{"partition literal array rejects trailing comma", "CHECK TABLE other.u PARTITION [1,]", nil},
		{"check settings after target", "CHECK TABLE hg_safe.t SETTINGS max_threads=1", tableRef("hg_safe", "t")},
		{"check settings after part", "CHECK TABLE hg_safe.t PART 'p' SETTINGS max_threads=1", tableRef("hg_safe", "t")},
		{"check settings after partition", "CHECK TABLE hg_safe.t PARTITION tuple(1) SETTINGS max_threads=1", tableRef("hg_safe", "t")},
		{"partition identifier is not ParserPartition", "CHECK TABLE other.u PARTITION hg_safe", nil},
		{"quoted partition keyword is not ParserPartition", "CHECK TABLE other.u PARTITION `DATABASE` hg_safe", nil},
		{"check rejects trailing garbage", "CHECK TABLE hg_safe.t garbage", nil},
		{"drop replica zkpath is not a table", "SYSTEM DROP REPLICA 'hg_safe' FROM ZKPATH '/clickhouse/hg_unsafe.db1__t'", nil},
		{"drop replica shard is not a table but later table is", "SYSTEM DROP REPLICA 'r' FROM SHARD 'hg_safe' FROM TABLE other.u", tableRef("other", "u")},
		{"unknown optimize settings", "OPTIMIZE TABLE other.u FINAL SETTINGS hg_safe=1", nil},
		{"unknown system identifier", "SYSTEM RELOAD CONFIG hg_safe.db1__t", nil},
		{"comments and literals", "SYSTEM RELOAD CONFIG /* hg_safe.fake */ 'hg_unsafe.db1__t' -- db1.t\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestNameRefs_SystemCommandSpecificTails(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{"flush object path", "SYSTEM FLUSH OBJECT STORAGE QUEUE hg_safe.t PATH 'p'", tableRef("hg_safe", "t")},
		{"flush object cluster after target", "SYSTEM FLUSH OBJECT STORAGE QUEUE hg_safe.t ON CLUSTER c PATH 'p'", tableRef("hg_safe", "t")},
		{"flush object missing path", "SYSTEM FLUSH OBJECT STORAGE QUEUE hg_safe.t", nil},
		{"flush object non-string path", "SYSTEM FLUSH OBJECT STORAGE QUEUE hg_safe.t PATH p", nil},
		{"sync replica strict", "SYSTEM SYNC REPLICA hg_safe.t STRICT", tableRef("hg_safe", "t")},
		{"sync replica if exists", "SYSTEM SYNC REPLICA hg_safe.t IF EXISTS", tableRef("hg_safe", "t")},
		{"sync replica lightweight", "SYSTEM SYNC REPLICA hg_safe.t LIGHTWEIGHT FROM 'r1', 'r2'", tableRef("hg_safe", "t")},
		{"sync replica pull", "SYSTEM SYNC REPLICA hg_safe.t PULL", tableRef("hg_safe", "t")},
		{"sync replica malformed list", "SYSTEM SYNC REPLICA hg_safe.t LIGHTWEIGHT FROM 'r1',", nil},
		{"sync database strict", "SYSTEM SYNC DATABASE REPLICA hg_safe STRICT", databaseRef("hg_safe")},
		{"invalid cluster after wait", "SYSTEM WAIT LOADING PARTS hg_safe.t ON CLUSTER c", nil},
		{"invalid cluster after ttl", "SYSTEM START TTL MERGES hg_safe.t ON CLUSTER c", nil},
		{"valid cluster after dictionary", "SYSTEM RELOAD DICTIONARY hg_safe.t ON CLUSTER c", tableRef("hg_safe", "t")},
		{"empty cluster after dictionary is invalid", "SYSTEM RELOAD DICTIONARY hg_safe.t ON CLUSTER ''", nil},
		{"settings scalar", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS max_threads=1", tableRef("hg_safe", "t")},
		{"settings map and parameter collection", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS x={'a':'b'}, param_y=[1, 2]", tableRef("hg_safe", "t")},
		{"settings bool shorthand", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS async_insert", tableRef("hg_safe", "t")},
		{"settings substitution", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS max_threads={threads:UInt64}", tableRef("hg_safe", "t")},
		{"settings substitution accepts bare keyword spelling", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS max_threads={SELECT:UInt64}", tableRef("hg_safe", "t")},
		{"settings substitution rejects quoted name", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS max_threads={`threads`:UInt64}", nil},
		{"settings disk", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS disk=disk(type='s3', path='x')", tableRef("hg_safe", "t")},
		{"settings disk function name is case sensitive", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS disk=DISK(type='s3')", nil},
		{"dynamic JSON path setting name", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS foo.:bar=1", tableRef("hg_safe", "t")},
		{"JSON array setting name", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS foo.bar[]=1", tableRef("hg_safe", "t")},
		{"settings disk rejects partial parse", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS disk=disk(type='s3' garbage)", nil},
		{"parameter collection rejects identifier", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x=[foo]", nil},
		{"parameter map rejects identifiers", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x={foo:bar}", nil},
		{"parameter map rejects collection key", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x={[1]:2}", nil},
		{"empty parameter name is invalid", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_=1", nil},
		{"uppercase parameter prefix is ordinary", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS PARAM_x=foo", nil},
		{"parameter dynamic JSON path", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x=foo.:bar", tableRef("hg_safe", "t")},
		{"parameter JSON prefix path", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x=foo.^bar", tableRef("hg_safe", "t")},
		{"parameter JSON at path", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x=foo.@bar", tableRef("hg_safe", "t")},
		{"parameter JSON array addition", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x=foo.bar[]", tableRef("hg_safe", "t")},
		{"parameter nested JSON array addition", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x=foo.bar[][].baz", tableRef("hg_safe", "t")},
		{"parameter value forbids query parameter", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x={v:Identifier}", nil},
		{"parameter component forbids query parameter", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x=foo.{v:Identifier}", nil},
		{"dynamic JSON component forbids array addition", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x=foo.:bar[]", nil},
		{"JSON prefix component forbids array addition", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x=foo.^bar[]", nil},
		{"JSON at component forbids array addition", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS param_x=foo.@bar[]", nil},
		{"settings infinity literal", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS max_threads=INFINITY", tableRef("hg_safe", "t")},
		{"settings signed infinity literal", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS max_threads=-INFINITY", tableRef("hg_safe", "t")},
		{"ordinary setting rejects identifier value", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS max_threads=garbage", nil},
		{"ordinary setting rejects array", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS max_threads=[1, 2]", nil},
		{"ordinary setting rejects tuple", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS max_threads=(1, 2)", nil},
		{"ordinary setting rejects non-string map", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS max_threads={'a':1}", nil},
		{"settings trailing garbage", "SYSTEM FLUSH DISTRIBUTED hg_safe.t SETTINGS max_threads=1 garbage", nil},
		{"schedule parts", "SYSTEM SCHEDULE MERGE hg_safe.t PARTS 'p1', 'p2'", tableRef("hg_safe", "t")},
		{"schedule merge requires parts", "SYSTEM SCHEDULE MERGE hg_safe.t", nil},
		{"schedule singular part", "SYSTEM SCHEDULE MERGE hg_safe.t PART 'p1'", nil},
		{"drop replica table", "SYSTEM DROP REPLICA 'r' FROM SHARD 's' FROM TABLE hg_safe.t", tableRef("hg_safe", "t")},
		{"drop database replica", "SYSTEM DROP DATABASE REPLICA 'r' FROM DATABASE hg_safe WITH TABLES", databaseRef("hg_safe")},
		{"drop replica missing literal", "SYSTEM DROP REPLICA garbage FROM TABLE hg_safe.t", nil},
		{"drop replica trailing", "SYSTEM DROP REPLICA 'r' FROM TABLE hg_safe.t garbage", nil},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestNameRefs_AlterDatabaseExactGrammar(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{"comment", "ALTER DATABASE hg_safe MODIFY COMMENT 'x'", databaseRef("hg_safe")},
		{"cluster follows database", "ALTER DATABASE hg_safe ON CLUSTER c MODIFY COMMENT 'x'", databaseRef("hg_safe")},
		{"multiple comments", "ALTER DATABASE hg_safe MODIFY COMMENT 'x', MODIFY COMMENT 'y'", databaseRef("hg_safe")},
		{"setting", "ALTER DATABASE hg_safe MODIFY SETTING max_threads=1", databaseRef("hg_safe")},
		{"parenthesized comment", "ALTER DATABASE hg_safe (MODIFY COMMENT 'x')", databaseRef("hg_safe")},
		{"parenthesized comments", "ALTER DATABASE hg_safe (MODIFY COMMENT 'x'), (MODIFY COMMENT 'y')", databaseRef("hg_safe")},
		{"parenthesized setting", "ALTER DATABASE hg_safe (MODIFY SETTING max_threads=1)", databaseRef("hg_safe")},
		{"parenthesized setting then comment", "ALTER DATABASE hg_safe (MODIFY SETTING max_threads=1), (MODIFY COMMENT 'x')", databaseRef("hg_safe")},
		{"missing command", "ALTER DATABASE hg_safe", nil},
		{"garbage command", "ALTER DATABASE hg_safe garbage", nil},
		{"cluster cannot precede database", "ALTER DATABASE ON CLUSTER c hg_safe MODIFY COMMENT 'x'", nil},
		{"empty cluster is invalid", "ALTER DATABASE hg_safe ON CLUSTER '' MODIFY COMMENT 'x'", nil},
		{"mixed parenthesized then bare commands", "ALTER DATABASE hg_safe (MODIFY COMMENT 'x'), MODIFY COMMENT 'y'", nil},
		{"mixed bare then parenthesized commands", "ALTER DATABASE hg_safe MODIFY COMMENT 'x', (MODIFY COMMENT 'y')", nil},
		{"identifier parameter type is exact", "ALTER DATABASE {db:identifier} MODIFY COMMENT 'x'", nil},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestNameRefs_LiveViewIdentifierParameterTypeIsCaseSensitive(t *testing.T) {
	e := newTestEngine(t)
	for _, sql := range []string{
		"CREATE LIVE VIEW {v:identifier} AS SELECT * FROM hg_safe.t",
		"CREATE LIVE VIEW other.v AS SELECT * FROM other.u AS {a:identifier} JOIN hg_safe.t ON 1",
	} {
		assertNameRefs(t, e, sql, nil)
	}
}

func TestNameRefs_DropAndTruncateExactModifiers(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{"truncate database both guards", "TRUNCATE DATABASE IF EXISTS IF EMPTY hg_safe", databaseRef("hg_safe")},
		{"truncate database no delay", "TRUNCATE DATABASE hg_safe NO DELAY", databaseRef("hg_safe")},
		{"truncate all like", "TRUNCATE ALL TABLES FROM hg_safe LIKE 'x%'", databaseRef("hg_safe")},
		{"truncate tables without all", "TRUNCATE TABLES FROM hg_safe", databaseRef("hg_safe")},
		{"truncate all not ilike sync", "TRUNCATE ALL TABLES FROM hg_safe NOT ILIKE 'x%' SYNC", databaseRef("hg_safe")},
		{"truncate all dangling not", "TRUNCATE ALL TABLES FROM hg_safe NOT", nil},
		{"drop dictionary if empty", "DROP DICTIONARY IF EMPTY hg_safe.d", tableRef("hg_safe", "d")},
		{"drop dictionary both guards", "DROP DICTIONARY IF EXISTS IF EMPTY hg_safe.d", tableRef("hg_safe", "d")},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestNameRefs_LiveViewUsesASTSourceSemantics(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{
			"columns functions aliases and settings",
			"CREATE LIVE VIEW other.v AS SELECT hg_safe, count(hg_unsafe), hg_safe.x AS hg_unsafe FROM other.u AS hg_safe SETTINGS hg_unsafe=1",
			append(tableRef("other", "v"), tableRef("other", "u")...),
		},
		{
			"cte alias is not a table",
			"CREATE LIVE VIEW other.v AS WITH hg_safe AS (SELECT * FROM db1.t) SELECT * FROM hg_safe JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("db1", "t")...), tableRef("other", "u")...),
		},
		{
			"recognized table function namespace",
			"CREATE LIVE VIEW other.v AS SELECT * FROM remote('host', 'hg_safe', 'db1__t')",
			append(tableRef("other", "v"), tableRef("hg_safe", "db1__t")...),
		},
		{
			"unresolved table function database is not a bare table",
			"CREATE LIVE VIEW other.v AS SELECT * FROM remote('host', concat('other'), 't')",
			tableRef("other", "v"),
		},
		{
			"partially resolved table function retains database namespace",
			"CREATE LIVE VIEW other.v AS SELECT * FROM remote('host', 'hg_safe', concat('t'))",
			append(tableRef("other", "v"), databaseRef("hg_safe")...),
		},
		{
			"current database table function retains table role",
			"CREATE LIVE VIEW other.v AS SELECT * FROM merge('t')",
			append(tableRef("other", "v"), tableRef("", "t")...),
		},
		{
			"scalar projection function is not a table source",
			"CREATE LIVE VIEW other.v AS SELECT merge('t')",
			tableRef("other", "v"),
		},
		{
			"escaped quoted AST source",
			"CREATE LIVE VIEW other.v AS SELECT * FROM `hg\\x5Funsafe`.`db1__t`",
			append(tableRef("other", "v"), tableRef("hg_unsafe", "db1__t")...),
		},
		{
			"decoy cannot mask later source",
			"CREATE LIVE VIEW other.v AS SELECT hg_safe, count(hg_unsafe) FROM other.u JOIN db1.t ON 1",
			append(append(tableRef("other", "v"), tableRef("other", "u")...), tableRef("db1", "t")...),
		},
		{
			"sources are stable deduped",
			"CREATE LIVE VIEW other.v AS SELECT * FROM db1.t JOIN db1.t AS again ON 1",
			append(tableRef("other", "v"), tableRef("db1", "t")...),
		},
		{
			"string decoy cannot reorder real sources",
			"CREATE LIVE VIEW other.v AS SELECT 'db1.t' AS decoy FROM hg_unsafe.x JOIN db1.t ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...), tableRef("db1", "t")...),
		},
		{
			"column qualifier decoy cannot reorder real sources",
			"CREATE LIVE VIEW other.v AS SELECT db1.t FROM hg_unsafe.x JOIN db1.t AS db1 ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...), tableRef("db1", "t")...),
		},
		{
			"table alias decoy cannot reorder real sources",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_unsafe.x AS db1 JOIN db1.t AS logical ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...), tableRef("db1", "t")...),
		},
		{
			"cte alias decoy cannot reorder real sources",
			"CREATE LIVE VIEW other.v AS WITH db1 AS (SELECT * FROM hg_unsafe.x) SELECT * FROM db1 JOIN db1.t ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...), tableRef("db1", "t")...),
		},
		{
			"duplicate actual sources keep first source precedence",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_unsafe.x JOIN hg_unsafe.x AS duplicate ON 1 JOIN db1.t ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...), tableRef("db1", "t")...),
		},
		{
			"mixed roles retain grammar order",
			`CREATE LIVE VIEW other.v AS
				WITH c AS (SELECT * FROM db.cte)
				SELECT remote('h', 'hg_safe', 'scalar'), (SELECT x FROM db.projection)
				FROM c
				JOIN remote('h', 'hg_unsafe', 'join_fn') AS r ON EXISTS (SELECT 1 FROM db.join_on)
				JOIN db.tail ON 1`,
			append(append(append(append(append(
				tableRef("other", "v"),
				tableRef("db", "cte")...),
				tableRef("db", "projection")...),
				tableRef("hg_unsafe", "join_fn")...),
				tableRef("db", "join_on")...),
				tableRef("db", "tail")...),
		},
		{
			"set sources are left before right",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_unsafe.x UNION ALL SELECT * FROM db1.t",
			append(append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...), tableRef("db1", "t")...),
		},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestNameRefs_LiveViewConsumesOnClusterBeforeClauseMarkers(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{
			"bare TO keyword is the cluster name",
			"CREATE LIVE VIEW other.v ON CLUSTER to TO hg_safe.sink AS SELECT * FROM other.u",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "sink")...), tableRef("other", "u")...),
		},
		{
			"bare AS keyword is the cluster name",
			"CREATE LIVE VIEW other.v ON CLUSTER as AS SELECT * FROM db1.t",
			append(tableRef("other", "v"), tableRef("db1", "t")...),
		},
		{
			"quoted keyword cluster name",
			"CREATE LIVE VIEW other.v ON CLUSTER `to` TO hg_safe.sink AS SELECT * FROM other.u",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "sink")...), tableRef("other", "u")...),
		},
		{
			"double quoted keyword cluster name",
			`CREATE LIVE VIEW other.v ON CLUSTER "as" AS SELECT * FROM db1.t`,
			append(tableRef("other", "v"), tableRef("db1", "t")...),
		},
		{
			"quoted dotted cluster name",
			"CREATE LIVE VIEW other.v ON CLUSTER `cluster.to` TO hg_safe.sink AS SELECT * FROM other.u",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "sink")...), tableRef("other", "u")...),
		},
		{
			"string cluster name",
			"CREATE LIVE VIEW other.v ON CLUSTER 'as' AS SELECT * FROM db1.t",
			append(tableRef("other", "v"), tableRef("db1", "t")...),
		},
		{
			"identifier parameter cluster is invalid in pinned LIVE VIEW grammar",
			"CREATE LIVE VIEW hg_safe.v ON CLUSTER {cluster:Identifier} TO hg_unsafe.sink AS SELECT * FROM db1.t",
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestNameRefs_LiveViewConsumesColumnPreambleAndRejectsOpaqueTO(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{
			"AS column name is not the query delimiter",
			"CREATE LIVE VIEW other.v (AS UInt64) AS SELECT * FROM db1.t",
			append(tableRef("other", "v"), tableRef("db1", "t")...),
		},
		{
			"TO column name and nested type are not destinations",
			"CREATE LIVE VIEW other.v (TO Nested(x UInt64), y String) AS SELECT * FROM db1.t",
			append(tableRef("other", "v"), tableRef("db1", "t")...),
		},
		{
			"opaque TO target does not erase proven primary or later source",
			"CREATE LIVE VIEW hg_safe.v TO {sink:Identifier} AS SELECT * FROM hg_unsafe.x",
			append(tableRef("hg_safe", "v"), tableRef("hg_unsafe", "x")...),
		},
		{
			"opaque TO target does not suppress a later proven source",
			"CREATE LIVE VIEW other.v TO {sink:Identifier} AS SELECT * FROM hg_unsafe.x",
			append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...),
		},
		{
			"opaque qualified TO target does not fabricate its database",
			"CREATE LIVE VIEW other.v TO hg_safe.{sink:Identifier} AS SELECT * FROM hg_unsafe.x",
			append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...),
		},
		{
			"UUID keeps primary destination and source ordering",
			"CREATE LIVE VIEW hg_safe.v UUID '01234567-89ab-cdef-0123-456789abcdef' ON CLUSTER to TO hg_unsafe.sink AS SELECT * FROM db1.t",
			append(append(tableRef("hg_safe", "v"), tableRef("hg_unsafe", "sink")...), tableRef("db1", "t")...),
		},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestNameRefs_LiveViewSQLSecurityPreamble(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"definer", "CREATE SQL SECURITY DEFINER LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"invoker", "CREATE SQL SECURITY INVOKER LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"bare AS definer before live view", "CREATE DEFINER=AS LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"bare TO definer plus security before live view", "CREATE DEFINER=TO SQL SECURITY DEFINER LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"security then definer before live view", "CREATE SQL SECURITY DEFINER DEFINER=TO LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"one string username before live view", "CREATE DEFINER='user' SQL SECURITY DEFINER LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"unquoted user and host before live view", "CREATE DEFINER=user@host SQL SECURITY DEFINER LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"unquoted user and string host before live view", "CREATE DEFINER=user@'host' SQL SECURITY DEFINER LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"quoted user may contain multiple at signs", "CREATE DEFINER=`a@b@c` LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"quoted user may have a separate host", "CREATE DEFINER=`user`@host LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"quoted user may have a separate string host", "CREATE DEFINER=`user`@'host' LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"quoted user may have a separate quoted host", "CREATE DEFINER=`user`@\"host\" LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"quoted user may have a spaced identifier host", "CREATE DEFINER=`user` @ host LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"quoted user may have a spaced string host", "CREATE DEFINER=`user` @ 'host' LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"quoted current user is an ordinary user with host", "CREATE DEFINER=`CURRENT_USER`@host LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"string username may contain multiple at signs", "CREATE DEFINER='a@b@c' LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"string host may contain an identifier parameter spelling", "CREATE DEFINER=user@'{host:Identifier}' LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"string username may contain a non-leading identifier parameter spelling", "CREATE DEFINER='prefix{name:Identifier}' LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"string username lowercase parameter type is ordinary content", "CREATE DEFINER='{name:identifier}' LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"string username spaced parameter spelling is ordinary content", "CREATE DEFINER='{name : Identifier}' LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"role keywords as unquoted user and host", "CREATE DEFINER=AS@TO LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"current user before live view", "CREATE DEFINER=CURRENT_USER LIVE VIEW other.v AS SELECT * FROM db1.t"},
		{"security after target", "CREATE LIVE VIEW other.v SQL SECURITY INVOKER AS SELECT * FROM db1.t"},
		{"bare AS definer after target", "CREATE LIVE VIEW other.v DEFINER=AS AS SELECT * FROM db1.t"},
		{"bare TO definer after columns", "CREATE LIVE VIEW other.v (x UInt64) DEFINER=TO AS SELECT * FROM db1.t"},
		{"unquoted user and host after target", "CREATE LIVE VIEW other.v DEFINER=user@host SQL SECURITY INVOKER AS SELECT * FROM db1.t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.ParseOne(tc.sql); err != nil {
				t.Fatalf("ParseOne(%q): %v", tc.sql, err)
			}
			assertNameRefs(t, e, tc.sql, append(tableRef("other", "v"), tableRef("db1", "t")...))
		})
	}
}

func TestNameRefs_LiveViewExactPinnedGrammar(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{
			"ATTACH is accepted",
			"ATTACH LIVE VIEW hg_safe.v UUID '01234567-89ab-cdef-0123-456789abcdef' TO hg_unsafe.sink UUID '11111111-2222-3333-4444-555555555555' (x UInt64) AS SELECT * FROM db1.t COMMENT 'live'",
			append(append(tableRef("hg_safe", "v"), tableRef("hg_unsafe", "sink")...), tableRef("db1", "t")...),
		},
		{
			"opaque primary is skipped but source remains",
			"CREATE LIVE VIEW hg_safe.{view:Identifier} AS SELECT * FROM hg_unsafe.x",
			tableRef("hg_unsafe", "x"),
		},
		{
			"opaque TO with UUID is skipped but source remains",
			"CREATE LIVE VIEW other.v TO {sink:Identifier} UUID '11111111-2222-3333-4444-555555555555' AS SELECT * FROM hg_unsafe.x",
			append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...),
		},
		{
			"heredoc UUID is a pinned string literal",
			"CREATE LIVE VIEW hg_safe.v UUID $uuid$01234567-89ab-cdef-0123-456789abcdef$uuid$ AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"empty-tag heredoc UUID is accepted",
			"CREATE LIVE VIEW hg_safe.v UUID $$01234567-89ab-cdef-0123-456789abcdef$$ AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"uppercase UUID is accepted",
			"CREATE LIVE VIEW hg_safe.v UUID '01234567-89AB-CDEF-0123-456789ABCDEF' AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"undashed UUID is accepted",
			"CREATE LIVE VIEW hg_safe.v UUID '0123456789abcdef0123456789abcdef' AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{"invalid primary UUID content", "CREATE LIVE VIEW hg_safe.v UUID 'not-a-uuid' AS SELECT * FROM db1.t", nil},
		{"invalid heredoc UUID content", "CREATE LIVE VIEW hg_safe.v UUID $uuid$not-a-uuid$uuid$ AS SELECT * FROM db1.t", nil},
		{"UUID identifier parameter is invalid", "CREATE LIVE VIEW hg_safe.v UUID {uuid:Identifier} AS SELECT * FROM db1.t", nil},
		{"missing primary UUID literal is invalid", "CREATE LIVE VIEW hg_safe.v UUID AS SELECT * FROM db1.t", nil},
		{"invalid TO UUID content", "CREATE LIVE VIEW hg_safe.v TO hg_unsafe.sink UUID 'not-a-uuid' AS SELECT * FROM db1.t", nil},
		{"invalid TO heredoc UUID content", "CREATE LIVE VIEW hg_safe.v TO hg_unsafe.sink UUID $u$not-a-uuid$u$ AS SELECT * FROM db1.t", nil},
		{"TO UUID identifier parameter is invalid", "CREATE LIVE VIEW hg_safe.v TO hg_unsafe.sink UUID {uuid:Identifier} AS SELECT * FROM db1.t", nil},
		{"OR REPLACE is not LIVE VIEW grammar", "CREATE OR REPLACE LIVE VIEW hg_safe.v AS SELECT * FROM db1.t", nil},
		{"REFRESH is not LIVE VIEW grammar", "CREATE LIVE VIEW hg_safe.v REFRESH 1 AS SELECT * FROM db1.t", nil},
		{"empty column list is invalid", "CREATE LIVE VIEW hg_safe.v () AS SELECT * FROM db1.t", nil},
		{"malformed table property list is invalid", "CREATE LIVE VIEW hg_safe.v (nonsense) AS SELECT * FROM db1.t", nil},
		{"malformed column is not masked by a valid index", "CREATE LIVE VIEW hg_safe.v (nonsense, INDEX idx nonsense TYPE minmax GRANULARITY 1) AS SELECT * FROM db1.t", nil},
		{"trailing column comma is invalid", "CREATE LIVE VIEW hg_safe.v (x UInt64,) AS SELECT * FROM db1.t", nil},
		{"trailing property comma is invalid", "CREATE LIVE VIEW hg_safe.v (INDEX idx x TYPE minmax GRANULARITY 1,) AS SELECT * FROM db1.t", nil},
		{
			"default expression supplies an omitted column type",
			"CREATE LIVE VIEW hg_safe.v (x DEFAULT 1) AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"materialized expression supplies an omitted column type",
			"CREATE LIVE VIEW hg_safe.v (x MATERIALIZED 1) AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"alias expression supplies an omitted column type",
			"CREATE LIVE VIEW hg_safe.v (x ALIAS 1) AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"ephemeral expression supplies an omitted column type",
			"CREATE LIVE VIEW hg_safe.v (x EPHEMERAL 1) AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"INDEX property list is parser proven",
			"CREATE LIVE VIEW hg_safe.v (x UInt64, INDEX idx x TYPE minmax GRANULARITY 1) AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"INDEX-only property list is parser proven",
			"CREATE LIVE VIEW hg_safe.v (INDEX idx x TYPE minmax GRANULARITY 1) AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"PROJECTION-only property list is parser proven",
			"CREATE LIVE VIEW hg_safe.v (PROJECTION p (SELECT x ORDER BY x)) AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"PRIMARY KEY-only property list is parser proven",
			"CREATE LIVE VIEW hg_safe.v (PRIMARY KEY x) AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"FOREIGN KEY-only property list is parser proven",
			"CREATE LIVE VIEW hg_safe.v (FOREIGN KEY (x) REFERENCES other.t(x)) AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{
			"CONSTRAINT property list is parser proven",
			"CREATE LIVE VIEW hg_safe.v (x UInt64, CONSTRAINT positive CHECK x > 0) AS SELECT * FROM db1.t",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
		{"security cannot appear both before and after", "CREATE SQL SECURITY DEFINER LIVE VIEW hg_safe.v SQL SECURITY INVOKER AS SELECT * FROM db1.t", nil},
		{"definer identifier parameter is invalid", "CREATE DEFINER={user:Identifier} LIVE VIEW hg_safe.v AS SELECT * FROM db1.t", nil},
		{"string username starting with identifier parameter spelling is invalid", "CREATE DEFINER='{name:Identifier}suffix' LIVE VIEW hg_safe.v AS SELECT * FROM db1.t", nil},
		{"current user cannot have a host", "CREATE DEFINER=CURRENT_USER@host LIVE VIEW hg_safe.v AS SELECT * FROM db1.t", nil},
		{"string username cannot have a host", "CREATE DEFINER='user'@'host' LIVE VIEW hg_safe.v AS SELECT * FROM db1.t", nil},
		{"post-AS security is not SELECT tail", "CREATE LIVE VIEW hg_safe.v AS SELECT * FROM db1.t SQL SECURITY DEFINER", nil},
		{"garbage after trailing COMMENT is invalid", "CREATE LIVE VIEW hg_safe.v AS SELECT * FROM db1.t COMMENT 'ok' DEFINER", nil},
		{"FORMAT is outside SelectWithUnion", "CREATE LIVE VIEW hg_safe.v AS SELECT * FROM db1.t FORMAT JSON", nil},
		{
			"valid SELECT SETTINGS tail is retained",
			"CREATE LIVE VIEW hg_safe.v AS SELECT * FROM db1.t SETTINGS max_threads=1",
			append(tableRef("hg_safe", "v"), tableRef("db1", "t")...),
		},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestValidLiveViewColumnsProbeRequiresExactSentinels(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{"CREATE TABLE __hg_live_view_columns_probe__ (x UInt64) ENGINE=Memory", true},
		{"CREATE TABLE evil (x UInt64) ENGINE=Memory", false},
		{"CREATE TABLE __hg_live_view_columns_probe__ (x UInt64) ENGINE=Other", false},
		{"CREATE TABLE db.__hg_live_view_columns_probe__ (x UInt64) ENGINE=Memory", false},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if got := validLiveViewColumnsProbe(ast); got != tc.want {
				t.Fatalf("validLiveViewColumnsProbe() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClassifyLiveView_ExactPinnedGrammar(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want LiveViewClass
	}{
		{
			"valid UUID heredoc and parser-proven properties",
			"ATTACH LIVE VIEW other.v UUID $uuid$01234567-89ab-cdef-0123-456789abcdef$uuid$ (x UInt64, INDEX idx x TYPE minmax GRANULARITY 1) AS SELECT * FROM other.u COMMENT 'live'",
			ExactLiveView,
		},
		{"quoted multi-at definer is valid", "CREATE DEFINER=`a@b@c` LIVE VIEW other.v AS SELECT * FROM other.u", ExactLiveView},
		{"cluster Identifier parameter is invalid", "CREATE LIVE VIEW other.v ON CLUSTER {cluster:Identifier} AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"empty tagged dollar cluster is invalid", "CREATE LIVE VIEW other.v ON CLUSTER $tag$$tag$ AS SELECT * FROM hg_safe.t", MalformedLiveViewPrefix},
		{"empty tagged dollar definer is invalid", "CREATE DEFINER=$tag$$tag$ LIVE VIEW other.v AS SELECT * FROM hg_safe.t", MalformedLiveViewPrefix},
		{"primary UUID content is invalid", "CREATE LIVE VIEW other.v UUID 'not-a-uuid' AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"TO UUID content is invalid", "CREATE LIVE VIEW other.v TO other.s UUID $u$invalid$u$ AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"UUID parameter is invalid", "CREATE LIVE VIEW other.v UUID {uuid:Identifier} AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"TO UUID parameter is invalid", "CREATE LIVE VIEW other.v TO other.s UUID {uuid:Identifier} AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"property declaration is invalid", "CREATE LIVE VIEW other.v (nonsense) AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"post-AS security is invalid", "CREATE LIVE VIEW other.v AS SELECT * FROM other.u SQL SECURITY DEFINER", MalformedLiveViewPrefix},
		{"tokens after COMMENT are invalid", "CREATE LIVE VIEW other.v AS SELECT * FROM other.u COMMENT 'live' DEFINER", MalformedLiveViewPrefix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ClassifyLiveView(e, ast, tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("ClassifyLiveView(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

type tokenCountingEngine struct {
	Engine
	tokenizeCalls int
}

func (e *tokenCountingEngine) Tokenize(sql string) (AST, error) {
	e.tokenizeCalls++
	return e.Engine.Tokenize(sql)
}

func TestClassifyLiveView_EscapedTargetUsesOneTokenization(t *testing.T) {
	base := newTestEngine(t)
	sql := "CREATE LIVE VIEW `hg\\x5Fsafe`.v AS SELECT 1"
	ast, err := base.ParseOne(sql)
	if err != nil {
		t.Fatal(err)
	}
	counted := &tokenCountingEngine{Engine: base}
	got, err := ClassifyLiveView(counted, ast, sql)
	if err != nil {
		t.Fatal(err)
	}
	if got != ExactLiveView {
		t.Fatalf("class = %v, want ExactLiveView", got)
	}
	if counted.tokenizeCalls != 1 {
		t.Fatalf("Tokenize calls = %d, want exactly one", counted.tokenizeCalls)
	}
}

func TestClassifyLiveView_OpaqueCurlyQuotedDefinerDoesNotTokenize(t *testing.T) {
	base := newTestEngine(t)
	for _, sql := range []string{
		"CREATE DEFINER=‘LIVE VIEW’ VIEW other.v AS SELECT 1",
		"CREATE DEFINER=“LIVE VIEW” VIEW other.v AS SELECT 1",
	} {
		t.Run(sql, func(t *testing.T) {
			counted := &tokenCountingEngine{Engine: base}
			got, err := ClassifyLiveView(counted, AST(`{"raw":{}}`), sql)
			if err != nil {
				t.Fatal(err)
			}
			if got != NotLiveView {
				t.Fatalf("class = %v, want NotLiveView", got)
			}
			if counted.tokenizeCalls != 0 {
				t.Fatalf("Tokenize calls = %d, want zero", counted.tokenizeCalls)
			}
		})
	}
}

func TestClassifyLiveView_BoundedStructuredPrefix(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql  string
		want LiveViewClass
	}{
		{"CREATE LIVE VIEW other.v AS SELECT * FROM other.u", ExactLiveView},
		{"CREATE DEFINER=alice LIVE VIEW other.v UUID 'invalid' AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"CREATE DEFINER={user:Identifier} LIVE VIEW other.v UUID 'invalid' AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"CREATE OR REPLACE DEFINER=alice LIVE VIEW other.v AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"CREATE OR ALTER DEFINER=alice LIVE VIEW other.v AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"CREATE TEMPORARY DEFINER=alice LIVE VIEW other.v AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"CREATE MATERIALIZED DEFINER=alice LIVE VIEW other.v AS SELECT * FROM other.u", MalformedLiveViewPrefix},
		{"CREATE OR REPLACE DEFINER=live VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE DEFINER=live VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE SQL SECURITY DEFINER VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE SQL SECURITY DEFINER DEFINER=live VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE VIEW live.view AS SELECT * FROM other.u", NotLiveView},
		{"CREATE VIEW other.v AS SELECT live VIEW FROM other.u", NotLiveView},
		{"SELECT 'CREATE LIVE VIEW other.v AS SELECT 1'", NotLiveView},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			ast, err := e.ParseOne(tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ClassifyLiveView(e, ast, tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("ClassifyLiveView(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

func TestClassifyLiveView_MalformedSecurityUsesExactCursor(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		sql  string
		want LiveViewClass
	}{
		{"CREATE DEFINER={user:Identifier} LIVE VIEW other.v UUID 'invalid' AS SELECT 1", MalformedLiveViewPrefix},
		{"CREATE DEFINER='user'@'host' LIVE VIEW other.v UUID 'invalid' AS SELECT 1", MalformedLiveViewPrefix},
		{"CREATE DEFINER=alice DEFINER=bob LIVE VIEW other.v UUID 'invalid' AS SELECT 1", MalformedLiveViewPrefix},
		{"CREATE DEFINER=@host LIVE VIEW other.v AS SELECT 1", MalformedLiveViewPrefix},
		{"CREATE DEFINER=() LIVE VIEW other.v AS SELECT 1", MalformedLiveViewPrefix},
		{"CREATE DEFINER=(SELECT 1) LIVE VIEW other.v AS SELECT 1", MalformedLiveViewPrefix},
		{"CREATE DEFINER=alice@@host LIVE VIEW other.v AS SELECT 1", MalformedLiveViewPrefix},
		{"CREATE DEFINER={user:String} LIVE VIEW other.v AS SELECT 1", MalformedLiveViewPrefix},
		{"CREATE DEFINER=alice@{host:String} LIVE VIEW other.v AS SELECT 1", MalformedLiveViewPrefix},
		{"CREATE DEFINER=1 LIVE VIEW other.v AS SELECT 1", MalformedLiveViewPrefix},
		{"CREATE DEFINER=live VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE DEFINER=1 VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE DEFINER=alice@live VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE DEFINER=@live VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE DEFINER=() VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE DEFINER=(LIVE VIEW) VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE DEFINER='live' VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE SQL SECURITY DEFINER VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE SQL SECURITY DEFINER DEFINER=live VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE DEFINER={user:Identifier} VIEW other.v AS SELECT 1", NotLiveView},
		{"CREATE SQL SECURITY BOGUS LIVE VIEW other.v UUID 'invalid' AS SELECT 1", NotLiveView}, // raw, not parser-proven create_view
	} {
		t.Run(tc.sql, func(t *testing.T) {
			var ast AST
			if tc.sql == "CREATE DEFINER=(LIVE VIEW) VIEW other.v AS SELECT 1" {
				// Polyglot rejects this decoy before producing an AST.  A parser-
				// proven create_view root isolates the recovery cursor behavior:
				// LIVE VIEW nested inside DEFINER must not classify the statement.
				ast = AST(`{"create_view":{}}`)
			} else {
				var err error
				ast, err = e.ParseOne(tc.sql)
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := ClassifyLiveView(e, ast, tc.sql)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("ClassifyLiveView(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

func TestNameRefs_LiveViewDefinerNamesAreNotObjectRefs(t *testing.T) {
	e := newTestEngine(t)
	assertNameRefs(t, e,
		"CREATE DEFINER=hg_safe LIVE VIEW other.v AS SELECT * FROM other.u",
		append(tableRef("other", "v"), tableRef("other", "u")...),
	)
}

func TestNameRefs_LiveViewInTableOperandsUseReadOrder(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{
			"infix and GLOBAL IN",
			"CREATE LIVE VIEW other.v AS SELECT id IN db1.t, id GLOBAL IN hg_safe.x FROM other.u",
			append(append(append(tableRef("other", "v"), tableRef("db1", "t")...), tableRef("hg_safe", "x")...), tableRef("other", "u")...),
		},
		{
			"callable in",
			"CREATE LIVE VIEW other.v AS SELECT in(id, hg_unsafe.x) FROM other.u",
			append(append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...), tableRef("other", "u")...),
		},
		{
			"arbitrary scalar argument is not a table operand",
			"CREATE LIVE VIEW other.v AS SELECT equals(id, hg_safe.x) FROM db1.t",
			append(tableRef("other", "v"), tableRef("db1", "t")...),
		},
		{
			"in-scope CTE is not an IN table",
			"CREATE LIVE VIEW other.v AS WITH t AS (SELECT * FROM other.u) SELECT id IN t FROM other.base",
			append(append(tableRef("other", "v"), tableRef("other", "u")...), tableRef("other", "base")...),
		},
		{
			"partially unresolved IN target is skipped but later source remains",
			"CREATE LIVE VIEW other.v AS SELECT id IN hg_safe.{target:Identifier} FROM hg_unsafe.x",
			append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...),
		},
		{
			"fully unresolved callable IN target is skipped but later source remains",
			"CREATE LIVE VIEW other.v AS SELECT in(id, {target:Identifier}) FROM hg_unsafe.x",
			append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...),
		},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestNameRefs_LiveViewVerifierRegressions(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []NameRef
	}{
		{
			"recursive CTE aliases stay scoped in bodies and output",
			"CREATE LIVE VIEW other.v AS WITH RECURSIVE t AS (SELECT * FROM t) SELECT * FROM t",
			tableRef("other", "v"),
		},
		{
			"output alias is not an infix IN table",
			"CREATE LIVE VIEW other.v AS SELECT tuple(1,2) AS t, 1 IN t",
			tableRef("other", "v"),
		},
		{
			"output alias is not a callable IN table",
			"CREATE LIVE VIEW other.v AS SELECT tuple(1,2) AS t, in(1, t)",
			tableRef("other", "v"),
		},
		{
			"FROM alias is known before projection IN",
			"CREATE LIVE VIEW other.v AS SELECT id IN t FROM other.u AS t",
			append(tableRef("other", "v"), tableRef("other", "u")...),
		},
		{
			"window PARTITION sources precede ORDER sources",
			"CREATE LIVE VIEW other.v AS SELECT sum(x) OVER (PARTITION BY (SELECT 1 FROM hg_unsafe.db1__x) ORDER BY (SELECT 1 FROM db1.t))",
			append(append(tableRef("other", "v"), tableRef("hg_unsafe", "db1__x")...), tableRef("db1", "t")...),
		},
		{
			"qualified opaque source does not fabricate physical prefix",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.{target:Identifier} JOIN hg_unsafe.db1__x ON 1",
			append(tableRef("other", "v"), tableRef("hg_unsafe", "db1__x")...),
		},
		{
			"parameterized table alias does not suppress later proven source",
			"CREATE LIVE VIEW other.v AS SELECT * FROM other.u AS {alias:Identifier} JOIN db1.t ON 1",
			append(append(tableRef("other", "v"), tableRef("other", "u")...), tableRef("db1", "t")...),
		},
		{
			"implicit parameterized table alias does not suppress later proven source",
			"CREATE LIVE VIEW other.v AS SELECT * FROM other.u {alias:Identifier} JOIN db1.t ON 1",
			append(append(tableRef("other", "v"), tableRef("other", "u")...), tableRef("db1", "t")...),
		},
		{
			"implicit parameterized table alias column list keeps source",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier}(x,y)",
			append(tableRef("other", "v"), tableRef("hg_safe", "t")...),
		},
		{
			"implicit parameterized table alias before FINAL keeps source",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} FINAL",
			append(tableRef("other", "v"), tableRef("hg_safe", "t")...),
		},
		{
			"implicit parameterized table alias before SAMPLE keeps source",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} SAMPLE 0.1",
			append(tableRef("other", "v"), tableRef("hg_safe", "t")...),
		},
		{
			"implicit parameterized table alias before OFFSET keeps source",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} OFFSET 1 ROW",
			append(tableRef("other", "v"), tableRef("hg_safe", "t")...),
		},
		{
			"implicit parameterized table alias before PARALLEL WITH keeps sources",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} PARALLEL WITH SELECT * FROM other.u",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"implicit parameterized table alias before ONLY JOIN keeps sources",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} ONLY JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"legacy ONLY JOIN keeps sources without an opaque alias",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t ONLY JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"qualified table component named only remains a table",
			"CREATE LIVE VIEW other.v AS SELECT 1 {x:Identifier} FROM hg_safe.only JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "only")...), tableRef("other", "u")...),
		},
		{
			"bare table named only remains a table",
			"CREATE LIVE VIEW other.v AS SELECT * FROM only JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("", "only")...), tableRef("other", "u")...),
		},
		{
			"explicit alias named only remains an alias",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t AS only JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"qualified table component named parallel is not a query delimiter",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.parallel WITH TOTALS",
			append(tableRef("other", "v"), tableRef("hg_safe", "parallel")...),
		},
		{
			"bare table named parallel is not a query delimiter",
			"CREATE LIVE VIEW other.v AS SELECT * FROM parallel WITH TOTALS",
			append(tableRef("other", "v"), tableRef("", "parallel")...),
		},
		{
			"explicit alias named parallel is not a query delimiter",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t AS parallel WITH TOTALS",
			append(tableRef("other", "v"), tableRef("hg_safe", "t")...),
		},
		{
			"parallel select expression before WITH TOTALS is not a query delimiter",
			"CREATE LIVE VIEW other.v AS SELECT parallel WITH TOTALS",
			tableRef("other", "v"),
		},
		{
			"parallel select expression before WITH ROLLUP is not a query delimiter",
			"CREATE LIVE VIEW other.v AS SELECT parallel WITH ROLLUP",
			tableRef("other", "v"),
		},
		{
			"parallel select expression before WITH CUBE is not a query delimiter",
			"CREATE LIVE VIEW other.v AS SELECT parallel WITH CUBE",
			tableRef("other", "v"),
		},
		{
			"implicit parameterized table alias before WITH TOTALS keeps source",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} WITH TOTALS",
			append(tableRef("other", "v"), tableRef("hg_safe", "t")...),
		},
		{
			"implicit parameterized table alias before GLOBAL join keeps sources",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} GLOBAL LEFT JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"implicit parameterized table alias before LOCAL join keeps sources",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} LOCAL LEFT JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"implicit parameterized table alias before ANY join keeps sources",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} ANY LEFT JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"implicit parameterized table alias before ALL join keeps sources",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} ALL INNER JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"implicit parameterized table alias before ASOF join keeps sources",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} ASOF LEFT JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"implicit parameterized table alias before SEMI join keeps sources",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} SEMI LEFT JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"implicit parameterized table alias before ANTI join keeps sources",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} ANTI LEFT JOIN other.u ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"implicit parameterized table alias before PASTE join keeps sources",
			"CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.t {alias:Identifier} PASTE JOIN other.u",
			append(append(tableRef("other", "v"), tableRef("hg_safe", "t")...), tableRef("other", "u")...),
		},
		{
			"implicit parameterized output alias does not suppress FROM source",
			"CREATE LIVE VIEW other.v AS SELECT 1 {alias:Identifier} FROM db1.t",
			append(tableRef("other", "v"), tableRef("db1", "t")...),
		},
		{
			"opaque expression parameter does not poison implicit alias",
			"CREATE LIVE VIEW other.v AS SELECT {column:Identifier}, 1 {alias:Identifier} FROM hg_unsafe.x",
			append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...),
		},
		{
			"opaque CAST type parameter does not poison implicit alias",
			"CREATE LIVE VIEW other.v AS SELECT CAST(1 AS {type:Identifier}), 1 {alias:Identifier} FROM hg_unsafe.x",
			append(tableRef("other", "v"), tableRef("hg_unsafe", "x")...),
		},
		{
			"unicode before source keeps token spans byte-correct",
			"CREATE LIVE VIEW other.v AS SELECT '雪' AS marker FROM hg_unsafe.db1__t JOIN db1.t ON 1",
			append(append(tableRef("other", "v"), tableRef("hg_unsafe", "db1__t")...), tableRef("db1", "t")...),
		},
	} {
		t.Run(tc.name, func(t *testing.T) { assertNameRefs(t, e, tc.sql, tc.want) })
	}
}

func TestParseLiveViewQueryExact_ParameterizedAliasMarkerIsReturned(t *testing.T) {
	e := newTestEngine(t)
	query := "SELECT * FROM other.u AS {alias:Identifier} JOIN db1.t ON 1"
	toks, err := tokenizeRaw(e, query)
	if err != nil {
		t.Fatal(err)
	}
	ast, ok := parseLiveViewQueryExact(e, query, toks, 0, len(toks))
	if !ok {
		t.Fatal("parameterized alias query was not accepted")
	}
	if !strings.Contains(string(ast), `"`+opaqueIdentifierParameterKey+`":true`) {
		t.Fatalf("returned AST lost opaque alias marker: %s", ast)
	}
	refs, err := CollectEmbeddedReadSources(ast)
	if err != nil {
		t.Fatal(err)
	}
	want := []ReadSourceRef{
		{Kind: ReadSourceTable, Target: TableTarget{DB: "other", Table: "u"}, Resolved: true, databaseIdentifier: true, tableIdentifier: true},
		{Kind: ReadSourceTable, Target: TableTarget{DB: "db1", Table: "t"}, Resolved: true, databaseIdentifier: true, tableIdentifier: true},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("sources = %#v, want %#v", refs, want)
	}
}

func TestNameRefs_ContextMutationsChangeOnlyRealTargets(t *testing.T) {
	e := newTestEngine(t)
	columnSQL := "CREATE LIVE VIEW other.v AS SELECT hg_safe.x FROM other.u AS hg_safe"
	tableSQL := "CREATE LIVE VIEW other.v AS SELECT * FROM hg_safe.x"
	assertNameRefs(t, e, columnSQL, append(tableRef("other", "v"), tableRef("other", "u")...))
	assertNameRefs(t, e, tableSQL, append(tableRef("other", "v"), tableRef("hg_safe", "x")...))

	partitionSQL := "CHECK TABLE other.u PARTITION 'hg_safe'"
	targetSQL := "CHECK TABLE hg_safe.u PARTITION 'other'"
	assertNameRefs(t, e, partitionSQL, tableRef("other", "u"))
	assertNameRefs(t, e, targetSQL, tableRef("hg_safe", "u"))
}

func TestNameRefs_QuotedIdentifiersUseParserDecoding(t *testing.T) {
	e := newTestEngine(t)
	for _, sql := range []string{
		"SYSTEM START MERGES `hg\\x5Funsafe`.`db1__t`",
		"SYSTEM START MERGES \"hg\\x5Funsafe\".\"db1__t\"",
	} {
		assertNameRefs(t, e, sql, tableRef("hg_unsafe", "db1__t"))
	}
	assertNameRefs(t, e, "SYSTEM START MERGES hg_safe.`x``y`", tableRef("hg_safe", "x`y"))
	assertNameRefs(t, e, `SYSTEM START MERGES hg_safe."x""y"`, tableRef("hg_safe", `x"y`))
	assertNameRefs(t, e, "SYSTEM START MERGES hg_safe.`x\\'y`", tableRef("hg_safe", "x'y"))
	assertNameRefs(t, e, "SYSTEM START MERGES hg_safe.`x\\`y`", tableRef("hg_safe", "x`y"))
	assertNameRefs(t, e, "SYSTEM START MERGES hg_safe.`x\\\\y`", tableRef("hg_safe", "x\\y"))
	assertNameRefs(t, e, "TRUNCATE DATABASE `hg\\x5Fsafe`", databaseRef("hg_safe"))
}

func TestNameRefs_TableFunctionIdentifierAndLiteralOriginsStayDistinct(t *testing.T) {
	e := newTestEngine(t)
	assertNameRefs(t, e,
		"CREATE LIVE VIEW other.v AS SELECT * FROM remote('h', `hg\\x5Fsafe`, 'db1__t')",
		append(tableRef("other", "v"), tableRef("hg_safe", "db1__t")...),
	)
	assertNameRefs(t, e,
		`CREATE LIVE VIEW other.v AS SELECT * FROM remote('h', 'hg\\x5Fsafe', 'db1__t')`,
		append(tableRef("other", "v"), tableRef(`hg\x5Fsafe`, "db1__t")...),
	)
}

func TestPrewhereTargets(t *testing.T) {
	e := newTestEngine(t)
	for _, tc := range []struct {
		name string
		sql  string
		want []TableTarget
	}{
		{"main table", "SELECT a FROM db1.t PREWHERE a > 1", []TableTarget{{DB: "db1", Table: "t"}}},
		{"parser-decoded quoted table", "SELECT a FROM `\\x64b1`.t PREWHERE a > 1", []TableTarget{{DB: "db1", Table: "t"}}},
		{"binds to the FROM table, not the JOIN", "SELECT * FROM other.u AS x JOIN db1.t AS s ON 1 PREWHERE x.a > 1", []TableTarget{{DB: "other", Table: "u"}}},
		{"literal is not a keyword", "SELECT 'PREWHERE' FROM db1.t", nil},
		{"absent", "SELECT a FROM db1.t WHERE a > 1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PrewhereTargets(e, tc.sql)
			if err != nil {
				t.Fatalf("PrewhereTargets: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i].DB != tc.want[i].DB || got[i].Table != tc.want[i].Table {
					t.Fatalf("got %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}

func tableRef(db, table string) []NameRef {
	return []NameRef{{Kind: NameRefTable, DB: db, Table: table}}
}

func databaseRef(db string) []NameRef {
	return []NameRef{{Kind: NameRefDatabase, DB: db}}
}

func assertNameRefs(t *testing.T, e Engine, sql string, want []NameRef) {
	t.Helper()
	got, err := NameRefs(e, sql)
	if err != nil {
		t.Fatalf("NameRefs(%q): %v", sql, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NameRefs(%q) = %+v, want %+v", sql, got, want)
	}
}
