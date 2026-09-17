package rewriter

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/housegate/rewriter-go/internal/engine"
)

// These private value types independently implement the version-1 wire format.
// Field order is part of the commitment. Do not add omitempty or normalize input.
type snapshotArtifact struct {
	Platform         string `json:"platform"`
	ExecutableDigest string `json:"executable_digest"`
	FFIDigest        string `json:"ffi_digest"`
}
type snapshotArtifactSet struct {
	Version   uint32             `json:"version"`
	Artifacts []snapshotArtifact `json:"artifacts"`
}
type snapshotSetting struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type snapshotLimits struct {
	MaxSQLBytes        uint64 `json:"max_sql_bytes"`
	MaxDescriptorBytes uint64 `json:"max_descriptor_bytes"`
	MaxOutputRows      uint64 `json:"max_output_rows"`
	MaxOutputBytes     uint64 `json:"max_output_bytes"`
	MaxRestoreBytes    uint64 `json:"max_restore_bytes"`
	MaxSortMemoryBytes uint64 `json:"max_sort_memory_bytes"`
	MaxSpillBytes      uint64 `json:"max_spill_bytes"`
	MaxExecutionMS     uint64 `json:"max_execution_ms"`
}
type snapshotRecord struct {
	Version                   uint32            `json:"version"`
	ClickHouseBuildDigest     string            `json:"clickhouse_build_digest"`
	Platform                  string            `json:"platform"`
	NativeAnalyzerBuildDigest string            `json:"native_analyzer_build_digest"`
	GRPCAnalyzerBuildDigest   string            `json:"grpc_analyzer_build_digest"`
	TZDataDigest              string            `json:"tzdata_digest"`
	Settings                  []snapshotSetting `json:"settings"`
	ScalarOperators           []string          `json:"scalar_operators"`
	ColumnProfileID           string            `json:"column_profile_id"`
	OutputOrderID             string            `json:"output_order_id"`
	Limits                    snapshotLimits    `json:"limits"`
}
type snapshotProfileEntry struct {
	QueryProfileID    string              `json:"query_profile_id"`
	Record            snapshotRecord      `json:"record"`
	NativeArtifactSet snapshotArtifactSet `json:"native_artifact_set"`
}
type snapshotProfileFile struct {
	Version  uint32                 `json:"version"`
	Profiles []snapshotProfileEntry `json:"profiles"`
}

func snapshotCanonicalDigest(domain string, v any) string {
	// All callers pass the closed scalar/struct/slice types above, which cannot
	// fail JSON encoding. This prefix and NUL are the independent replay contract.
	b, _ := json.Marshal(v)
	return fmt.Sprintf("0x%x", sha256.Sum256(append([]byte("housegate-replay-mvp-v0:"+domain+"\x00"), b...)))
}
func snapshotDigest(s string) bool {
	if len(s) != 66 || !strings.HasPrefix(s, "0x") {
		return false
	}
	for _, c := range s[2:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func snapshotName(s string) bool { return s != "" && strings.TrimSpace(s) == s }
func snapshotTupleLess(a, b snapshotArtifact) bool {
	if a.Platform != b.Platform {
		return a.Platform < b.Platform
	}
	if a.ExecutableDigest != b.ExecutableDigest {
		return a.ExecutableDigest < b.ExecutableDigest
	}
	return a.FFIDigest < b.FFIDigest
}
func decodeSnapshotProfiles(raw []byte) (snapshotProfileFile, error) {
	var f snapshotProfileFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, fmt.Errorf("snapshot profiles: JSON: %w", err)
	}
	canonical, err := json.Marshal(f)
	if err != nil || !bytes.Equal(raw, canonical) {
		return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: noncanonical or incomplete JSON")
	}
	// Exact typed re-encoding rejects duplicate/unknown/missing fields, alternate
	// ordering/escapes/numbers, and null scalars/objects at every depth. Nil slices
	// must also be rejected explicitly: otherwise null would re-encode as null.
	if f.Version != 1 || len(f.Profiles) == 0 {
		return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: require version 1 and nonempty profiles")
	}
	for i, p := range f.Profiles {
		if !snapshotDigest(p.QueryProfileID) || i > 0 && f.Profiles[i-1].QueryProfileID >= p.QueryProfileID {
			return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: invalid or unsorted/duplicate Q")
		}
		r := p.Record
		if r.Version != 1 || !snapshotName(r.Platform) || !snapshotName(r.ColumnProfileID) || !snapshotName(r.OutputOrderID) {
			return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: invalid record version or identity")
		}
		for _, d := range []string{r.ClickHouseBuildDigest, r.NativeAnalyzerBuildDigest, r.GRPCAnalyzerBuildDigest, r.TZDataDigest} {
			if !snapshotDigest(d) {
				return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: malformed record digest")
			}
		}
		if r.Settings == nil || r.ScalarOperators == nil {
			return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: null settings/operators")
		}
		for j, s := range r.Settings {
			if !snapshotName(s.Name) || j > 0 && r.Settings[j-1].Name >= s.Name {
				return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: invalid or unsorted/duplicate setting")
			}
		}
		for j, s := range r.ScalarOperators {
			if !snapshotName(s) || j > 0 && r.ScalarOperators[j-1] >= s {
				return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: invalid or unsorted/duplicate operator")
			}
		}
		l := r.Limits
		for _, n := range []uint64{l.MaxSQLBytes, l.MaxDescriptorBytes, l.MaxOutputRows, l.MaxOutputBytes, l.MaxRestoreBytes, l.MaxSortMemoryBytes, l.MaxSpillBytes, l.MaxExecutionMS} {
			if n == 0 {
				return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: zero limit")
			}
		}
		set := p.NativeArtifactSet
		if set.Version != 1 || len(set.Artifacts) == 0 {
			return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: invalid native artifact set")
		}
		for j, a := range set.Artifacts {
			if !snapshotName(a.Platform) || !snapshotDigest(a.ExecutableDigest) || !snapshotDigest(a.FFIDigest) || j > 0 && !snapshotTupleLess(set.Artifacts[j-1], a) {
				return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: invalid or unsorted/duplicate native tuple")
			}
		}
		if snapshotCanonicalDigest("snapshot-native-analyzer-artifact-set-v1", set) != r.NativeAnalyzerBuildDigest {
			return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: native commitment mismatch")
		}
		if snapshotCanonicalDigest("snapshot-query-profile-v1", r) != p.QueryProfileID {
			return snapshotProfileFile{}, fmt.Errorf("snapshot profiles: query commitment mismatch")
		}
	}
	return f, nil
}

// NewServiceWithSnapshotQueryProfiles loads explicit canonical profiles and an
// immutable measured Linux FFI. Neither path may be empty; environment/default
// resolution is never used. Valid nonlocal entries remain unavailable.
//
// Measured membership is necessary, not sufficient for semantic support or
// admission. Analyze/Prepare additionally require exact supported settings and
// operators. Ordinary NewService behavior is unchanged.
func NewServiceWithSnapshotQueryProfiles(libPath, profilePath string) (*Service, error) {
	e, local, err := newMeasuredSnapshotRuntime(libPath, profilePath)
	if err != nil {
		return nil, err
	}
	return &Service{engine: e, measuredSnapshotProfiles: local}, nil
}

// NewNativeRewriterWithSnapshotQueryProfiles owns its own measured engine; closing
// a Service or another NativeRewriter never invalidates this instance.
func NewNativeRewriterWithSnapshotQueryProfiles(libPath, profilePath string, opts ...Option) (*NativeRewriter, error) {
	e, local, err := newMeasuredSnapshotRuntime(libPath, profilePath)
	if err != nil {
		return nil, err
	}
	r := New(e, opts...)
	r.measuredSnapshotProfiles = local
	return r, nil
}

func newMeasuredSnapshotRuntime(libPath, profilePath string) (engine.Engine, map[string]string, error) {
	if strings.TrimSpace(libPath) == "" || strings.TrimSpace(profilePath) == "" {
		return nil, nil, fmt.Errorf("snapshot profiles: explicit library and profile paths are required")
	}
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot profiles: read: %w", err)
	}
	f, err := decodeSnapshotProfiles(raw)
	if err != nil {
		return nil, nil, err
	}
	e, id, err := engine.NewMeasuredPolyglot(libPath)
	if err != nil {
		return nil, nil, err
	}
	// Immutable strings own the canonical records; no decoder slice/map escapes.
	// Analyze/Prepare separately enforce semantic support before acknowledging Q.
	local := make(map[string]string)
	for _, p := range f.Profiles {
		for _, a := range p.NativeArtifactSet.Artifacts {
			if a.Platform == id.Platform && a.ExecutableDigest == id.ExecutableDigest && a.FFIDigest == id.FFIDigest {
				b, _ := json.Marshal(p.Record)
				local[p.QueryProfileID] = string(b)
				break
			}
		}
	}
	return e, local, nil
}
