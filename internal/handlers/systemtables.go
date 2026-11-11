package handlers

import (
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// systemTableScan reads every caller-input object reference of the
// statement once (engine.CollectObjectRefs) and returns the first, in
// collector order (documented there), that names a system table outside the
// allowlist (spec
// 2026-09-26 §5, "The system database", step 1) or any table of
// information_schema, or refused=false. siObject
// reports that the statement also names a storage-integrity object — an SI
// physical or reserved database, an Active SI table (qualified or through the
// session's logical database), or an SI logical database through a
// non-exact carrier — so that, while the SI surface is active, the SI
// pipeline answers first. Dynamic mode only. The references come from the
// caller's statement, never from SQL a handler emitted, so the rewriter's
// own system.tables / system.columns synthesis for SHOW TABLES and the SI
// DESCRIBE keeps working.
func systemTableScan(e engine.Engine, ast engine.AST, sql string, sel nameresolve.Selection) (hit systemRefusal, refused, siObject bool, err error) {
	if sel.Mode != nameresolve.ModeDynamic {
		return systemRefusal{}, false, false, nil
	}
	refs, err := engine.CollectObjectRefs(e, ast, sql)
	if err != nil {
		return systemRefusal{}, false, false, err
	}
	dyn := sel.Dynamic
	ctx := dyn.GetUpstreamLogicalDatabaseInContext()
	for _, ref := range refs {
		if engine.IsSystemDatabase(ref.DB, ctx) {
			// A pattern or a non-static name (Exact false) is never allowed,
			// whatever its text.
			if !refused && !(ref.Exact && nameresolve.SystemTableAllowed(ref.Table)) {
				hit, refused = systemRefusal{db: nameresolve.SystemDatabase, table: ref.Table}, true
			}
			continue
		}
		if db := refDatabase(ref, ctx); nameresolve.MetadataDatabaseRefused(db) {
			// information_schema: every table, pattern or name is refused.
			if !refused {
				hit, refused = systemRefusal{db: db, table: ref.Table}, true
			}
			continue
		}
		if !nameresolve.StorageIntegritySurfaceActive(dyn) || siObject {
			continue
		}
		db := ref.DB
		if db == "" {
			db = ctx
		}
		switch {
		case nameresolve.IsStorageIntegrityPhysicalDatabase(db, dyn):
			siObject = true
		case !ref.Exact && nameresolve.IsStorageIntegrityLogicalDatabase(db, dyn):
			siObject = true
		default:
			if _, _, hit := nameresolve.LookupStorageIntegrity(ref.DB, ref.Table, dyn); hit {
				siObject = true
			}
		}
	}
	return hit, refused, siObject, nil
}

// systemRefusal is the first refused metadata reference of a statement: a
// system table outside the allowlist (db is nameresolve.SystemDatabase), or
// any table of information_schema (db as the statement spells it).
type systemRefusal struct {
	db, table string
}

// message is the cross-engine refusal text for r.
func (r systemRefusal) message() string {
	if r.db != nameresolve.SystemDatabase {
		return nameresolve.MetadataDatabaseRefusedMessage(r.db)
	}
	return nameresolve.SystemTableRefusedMessage(r.table)
}

// refDatabase is the database a reference reads: its qualifier, or the
// session's database for an unqualified name.
func refDatabase(ref engine.ObjectRef, contextDB string) string {
	if ref.DB == "" {
		return contextDB
	}
	return ref.DB
}

// rejectSystemTable turns a systemTableScan hit into resp's refusal:
// UnsupportedStatement, the refusal's message, the metadata table recorded
// as accessed (once), and no rewrite maps, so nothing of a partial rewrite
// leaks.
func rejectSystemTable(resp *pb.RewriteSQLResponse, hit systemRefusal, sel nameresolve.Selection) {
	resp.Code = pb.RewriteCode_UnsupportedStatement
	resp.Message = hit.message()
	resp.TableRewrites = nil
	resp.DatabaseRewrites = nil
	resp.PrivilegesDeltas = nil
	if hit.table != "" {
		recordAccessedWriteUnique(resp, engine.TableTarget{DB: hit.db, Table: hit.table}, sel)
	}
}

// RejectSystemTablesOnSuccess is the backstop for a statement whose check
// the preflight deferred: while the SI surface is active and the statement
// names a storage-integrity object, the SI pipeline answers first (spec
// 2026-09-26 §5: SI messages keep precedence), and the check runs on its
// would-be Success, before the mid-statement drop gate. It runs on every
// SI-active Success, so it is also the backstop for any path the preflight
// did not see. It reports whether it refused resp; a refusal clears the
// rewrite maps and keeps the original_accessed_tables the SI pipeline
// recorded (with the system table added once). A collector error refuses
// with the SI catch-all, like every other handler error under the active
// surface.
func RejectSystemTablesOnSuccess(e engine.Engine, ast engine.AST, sql string, sel nameresolve.Selection, resp *pb.RewriteSQLResponse) bool {
	if sel.Mode != nameresolve.ModeDynamic || !nameresolve.StorageIntegritySurfaceActive(sel.Dynamic) {
		return false
	}
	hit, refused, _, err := systemTableScan(e, ast, sql, sel)
	if err != nil {
		resp.Code = pb.RewriteCode_UnsupportedStatement
		resp.Message = nameresolve.StorageIntegrityUnmodelledMessage
		resp.TableRewrites = nil
		resp.DatabaseRewrites = nil
		resp.PrivilegesDeltas = nil
		resp.SqlAfterRewrite = sql
		return true
	}
	if !refused {
		return false
	}
	rejectSystemTable(resp, hit, sel)
	resp.SqlAfterRewrite = sql
	return true
}

// systemTableShowRefusal classifies a SHOW kind that reads a system table
// outside the allowlist (the SHOW → system mapping measured on ClickHouse
// 26.2 and 25.8). It returns the table the form reads, or refused=false for
// a kind that reads an allowed table (CLUSTER[S], SETTINGS, CHANGED
// SETTINGS, SETTING, FUNCTIONS, ENGINES, PRIVILEGES) or is handled elsewhere
// (TABLES, DATABASES, CREATE, COLUMNS / INDEX family). unknown reports a
// kind outside both lists; the caller refuses it.
//
// SHOW GRANTS / ACCESS and SHOW FILESYSTEM CACHES read access storage and the
// cache registry directly; the message names the equivalent system table.
func systemTableShowRefusal(info engine.DBLevelInfo) (table string, refused, unknown bool) {
	switch info.ShowWhat {
	case "PROCESSLIST":
		return "processes", true, false
	case "MERGES":
		return "merges", true, false
	case "DICTIONARIES":
		return "dictionaries", true, false
	case "USERS", "ACCESS":
		return "users", true, false
	case "ROLES":
		return "roles", true, false
	case "GRANTS":
		return "grants", true, false
	case "PROFILES":
		return "settings_profiles", true, false
	case "ROW", "POLICIES":
		return "row_policies", true, false
	case "QUOTA":
		return "quota_usage", true, false
	case "QUOTAS":
		return "quotas", true, false
	case "FILESYSTEM", "CACHES":
		return "filesystem_cache_settings", true, false
	case "CURRENT":
		if info.ShowWhatNext == "QUOTA" {
			return "quota_usage", true, false
		}
		return "current_roles", true, false
	case "ENABLED":
		return "enabled_roles", true, false
	case "SETTINGS":
		if info.ShowWhatNext == "PROFILES" {
			return "settings_profiles", true, false
		}
		return "", false, false
	case "CLUSTER", "CLUSTERS", "FUNCTIONS", "ENGINES", "CHANGED", "SETTING", "PRIVILEGES",
		"TABLES", "DATABASES", "CREATE", "COLUMNS", "FIELDS", "INDEX", "INDEXES", "INDICES", "KEYS":
		return "", false, false
	}
	return "", false, true
}
