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
	Securable   GrantSecurable // the ON target, decoded from tokens by identity
	Principals  []string       // grantee names in source order ("u", "CURRENT_USER", "ALL")
	GrantOption bool           // WITH GRANT OPTION (GRANT) / GRANT OPTION FOR (REVOKE)
	Marker      string         // canonical CH SQL (ON CLUSTER stripped) for the marker SELECT
}

// GrantSecurable is a GRANT / REVOKE ON target decoded by identity, from the
// raw tokens: DB and Table are the decoded identifier texts, never a flat
// "db.table" string re-split later — `db1.t` is the table named "db1.t" in
// the current database, not table t of db1. AllTables / AnyDatabase record an
// unquoted '*' in the table / database position (ON db.*, ON *, ON *.*).
// Unrepresentable is set when a quoted identifier contains '*': a privilege
// delta has no way to tell that name from a wildcard, so the handler refuses
// it rather than widen it into a database-scope grant.
type GrantSecurable struct {
	DB, Table       string
	AllTables       bool
	AnyDatabase     bool
	Unrepresentable bool
}

// Flat renders the securable the way polyglot's generic node spells it
// ("db.t" / "db.*" / "*.*" / "t" / "*"), unquoted. It is a consistency check
// against that node only, never an identity.
func (s GrantSecurable) Flat() string {
	table := s.Table
	if s.AllTables {
		table = "*"
	}
	db := s.DB
	if s.AnyDatabase {
		db = "*"
	}
	if db == "" {
		return table
	}
	return db + "." + table
}

// Quoted renders the securable as ClickHouse SQL, backtick-quoting any
// identifier that is not a plain name so the marker names the same object
// the statement did.
func (s GrantSecurable) Quoted() string {
	part := func(name string, star bool) string {
		if star {
			return "*"
		}
		if needsQuoting(name) {
			return "`" + strings.ReplaceAll(name, "`", "``") + "`"
		}
		return name
	}
	table := part(s.Table, s.AllTables)
	if s.DB == "" && !s.AnyDatabase {
		return table
	}
	return part(s.DB, s.AnyDatabase) + "." + table
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
			break // recover ON target before handler rejects; generic parse still skipped
		}
		return gp, nil
	default:
		return gp, nil // not a GRANT/REVOKE
	}

	gp.HasOn = tokensHaveSecurableOn(toks)
	gp.HasReplace = tokensHaveReplaceOption(toks)
	if gp.HasOn {
		gp.Securable = tokenSecurable(toks)
	}
	if gp.IsAttach {
		return gp, nil // target recovered; generic parse would fail
	}
	if !gp.HasOn || gp.HasReplace {
		return gp, nil // handler rejects on the flags; generic parse would fail
	}

	cleaned := stripOnCluster(sql, toks)
	node, perr := e.ParseGeneric(cleaned)
	if perr != nil {
		return gp, nil // exotic but valid GRANT; handler rejects as Unsupported (Structured=false)
	}
	flat, derr := decodeGrantNode(node, &gp)
	if derr != nil {
		return gp, nil
	}
	if flat != gp.Securable.Flat() {
		// The generic node names another target than the tokens do (an exotic
		// securable such as a wildcard prefix): leave it unstructured so the
		// handler refuses it rather than grant on either reading.
		return gp, nil
	}
	node, err = setGrantSecurableName(node, gp.Securable.Quoted())
	if err != nil {
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

// tokenSecurable recovers the ON target before generic parsing. This keeps the
// target available even for forms that the generic dialect cannot structure
// (notably WITH REPLACE OPTION), allowing policy handlers to classify a
// protocol-owned target before returning a generic rejection. The target is
// decoded by identity: each identifier token is one name, and only an
// unquoted '*' is a wildcard.
func tokenSecurable(toks []rawToken) GrantSecurable {
	type part struct {
		name string
		star bool // unquoted '*'
		ok   bool
	}
	read := func(tok rawToken) part {
		if tok.TokenType != "QUOTED_IDENTIFIER" && tok.Text == "*" {
			return part{star: true, ok: true}
		}
		if !isNameTok(tok.TokenType) {
			return part{}
		}
		return part{name: tok.Text, ok: true}
	}
	for i := 0; i < len(toks); i++ {
		if toks[i].TokenType != "ON" || i+1 >= len(toks) || toks[i+1].TokenType == "CLUSTER" {
			continue
		}
		j := i + 1
		first := read(toks[j])
		if !first.ok {
			return GrantSecurable{}
		}
		var sec GrantSecurable
		if j+2 < len(toks) && toks[j+1].TokenType == "DOT" {
			if second := read(toks[j+2]); second.ok {
				sec = GrantSecurable{DB: first.name, AnyDatabase: first.star, Table: second.name, AllTables: second.star}
			}
		}
		if sec == (GrantSecurable{}) {
			sec = GrantSecurable{Table: first.name, AllTables: first.star}
		}
		sec.Unrepresentable = strings.Contains(sec.DB, "*") || strings.Contains(sec.Table, "*")
		return sec
	}
	return GrantSecurable{}
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

// decodeGrantNode reads the generic-dialect grant/revoke node into gp and
// returns the node's flat securable name ("db.t"), which the caller only
// compares with the token-decoded identity.
func decodeGrantNode(node AST, gp *GrantParse) (string, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(node, &env); err != nil {
		return "", err
	}
	body, ok := env["grant"]
	if !ok {
		if body, ok = env["revoke"]; !ok {
			return "", fmt.Errorf("engine: not a grant/revoke node")
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
		return "", err
	}
	for _, p := range raw.Privileges {
		gp.Privileges = append(gp.Privileges, GrantPrivilege{Name: p.Name, Columns: p.Columns})
	}
	for _, pr := range raw.Principals {
		gp.Principals = append(gp.Principals, pr.Name.Name)
	}
	gp.GrantOption = raw.GrantOption
	return raw.Securable.Name, nil
}

// setGrantSecurableName replaces the generic grant/revoke node's flat
// securable name with name (the quoted rendering), so the regenerated marker
// names the object the statement did: polyglot's flat name renders `db1.t`
// as db1.t.
func setGrantSecurableName(node AST, name string) (AST, error) {
	var env map[string]any
	if err := json.Unmarshal(node, &env); err != nil {
		return nil, err
	}
	for _, key := range []string{"grant", "revoke"} {
		body, ok := env[key].(map[string]any)
		if !ok {
			continue
		}
		sec, ok := body["securable"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("engine: grant node has no securable")
		}
		sec["name"] = name
		out, err := json.Marshal(env)
		if err != nil {
			return nil, err
		}
		return AST(out), nil
	}
	return nil, fmt.Errorf("engine: not a grant/revoke node")
}
