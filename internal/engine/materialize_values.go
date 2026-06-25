package engine

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	nanosPerSecond = int64(1_000_000_000)
	nanosPerMilli  = int64(1_000_000)
	secondsPerDay  = int64(86_400)
	maxUInt32      = uint64(1<<32 - 1)
)

func (m *materializer) materializeNow(name string) (map[string]any, string, string, error) {
	seconds, err := m.nowSeconds(name)
	if err != nil {
		return nil, "", "", err
	}
	sql := "toDateTime(" + strconv.FormatInt(seconds, 10) + ")"
	return functionCall("toDateTime", numLiteral(seconds)), sql, "DateTime", nil
}

func (m *materializer) materializeUTCTimestamp(name string) (map[string]any, string, string, error) {
	seconds, err := m.nowSeconds(name)
	if err != nil {
		return nil, "", "", err
	}
	sql := "toDateTime(" + strconv.FormatInt(seconds, 10) + ", 'UTC')"
	return functionCall("toDateTime", numLiteral(seconds), litStr("UTC")), sql, "DateTime", nil
}

func (m *materializer) materializeNow64(name string) (map[string]any, string, string, error) {
	if m.inputs.NowUnixNS == nil {
		return nil, "", "", fmt.Errorf("%s requires now_unix_ns: %w", name, ErrMaterializationInputMissing)
	}
	millis := *m.inputs.NowUnixNS / nanosPerMilli
	sql := "fromUnixTimestamp64Milli(" + strconv.FormatInt(millis, 10) + ")"
	return functionCall("fromUnixTimestamp64Milli", numLiteral(millis)), sql, "DateTime64(3)", nil
}

func (m *materializer) materializeToday(name string) (map[string]any, string, string, error) {
	seconds, err := m.nowSeconds(name)
	if err != nil {
		return nil, "", "", err
	}
	return dateFromUnixSeconds(seconds), dateFromUnixSecondsSQL(seconds), "Date", nil
}

func (m *materializer) materializeYesterday(name string) (map[string]any, string, string, error) {
	seconds, err := m.nowSeconds(name)
	if err != nil {
		return nil, "", "", err
	}
	yesterdaySeconds := seconds - secondsPerDay
	return dateFromUnixSeconds(yesterdaySeconds), dateFromUnixSecondsSQL(yesterdaySeconds), "Date", nil
}

func (m *materializer) materializeRandomByName(name string) (map[string]any, string, string, error) {
	switch strings.ToLower(name) {
	case "rand", "rand32":
		return m.materializeRandom32(name)
	default:
		return m.materializeRandom64(name)
	}
}

func (m *materializer) materializeRandom32(name string) (map[string]any, string, string, error) {
	value, err := m.nextRandomUint64(name)
	if err != nil {
		return nil, "", "", err
	}
	if value > maxUInt32 {
		return nil, "", "", fmt.Errorf("%s value %d exceeds UInt32: %w", name, value, ErrMaterializationInvalidInput)
	}
	sql := strconv.FormatUint(value, 10)
	return numLiteralUint(value), sql, "UInt32", nil
}

func (m *materializer) materializeRandom64(name string) (map[string]any, string, string, error) {
	value, err := m.nextRandomUint64(name)
	if err != nil {
		return nil, "", "", err
	}
	sql := strconv.FormatUint(value, 10)
	return numLiteralUint(value), sql, "UInt64", nil
}

func (m *materializer) materializeRandomFloat64(name string) (map[string]any, string, string, error) {
	if m.randomFloatIndex >= len(m.inputs.RandomFloat64Values) {
		return nil, "", "", fmt.Errorf("%s requires random_float64_values: %w", name, ErrMaterializationInputMissing)
	}
	value := m.inputs.RandomFloat64Values[m.randomFloatIndex]
	m.randomFloatIndex++
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= 1 {
		return nil, "", "", fmt.Errorf("%s value %v outside [0,1): %w", name, value, ErrMaterializationInvalidInput)
	}
	if value == 0 {
		value = 0
	}
	literal := strconv.FormatFloat(value, 'g', -1, 64)
	sql := "toFloat64(" + literal + ")"
	return functionCall("toFloat64", numLiteralFloat(literal)), sql, "Float64", nil
}

func (m *materializer) materializeUUID(name string) (map[string]any, string, string, error) {
	if m.uuidIndex >= len(m.inputs.UUIDValues) {
		return nil, "", "", fmt.Errorf("%s requires uuid_values: %w", name, ErrMaterializationInputMissing)
	}
	value := strings.ToLower(m.inputs.UUIDValues[m.uuidIndex])
	m.uuidIndex++
	if !isUUID(value) {
		return nil, "", "", fmt.Errorf("%s uuid %q: %w", name, value, ErrMaterializationInvalidInput)
	}
	sql := "toUUID('" + value + "')"
	return functionCall("toUUID", litStr(value)), sql, "UUID", nil
}

func (m *materializer) nowSeconds(name string) (int64, error) {
	if m.inputs.NowUnixNS == nil {
		return 0, fmt.Errorf("%s requires now_unix_ns: %w", name, ErrMaterializationInputMissing)
	}
	return *m.inputs.NowUnixNS / nanosPerSecond, nil
}

func (m *materializer) nextRandomUint64(name string) (uint64, error) {
	if m.randomIndex >= len(m.inputs.RandomUint64Values) {
		return 0, fmt.Errorf("%s requires random_uint64_values: %w", name, ErrMaterializationInputMissing)
	}
	value := m.inputs.RandomUint64Values[m.randomIndex]
	m.randomIndex++
	return value, nil
}

func dateFromUnixSeconds(seconds int64) map[string]any {
	return functionCall("toDate", functionCall("toDateTime", numLiteral(seconds)))
}

func dateFromUnixSecondsSQL(seconds int64) string {
	return "toDate(toDateTime(" + strconv.FormatInt(seconds, 10) + "))"
}
