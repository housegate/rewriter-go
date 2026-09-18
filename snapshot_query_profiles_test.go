package rewriter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

type profileVector struct {
	Sets    []string `json:"native_artifact_sets_json"`
	Native  []string `json:"native_analyzer_build_digests"`
	Records []string `json:"query_profile_records_json"`
	Queries []string `json:"query_profile_ids"`
	File    string   `json:"profile_file_json"`
	FileSHA string   `json:"profile_file_sha256"`
}

func loadProfileVector(t *testing.T) profileVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/snapshot_query_build_identity_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v profileVector
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func TestSnapshotProfileIndependentVector(t *testing.T) {
	v := loadProfileVector(t)
	f, err := decodeSnapshotProfiles([]byte(v.File))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f)
	if string(raw) != v.File {
		t.Fatal("canonical file bytes changed")
	}
	h := sha256.Sum256(raw)
	if hex.EncodeToString(h[:]) != v.FileSHA {
		t.Fatal("file digest changed")
	}
	for i, p := range f.Profiles {
		b, _ := json.Marshal(p.NativeArtifactSet)
		if string(b) != v.Sets[i] {
			t.Fatal("set bytes changed")
		}
		b, _ = json.Marshal(p.Record)
		if string(b) != v.Records[i] {
			t.Fatal("record bytes changed")
		}
		if snapshotCanonicalDigest("snapshot-native-analyzer-artifact-set-v1", p.NativeArtifactSet) != v.Native[i] {
			t.Fatal("N changed")
		}
		if snapshotCanonicalDigest("snapshot-query-profile-v1", p.Record) != v.Queries[i] {
			t.Fatal("Q changed")
		}
	}
}

// Recursively mutate each ordered object field, including empty-valued fields.
// Expectations are independent of decoder implementation; no invalid test is
// repaired by recomputing through the decoder.
func TestSnapshotProfileRecursiveCanonicalRefusals(t *testing.T) {
	v := loadProfileVector(t)
	reject := func(name, bad string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			if bad == v.File {
				t.Fatal("ineffective mutation")
			}
			if _, err := decodeSnapshotProfiles([]byte(bad)); err == nil {
				t.Fatal("accepted invalid nested input")
			}
		})
	}
	var walk func(json.RawMessage, int, string)
	walk = func(node json.RawMessage, base int, path string) {
		if len(node) == 0 {
			t.Fatal("empty test node")
		}
		switch node[0] {
		case '{':
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(node, &fields); err != nil {
				t.Fatal(err)
			}
			for key, value := range fields {
				keyJSON, _ := json.Marshal(key)
				field := string(keyJSON) + ":" + string(value)
				relative := strings.Index(string(node), field)
				if relative < 0 {
					t.Fatal("missing literal field", path, key)
				}
				off := base + relative
				after := off + len(field)
				for kind, replacement := range map[string]string{"null": string(keyJSON) + ":null", "duplicate": field + "," + field, "unknown": field + `,"unknown_field":0`, "case": `"` + strings.ToUpper(key) + `":` + string(value)} {
					reject(path+"/"+key+"/"+kind, v.File[:off]+replacement+v.File[after:])
				}
				from, to := off, after
				if v.File[after] == ',' {
					to++
				} else if off > 0 && v.File[off-1] == ',' {
					from--
				}
				reject(path+"/"+key+"/missing", v.File[:from]+v.File[to:])
				walk(value, off+len(keyJSON)+1, path+"/"+key)
			}
			// Reorder an otherwise valid complete object, not a JSON syntax error.
			d := json.NewDecoder(bytes.NewReader(node))
			d.Token()
			key, _ := d.Token()
			var first json.RawMessage
			d.Decode(&first)
			keyJSON, _ := json.Marshal(key)
			field := string(keyJSON) + ":" + string(first)
			if len(fields) > 1 {
				reordered := "{" + string(node[2+len(field):len(node)-1]) + "," + field + "}"
				reject(path+"/field_order", v.File[:base]+reordered+v.File[base+len(node):])
			}
		case '[':
			var elements []json.RawMessage
			if err := json.Unmarshal(node, &elements); err != nil {
				t.Fatal(err)
			}
			pos := 1
			for i, element := range elements {
				relative := pos + strings.Index(string(node[pos:]), string(element))
				reject(fmt.Sprint(path, "/", i, "/null_element"), v.File[:base+relative]+"null"+v.File[base+relative+len(element):])
				walk(element, base+relative, fmt.Sprint(path, "/", i))
				pos = relative + len(element)
			}
		}
	}
	walk(json.RawMessage(v.File), 0, "file")
	for name, bad := range map[string]string{"whitespace": v.File + "\n", "trailing": v.File + "{}", "empty": "{}", "null": "null", "float_version": strings.Replace(v.File, `"version":1`, `"version":1.0`, 1), "escape": strings.Replace(v.File, "linux", `l\u0069nux`, 1)} {
		reject(name, bad)
	}
}
func TestSnapshotProfileValueRefusals(t *testing.T) {
	v := loadProfileVector(t)
	mutations := map[string]func(*snapshotProfileFile){
		"version":           func(f *snapshotProfileFile) { f.Version = 2 },
		"no_profiles":       func(f *snapshotProfileFile) { f.Profiles = []snapshotProfileEntry{} },
		"profile_order":     func(f *snapshotProfileFile) { f.Profiles[0], f.Profiles[1] = f.Profiles[1], f.Profiles[0] },
		"duplicate_profile": func(f *snapshotProfileFile) { f.Profiles[1] = f.Profiles[0] },
		"wrong_Q":           func(f *snapshotProfileFile) { f.Profiles[0].QueryProfileID = "0x" + strings.Repeat("0", 64) },
		"wrong_N": func(f *snapshotProfileFile) {
			f.Profiles[0].Record.NativeAnalyzerBuildDigest = "0x" + strings.Repeat("0", 64)
		},
		"uppercase_digest": func(f *snapshotProfileFile) {
			f.Profiles[0].Record.TZDataDigest = strings.ToUpper(f.Profiles[0].Record.TZDataDigest)
		},
		"record_version": func(f *snapshotProfileFile) { f.Profiles[0].Record.Version = 2 },
		"platform":       func(f *snapshotProfileFile) { f.Profiles[0].Record.Platform = " " },
		"column":         func(f *snapshotProfileFile) { f.Profiles[0].Record.ColumnProfileID = "" },
		"output":         func(f *snapshotProfileFile) { f.Profiles[0].Record.OutputOrderID = "" },
		"settings_order": func(f *snapshotProfileFile) {
			r := &f.Profiles[0].Record
			r.Settings[0], r.Settings[1] = r.Settings[1], r.Settings[0]
		},
		"settings_duplicate": func(f *snapshotProfileFile) { r := &f.Profiles[0].Record; r.Settings[1] = r.Settings[0] },
		"operators_order": func(f *snapshotProfileFile) {
			r := &f.Profiles[0].Record
			r.ScalarOperators[0], r.ScalarOperators[1] = r.ScalarOperators[1], r.ScalarOperators[0]
		},
		"operators_duplicate": func(f *snapshotProfileFile) { r := &f.Profiles[0].Record; r.ScalarOperators[1] = r.ScalarOperators[0] },
		"set_version":         func(f *snapshotProfileFile) { f.Profiles[0].NativeArtifactSet.Version = 2 },
		"no_artifacts":        func(f *snapshotProfileFile) { f.Profiles[0].NativeArtifactSet.Artifacts = []snapshotArtifact{} },
		"tuple_order": func(f *snapshotProfileFile) {
			s := &f.Profiles[0].NativeArtifactSet
			s.Artifacts[0], s.Artifacts[1] = s.Artifacts[1], s.Artifacts[0]
		},
		"tuple_duplicate": func(f *snapshotProfileFile) { s := &f.Profiles[0].NativeArtifactSet; s.Artifacts[1] = s.Artifacts[0] },
		"tuple_platform":  func(f *snapshotProfileFile) { f.Profiles[0].NativeArtifactSet.Artifacts[0].Platform = "" },
		"tuple_executable": func(f *snapshotProfileFile) {
			f.Profiles[0].NativeArtifactSet.Artifacts[0].ExecutableDigest = "version1"
		},
		"tuple_ffi": func(f *snapshotProfileFile) { f.Profiles[0].NativeArtifactSet.Artifacts[0].FFIDigest = "0x00" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			f, err := decodeSnapshotProfiles([]byte(v.File))
			if err != nil {
				t.Fatal(err)
			}
			mutate(&f)
			raw, _ := json.Marshal(f)
			if _, err = decodeSnapshotProfiles(raw); err == nil {
				t.Fatal("accepted invalid values")
			}
		})
	}
	for i := 0; i < 8; i++ {
		t.Run(fmt.Sprint("limit", i), func(t *testing.T) {
			f, _ := decodeSnapshotProfiles([]byte(v.File))
			r := reflect.ValueOf(&f.Profiles[0].Record.Limits).Elem()
			r.Field(i).SetUint(0)
			raw, _ := json.Marshal(f)
			if _, err := decodeSnapshotProfiles(raw); err == nil {
				t.Fatal("accepted zero limit")
			}
		})
	}
}
func TestSnapshotProfileExplicitPaths(t *testing.T) {
	for _, paths := range [][2]string{{"", "x"}, {"x", ""}, {" ", "x"}, {"missing", "missing"}} {
		if s, err := NewServiceWithSnapshotQueryProfiles(paths[0], paths[1]); err == nil {
			s.Close()
			t.Fatal("accepted absent explicit input")
		}
	}
}
