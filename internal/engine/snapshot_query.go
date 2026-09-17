package engine

import (
	"bytes"
	"encoding/json"
	"math/big"
	"sort"
	"strings"
)

// SnapshotColumn and SnapshotTable are authenticated caller metadata. The engine
// does not establish their authenticity; it binds AST nodes to these identities.
type SnapshotColumn struct {
	Name, Type string
	Ordinary   bool
}
type SnapshotTable struct {
	Database, Name, ID string
	Columns            []SnapshotColumn
}
type SnapshotOptions struct {
	Database    string
	Catalog     []SnapshotTable
	Materialize bool
	Random      []uint64
}
type SnapshotError struct{ Kind, Message string }

func (e *SnapshotError) Error() string      { return e.Message }
func snapshotErr(kind, detail string) error { return &SnapshotError{kind, "snapshot query " + detail} }

var (
	snapshotSyntax          = snapshotErr("UNSUPPORTED", "syntax is outside the closed profile")
	snapshotBinding         = snapshotErr("UNSUPPORTED", "column or relation cannot be bound in its lexical scope")
	snapshotCatalog         = snapshotErr("UNSUPPORTED", "relation is absent from the authenticated catalog")
	snapshotGeneration      = snapshotErr("UNSUPPORTED", "requires ordinary columns with empty generation expressions")
	snapshotType            = snapshotErr("UNSUPPORTED", "type is outside the column profile")
	snapshotOutput          = snapshotErr("UNSUPPORTED", "output type does not exactly match the target")
	snapshotScalar          = snapshotErr("UNSUPPORTED", "scalar output is outside the exact integer profile")
	snapshotLiteral         = snapshotErr("UNSUPPORTED", "literal form is outside the exact integer profile")
	snapshotRange           = snapshotErr("INVALID_INPUT", "integer literal is outside the exact target range")
	snapshotTarget          = snapshotErr("INVALID_INPUT", "target columns do not match the authenticated schema")
	snapshotReserved        = snapshotErr("UNSUPPORTED", "cannot reference the reserved row id")
	snapshotParameter       = snapshotErr("UNSUPPORTED", "parameters are not supported")
	snapshotVolatile        = snapshotErr("UNSUPPORTED", "volatile function is outside the closed profile")
	snapshotMaterialization = snapshotErr("MATERIALIZATION_FAILED", "materialization is incomplete or unsupported")
	snapshotResidual        = snapshotErr("MATERIALIZATION_FAILED", "contains residual volatility")
	snapshotStatement       = snapshotErr("INVALID_INPUT", "requires one recognized statement")
)

type snapshotObject = map[string]any

func sm(v any) snapshotObject { m, _ := v.(map[string]any); return m }
func sa(v any) []any          { a, _ := v.([]any); return a }
func ss(v any) string         { s, _ := v.(string); return s }
func inactive(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case bool:
		return !x
	case []any:
		return len(x) == 0
	case json.Number:
		return x == "0"
	case string:
		return x == ""
	}
	return false
}

// Every container has a closed field set. Future nonempty fields cannot silently
// inherit support, and unknown fields are refused even when their value is null.
func snapshotFields(m snapshotObject, active, inert string) error {
	allowed := map[string]bool{}
	for _, k := range strings.Fields(active) {
		allowed[k] = true
	}
	for _, k := range strings.Fields(inert) {
		allowed[k] = false
	}
	for k, v := range m {
		a, ok := allowed[k]
		if !ok || !a && !inactive(v) {
			return snapshotSyntax
		}
	}
	return nil
}

const comments = "leading_comments trailing_comments left_comments operator_comments pre_alias_comments"

func cleanComments(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, a := range x {
			if strings.HasSuffix(k, "comments") {
				x[k] = []any{}
			} else {
				cleanComments(a)
			}
		}
	case []any:
		for _, a := range x {
			cleanComments(a)
		}
	}
}
func snapshotIdentifier(v any) (string, error) {
	m := sm(v)
	if m == nil {
		return "", snapshotSyntax
	}
	if err := snapshotFields(m, "name quoted span trailing_comments", ""); err != nil {
		return "", err
	}
	s := ss(m["name"])
	if s == "" || strings.ContainsRune(s, 0) {
		return "", snapshotSyntax
	}
	if s == "_hg_row_id" {
		return "", snapshotReserved
	}
	if m["quoted"] != true && strings.HasPrefix(s, "{") {
		return "", snapshotParameter
	}
	return s, nil
}
func sid(s string) snapshotObject {
	q := s == ""
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			q = true
		}
	}
	return snapshotObject{"name": s, "quoted": q, "trailing_comments": []any{}}
}

type snapshotValue struct {
	typ, name        string
	literal          *big.Int
	scalar, nullable bool
	node             snapshotObject
}
type snapshotRelation struct {
	columns   []snapshotValue
	bases     map[string]bool
	nodes     []*snapshotBoundRelation
	qualifier string
	used      map[*snapshotDefinition]bool
	bound     *snapshotBoundRelation
}
type snapshotBoundRelation struct {
	id              string
	node            snapshotObject
	columns         []snapshotObject
	otherQualifiers map[string]bool
}
type snapshotQuery struct {
	columns []snapshotValue
	bases   map[string]bool
	nodes   []*snapshotBoundRelation
	leaves  []snapshotObject
	used    map[*snapshotDefinition]bool
}
type snapshotDefinition struct {
	node  snapshotObject
	query *snapshotQuery
}
type snapshotWith struct {
	node snapshotObject
	defs []*snapshotDefinition
}
type snapshotScope map[string]*snapshotDefinition

func cloneSnapshotScope(s snapshotScope) snapshotScope {
	c := snapshotScope{}
	for k, v := range s {
		c[k] = v
	}
	return c
}
func mergeSnapshot(q *snapshotQuery, r *snapshotQuery) {
	for id := range r.bases {
		q.bases[id] = true
	}
	q.nodes = append(q.nodes, r.nodes...)
	for d := range r.used {
		q.used[d] = true
	}
}

type SnapshotPlan struct {
	Ordinary               bool
	SQL, TargetID          string
	TargetColumns, ReadIDs []string
	root, body             snapshotObject
	target                 SnapshotTable
	query                  *snapshotQuery
	order                  []int
	withs                  []*snapshotWith
}
type snapshotAnalyzer struct {
	opts    SnapshotOptions
	catalog map[[2]string]SnapshotTable
	ids     map[string]SnapshotTable
	random  int
	allCTEs map[string]bool
	withs   []*snapshotWith
}

// AnalyzeSnapshot uses the actual parser and a separate closed typed scope graph.
// It never invokes the ordinary rewrite or materialization fail-open paths.
func AnalyzeSnapshot(e Engine, sql string, opts SnapshotOptions) (*SnapshotPlan, error) {
	ast, err := e.ParseOne(sql)
	if err != nil {
		return nil, snapshotStatement
	}
	var root snapshotObject
	d := json.NewDecoder(bytes.NewReader(ast))
	d.UseNumber()
	if err = d.Decode(&root); err != nil || len(root) != 1 {
		return nil, snapshotStatement
	}
	if root["command"] != nil {
		generic, ge := e.ParseGeneric(sql)
		if ge == nil {
			var typed snapshotObject
			if json.Unmarshal(generic, &typed) == nil && len(typed) == 1 {
				if u := sm(typed["use"]); u != nil {
					if snapshotFields(u, "this", "kind") == nil {
						if _, er := snapshotIdentifier(u["this"]); er == nil {
							return &SnapshotPlan{Ordinary: true}, nil
						}
					}
				}
			}
		}
		return nil, snapshotStatement
	}
	ins := sm(root["insert"])
	if ins == nil {
		for _, k := range []string{"select", "union", "use", "create", "create_table", "update", "delete", "drop", "alter", "grant", "revoke", "truncate", "describe", "show"} {
			if _, ok := root[k]; ok {
				return &SnapshotPlan{Ordinary: true}, nil
			}
		}
		return nil, snapshotStatement
	}
	if ins["query"] == nil && len(sa(ins["values"])) > 0 {
		return &SnapshotPlan{Ordinary: true}, nil
	}
	if err = snapshotFields(ins, "table columns query with", "values overwrite partition directory returning output on_conflict leading_comments if_exists ignore source_alias alias alias_explicit_as default_values by_name is_replace replace_where source"); err != nil {
		return nil, err
	}
	a := &snapshotAnalyzer{opts: opts, catalog: map[[2]string]SnapshotTable{}, ids: map[string]SnapshotTable{}, allCTEs: map[string]bool{}}
	for _, t := range opts.Catalog {
		key := [2]string{t.Database, t.Name}
		if t.ID == "" || t.Database == "" || t.Name == "" {
			return nil, snapshotCatalog
		}
		if _, ok := a.catalog[key]; ok {
			return nil, snapshotCatalog
		}
		if _, ok := a.ids[t.ID]; ok {
			return nil, snapshotCatalog
		}
		a.catalog[key] = t
		a.ids[t.ID] = t
	}
	// Token/AST fidelity is checked before any normalization or replacement. The
	// parsed numeric string must match the original lexer spelling without floats.
	tokens, err := e.Tokenize(sql)
	if err != nil {
		return nil, snapshotStatement
	}
	if err = bindSnapshotLexemes(root, tokens, sql); err != nil {
		return nil, err
	}
	used, er := materializeSnapshotAST(root, opts)
	if er != nil {
		return nil, er
	}
	a.random = used
	cleanComments(root)
	target, err := a.base(sm(ins["table"]))
	if err != nil {
		return nil, err
	}
	if len(target.Columns) == 0 {
		return nil, snapshotTarget
	}
	if err = eligibleSnapshotTable(target, false); err != nil {
		return nil, err
	}
	names := []string{}
	for _, c := range sa(ins["columns"]) {
		s, er := snapshotIdentifier(c)
		if er != nil {
			return nil, er
		}
		names = append(names, s)
	}
	if len(names) == 0 {
		for _, c := range target.Columns {
			names = append(names, c.Name)
		}
	}
	if len(names) != len(target.Columns) {
		return nil, snapshotTarget
	}
	seen := map[string]bool{}
	order := make([]int, len(names))
	for i, c := range target.Columns {
		j := -1
		for n, s := range names {
			if s == c.Name {
				if j >= 0 {
					return nil, snapshotTarget
				}
				j = n
			}
		}
		if j < 0 || seen[c.Name] {
			return nil, snapshotTarget
		}
		seen[c.Name] = true
		order[i] = j
	}
	scope, err := a.with(ins["with"], snapshotScope{})
	if err != nil {
		return nil, err
	}
	body := sm(ins["query"])
	q, err := a.query(body, scope)
	if err != nil {
		return nil, err
	}
	if len(q.columns) != len(names) {
		return nil, snapshotTarget
	}

	for _, c := range target.Columns {
		if !snapshotColumnType(c.Type) && !strings.HasPrefix(c.Type, "FixedString(") && !strings.HasPrefix(c.Type, "DateTime(") && !strings.HasPrefix(c.Type, "DateTime64(") {
			return nil, snapshotType
		}
	}
	for i, c := range target.Columns {
		v := q.columns[order[i]]
		if err = checkSnapshotOutput(v, c.Type, len(q.leaves) == 1); err != nil {
			return nil, err
		}
	}
	if err = eligibleSnapshotTable(target, true); err != nil {
		return nil, err
	}
	ids := []string{}
	for id := range q.bases {
		if err = eligibleSnapshotTable(a.ids[id], true); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if a.random != len(opts.Random) {
		return nil, snapshotMaterialization
	}
	snapshotNormalizeIdentifiers(e, root)
	b, err := json.Marshal(root)
	if err != nil {
		return nil, snapshotSyntax
	}
	logical, err := generateSnapshot(e, b)
	if err != nil {
		return nil, snapshotSyntax
	}
	return &SnapshotPlan{SQL: logical, TargetID: target.ID, TargetColumns: names, ReadIDs: ids, root: root, body: body, target: target, query: q, order: order, withs: a.withs}, nil
}
func (a *snapshotAnalyzer) base(t snapshotObject) (SnapshotTable, error) {
	if t == nil {
		return SnapshotTable{}, snapshotSyntax
	}
	if err := snapshotFields(t, "name schema alias alias_explicit_as", "catalog column_aliases trailing_comments when only final_ hints"); err != nil {
		return SnapshotTable{}, err
	}
	name, err := snapshotIdentifier(t["name"])
	if err != nil {
		return SnapshotTable{}, err
	}
	db := a.opts.Database
	if t["schema"] != nil {
		db, err = snapshotIdentifier(t["schema"])
		if err != nil {
			return SnapshotTable{}, err
		}
	}
	v, ok := a.catalog[[2]string{db, name}]
	if !ok {
		return v, snapshotCatalog
	}
	return v, nil
}
func eligibleSnapshotTable(t SnapshotTable, types bool) error {
	seen := map[string]bool{}
	for _, c := range t.Columns {
		if !c.Ordinary {
			return snapshotGeneration
		}
		if c.Name == "_hg_row_id" {
			return snapshotReserved
		}
		if c.Name == "" || seen[c.Name] {
			return snapshotTarget
		}
		seen[c.Name] = true
		if types && !snapshotColumnType(c.Type) {
			return snapshotType
		}
	}
	return nil
}
func snapshotInteger(t string) (int, bool) {
	switch t {
	case "Int8":
		return 8, true
	case "Int16":
		return 16, true
	case "Int32":
		return 32, true
	case "Int64":
		return 64, true
	case "UInt8":
		return 8, false
	case "UInt16":
		return 16, false
	case "UInt32":
		return 32, false
	case "UInt64":
		return 64, false
	}
	return 0, false
}
func snapshotColumnType(t string) bool {
	if n, _ := snapshotInteger(t); n != 0 {
		return true
	}
	switch t {
	case "String", "Bool", "Float32", "Float64", "Date", "DateTime", "FixedString(32)":
		return true
	}
	return snapshotTemporalType(t)
}
func inSnapshotRange(n *big.Int, t string) bool {
	bits, signed := snapshotInteger(t)
	if bits == 0 {
		return false
	}
	max := new(big.Int).Lsh(big.NewInt(1), uint(bits))
	min := new(big.Int)
	if signed {
		max.Rsh(max, 1)
		min.Neg(new(big.Int).Set(max))
	}
	max.Sub(max, big.NewInt(1))
	return n.Cmp(min) >= 0 && n.Cmp(max) <= 0
}
func checkSnapshotOutput(v snapshotValue, t string, direct bool) error {
	if v.literal != nil && !v.nullable && direct {
		if !snapshotColumnType(t) {
			return snapshotType
		}
		if n, _ := snapshotInteger(t); n == 0 {
			return snapshotOutput
		}
		if !inSnapshotRange(v.literal, t) {
			return snapshotRange
		}
		return nil
	}
	if v.nullable {
		if v.scalar && direct {
			if v.typ != t {
				if v.literal != nil {
					return snapshotOutput
				}
				return snapshotScalar
			}
			if n, _ := snapshotInteger(t); n == 0 {
				return snapshotScalar
			}
			return nil
		}
		return snapshotScalar
	}
	if v.typ != t {
		if v.typ == "String" {
			if n, _ := snapshotInteger(t); n != 0 && v.node["literal"] != nil {
				return snapshotLiteral
			}
		}
		return snapshotOutput
	}
	return nil
}
func (a *snapshotAnalyzer) with(raw any, outer snapshotScope) (snapshotScope, error) {
	scope := cloneSnapshotScope(outer)
	if raw == nil {
		return scope, nil
	}
	w := sm(raw)
	if err := snapshotFields(w, "ctes", "recursive leading_comments"); err != nil {
		return nil, err
	}
	for _, v := range sa(w["ctes"]) {
		c := sm(v)
		s, err := snapshotIdentifier(c["alias"])
		if err != nil {
			return nil, err
		}
		a.allCTEs[s] = true
	}
	clause := &snapshotWith{node: w}
	a.withs = append(a.withs, clause)
	local := map[string]bool{}
	for _, v := range sa(w["ctes"]) {
		c := sm(v)
		if err := snapshotFields(c, "alias this alias_first", "columns materialized"); err != nil {
			return nil, err
		}
		if c["alias_first"] != true {
			return nil, snapshotSyntax
		}
		name, err := snapshotIdentifier(c["alias"])
		if err != nil {
			return nil, err
		}
		if local[name] {
			return nil, snapshotBinding
		}
		q, err := a.query(sm(c["this"]), scope)
		if err != nil {
			return nil, err
		}
		def := &snapshotDefinition{node: c, query: q}
		clause.defs = append(clause.defs, def)
		scope[name] = def
		local[name] = true
	}
	return scope, nil
}
func (a *snapshotAnalyzer) query(n snapshotObject, outer snapshotScope) (*snapshotQuery, error) {
	if len(n) != 1 {
		return nil, snapshotSyntax
	}
	q := &snapshotQuery{bases: map[string]bool{}, used: map[*snapshotDefinition]bool{}}
	if u := sm(n["union"]); u != nil {
		if err := snapshotFields(u, "left right all with", "distinct order_by limit offset by_name corresponding strict"); err != nil || u["all"] != true {
			return nil, snapshotSyntax
		}
		scope, err := a.with(u["with"], outer)
		if err != nil {
			return nil, err
		}
		l, err := a.query(sm(u["left"]), scope)
		if err != nil {
			return nil, err
		}
		r, err := a.query(sm(u["right"]), scope)
		if err != nil {
			return nil, err
		}
		if len(l.columns) != len(r.columns) {
			return nil, snapshotTarget
		}
		q.columns = append([]snapshotValue(nil), l.columns...)
		for i, v := range q.columns {
			if v.nullable || r.columns[i].nullable {
				return nil, snapshotScalar
			}
			if v.typ != r.columns[i].typ {
				return nil, snapshotOutput
			}
			q.columns[i].literal = nil
			q.columns[i].scalar = false
		}
		mergeSnapshot(q, l)
		mergeSnapshot(q, r)
		q.leaves = append(l.leaves, r.leaves...)
		return q, nil
	}
	s := sm(n["select"])
	if s == nil {
		return nil, snapshotSyntax
	}
	if err := snapshotFields(s, "expressions from joins where_clause with", "lateral_views group_by having qualify order_by distribute_by cluster_by sort_by limit offset fetch distinct distinct_on top sample windows hint connect into locks leading_comments"); err != nil {
		return nil, err
	}
	scope, err := a.with(s["with"], outer)
	if err != nil {
		return nil, err
	}
	rels := []snapshotRelation{}
	if from := sm(s["from"]); from != nil {
		if err := snapshotFields(from, "expressions", ""); err != nil {
			return nil, err
		}
		if len(sa(from["expressions"])) != 1 {
			return nil, snapshotSyntax
		}
		r, err := a.relation(sm(sa(from["expressions"])[0]), scope)
		if err != nil {
			return nil, err
		}
		rels = append(rels, r)
	}
	for _, jv := range sa(s["joins"]) {
		j := sm(jv)
		if err := snapshotFields(j, "this on kind join_hint use_inner_keyword", "using side method global_ use_outer_keyword deferred_condition nesting_group directed"); err != nil {
			return nil, err
		}
		if j["kind"] != "Inner" || j["join_hint"] != "ALL" || len(rels) == 0 {
			return nil, snapshotSyntax
		}
		r, err := a.relation(sm(j["this"]), scope)
		if err != nil {
			return nil, err
		}
		rels = append(rels, r)
		on := sm(j["on"])
		if on["eq"] == nil {
			return nil, snapshotSyntax
		}
		_, err = a.expr(on, rels, scope, q)
		if err != nil {
			return nil, err
		}
	}
	quals := map[string]bool{}
	for _, r := range rels {
		if r.qualifier != "" && quals[r.qualifier] {
			return nil, snapshotBinding
		}
		quals[r.qualifier] = true
		for id := range r.bases {
			q.bases[id] = true
		}
		q.nodes = append(q.nodes, r.nodes...)
		for d := range r.used {
			q.used[d] = true
		}
	}
	for _, r := range rels {
		if r.bound != nil {
			r.bound.otherQualifiers = map[string]bool{}
			for _, other := range rels {
				if other.qualifier != r.qualifier {
					r.bound.otherQualifiers[other.qualifier] = true
				}
			}
		}
	}
	for _, v := range sa(s["expressions"]) {
		x, err := a.expr(sm(v), rels, scope, q)
		if err != nil {
			return nil, err
		}
		q.columns = append(q.columns, x)
	}
	if len(q.columns) == 0 {
		return nil, snapshotSyntax
	}
	if w := sm(s["where_clause"]); w != nil {
		if err := snapshotFields(w, "this", comments); err != nil {
			return nil, err
		}
		x, err := a.expr(sm(w["this"]), rels, scope, q)
		if err != nil {
			return nil, err
		}
		if n, _ := snapshotInteger(x.typ); n == 0 && x.typ != "Bool" {
			return nil, snapshotSyntax
		}
	}
	q.leaves = []snapshotObject{s}
	return q, nil
}
func (a *snapshotAnalyzer) relation(n snapshotObject, scope snapshotScope) (snapshotRelation, error) {
	r := snapshotRelation{bases: map[string]bool{}, used: map[*snapshotDefinition]bool{}}
	if len(n) != 1 {
		return r, snapshotSyntax
	}
	if t := sm(n["table"]); t != nil {
		if err := snapshotFields(t, "name schema alias alias_explicit_as", "catalog column_aliases trailing_comments when only final_ hints"); err != nil {
			return r, err
		}
		name, err := snapshotIdentifier(t["name"])
		if err != nil {
			return r, err
		}
		r.qualifier = name
		if t["alias"] != nil {
			r.qualifier, err = snapshotIdentifier(t["alias"])
			if err != nil {
				return r, err
			}
		}
		if t["schema"] == nil {
			if c := scope[name]; c != nil {
				r.columns = derivedSnapshotColumns(c.query.columns)
				r.bases = c.query.bases
				r.nodes = c.query.nodes
				r.used[c] = true
				for d := range c.query.used {
					r.used[d] = true
				}
				return r, nil
			}
			if _, baseExists := a.catalog[[2]string{a.opts.Database, name}]; a.allCTEs[name] && !baseExists {
				return r, snapshotBinding
			}
		}
		base, err := a.base(t)
		if err != nil {
			return r, err
		}
		r.bases[base.ID] = true
		r.bound = &snapshotBoundRelation{id: base.ID, node: t}
		r.nodes = []*snapshotBoundRelation{r.bound}
		for _, c := range base.Columns {
			r.columns = append(r.columns, snapshotValue{typ: c.Type, name: c.Name})
		}
		return r, nil
	}
	if s := sm(n["subquery"]); s != nil {
		if err := snapshotFields(s, "this alias alias_explicit_as", "column_aliases lateral modifiers_inside order_by limit offset sample trailing_comments"); err != nil {
			return r, err
		}
		q, err := a.query(sm(s["this"]), scope)
		if err != nil {
			return r, err
		}
		r.columns = derivedSnapshotColumns(q.columns)
		r.bases = q.bases
		r.nodes = q.nodes
		r.used = q.used
		if s["alias"] != nil {
			r.qualifier, err = snapshotIdentifier(s["alias"])
			if err != nil {
				return r, err
			}
		}
		return r, nil
	}
	return r, snapshotSyntax
}
func derivedSnapshotColumns(in []snapshotValue) []snapshotValue {
	out := append([]snapshotValue(nil), in...)
	for i := range out {
		out[i].literal = nil
		out[i].scalar = false
	}
	return out
}

func (a *snapshotAnalyzer) expr(n snapshotObject, rels []snapshotRelation, scope snapshotScope, q *snapshotQuery) (snapshotValue, error) {
	v := snapshotValue{node: n}
	if len(n) != 1 {
		return v, snapshotSyntax
	}
	if l := sm(n["literal"]); l != nil {
		if err := snapshotFields(l, "literal_type value", comments); err != nil {
			return v, err
		}
		switch ss(l["literal_type"]) {
		case "number":
			x, ok := new(big.Int).SetString(ss(l["value"]), 10)
			if !ok || !snapshotDecimal(ss(l["value"])) {
				return v, snapshotLiteral
			}
			if !inSnapshotRange(x, "UInt64") {
				return v, snapshotRange
			}
			v.literal = x
			v.typ = smallestSnapshotInteger(x)
			l["value"] = x.String()
			return v, nil
		case "string":
			v.typ = "String"
			return v, nil
		case "dollar_string":
			s := ss(l["value"])
			_, body, ok := strings.Cut(s, "\x00")
			if !ok {
				body = s
			}
			l["literal_type"] = "string"
			l["value"] = body
			v.typ = "String"
			return v, nil
		default:
			return v, snapshotLiteral
		}
	}
	if c := sm(n["column"]); c != nil {
		if err := snapshotFields(c, "name table span", "join_mark trailing_comments"); err != nil {
			return v, err
		}
		name, err := snapshotIdentifier(c["name"])
		if err != nil {
			return v, err
		}
		qual := ""
		if c["table"] != nil {
			qual, err = snapshotIdentifier(c["table"])
			if err != nil {
				return v, err
			}
		}
		found := 0
		for _, r := range rels {
			if qual != "" && r.qualifier != qual {
				continue
			}
			for _, x := range r.columns {
				if x.name == name {
					v = x
					if qual != "" && r.bound != nil {
						r.bound.columns = append(r.bound.columns, c)
					}
					v.node = n
					v.literal = nil
					v.scalar = false
					found++
				}
			}
		}
		if found != 1 {
			if sm(c["name"])["quoted"] != true && strings.HasPrefix(name, "0b") {
				return v, snapshotLiteral
			}
			return v, snapshotBinding
		}
		v.name = name
		return v, nil
	}
	if p := sm(n["paren"]); p != nil {
		if err := snapshotFields(p, "this", comments); err != nil {
			return v, err
		}
		x, err := a.expr(sm(p["this"]), rels, scope, q)
		if err != nil {
			return v, err
		}
		if x.literal != nil || x.scalar {
			replaceMap(n, sm(p["this"]))
			x.node = n
		}
		return x, nil
	}
	if p := sm(n["alias"]); p != nil {
		if err := snapshotFields(p, "this alias alias_explicit_as alias_keyword", "column_aliases pre_alias_comments trailing_comments"); err != nil {
			return v, err
		}
		x, err := a.expr(sm(p["this"]), rels, scope, q)
		if err != nil {
			return v, err
		}
		name, err := snapshotIdentifier(p["alias"])
		if err != nil {
			return v, err
		}
		// ClickHouse substitutes projection aliases throughout their SELECT. Refuse
		// a shadowing definition whose expression is not the same bound column.
		for _, r := range rels {
			for _, c := range r.columns {
				if c.name == name {
					inner := sm(p["this"])
					col := sm(inner["column"])
					if col == nil || ss(sm(col["name"])["name"]) != name || (col["table"] != nil && ss(sm(col["table"])["name"]) != r.qualifier) {
						return v, snapshotBinding
					}
				}
			}
		}

		x.name = name
		x.node = n
		return x, nil
	}
	if neg := sm(n["neg"]); neg != nil {
		if err := snapshotFields(neg, "this", comments); err != nil {
			return v, err
		}
		inner := sm(neg["this"])
		for inner["paren"] != nil {
			inner = sm(sm(inner["paren"])["this"])
		}
		lit := sm(inner["literal"])
		if lit == nil && !snapshotOnlyLiterals(inner) {
			return v, snapshotSyntax
		}
		if lit == nil || lit["literal_type"] != "number" || !snapshotDecimal(ss(lit["value"])) {
			return v, snapshotLiteral
		}
		x, ok := new(big.Int).SetString(ss(lit["value"]), 10)
		if !ok {
			return v, snapshotLiteral
		}
		x.Neg(x)
		if !inSnapshotRange(x, "Int64") {
			return v, snapshotRange
		}
		v.literal = x
		v.typ = smallestSnapshotInteger(x)
		replaceMap(n, snapshotNumber(x.String()))
		return v, nil
	}
	if s := sm(n["subquery"]); s != nil {
		if err := snapshotFields(s, "this", "alias alias_explicit_as column_aliases lateral modifiers_inside order_by limit offset sample trailing_comments"); err != nil {
			return v, err
		}
		inner, err := a.query(sm(s["this"]), scope)
		if err != nil {
			return v, err
		}
		if len(inner.columns) != 1 {
			return v, snapshotScalar
		}
		mergeSnapshot(q, inner)
		v = inner.columns[0]
		v.node = n
		v.scalar = true
		v.nullable = true
		v.name = ""
		return v, nil
	}
	for _, op := range []string{"eq", "neq", "gt", "gte", "lt", "lte", "and", "or"} {
		if b := sm(n[op]); b != nil {
			if err := snapshotFields(b, "left right", comments); err != nil {
				return v, err
			}
			l, err := a.expr(sm(b["left"]), rels, scope, q)
			if err != nil {
				return v, err
			}
			if ln, _ := snapshotInteger(l.typ); ln == 0 {
				return v, snapshotSyntax
			}
			r, err := a.expr(sm(b["right"]), rels, scope, q)
			if err != nil {
				if err == snapshotLiteral {
					return v, snapshotSyntax
				}
				return v, err
			}
			if ln, _ := snapshotInteger(l.typ); ln == 0 {
				return v, snapshotSyntax
			}
			if rn, _ := snapshotInteger(r.typ); rn == 0 {
				return v, snapshotSyntax
			}
			v.typ = "UInt8"
			v.nullable = l.nullable || r.nullable
			return v, nil
		}
	}
	if b := sm(n["not"]); b != nil {
		if err := snapshotFields(b, "this", comments); err != nil {
			return v, err
		}
		x, err := a.expr(sm(b["this"]), rels, scope, q)
		if err != nil {
			return v, err
		}
		if n, _ := snapshotInteger(x.typ); n == 0 {
			return v, snapshotSyntax
		}
		v.typ = "UInt8"
		v.nullable = x.nullable
		return v, nil
	}
	if b := sm(n["in"]); b != nil {
		if err := snapshotFields(b, "this query", "expressions not global"); err != nil {
			return v, err
		}
		x, err := a.expr(sm(b["this"]), rels, scope, q)
		if err != nil {
			return v, err
		}
		inner, err := a.query(sm(b["query"]), scope)
		if err != nil {
			return v, err
		}
		if len(inner.columns) != 1 {
			return v, snapshotSyntax
		}
		if n, _ := snapshotInteger(x.typ); n == 0 {
			return v, snapshotSyntax
		}
		if n, _ := snapshotInteger(inner.columns[0].typ); n == 0 {
			return v, snapshotSyntax
		}
		mergeSnapshot(q, inner)
		v.typ = "UInt8"
		return v, nil
	}
	name := ""
	if r, ok := n["rand"]; ok {
		name = "rand"
		if r != nil {
			if err := snapshotFields(sm(r), "", "seed lower upper"); err != nil {
				return v, snapshotVolatile
			}
		}
	} else if f := sm(n["function"]); f != nil {
		name = strings.ToLower(ss(f["name"]))
		if name == "rand32" || name == "rand64" {
			if err := snapshotFields(f, "name", "args distinct trailing_comments use_bracket_syntax no_parens quoted"); err != nil {
				return v, snapshotVolatile
			}
		} else if snapshotVolatileName(name) {
			return v, snapshotVolatile
		} else {
			return v, snapshotSyntax
		}
	}
	if name != "" {
		// The single lexical prepass owns all consumption. No later scope walk
		// can consume a second pool value for a referenced definition.
		return v, snapshotResidual
	}

	if u := sm(n["union"]); u != nil { // Some scalar-first UNIONs are expression nodes in the pinned AST.
		if left := sm(u["left"]); left["subquery"] != nil {
			if _, er := a.expr(left, rels, scope, q); er != nil {
				return v, er
			}
			if _, er := a.query(sm(u["right"]), scope); er != nil {
				return v, er
			}
			return v, snapshotScalar
		}
	}
	for k := range n {
		if snapshotVolatileName(k) {
			return v, snapshotVolatile
		}
		if k == "placeholder" || k == "parameter" {
			return v, snapshotParameter
		}
	}
	if snapshotOnlyLiterals(n) {
		return v, snapshotLiteral
	}
	return v, snapshotSyntax
}
func snapshotVolatileName(s string) bool {
	switch strings.ToLower(s) {
	case "rand", "random", "rand32", "rand64", "randcanonical", "randconstant", "randomstring", "now", "now64", "today", "yesterday", "current_timestamp", "currenttimestamp", "current_date", "currentdate", "localtime", "localtimestamp", "utc_timestamp", "generateuuidv4", "generateuuidv7", "nowinblock":
		return true
	}
	return false
}
func snapshotDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func smallestSnapshotInteger(n *big.Int) string {
	prefix := "UInt"
	if n.Sign() < 0 {
		prefix = "Int"
	}
	for _, bits := range []string{"8", "16", "32", "64"} {
		if inSnapshotRange(n, prefix+bits) {
			return prefix + bits
		}
	}
	return ""
}
func snapshotNumber(s string) snapshotObject {
	if strings.HasPrefix(s, "-") {
		return snapshotObject{"neg": snapshotObject{"this": snapshotNumber(s[1:])}}
	}
	return snapshotObject{"literal": snapshotObject{"literal_type": "number", "value": s}}
}
func snapshotOnlyLiterals(n snapshotObject) bool {
	if n["literal"] != nil {
		return true
	}
	if len(n) != 1 {
		return false
	}
	for k, v := range n {
		switch k {
		case "add", "sub", "mul", "div", "mod", "neg", "paren":
			b := sm(v)
			for key, c := range b {
				if key == "this" || key == "left" || key == "right" {
					if !snapshotOnlyLiterals(sm(c)) {
						return false
					}
				}
			}
			return true
		}
	}
	return false
}

// Prepare rewrites only the relation objects bound in the referenced graph and
// reorders each final projection into authenticated target-schema order.
func (p *SnapshotPlan) Prepare(e Engine, bindings map[string][2]string) (string, error) {
	if p.Ordinary {
		return "", snapshotSyntax
	}
	for _, w := range p.withs {
		kept := []any{}
		for _, d := range w.defs {
			if p.query.used[d] {
				kept = append(kept, d.node)
			}
		}
		w.node["ctes"] = kept
	}
	pruneSnapshotWiths(p.root)
	for _, r := range p.query.nodes {
		b, ok := bindings[r.id]
		if !ok {
			return "", snapshotCatalog
		}
		if r.node["alias"] == nil && (len(r.columns) > 0 || r.otherQualifiers[b[1]]) {
			r.node["alias"] = r.node["name"]
			r.node["alias_explicit_as"] = true
		}
		r.node["schema"] = sid(b[0])
		r.node["name"] = sid(b[1])
		if r.node["alias"] == nil {
			for _, c := range r.columns {
				c["table"] = sid(b[1])
			}
		}
	}
	for _, leaf := range p.query.leaves {
		old := sa(leaf["expressions"])
		ordered := make([]any, len(p.order))
		for i, j := range p.order {
			n := sm(old[j])
			v := p.query.columns[j]
			if v.literal != nil && !v.nullable && len(p.query.leaves) == 1 {
				n = wrapSnapshotOutput(n, litStr(v.literal.String()), p.target.Columns[i].Type)
			} else if v.scalar && len(p.query.leaves) == 1 {
				n = wrapSnapshotOutput(n, nil, p.target.Columns[i].Type)
			}
			ordered[i] = n
		}
		leaf["expressions"] = ordered
	}
	if w := sm(sm(p.root["insert"])["with"]); w != nil {
		for _, kind := range []string{"select", "union"} {
			if b := sm(p.body[kind]); b != nil {
				if sm(b["with"]) != nil {
					// INSERT and query WITHs are distinct lexical scopes. Keep them
					// nested, including sequential dependencies on shadowed names.
					wrapper, err := e.ParseOne("SELECT * FROM (SELECT 1) AS snapshot_output")
					if err != nil {
						return "", err
					}
					var outer snapshotObject
					if err = json.Unmarshal(wrapper, &outer); err != nil {
						return "", err
					}
					selectNode := sm(outer["select"])
					subquery := sm(sm(sa(sm(selectNode["from"])["expressions"])[0])["subquery"])
					subquery["this"] = p.body
					selectNode["with"] = w
					p.body = outer
				} else {
					b["with"] = w
				}
				break
			}
		}
	}
	b, err := json.Marshal(p.body)
	if err != nil {
		return "", err
	}
	return generateSnapshot(e, b)
}
func wrapSnapshotOutput(n snapshotObject, value snapshotObject, t string) snapshotObject {
	if al := sm(n["alias"]); al != nil {
		al["this"] = wrapSnapshotOutput(sm(al["this"]), value, t)
		return n
	}
	if value == nil {
		value = n
	}
	return functionCall("accurateCast", value, litStr(t))
}

// This independent column-profile-v1 check follows the HG authority's fixed
// spellings and temporal grammar; it intentionally imports no HG packages.
func snapshotTemporalType(t string) bool {
	if strings.HasPrefix(t, "DateTime('") && strings.HasSuffix(t, "')") {
		return snapshotTimezone(t[10 : len(t)-2])
	}
	if !strings.HasPrefix(t, "DateTime64(") || !strings.HasSuffix(t, ")") {
		return false
	}
	s := t[11 : len(t)-1]
	parts := strings.SplitN(s, ", ", 2)
	if len(parts[0]) != 1 || parts[0][0] < '0' || parts[0][0] > '9' {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	z := parts[1]
	return len(z) >= 2 && z[0] == '\'' && z[len(z)-1] == '\'' && snapshotTimezone(z[1:len(z)-1])
}
func snapshotTimezone(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/_+-", r)) {
			return false
		}
	}
	return true
}

func pruneSnapshotWiths(v any) {
	switch x := v.(type) {
	case map[string]any:
		if w := sm(x["with"]); w != nil && len(sa(w["ctes"])) == 0 {
			x["with"] = nil
		}
		for _, c := range x {
			pruneSnapshotWiths(c)
		}
	case []any:
		for _, c := range x {
			pruneSnapshotWiths(c)
		}
	}
}
