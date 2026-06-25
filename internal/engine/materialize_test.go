package engine

import (
	"errors"
	"strings"
	"testing"
)

func TestMaterializeNonDeterminism_replacesSupportedFunctions(t *testing.T) {
	// Given
	nowUnixNS := int64(1710000000123456789)
	ast := AST(`{"insert":{"values":[[{"function":{"name":"now","args":[]}},{"rand":{"seed":null,"lower":null,"upper":null}},{"random":null},{"function":{"name":"rand64","args":[]}},{"function":{"name":"generateUUIDv4","args":[]}}]]}}`)

	// When
	got, err := MaterializeNonDeterminism(ast, MaterializationInputs{
		NowUnixNS:          &nowUnixNS,
		RandomUint64Values: []uint64{7, 9, 11},
		UUIDValues:         []string{"018f3c8a-3e8a-7d0a-b5b0-6e95e3f0b001"},
	})

	// Then
	if err != nil {
		t.Fatalf("MaterializeNonDeterminism: %v", err)
	}
	out := string(got.AST)
	for _, fn := range []string{"\"now\"", "\"rand\"", "\"random\"", "\"rand64\"", "\"generateUUIDv4\""} {
		if strings.Contains(out, fn) {
			t.Fatalf("materialized AST still contains %s: %s", fn, out)
		}
	}
	if len(got.Replacements) != 5 {
		t.Fatalf("replacements = %d, want 5: %+v", len(got.Replacements), got.Replacements)
	}
	if got.Replacements[0].LiteralSQL != "toDateTime(1710000000)" {
		t.Fatalf("now literal = %q", got.Replacements[0].LiteralSQL)
	}
	if got.Replacements[1].LiteralSQL != "7" {
		t.Fatalf("rand literal = %q", got.Replacements[1].LiteralSQL)
	}
	if got.Replacements[2].LiteralSQL != "9" {
		t.Fatalf("random literal = %q", got.Replacements[2].LiteralSQL)
	}
	if got.Replacements[3].LiteralSQL != "11" {
		t.Fatalf("rand64 literal = %q", got.Replacements[3].LiteralSQL)
	}
	if got.Replacements[4].LiteralSQL != "toUUID('018f3c8a-3e8a-7d0a-b5b0-6e95e3f0b001')" {
		t.Fatalf("uuid literal = %q", got.Replacements[4].LiteralSQL)
	}
}

func TestMaterializeNonDeterminism_replacesAdditionalSafeScalarFunctions(t *testing.T) {
	// Given
	nowUnixNS := int64(1710000000123456789)
	ast := AST(`{"insert":{"values":[[{"function":{"name":"now64","args":[]}},{"function":{"name":"today","args":[]}},{"function":{"name":"current_date","args":[]}},{"function":{"name":"curdate","args":[]}},{"function":{"name":"yesterday","args":[]}},{"function":{"name":"UTCTimestamp","args":[]}},{"function":{"name":"current_timestamp","args":[]}},{"function":{"name":"localtimestamp","args":[]}},{"function":{"name":"localtime","args":[]}},{"function":{"name":"rand32","args":[]}},{"function":{"name":"randCanonical","args":[]}}]]}}`)

	// When
	got, err := MaterializeNonDeterminism(ast, MaterializationInputs{
		NowUnixNS:           &nowUnixNS,
		RandomUint64Values:  []uint64{13},
		RandomFloat64Values: []float64{0.125},
	})

	// Then
	if err != nil {
		t.Fatalf("MaterializeNonDeterminism: %v", err)
	}
	out := string(got.AST)
	for _, fn := range []string{"\"now64\"", "\"today\"", "\"current_date\"", "\"curdate\"", "\"yesterday\"", "\"UTCTimestamp\"", "\"current_timestamp\"", "\"localtimestamp\"", "\"localtime\"", "\"rand32\"", "\"randCanonical\""} {
		if strings.Contains(out, fn) {
			t.Fatalf("materialized AST still contains %s: %s", fn, out)
		}
	}
	wantLiterals := []string{
		"fromUnixTimestamp64Milli(1710000000123)",
		"toDate(toDateTime(1710000000))",
		"toDate(toDateTime(1710000000))",
		"toDate(toDateTime(1710000000))",
		"toDate(toDateTime(1709913600))",
		"toDateTime(1710000000, 'UTC')",
		"toDateTime(1710000000)",
		"toDateTime(1710000000)",
		"toDateTime(1710000000)",
		"13",
		"toFloat64(0.125)",
	}
	if len(got.Replacements) != len(wantLiterals) {
		t.Fatalf("replacements = %d, want %d: %+v", len(got.Replacements), len(wantLiterals), got.Replacements)
	}
	for i, want := range wantLiterals {
		if got.Replacements[i].LiteralSQL != want {
			t.Fatalf("replacement %d literal = %q, want %q", i, got.Replacements[i].LiteralSQL, want)
		}
	}
}

func TestMaterializeNonDeterminism_failsClosed_whenInputMissing(t *testing.T) {
	// Given
	ast := AST(`{"insert":{"values":[[{"rand":{"seed":null,"lower":null,"upper":null}}]]}}`)

	// When
	_, err := MaterializeNonDeterminism(ast, MaterializationInputs{})

	// Then
	if !errors.Is(err, ErrMaterializationInputMissing) {
		t.Fatalf("err = %v, want ErrMaterializationInputMissing", err)
	}
}

func TestMaterializeNonDeterminism_failsClosed_whenRand32InputOverflows(t *testing.T) {
	// Given
	ast := AST(`{"insert":{"values":[[{"function":{"name":"rand32","args":[]}}]]}}`)

	// When
	_, err := MaterializeNonDeterminism(ast, MaterializationInputs{
		RandomUint64Values: []uint64{1 << 32},
	})

	// Then
	if !errors.Is(err, ErrMaterializationInvalidInput) {
		t.Fatalf("err = %v, want ErrMaterializationInvalidInput", err)
	}
}

func TestMaterializeNonDeterminism_failsClosed_whenRandCanonicalInputInvalid(t *testing.T) {
	// Given
	ast := AST(`{"insert":{"values":[[{"function":{"name":"randCanonical","args":[]}}]]}}`)

	// When
	_, err := MaterializeNonDeterminism(ast, MaterializationInputs{
		RandomFloat64Values: []float64{1},
	})

	// Then
	if !errors.Is(err, ErrMaterializationInvalidInput) {
		t.Fatalf("err = %v, want ErrMaterializationInvalidInput", err)
	}
}

func TestMaterializeNonDeterminism_failsClosed_whenFunctionUnsupported(t *testing.T) {
	// Given
	ast := AST(`{"insert":{"values":[[{"function":{"name":"generateUUIDv7","args":[]}}]]}}`)

	// When
	_, err := MaterializeNonDeterminism(ast, MaterializationInputs{})

	// Then
	if !errors.Is(err, ErrMaterializationUnsupported) {
		t.Fatalf("err = %v, want ErrMaterializationUnsupported", err)
	}
}
