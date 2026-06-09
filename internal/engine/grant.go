package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// GrantPrivilege is one privilege in a GRANT/REVOKE, with the columns of a
// column-level grant (non-empty → the handler rejects it).
type GrantPrivilege struct {
	Name    string   // "SELECT", "ALTER UPDATE", "ALL", "CURRENT GRANTS", … (as polyglot emits)
	Columns []string // GRANT SELECT(c1,c2) → ["c1","c2"]; empty otherwise
}

// GrantParse is the recovered structure of a GRANT / REVOKE. The engine extracts
// the shape (token-level flags for the forms polyglot's generic dialect can't
// parse, plus the generic node for the rest); the handler owns every policy
// decision and reject code.
type GrantParse struct {
	IsGrantVerb bool // leading token GRANT / REVOKE / ATTACH GRANT — else not ours
	IsRevoke    bool
	IsAttach    bool // ATTACH GRANT (system-internal form)
	HasReplace  bool // … WITH REPLACE OPTION
	HasOn       bool // an ON <securable> clause exists (absent → role-membership grant)
	Structured  bool // generic-dialect parse succeeded → the fields below are populated

	Privileges  []GrantPrivilege
	Securable   string   // "db.t" / "db.*" / "*.*" / "t" (flat, from the generic node)
	Principals  []string // grantee names in source order ("u", "CURRENT_USER", "ALL")
	GrantOption bool     // WITH GRANT OPTION (GRANT) / GRANT OPTION FOR (REVOKE)
	Marker      string   // canonical CH SQL (ON CLUSTER stripped) for the marker SELECT
}

// ParseGrant recovers GRANT/REVOKE structure. The clickhouse dialect renders
// GRANT/REVOKE as an opaque `command` node; the generic dialect structures them
// but FAILS on ATTACH GRANT, `… WITH REPLACE OPTION`, and the role-membership form
// (no ON clause), and on `ON CLUSTER …`. ParseGrant detects those at the token
// level (so the handler can reject with the right code instead of surfacing a
// generic parse error as a SyntaxError), strips any `ON CLUSTER <name>` fragment
// (which the generic parser rejects and the C++ handler drops anyway), then
// generic-parses the remainder and decodes the node. The marker SQL is the
// (cluster-free) generic AST regenerated under the clickhouse dialect — the same
// canonicalization the C++ handler gets from formatAst.
func ParseGrant(e Engine, sql string) (GrantParse, error) {
	toks, err := tokenizeRaw(e, sql)
	if err != nil {
		return GrantParse{}, err
	}
	if len(toks) == 0 {
		return GrantParse{}, nil
	}
	var gp GrantParse
	switch strings.ToUpper(toks[0].Text) {
	case "GRANT":
		gp.IsGrantVerb = true
	case "REVOKE":
		gp.IsGrantVerb, gp.IsRevoke = true, true
	case "ATTACH":
		if len(toks) >= 2 && strings.EqualFold(toks[1].Text, "GRANT") {
			gp.IsGrantVerb, gp.IsAttach = true, true
			return gp, nil // handler rejects; generic parse would fail
		}
		return gp, nil
	default:
		return gp, nil // not a GRANT/REVOKE
	}

	gp.HasOn = tokensHaveSecurableOn(toks)
	gp.HasReplace = tokensHaveReplaceOption(toks)
	if !gp.HasOn || gp.HasReplace {
		return gp, nil // handler rejects on the flags; generic parse would fail
	}

	cleaned := stripOnCluster(sql, toks)
	node, perr := e.ParseGeneric(cleaned)
	if perr != nil {
		return gp, nil // exotic but valid GRANT; handler rejects as Unsupported (Structured=false)
	}
	if derr := decodeGrantNode(node, &gp); derr != nil {
		return gp, nil
	}
	marker, gerr := e.Generate(node)
	if gerr != nil {
		return gp, nil
	}
	gp.Marker = marker
	gp.Structured = true
	return gp, nil
}

// tokensHaveSecurableOn reports whether an ON token introduces a securable (i.e.
// an ON NOT immediately followed by CLUSTER). Role-membership grants have no ON;
// `ON CLUSTER` alone is not a securable ON.
func tokensHaveSecurableOn(toks []rawToken) bool {
	for i, tk := range toks {
		if tk.TokenType == "ON" && (i+1 >= len(toks) || toks[i+1].TokenType != "CLUSTER") {
			return true
		}
	}
	return false
}

// tokensHaveReplaceOption reports whether the token stream contains `WITH REPLACE`
// (the `… WITH REPLACE OPTION` form polyglot's generic dialect rejects).
func tokensHaveReplaceOption(toks []rawToken) bool {
	for i := 0; i+1 < len(toks); i++ {
		if strings.EqualFold(toks[i].Text, "WITH") && strings.EqualFold(toks[i+1].Text, "REPLACE") {
			return true
		}
	}
	return false
}

// stripOnCluster removes the `ON CLUSTER <name>` byte span (a SECOND ON followed
// by CLUSTER + the cluster name) from sql, using the token spans. Leaves the rest
// verbatim; the resulting double space is harmless (the marker is regenerated).
func stripOnCluster(sql string, toks []rawToken) string {
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].TokenType == "ON" && toks[i+1].TokenType == "CLUSTER" {
			start, end := toks[i].Span.Start, toks[i+2].Span.End
			if start >= 0 && end <= len(sql) && start < end {
				return sql[:start] + sql[end:]
			}
		}
	}
	return sql
}

// decodeGrantNode reads the generic-dialect grant/revoke node into gp.
func decodeGrantNode(node AST, gp *GrantParse) error {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(node, &env); err != nil {
		return err
	}
	body, ok := env["grant"]
	if !ok {
		if body, ok = env["revoke"]; !ok {
			return fmt.Errorf("engine: not a grant/revoke node")
		}
	}
	var raw struct {
		Privileges []struct {
			Name    string   `json:"name"`
			Columns []string `json:"columns"`
		} `json:"privileges"`
		Securable struct {
			Name string `json:"name"`
		} `json:"securable"`
		Principals []struct {
			Name struct {
				Name string `json:"name"`
			} `json:"name"`
		} `json:"principals"`
		GrantOption bool `json:"grant_option"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	for _, p := range raw.Privileges {
		gp.Privileges = append(gp.Privileges, GrantPrivilege{Name: p.Name, Columns: p.Columns})
	}
	gp.Securable = raw.Securable.Name
	for _, pr := range raw.Principals {
		gp.Principals = append(gp.Principals, pr.Name.Name)
	}
	gp.GrantOption = raw.GrantOption
	return nil
}
