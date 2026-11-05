package nameresolve

import (
	"slices"
	"sort"
	"strings"

	"github.com/housegate/rewriter-proto/gen/pb"
)

// HiddenNames lists what an inverted RewriteErrorMessage text may not
// contain: the physical side of the request's routing that the caller has no
// name for. identifiers are physical and protocol databases, matched as
// case-sensitive whole identifiers; literals are remote addresses, their
// hosts and passwords, matched as substrings (parts shorter than four bytes
// are dropped). Names the caller may write itself (logical databases, a
// known-physical database that is not protected) are left out. Mirrors
// rewriter-grpc hiddenNamesFor.
func HiddenNames(opts []*pb.RewriteOption) (identifiers, literals []string) {
	physical := map[string]bool{}
	visible := map[string]bool{}
	sel := FindActive(opts)
	switch sel.Mode {
	case ModeDynamic:
		a := sel.Dynamic
		// Candidates, kept only when the table-reference policy protects
		// them (ProtectedDatabase): the SI databases count while the surface
		// is active, and reserved_databases only under contract V2.
		candidates := map[string]bool{}
		for _, db := range a.GetProtectedDatabases() {
			candidates[db] = true
		}
		for logical, db := range a.GetDatabaseMap() {
			visible[logical] = true
			candidates[db] = true
		}
		si := a.GetStorageIntegrity()
		for _, db := range si.GetReservedDatabases() {
			candidates[db] = true
		}
		for _, table := range si.GetTables() {
			candidates[databasePart(table.GetSafeTable())] = true
			candidates[databasePart(table.GetUnsafeTable())] = true
		}
		for db := range candidates {
			if ProtectedDatabase(db, a) {
				physical[db] = true
			}
		}
		for _, db := range a.GetKnownPhysicalDatabases() {
			if !physical[db] {
				visible[db] = true
			}
		}
		if ctx := a.GetUpstreamLogicalDatabaseInContext(); ctx != "" {
			visible[ctx] = true
		}
		for _, key := range sortedKeys(a.GetRemoteUpstreams()) {
			upstream := a.GetRemoteUpstreams()[key]
			literals = appendRemoteAddress(literals, upstream.GetAddr())
			literals = appendSecret(literals, upstream.GetPassword())
		}
	case ModeStatic:
		a := sel.Static
		addOrigin := func(origin string) {
			if db := databasePart(origin); db != "" {
				visible[db] = true
			}
		}
		for origin := range a.GetTableMap() {
			addOrigin(origin)
		}
		for origin, target := range a.GetTableWithDatabaseMap() {
			addOrigin(origin)
			physical[target.GetDatabase()] = true
		}
		for _, origin := range sortedKeys(a.GetRemoteTableMap()) {
			remote := a.GetRemoteTableMap()[origin]
			addOrigin(origin)
			physical[remote.GetDatabase()] = true
			literals = appendRemoteAddress(literals, remote.GetAddr())
			literals = appendSecret(literals, remote.GetPassword())
		}
	}
	for db := range physical {
		if db != "" && !visible[db] {
			identifiers = append(identifiers, db)
		}
	}
	sort.Strings(identifiers)
	return identifiers, literals
}

const minHiddenLiteral = 4

// databasePart is the text before the first dot of a qualified name, or ""
// when there is none.
func databasePart(qualified string) string {
	if dot := strings.IndexByte(qualified, '.'); dot >= 0 {
		return qualified[:dot]
	}
	return ""
}

// appendRemoteAddress adds a remote address as configured and every host it
// names: ClickHouse's remote() takes a comma-separated list of shards and
// `|`-separated replicas, each host, host:port, [ipv6] or [ipv6]:port. An
// error may name a host without the rest of the address. Mirrors
// rewriter-grpc addRemoteAddress.
func appendRemoteAddress(literals []string, addr string) []string {
	add := func(v string) {
		v = strings.Trim(v, " \t")
		if len(v) >= minHiddenLiteral && !slices.Contains(literals, v) {
			literals = append(literals, v)
		}
	}
	add(addr)
	for _, part := range strings.FieldsFunc(addr, func(r rune) bool { return r == ',' || r == '|' }) {
		part = strings.TrimLeft(part, " \t")
		add(part)
		if strings.HasPrefix(part, "[") {
			if end := strings.IndexByte(part, ']'); end >= 0 {
				add(part[1:end])
			}
		} else if colon := strings.IndexByte(part, ':'); colon >= 0 && strings.LastIndexByte(part, ':') == colon {
			add(part[:colon])
		}
	}
	return literals
}

func appendSecret(literals []string, secret string) []string {
	if len(secret) >= minHiddenLiteral {
		literals = append(literals, secret)
	}
	return literals
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
