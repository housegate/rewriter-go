package handlers

import (
	"fmt"
	"strings"

	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
	"github.com/housegate/rewriter-proto/gen/pb"
)

// reservedColumnRejectFmt is the shared (Go + C++) message for a user
// reference to the protocol row-id column (Spec G D3).
const reservedColumnRejectFmt = "reserved column %s is not addressable"

// splitPhysicalName splits "hg_safe.db1__t" at the FIRST dot into
// (db, table). Physical SI names never contain a dot inside the table part
// (CHTableName replaces "." with "__"). A name without a dot is a bare table.
func splitPhysicalName(name string) (string, string) {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}

// storageIntegritySurfaceSQL renders the derived-table body for one SI table:
//
//	SAFE (and UNSPECIFIED): SELECT * EXCEPT (<rid>) FROM <safe>
//	UNSAFE_LATEST:          … UNION ALL SELECT * EXCEPT (<rid>) FROM <unsafe>
//	                        [WHERE _part NOT IN ('p1', 'p2')]   (omitted when empty)
//
// The caller parses it with Engine.ParseOne and hands the AST to
// engine.ActionSubquery. Identifiers go through engine.QuoteQualified so the
// text re-parses under polyglot's ClickHouse dialect; part names are
// single-quoted literals with ” escaping.
func storageIntegritySurfaceSQL(tbl *pb.StorageIntegrityArgs_Table, args *pb.StorageIntegrityArgs) string {
	rid := args.GetReservedRowIdColumn()
	if rid == "" {
		rid = nameresolve.DefaultReservedRowIDColumn
	}
	ridSQL := engine.QuoteQualified("", rid)
	if rid != nameresolve.DefaultReservedRowIDColumn {
		ridSQL = "`" + strings.ReplaceAll(rid, "`", "``") + "`"
	}
	project := "SELECT * EXCEPT (" + ridSQL + ") FROM "
	sdb, st := splitPhysicalName(tbl.GetSafeTable())
	out := project + engine.QuoteQualified(sdb, st)
	if args.GetReadMode() != pb.StorageIntegrityArgs_READ_MODE_UNSAFE_LATEST {
		return out
	}
	udb, ut := splitPhysicalName(tbl.GetUnsafeTable())
	out += " UNION ALL " + project + engine.QuoteQualified(udb, ut)
	if parts := tbl.GetExcludedUnsafeParts(); len(parts) > 0 {
		quoted := make([]string, len(parts))
		for i, p := range parts {
			quoted[i] = "'" + escapeSQLLiteral(p) + "'"
		}
		out += " WHERE _part NOT IN (" + strings.Join(quoted, ", ") + ")"
	}
	return out
}

// storageIntegrityDecision builds the ActionSubquery decision for one SI
// table reference and records table_rewrites[origin] = safe_table.
func storageIntegrityDecision(e engine.Engine, tt engine.TableTarget, tbl *pb.StorageIntegrityArgs_Table, args *pb.StorageIntegrityArgs, rewrites map[string]string) (engine.TableDecision, error) {
	body, err := e.ParseOne(storageIntegritySurfaceSQL(tbl, args))
	if err != nil {
		return engine.TableDecision{}, fmt.Errorf("storage-integrity surface for %s: %w", qualify(tt.DB, tt.Table), err)
	}
	if rid := args.GetReservedRowIdColumn(); rid != "" && rid != nameresolve.DefaultReservedRowIDColumn {
		body, err = engine.QuoteIdentifier(body, rid)
		if err != nil {
			return engine.TableDecision{}, fmt.Errorf("storage-integrity surface for %s: %w", qualify(tt.DB, tt.Table), err)
		}
	}
	recordRewrite(rewrites, tt, "", tbl.GetSafeTable())
	return engine.TableDecision{Action: engine.ActionSubquery, Subquery: body}, nil
}
