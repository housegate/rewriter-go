package harness

// This is a finite, test-only Prepare qualification runner. It is not the
// authenticated restore/canonical-output executor used by a production role.
import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	rewriter "github.com/housegate/rewriter-go"
	"github.com/housegate/rewriter-proto/gen/pb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type snapshotRPC struct{ oracle *Oracle }

func (r snapshotRPC) AnalyzeSnapshotQuery(ctx context.Context, q *pb.AnalyzeSnapshotQueryRequest) (*pb.AnalyzeSnapshotQueryResponse, error) {
	return r.oracle.client.AnalyzeSnapshotQuery(ctx, q)
}
func (r snapshotRPC) PrepareSnapshotQuery(ctx context.Context, q *pb.PrepareSnapshotQueryRequest) (*pb.PrepareSnapshotQueryResponse, error) {
	return r.oracle.client.PrepareSnapshotQuery(ctx, q)
}

type executionColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}
type executionTable struct {
	Name    string            `json:"name"`
	Columns []executionColumn `json:"columns"`
	Rows    [][]string        `json:"rows"`
}
type executionFixture struct {
	Name       string           `json:"name"`
	CorpusCase string           `json:"corpus_case"`
	SQL        string           `json:"sql,omitempty"`
	Ordinary   bool             `json:"ordinary,omitempty"`
	Types      []string         `json:"types,omitempty"`
	Rows       [][]string       `json:"rows,omitempty"`
	Tables     []executionTable `json:"tables,omitempty"`
	ErrorCode  int              `json:"error_code,omitempty"`
}
type clickhouseJSON struct {
	Meta []executionColumn `json:"meta"`
	Data [][]any           `json:"data"`
	Rows int               `json:"rows"`
}
type executionResult struct {
	Completed bool              `json:"completed"`
	ErrorCode int               `json:"error_code"`
	ExitCode  int               `json:"exit_code"`
	Columns   []executionColumn `json:"columns,omitempty"`
	Rows      [][]string        `json:"rows,omitempty"`
	Stdout    string            `json:"stdout"`
	Stderr    string            `json:"stderr"`
	Argv      []string          `json:"argv"`
}
type executionProfile struct {
	Profiles []struct {
		Q      string `json:"query_profile_id"`
		Record struct {
			ClickHouse string `json:"clickhouse_build_digest"`
			TZData     string `json:"tzdata_digest"`
			Platform   string `json:"platform"`
			Settings   []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"settings"`
		} `json:"record"`
	} `json:"profiles"`
}

func fileDigest(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil {
		return "", e
	}
	if !s.Mode().IsRegular() {
		return "", fmt.Errorf("not regular: %s", path)
	}
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return fmt.Sprintf("0x%x", h.Sum(nil)), nil
}
func loadExecutionFixtures(t *testing.T) []executionFixture {
	t.Helper()
	b, e := os.ReadFile("testdata/snapshot_query_execution_cases.json")
	if e != nil {
		t.Fatal(e)
	}
	var f struct {
		Version int                `json:"version"`
		Cases   []executionFixture `json:"cases"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&f); e != nil {
		t.Fatal(e)
	}
	if e = d.Decode(new(any)); e != io.EOF {
		t.Fatalf("trailing fixtures: %v", e)
	}
	if f.Version != 1 || len(f.Cases) != 102 {
		t.Fatal("execution fixture version/count changed")
	}
	names := map[string]bool{}
	executions := 0
	for _, c := range f.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatal("duplicate/empty fixture")
		}
		names[c.Name] = true
		if len(c.Types) > 0 {
			executions++
		}
	}
	if executions != 76 {
		t.Fatal("execution fixture matrix incomplete")
	}
	return f.Cases
}

// Verify the controlled fallback directory against every regular archive entry.
// Embedded TZif preference is separately probed by private CI; neither this check
// nor the bounded probe claims all-zone functional qualification.
func checkExecutionTZ(t *testing.T, archive, dir string) {
	t.Helper()
	f, e := os.Open(archive)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	seen := map[string]bool{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg || !strings.HasPrefix(h.Name, "zoneinfo/") {
			t.Fatal("invalid timezone archive member")
		}
		rel := strings.TrimPrefix(h.Name, "zoneinfo/")
		if !filepath.IsLocal(rel) || seen[rel] {
			t.Fatal("unsafe timezone path")
		}
		seen[rel] = true
		want, e := io.ReadAll(io.LimitReader(tr, 1<<20))
		if e != nil || int64(len(want)) != h.Size {
			t.Fatal("invalid timezone entry size")
		}
		p := filepath.Join(dir, rel)
		st, e := os.Lstat(p)
		if e != nil || !st.Mode().IsRegular() {
			t.Fatalf("invalid selected timezone file %s", p)
		}
		got, e := os.ReadFile(p)
		if e != nil || !bytes.Equal(want, got) {
			t.Fatalf("selected timezone bytes differ: %s", p)
		}
	}
	if len(seen) != 600 {
		t.Fatalf("timezone entries: %d", len(seen))
	}
	if e := filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		r, e := filepath.Rel(dir, p)
		if e != nil || !seen[r] {
			return fmt.Errorf("unexpected timezone path %s", p)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"/usr/share/zoneinfo", "/apex", "/data", "/system", "/config", "/pkg"} {
		files, e := os.ReadDir(p)
		if e != nil || len(files) != 0 {
			t.Fatalf("timezone fallback root not masked: %s", p)
		}
	}
	if os.Getenv("TZ") != "UTC" || os.Getenv("TZDIR") != dir {
		t.Fatal("controlled TZ/TZDIR required")
	}
}

type boundedOutput struct {
	bytes.Buffer
	exceeded bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4<<20 {
		b.exceeded = true
		return 0, fmt.Errorf("test output exceeds 4 MiB")
	}
	return b.Buffer.Write(p)
}

var exceptionCode = regexp.MustCompile(`(?m)^Code: ([0-9]+)\.`)
var fixtureNumber = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)
var fixtureIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
var fixtureType = regexp.MustCompile(`^(?:U?Int(?:8|16|32|64)|Float(?:32|64))$`)

func executePrepared(t *testing.T, binary string, p executionProfile, f executionFixture, exactSQL string) executionResult {
	t.Helper()
	// Readbacks and returned SELECT execute in the same process/session. The SQL
	// returned by Prepare is passed byte-for-byte as the final query, not rewritten.
	readback := []string{}
	argv := []string{"local", "--path", t.TempDir(), "--multiquery", "--format", "JSONCompact"}
	for _, s := range p.Profiles[0].Record.Settings {
		argv = append(argv, "--"+s.Name+"="+s.Value)
		readback = append(readback, "getSetting('"+s.Name+"')")
	}
	sql := "SELECT " + strings.Join(readback, ", ") + "; CREATE DATABASE snapshot_scratch; "
	for _, table := range f.Tables {
		if !fixtureIdentifier.MatchString(table.Name) {
			t.Fatal("invalid fixture table")
		}
		cols := []string{}
		for _, c := range table.Columns {
			if !fixtureIdentifier.MatchString(c.Name) || !fixtureType.MatchString(c.Type) {
				t.Fatal("invalid fixture schema")
			}
			cols = append(cols, c.Name+" "+c.Type)
		}
		sql += "CREATE TABLE snapshot_scratch." + table.Name + " (" + strings.Join(cols, ",") + ") ENGINE=Memory; "
		if len(table.Rows) > 0 {
			rows := []string{}
			for _, r := range table.Rows {
				if len(r) != len(cols) {
					t.Fatal("invalid fixture row width")
				}
				for _, v := range r {
					if !fixtureNumber.MatchString(v) {
						t.Fatal("invalid fixture number")
					}
				}
				rows = append(rows, "("+strings.Join(r, ",")+")")
			}
			sql += "INSERT INTO snapshot_scratch." + table.Name + " VALUES " + strings.Join(rows, ",") + "; "
		}
	}
	sql += exactSQL
	argv = append(argv, "--query", sql)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, argv...)
	var out, errout boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = &errout
	err := cmd.Run()
	r := executionResult{ExitCode: -1, Stdout: out.String(), Stderr: errout.String(), Argv: append([]string{binary}, argv...)}
	if cmd.ProcessState != nil {
		r.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil || out.exceeded || errout.exceeded {
		t.Fatalf("test infrastructure bound exceeded; partial output discarded: %v", ctx.Err())
	}
	// Even an expected query exception must have applied every setting first.
	d := json.NewDecoder(strings.NewReader(r.Stdout))
	d.UseNumber()
	var settings clickhouseJSON
	if e := d.Decode(&settings); e != nil || settings.Rows != 1 || len(settings.Data) != 1 || len(settings.Data[0]) != 6 {
		t.Fatalf("missing same-session setting readback: %v; stdout=%s stderr=%s", e, r.Stdout, r.Stderr)
	}
	wantSettings := []string{"false", "1", "throw", "throw", "throw", "throw"}
	for i, v := range settings.Data[0] {
		if fmt.Sprint(v) != wantSettings[i] {
			t.Fatalf("setting %d: %v", i, v)
		}
	}
	if err != nil {
		m := exceptionCode.FindStringSubmatch(r.Stderr)
		if len(m) != 2 {
			t.Fatalf("process failure without ClickHouse exception: %v; stderr=%s", err, r.Stderr)
		}
		r.ErrorCode, _ = strconv.Atoi(m[1])
		return r
	}
	var result clickhouseJSON
	if e := d.Decode(&result); e != nil {
		t.Fatalf("incomplete result: %v; %s", e, r.Stdout)
	}
	if e := d.Decode(new(any)); e != io.EOF {
		t.Fatalf("unexpected trailing result: %v", e)
	}
	if result.Rows != len(result.Data) {
		t.Fatal("partial result row count")
	}
	r.Columns = result.Meta
	r.Rows = [][]string{}
	for _, row := range result.Data {
		if len(row) != len(result.Meta) {
			t.Fatal("result metadata/row width mismatch")
		}
		values := []string{}
		for _, v := range row {
			n, ok := v.(json.Number)
			if !ok {
				t.Fatalf("expected lossless numeric output, got %T %v", v, v)
			}
			values = append(values, n.String())
		}
		r.Rows = append(r.Rows, values)
	}
	r.Completed = true
	return r
}

func TestSnapshotQueryPrepareExecution(t *testing.T) {
	if os.Getenv("SNAPSHOT_QUERY_ORDINARY") == "1" {
		t.Skip("explicit ordinary lane; no Prepare execution qualification")
	}
	for _, name := range []string{"SNAPSHOT_MEASURED_FFI", "SNAPSHOT_MEASURED_PROFILE", "SNAPSHOT_QUERY_CORPUS", "REWRITER_ORACLE_ADDR", "SNAPSHOT_CLICKHOUSE", "SNAPSHOT_TZDATA_ARCHIVE", "TZDIR", "SNAPSHOT_EXECUTION_EVIDENCE"} {
		if os.Getenv(name) == "" {
			t.Fatalf("qualification requires %s", name)
		}
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Fatal("execution qualification requires linux/amd64")
	}
	corpus := loadSnapshotCorpus(t)
	cases := map[string]snapshotCase{}
	for _, c := range corpus.Cases {
		cases[c.Name] = c
	}
	var p executionProfile
	raw, e := os.ReadFile(os.Getenv("SNAPSHOT_MEASURED_PROFILE"))
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &p); e != nil || len(p.Profiles) != 1 {
		t.Fatal("one measured execution profile required")
	}
	rec := p.Profiles[0].Record
	if rec.Platform != "linux/amd64" {
		t.Fatal("wrong SQL platform")
	}
	expectedSettings := []string{"cast_keep_nullable=0", "max_threads=1", "read_overflow_mode=throw", "result_overflow_mode=throw", "sort_overflow_mode=throw", "timeout_overflow_mode=throw"}
	actualSettings := []string{}
	for _, s := range rec.Settings {
		actualSettings = append(actualSettings, s.Name+"="+s.Value)
	}
	if !reflect.DeepEqual(expectedSettings, actualSettings) {
		t.Fatal("unsupported execution settings")
	}
	for path, digest := range map[string]string{os.Getenv("SNAPSHOT_CLICKHOUSE"): rec.ClickHouse, os.Getenv("SNAPSHOT_TZDATA_ARCHIVE"): rec.TZData} {
		got, e := fileDigest(path)
		if e != nil || got != digest {
			t.Fatalf("measured runtime mismatch %s: %s %v", path, got, e)
		}
		defer func(path, digest string) {
			got, e := fileDigest(path)
			if e != nil || got != digest {
				t.Errorf("runtime changed after execution: %s", path)
			}
		}(path, digest)
	}
	checkExecutionTZ(t, os.Getenv("SNAPSHOT_TZDATA_ARCHIVE"), os.Getenv("TZDIR"))
	svc, e := rewriter.NewServiceWithSnapshotQueryProfiles(os.Getenv("SNAPSHOT_MEASURED_FFI"), os.Getenv("SNAPSHOT_MEASURED_PROFILE"))
	if e != nil {
		t.Fatal(e)
	}
	defer svc.Close()
	native, e := rewriter.NewNativeRewriterWithSnapshotQueryProfiles(os.Getenv("SNAPSHOT_MEASURED_FFI"), os.Getenv("SNAPSHOT_MEASURED_PROFILE"))
	if e != nil {
		t.Fatal(e)
	}
	defer native.Close()
	oracle, e := DialOracle()
	if e != nil || oracle == nil {
		t.Fatalf("live oracle required: %v", e)
	}
	defer oracle.Close()
	evidence, e := os.OpenFile(os.Getenv("SNAPSHOT_EXECUTION_EVIDENCE"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer evidence.Close()
	encoder := json.NewEncoder(evidence)
	backends := []struct {
		name string
		api  snapshotAnalyzer
	}{{"service", svc}, {"native", native}, {"oracle", snapshotRPC{oracle}}}
	executes, refusals := 0, 0
	for _, f := range loadExecutionFixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			c, ok := cases[f.CorpusCase]
			if !ok {
				t.Fatal("missing frozen fixture")
			}
			var req pb.AnalyzeSnapshotQueryRequest
			snapshotDecode(t, c.Request, &req)
			if f.SQL != "" {
				req.Sql = f.SQL
			}
			var prepare pb.PrepareSnapshotQueryRequest
			if len(c.PrepareRequest) > 0 {
				snapshotDecode(t, c.PrepareRequest, &prepare)
			} else {
				prepare.Analysis = proto.Clone(&req).(*pb.AnalyzeSnapshotQueryRequest)
				for _, table := range req.Catalog {
					if table.Table == "events" {
						prepare.Bindings = append(prepare.Bindings, &pb.SnapshotScratchBinding{TableId: table.TableId, ScratchDatabase: "snapshot_scratch", ScratchTable: "r2"})
					}
				}
			}
			prepare.Analysis = proto.Clone(&req).(*pb.AnalyzeSnapshotQueryRequest)
			var firstA *pb.AnalyzeSnapshotQueryResponse
			var firstP *pb.PrepareSnapshotQueryResponse
			var firstColumns []executionColumn
			for _, backend := range backends {
				t.Run(backend.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					got, e := backend.api.AnalyzeSnapshotQuery(ctx, proto.Clone(&req).(*pb.AnalyzeSnapshotQueryRequest))
					t.Logf("ANALYZE request=%s response=%s error=%v", protojson.Format(&req), protojson.Format(got), e)
					if e != nil {
						t.Fatal(e)
					}
					if firstA == nil {
						firstA = got
					} else if e = snapshotCompare(firstA, got); e != nil {
						t.Fatal(e)
					}
					if f.SQL == "" {
						var want pb.AnalyzeSnapshotQueryResponse
						snapshotDecode(t, c.ExpectedResponse, &want)
						if e = snapshotCompare(&want, got); e != nil {
							t.Fatal(e)
						}
					}
					record := map[string]any{"fixture": f.Name, "backend": backend.name, "query_profile_id": p.Profiles[0].Q, "request": json.RawMessage(protojson.Format(&req)), "analyze": json.RawMessage(protojson.Format(got))}
					if f.Ordinary {
						want := &pb.AnalyzeSnapshotQueryResponse{ContractVersion: 1, QueryProfileId: p.Profiles[0].Q, Code: pb.SnapshotQueryCode_NOT_SNAPSHOT_QUERY}
						if e = snapshotCompare(want, got); e != nil {
							t.Fatal(e)
						}
					}
					if len(f.Types) == 0 {
						if got.Code == pb.SnapshotQueryCode_SUCCESS || got.SqlAfterMaterialization != "" || got.TargetTableId != "" || len(got.TargetColumns) > 0 || len(got.ReadTableIds) > 0 {
							t.Fatal("refusal carried executable data")
						}
						refusals++
						if e = encoder.Encode(record); e != nil {
							t.Fatal(e)
						}
						return
					}
					if got.Code != pb.SnapshotQueryCode_SUCCESS || got.ContractVersion != 1 || got.QueryProfileId != p.Profiles[0].Q {
						t.Fatalf("Analyze did not succeed: %s", got)
					}
					if f.SQL != "" {
						var want pb.AnalyzeSnapshotQueryResponse
						snapshotDecode(t, c.ExpectedResponse, &want)
						if got.TargetTableId != want.TargetTableId || !reflect.DeepEqual(got.TargetColumns, want.TargetColumns) || !reflect.DeepEqual(got.ReadTableIds, want.ReadTableIds) {
							t.Fatal("supplemental fixture target/closure mismatch")
						}
					}
					replay := proto.Clone(&req).(*pb.AnalyzeSnapshotQueryRequest)
					replay.Sql = got.SqlAfterMaterialization
					again, e := backend.api.AnalyzeSnapshotQuery(ctx, replay)
					if e != nil {
						t.Fatal(e)
					}
					if e = snapshotCompare(got, again); e != nil {
						t.Fatalf("logical replay: %v", e)
					}
					prepared, e := backend.api.PrepareSnapshotQuery(ctx, proto.Clone(&prepare).(*pb.PrepareSnapshotQueryRequest))
					if e != nil {
						t.Fatal(e)
					}
					if prepared.Code != pb.SnapshotQueryCode_SUCCESS || prepared.ContractVersion != 1 || prepared.QueryProfileId != p.Profiles[0].Q || prepared.SelectSql == "" {
						t.Fatalf("Prepare did not succeed: %s", prepared)
					}
					// Analyze reports INSERT-list order; Prepare reports target schema order.
					var targetOrder []string
					for _, table := range req.Catalog {
						if table.TableId == got.TargetTableId {
							for _, column := range table.Columns {
								targetOrder = append(targetOrder, column.Name)
							}
						}
					}
					if prepared.TargetTableId != got.TargetTableId || !reflect.DeepEqual(prepared.TargetColumns, targetOrder) || !reflect.DeepEqual(prepared.ReadTableIds, got.ReadTableIds) {
						t.Fatal("Prepare target schema order or read closure mismatch")
					}
					if firstP == nil {
						firstP = prepared
					} else if e = snapshotCompare(firstP, prepared); e != nil {
						t.Fatal(e)
					}
					if f.SQL == "" && len(c.ExpectedPrepareResponse) > 0 {
						var want pb.PrepareSnapshotQueryResponse
						snapshotDecode(t, c.ExpectedPrepareResponse, &want)
						if e = snapshotCompare(&want, prepared); e != nil {
							t.Fatal(e)
						}
					}
					result := executePrepared(t, os.Getenv("SNAPSHOT_CLICKHOUSE"), p, f, prepared.SelectSql)
					executes++
					record["prepare_request"] = json.RawMessage(protojson.Format(&prepare))
					record["prepare"] = json.RawMessage(protojson.Format(prepared))
					record["execution"] = result
					if e = encoder.Encode(record); e != nil {
						t.Fatal(e)
					}
					if result.ErrorCode != f.ErrorCode || result.Completed != (f.ErrorCode == 0) {
						t.Fatalf("completion/error: got completed=%t CH=%d exit=%d; want CH=%d; stderr=%s", result.Completed, result.ErrorCode, result.ExitCode, f.ErrorCode, result.Stderr)
					}
					if result.Completed {
						types := []string{}
						for _, col := range result.Columns {
							if col.Name == "" {
								t.Fatal("missing column name")
							}
							types = append(types, col.Type)
						}
						if !reflect.DeepEqual(types, f.Types) || !reflect.DeepEqual(result.Rows, f.Rows) {
							t.Fatalf("metadata/value mismatch: columns=%v rows=%v; want types=%v rows=%v", result.Columns, result.Rows, f.Types, f.Rows)
						}
						if firstColumns == nil {
							firstColumns = result.Columns
						} else if !reflect.DeepEqual(firstColumns, result.Columns) {
							t.Fatal("backend output column order/names differ")
						}
					}
				})
			}
		})
	}
	if executes != 228 || refusals != 78 {
		t.Errorf("incomplete execution matrix: executions=%d/228 analyzer-refusals=%d/78", executes, refusals)
	}
	if e = evidence.Sync(); e != nil {
		t.Fatal(e)
	}
	t.Logf("PREPARE_EXECUTION fixtures=102 backends=3 executions=%d refusals=%d", executes, refusals)
}

func TestSnapshotQueryExecutionFixtureContract(t *testing.T) {
	corpus := loadSnapshotCorpus(t)
	names := map[string]bool{}
	for _, c := range corpus.Cases {
		names[c.Name] = true
	}
	for _, f := range loadExecutionFixtures(t) {
		if !names[f.CorpusCase] {
			t.Fatalf("unknown corpus reference %s", f.CorpusCase)
		}
		if len(f.Types) == 0 && (len(f.Rows) != 0 || len(f.Tables) != 0 || f.ErrorCode != 0) {
			t.Fatalf("nonexecuting fixture has runtime data: %s", f.Name)
		}
		if f.ErrorCode != 0 && f.ErrorCode != 125 && f.ErrorCode != 349 {
			t.Fatalf("unapproved numeric error: %s", f.Name)
		}
		for _, row := range f.Rows {
			if len(row) != len(f.Types) {
				t.Fatalf("incorrect expected width: %s", f.Name)
			}
		}
	}
}
