package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const DefaultMaterializationProfileID = "sentio-p1-nondet-v1"

var (
	ErrMaterializationInputMissing = errors.New("engine: materialization input missing")
	ErrMaterializationUnsupported  = errors.New("engine: materialization unsupported")
	ErrMaterializationInvalidInput = errors.New("engine: materialization invalid input")
)

type MaterializationInputs struct {
	NowUnixNS           *int64
	RandomUint64Values  []uint64
	RandomFloat64Values []float64
	UUIDValues          []string
}

type MaterializationResult struct {
	AST          AST
	Replacements []MaterializedReplacement
}

type MaterializedReplacement struct {
	FunctionName string
	Ordinal      uint32
	LiteralSQL   string
	ValueType    string
}

func MaterializeNonDeterminism(ast AST, inputs MaterializationInputs) (MaterializationResult, error) {
	var root map[string]any
	if err := json.Unmarshal(ast, &root); err != nil {
		return MaterializationResult{}, fmt.Errorf("engine: decode materialization AST: %w", err)
	}

	m := materializer{inputs: inputs}
	if err := m.walk(root); err != nil {
		return MaterializationResult{}, err
	}
	if len(m.replacements) == 0 {
		return MaterializationResult{AST: ast}, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return MaterializationResult{}, fmt.Errorf("engine: encode materialization AST: %w", err)
	}
	return MaterializationResult{
		AST:          AST(out),
		Replacements: m.replacements,
	}, nil
}

type materializer struct {
	inputs           MaterializationInputs
	randomIndex      int
	randomFloatIndex int
	uuidIndex        int
	replacements     []MaterializedReplacement
}

type materializeValueFunc func(string) (map[string]any, string, string, error)

func (m *materializer) walk(node any) error {
	switch n := node.(type) {
	case map[string]any:
		if randNode, ok := n["rand"]; ok {
			replacement, record, err := m.materializeRandNode("rand", randNode)
			if err != nil {
				return err
			}
			clear(n)
			for k, v := range replacement {
				n[k] = v
			}
			m.replacements = append(m.replacements, record)
			return nil
		}
		if randomNode, ok := n["random"]; ok {
			replacement, record, err := m.materializeRandNode("random", randomNode)
			if err != nil {
				return err
			}
			replaceMap(n, replacement)
			m.replacements = append(m.replacements, record)
			return nil
		}
		if currentDateNode, ok := n["current_date"]; ok {
			replacement, record, err := m.materializeSpecialNode("current_date", currentDateNode, m.materializeToday)
			if err != nil {
				return err
			}
			replaceMap(n, replacement)
			m.replacements = append(m.replacements, record)
			return nil
		}
		if currentTimestampNode, ok := n["current_timestamp"]; ok {
			replacement, record, err := m.materializeSpecialNode("current_timestamp", currentTimestampNode, m.materializeNow)
			if err != nil {
				return err
			}
			replaceMap(n, replacement)
			m.replacements = append(m.replacements, record)
			return nil
		}
		if utcTimestampNode, ok := n["utc_timestamp"]; ok {
			replacement, record, err := m.materializeSpecialNode("utc_timestamp", utcTimestampNode, m.materializeUTCTimestamp)
			if err != nil {
				return err
			}
			replaceMap(n, replacement)
			m.replacements = append(m.replacements, record)
			return nil
		}
		if column, ok := n["column"].(map[string]any); ok {
			replacement, record, handled, err := m.materializeColumnPseudoFunction(column)
			if err != nil {
				return err
			}
			if handled {
				replaceMap(n, replacement)
				m.replacements = append(m.replacements, record)
				return nil
			}
		}
		if fn, ok := n["function"].(map[string]any); ok {
			replacement, record, handled, err := m.materializeFunction(fn)
			if err != nil {
				return err
			}
			if handled {
				replaceMap(n, replacement)
				m.replacements = append(m.replacements, record)
				return nil
			}
		}

		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := m.walk(n[k]); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range n {
			if err := m.walk(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *materializer) materializeRandNode(name string, node any) (map[string]any, MaterializedReplacement, error) {
	if randNodeHasArguments(node) {
		return nil, MaterializedReplacement{}, fmt.Errorf("%s with arguments: %w", name, ErrMaterializationUnsupported)
	}
	replacement, literalSQL, valueType, err := m.materializeRandomByName(name)
	if err != nil {
		return nil, MaterializedReplacement{}, err
	}
	return replacement, MaterializedReplacement{
		FunctionName: name,
		Ordinal:      uint32(len(m.replacements)),
		LiteralSQL:   literalSQL,
		ValueType:    valueType,
	}, nil
}

func (m *materializer) materializeColumnPseudoFunction(column map[string]any) (map[string]any, MaterializedReplacement, bool, error) {
	name, ok := bareColumnName(column)
	if !ok || strings.ToLower(name) != "current_timestamp" {
		return nil, MaterializedReplacement{}, false, nil
	}
	replacement, literalSQL, valueType, err := m.materializeNow(name)
	if err != nil {
		return nil, MaterializedReplacement{}, false, err
	}
	return replacement, MaterializedReplacement{
		FunctionName: name,
		Ordinal:      uint32(len(m.replacements)),
		LiteralSQL:   literalSQL,
		ValueType:    valueType,
	}, true, nil
}

func (m *materializer) materializeSpecialNode(name string, node any, materialize materializeValueFunc) (map[string]any, MaterializedReplacement, error) {
	if specialNodeHasArguments(node) {
		return nil, MaterializedReplacement{}, fmt.Errorf("%s with arguments: %w", name, ErrMaterializationUnsupported)
	}
	replacement, literalSQL, valueType, err := materialize(name)
	if err != nil {
		return nil, MaterializedReplacement{}, err
	}
	return replacement, MaterializedReplacement{
		FunctionName: name,
		Ordinal:      uint32(len(m.replacements)),
		LiteralSQL:   literalSQL,
		ValueType:    valueType,
	}, nil
}

func (m *materializer) materializeFunction(fn map[string]any) (map[string]any, MaterializedReplacement, bool, error) {
	name, _ := fn["name"].(string)
	normalized := strings.ToLower(name)
	if !isKnownNondeterministic(normalized) {
		return nil, MaterializedReplacement{}, false, nil
	}
	if !isSupportedNondeterministic(normalized) {
		return nil, MaterializedReplacement{}, false, fmt.Errorf("%s: %w", name, ErrMaterializationUnsupported)
	}
	if len(functionArgs(fn)) != 0 {
		return nil, MaterializedReplacement{}, false, fmt.Errorf("%s with arguments: %w", name, ErrMaterializationUnsupported)
	}

	var (
		node       map[string]any
		literalSQL string
		valueType  string
		err        error
	)
	switch normalized {
	case "now", "current_timestamp", "localtimestamp", "localtime":
		node, literalSQL, valueType, err = m.materializeNow(name)
	case "utctimestamp", "utc_timestamp":
		node, literalSQL, valueType, err = m.materializeUTCTimestamp(name)
	case "now64":
		node, literalSQL, valueType, err = m.materializeNow64(name)
	case "today", "current_date", "curdate":
		node, literalSQL, valueType, err = m.materializeToday(name)
	case "yesterday":
		node, literalSQL, valueType, err = m.materializeYesterday(name)
	case "rand", "rand32":
		node, literalSQL, valueType, err = m.materializeRandom32(name)
	case "rand64", "random":
		node, literalSQL, valueType, err = m.materializeRandom64(name)
	case "randcanonical":
		node, literalSQL, valueType, err = m.materializeRandomFloat64(name)
	case "generateuuidv4":
		node, literalSQL, valueType, err = m.materializeUUID(name)
	default:
		return nil, MaterializedReplacement{}, false, fmt.Errorf("%s: %w", name, ErrMaterializationUnsupported)
	}
	if err != nil {
		return nil, MaterializedReplacement{}, false, err
	}
	return node, MaterializedReplacement{
		FunctionName: name,
		Ordinal:      uint32(len(m.replacements)),
		LiteralSQL:   literalSQL,
		ValueType:    valueType,
	}, true, nil
}
