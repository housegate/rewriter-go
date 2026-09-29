package nameresolve

import (
	"fmt"
	"strings"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// ProtectedDatabase reports whether db may never be named by caller SQL in
// any position (spec 2026-09-26 §5): every database_map value, every
// protected_databases entry, and, while the storage-integrity surface is
// active, every SI physical or reserved database. It is independent of the
// SI contract so the policy holds when storage integrity is disabled.
func ProtectedDatabase(db string, a *pb.RewriteTableDynamicArgs) bool {
	if db == "" || a == nil {
		return false
	}
	for _, physical := range a.GetDatabaseMap() {
		if physical == db {
			return true
		}
	}
	for _, protected := range a.GetProtectedDatabases() {
		if protected == db {
			return true
		}
	}
	return IsStorageIntegrityPhysicalDatabase(db, a)
}

// ProtectedDatabaseRejectMessage is the cross-engine message for a protected
// database reference (spec 2026-09-26 T3).
func ProtectedDatabaseRejectMessage(db string) string {
	return "protected database " + db + " is not addressable"
}

// UnresolvedUnqualifiedTableMessage is the cross-engine message for an
// unqualified table name — including the one-part dotted quoted form
// `db1.t`, whose table is "db1.t" — that does not resolve through the
// session's logical database in dynamic mode (spec 2026-09-26 §5). ClickHouse
// resolves such a name in the session's current database, which is the
// physical database, so it is refused instead of forwarded.
//
// The table name is inserted verbatim, without escaping a `"` it contains,
// exactly like the T6 string-lookup message; both engines follow this.
func UnresolvedUnqualifiedTableMessage(table string) string {
	return unresolvedUnqualifiedPrefix + table + unresolvedUnqualifiedSuffix
}

const (
	unresolvedUnqualifiedPrefix = `unqualified table "`
	unresolvedUnqualifiedSuffix = `" does not resolve through the session's logical database`
)

// IsUnresolvedUnqualifiedTableMessage reports whether msg was built by
// UnresolvedUnqualifiedTableMessage.
func IsUnresolvedUnqualifiedTableMessage(msg string) bool {
	return len(msg) >= len(unresolvedUnqualifiedPrefix)+len(unresolvedUnqualifiedSuffix) &&
		strings.HasPrefix(msg, unresolvedUnqualifiedPrefix) && strings.HasSuffix(msg, unresolvedUnqualifiedSuffix)
}

// ValidateProtectedDatabases mirrors the reserved_databases rule: every entry
// is a simple identifier. Checked before any handler runs.
func ValidateProtectedDatabases(a *pb.RewriteTableDynamicArgs) error {
	for _, db := range a.GetProtectedDatabases() {
		if !simpleIdentifier(db) {
			return fmt.Errorf("protected_databases entry %q must be a simple identifier", db)
		}
	}
	return nil
}
