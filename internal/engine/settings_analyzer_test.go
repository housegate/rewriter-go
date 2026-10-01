package engine

import "testing"

// The analyzer switch passes only with a true literal (user ruling
// 2026-10-01): the closed spelling list is compared on the raw lexeme.
func TestTrueLiteralSpelling(t *testing.T) {
	for src, want := range map[string]bool{
		"1": true, "true": true, "TRUE": true, "True": true, "'1'": true, "'true'": true, "'TRUE'": true,
		"0": false, "false": false, "'0'": false, "0x1": false, "+1": false, "1.0": false, "01": false,
		"'01'": false, "' 1'": false, "x'31'": false, "$$true$$": false, `'tr\x75e'`: false, "": false,
	} {
		if got := TrueLiteralSpelling(src); got != want {
			t.Errorf("TrueLiteralSpelling(%q) = %v, want %v", src, got, want)
		}
	}
}

func TestSettingsBackstopAnalyzer(t *testing.T) {
	e := newTestEngine(t)
	for sql, want := range map[string]bool{
		"SELECT 1 SETTINGS enable_analyzer = 1":                             false,
		"SELECT 1 SETTINGS enable_analyzer = true, max_threads = 1":         false,
		"SELECT 1 SETTINGS allow_experimental_analyzer = 'True' FORMAT TSV": false,
		"SELECT * FROM (SELECT 1 SETTINGS enable_analyzer = '1')":           false,
		"SELECT 1 SETTINGS enable_analyzer = 0":                             true,
		"SELECT 1 SETTINGS `enable_analyzer` = 0":                           true,
		"SELECT 1 SETTINGS enable_analyzer = 1 + 0":                         true,
		"SELECT 1 SETTINGS enable_analyzer = 0x1":                           true,
		"SELECT 1 SETTINGS enable_analyzer = -1":                            true,
		"SELECT 1 SETTINGS enable_analyzer =":                               true,
		"SELECT 1 SETTINGS legacy_column_name_of_tuple_literal = 0":         true,
		"SELECT 1 SETTINGS profile = 'default'":                             true,
	} {
		_, hit := SettingsBackstop(e, sql)
		if hit != want {
			t.Errorf("SettingsBackstop(%q) = %v, want %v", sql, hit, want)
		}
	}
}
