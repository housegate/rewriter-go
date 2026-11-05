package nameresolve

import (
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
		protected := map[string]bool{}
		for _, db := range a.GetProtectedDatabases() {
			protected[db] = true
		}
		for logical, db := range a.GetDatabaseMap() {
			visible[logical] = true
			protected[db] = true
		}
		si := a.GetStorageIntegrity()
		for _, db := range si.GetReservedDatabases() {
			protected[db] = true
		}
		for _, table := range si.GetTables() {
			protected[databasePart(table.GetSafeTable())] = true
			protected[databasePart(table.GetUnsafeTable())] = true
		}
		for _, db := range a.GetKnownPhysicalDatabases() {
			if !protected[db] {
				visible[db] = true
			}
		}
		if ctx := a.GetUpstreamLogicalDatabaseInContext(); ctx != "" {
			visible[ctx] = true
		}
		physical = protected
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

// appendRemoteAddress adds a remote address and, when it carries a port, its
// host (an IPv6 literal is kept whole).
func appendRemoteAddress(literals []string, addr string) []string {
	if len(addr) >= minHiddenLiteral {
		literals = append(literals, addr)
	}
	colon := strings.LastIndexByte(addr, ':')
	if colon >= minHiddenLiteral && !strings.Contains(addr, "]") && strings.IndexByte(addr, ':') == colon {
		literals = append(literals, addr[:colon])
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
