package handlers

import (
	"strings"

	"github.com/housegate/rewriter-go/gen/pb"
	"github.com/housegate/rewriter-go/internal/engine"
	"github.com/housegate/rewriter-go/internal/nameresolve"
)

// RewriteGrant ports grant.cc. GRANT/REVOKE are never executed against the
// physical ClickHouse (a logical DB shares its physical with sibling tenants via
// prefix-sharing, so a CH-level grant would leak across tenants). The handler
// validates the statement against dynamic_args, emits one PrivilegeDelta per
// privilege (the per-element fan-out C++ gets from ClickHouse's parser, which
// splits `GRANT SELECT, INSERT` into one AccessRightsElement per privilege), and
// rewrites the SQL to a marker `SELECT '<canonical GRANT/REVOKE>' AS gstmt|rstmt`.
// Reject order matches grant.cc exactly (it decides which code wins when several
// conditions hold). Returns (resp, handled, err) with the RewriteWrite contract.
func RewriteGrant(e engine.Engine, ast engine.AST, sql string, opts []*pb.RewriteOption) (*pb.RewriteSQLResponse, bool, error) {
	kind, err := engine.NodeKind(ast)
	if err != nil {
		return nil, false, err
	}
	if kind != engine.NodeCommand {
		return nil, false, nil
	}
	gp, err := engine.ParseGrant(e, sql)
	if err != nil {
		return nil, false, err
	}
	if !gp.IsGrantVerb {
		return nil, false, nil // not GRANT/REVOKE → caller falls through
	}

	kw := "GRANT"
	stmt := pb.StatementType_STATEMENT_TYPE_GRANT
	if gp.IsRevoke {
		kw, stmt = "REVOKE", pb.StatementType_STATEMENT_TYPE_REVOKE
	}
	resp := newGrantResp(stmt)

	// Statement-level rejects, in grant.cc order.
	if gp.IsAttach {
		rejectUnsupported(resp, "ATTACH GRANT is not supported")
		return resp, true, nil
	}
	if !gp.HasOn {
		rejectUnsupported(resp, kw+" of a role to a user/role (role-membership grant) is not supported")
		return resp, true, nil
	}
	if gp.HasReplace {
		rejectUnsupported(resp, kw+" WITH REPLACE OPTION is not supported")
		return resp, true, nil
	}
	for _, p := range gp.Privileges {
		if strings.EqualFold(p.Name, "CURRENT GRANTS") {
			rejectUnsupported(resp, "GRANT CURRENT GRANTS is not supported")
			return resp, true, nil
		}
	}
	if !gp.Structured {
		// Pre-checks passed but polyglot's generic dialect couldn't structure it
		// (an exotic GRANT form). Parity-safe reject rather than mis-emit.
		rejectUnsupported(resp, kw+" form is not supported")
		return resp, true, nil
	}

	dyn := nameresolve.FindDynamicArgs(opts)
	if dyn == nil {
		rejectUnsupported(resp, kw+" requires a TableNameRewrite/dynamic_args option to validate against")
		return resp, true, nil
	}

	grantees, ok := buildGrantees(resp, kw, gp.Principals)
	if !ok {
		return resp, true, nil
	}

	origDB, origTable, scopeDatabase, anyDatabase := splitSecurable(gp.Securable)
	if anyDatabase {
		rejectUnsupported(resp, kw+" ON *.* (global scope) is not supported")
		return resp, true, nil
	}

	action := pb.PrivilegeDelta_ACTION_GRANT
	if gp.IsRevoke {
		action = pb.PrivilegeDelta_ACTION_REVOKE
	}

	// Per-privilege fan-out. Logical/physical resolution is lazy (resolved at the
	// first privilege) so the per-element reject precedence matches grant.cc:
	// column-level (Unsupported) is checked before logical/physical (Invalid).
	resolved := false
	var logical, physical, prefix string
	for _, p := range gp.Privileges {
		if len(p.Columns) > 0 {
			rejectUnsupported(resp, kw+" with column-level granularity is not supported")
			return resp, true, nil
		}
		if !resolved {
			logical = origDB
			if logical == "" {
				logical = dyn.GetUpstreamLogicalDatabaseInContext()
			}
			if logical == "" {
				rejectInvalid(resp, kw+" target '"+origTable+"' is unqualified and no upstream_logical_database_in_context is set; caller must send `USE <db>` or qualify the target")
				return resp, true, nil
			}
			var pok bool
			physical, pok = nameresolve.ResolvePhysicalDatabase(logical, dyn)
			if !pok {
				rejectInvalid(resp, kw+" target references logical database '"+logical+"' which is not in database_map and not a known physical database; user does not have this database to grant on")
				return resp, true, nil
			}
			prefix = nameresolve.BuildDynamicTablePrefix(logical, dyn)
			resolved = true
		}
		delta := &pb.PrivilegeDelta{
			Action:           action,
			OriginalDatabase: origDB,
			LogicalDatabase:  logical,
			PhysicalDatabase: physical,
			GrantOption:      gp.GrantOption,
			Privileges:       []string{p.Name},
			Grantees:         grantees,
		}
		if scopeDatabase {
			delta.Scope = pb.PrivilegeDelta_SCOPE_DATABASE
		} else {
			delta.Scope = pb.PrivilegeDelta_SCOPE_TABLE
			delta.OriginalTable = origTable
			delta.PhysicalTable = prefix + origTable
		}
		resp.PrivilegesDeltas = append(resp.PrivilegesDeltas, delta)
	}

	marker := "gstmt"
	if gp.IsRevoke {
		marker = "rstmt"
	}
	resp.SqlAfterRewrite = "SELECT '" + escapeSQLLiteral(gp.Marker) + "' AS " + marker
	return resp, true, nil
}

func newGrantResp(stmt pb.StatementType) *pb.RewriteSQLResponse {
	return &pb.RewriteSQLResponse{Code: pb.RewriteCode_Success, Message: "success", StatementType: stmt}
}

// buildGrantees translates principal names to proto Grantees. CURRENT_USER
// becomes a flagged grantee; an empty list is an Invalid reject (a well-formed
// GRANT always names a grantee).
//
// ALL / ANY is asymmetric, matching ClickHouse's parser (ParserRolesOrUsersSet
// recognizes the ALL/ANY keyword → set.all ONLY when allow_all is set, and
// allow_all == is_revoke):
//   - GRANT (allow_all=false): `TO ALL` parses as an ordinary identifier named
//     `ALL`, so the C++ oracle emits a normal grantee name="ALL" — we mirror it
//     via the default Grantee{Name: name}.
//   - REVOKE (allow_all=true): `FROM ALL` is the keyword → C++ rejects it as
//     UnsupportedStatement (grant.cc buildGrantees), so we reject here too.
func buildGrantees(resp *pb.RewriteSQLResponse, kw string, principals []string) ([]*pb.PrivilegeDelta_Grantee, bool) {
	var out []*pb.PrivilegeDelta_Grantee
	for _, name := range principals {
		if kw == "REVOKE" && (strings.EqualFold(name, "ALL") || strings.EqualFold(name, "ANY")) {
			rejectUnsupported(resp, "REVOKE FROM "+strings.ToUpper(name)+" is not supported")
			return nil, false
		}
		if strings.EqualFold(name, "CURRENT_USER") {
			out = append(out, &pb.PrivilegeDelta_Grantee{IsCurrentUser: true})
			continue
		}
		out = append(out, &pb.PrivilegeDelta_Grantee{Name: name})
	}
	if len(out) == 0 {
		rejectInvalid(resp, kw+" has no grantees")
		return nil, false
	}
	return out, true
}

// splitSecurable parses polyglot's flat securable ("db.t" / "db.*" / "*.*" / "t")
// into (database, table, scopeDatabase, anyDatabase). The table part "*" means
// ON db.* (SCOPE_DATABASE); the database part "*" means ON *.* (global, rejected).
// Splits on the LAST dot so a single-segment name is a bare table.
func splitSecurable(s string) (db, table string, scopeDatabase, anyDatabase bool) {
	if dot := strings.LastIndexByte(s, '.'); dot >= 0 {
		db, table = s[:dot], s[dot+1:]
	} else {
		table = s
	}
	if db == "*" {
		return db, table, false, true
	}
	if table == "*" {
		return db, "", true, false
	}
	return db, table, false, false
}
