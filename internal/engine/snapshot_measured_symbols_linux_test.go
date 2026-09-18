//go:build linux

package engine

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

func TestSnapshotMeasuredWeakImports(t *testing.T) {
	ffi := snapshotTestFFI(t)
	scenario := os.Getenv("SNAPSHOT_WEAK_CHILD")
	if scenario == "" {
		for _, name := range []string{"instrumentation_global", "semantic_absent", "semantic_global"} {
			t.Run(name, func(t *testing.T) {
				cmd := exec.Command("/proc/self/exe", "-test.run=^TestSnapshotMeasuredWeakImports$", "-test.v")
				cmd.Env = append(os.Environ(), "SNAPSHOT_WEAK_CHILD="+name)
				output, err := cmd.CombinedOutput()
				t.Log(string(output))
				if err != nil {
					t.Fatal(err)
				}
			})
		}
		return
	}
	symbol := "__gmon_start__"
	if strings.HasPrefix(scenario, "semantic") {
		symbol = "semantic_hookx"
		b, err := os.ReadFile(ffi)
		if err != nil {
			t.Fatal(err)
		}
		if len(symbol) != len("__gmon_start__") || bytes.Count(b, []byte("__gmon_start__\x00")) == 0 {
			t.Fatal("ineffective weak-import mutation")
		}
		b = bytes.ReplaceAll(b, []byte("__gmon_start__\x00"), []byte(symbol+"\x00"))
		ffi = filepath.Join(t.TempDir(), "weak-semantic.so")
		if err = os.WriteFile(ffi, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var calls func() int32
	var beforeCalls int32
	if strings.HasSuffix(scenario, "global") {
		dir := t.TempDir()
		source := filepath.Join(dir, "global.c")
		dso := filepath.Join(dir, "global.so")
		if err := os.WriteFile(source, []byte("static int count; void "+symbol+"(void) { ++count; } int weak_calls(void) { return count; }\n"), 0600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("cc", "-shared", "-fPIC", "-Wl,--hash-style=gnu", "-o", dso, source).CombinedOutput()
		if err != nil {
			t.Fatalf("compile test-owned global DSO: %v %s", err, out)
		}
		h, err := purego.Dlopen(dso, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			t.Fatal(err)
		}
		defer purego.Dlclose(h)
		if _, err = purego.Dlsym(purego.RTLD_DEFAULT, symbol); err != nil {
			t.Fatal("fixture symbol not globally bound", err)
		}
		purego.RegisterLibFunc(&calls, h, "weak_calls")
		beforeCalls = calls()
		t.Logf("actual RTLD_GLOBAL DSO binds %s; before_constructor_calls=%d", symbol, beforeCalls)
	}
	f, err := elf.Open(ffi)
	if err != nil {
		t.Fatal(err)
	}
	syms, err := f.DynamicSymbols()
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range syms {
		if s.Name == symbol && s.Section == elf.SHN_UNDEF && elf.ST_BIND(s.Info) == elf.STB_WEAK {
			found = true
			t.Logf("actual fixture import %s binding=WEAK version=%q library=%q", s.Name, s.Version, s.Library)
		}
	}
	if !found {
		t.Fatal("fixture has no requested weak import")
	}
	before, _ := os.ReadDir("/proc/self/fd")
	e, _, err := NewMeasuredPolyglot(ffi)
	if calls != nil {
		t.Logf("after_constructor_calls=%d (before=%d)", calls(), beforeCalls)
	}
	if err == nil {
		e.Close()
		t.Fatalf("accepted %s weak import %s", scenario, symbol)
	}
	if calls != nil && calls() != beforeCalls {
		t.Fatal("rejected weak dependency was executed")
	}
	if !strings.Contains(err.Error(), symbol) {
		t.Fatalf("wrong refusal: %v", err)
	}
	after, _ := os.ReadDir("/proc/self/fd")
	if len(before) != len(after) {
		t.Fatalf("refusal leaked FD: %d -> %d", len(before), len(after))
	}
	t.Logf("production constructor refused %s: %v; FD=%d -> %d", scenario, err, len(before), len(after))
}

func TestSnapshotMeasuredWeakRuntimeAndRegisteredEntrypoints(t *testing.T) {
	path := snapshotTestFFI(t)
	e, _, err := NewMeasuredPolyglot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	measured := e.(*snapshotMeasuredEngine)
	f, err := elf.NewFile(measured.sealed)
	if err != nil {
		t.Fatal(err)
	}
	syms, err := f.DynamicSymbols()
	if err != nil {
		t.Fatal(err)
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	weak := 0
	exports := map[string]bool{}
	for _, s := range syms {
		if s.Section != elf.SHN_UNDEF {
			exports[s.Name] = true
			continue
		}
		if elf.ST_BIND(s.Info) != elf.STB_WEAK || s.Version == "" {
			continue
		}
		address, err := purego.Dlsym(measured.handle, s.Name)
		if err != nil {
			t.Fatal(err)
		}
		if !snapshotAddressInRuntime(maps, address, s.Library) {
			t.Fatalf("weak versioned import %s is not in %s", s.Name, s.Library)
		}
		weak++
		t.Logf("checked actual WEAK runtime mapping: %s %s %s", s.Name, s.Version, s.Library)
	}
	if weak == 0 {
		t.Fatal("pinned FFI weak versioned runtime imports disappeared")
	}
	// Compare against the actual pinned binding registration list, not a guessed
	// subset of function names. No dependency source or FFI is modified.
	_, file, _, _ := runtime.Caller(0)
	binding := filepath.Join(filepath.Dir(file), "..", "..", "third_party", "polyglot-src", "packages", "go", "internal", "ffi", "library.go")
	source, err := os.ReadFile(binding)
	if err != nil {
		t.Fatal(err)
	}
	registered := regexp.MustCompile(`\{"(polyglot_[^"]+)", &l\.`).FindAllSubmatch(source, -1)
	if len(registered) == 0 {
		t.Fatal("no binding registrations found")
	}
	var st unix.Stat_t
	if err = unix.Fstat(int(measured.sealed.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	for _, m := range registered {
		name := string(m[1])
		if !exports[name] {
			t.Fatalf("registered entrypoint absent from checked dynamic view: %s", name)
		}
		address, err := purego.Dlsym(measured.handle, name)
		if err != nil {
			t.Fatal(err)
		}
		if !snapshotAddressInFile(maps, address, &st) {
			t.Fatalf("registered entrypoint outside sealed image: %s", name)
		}
		t.Logf("checked registered entrypoint: %s", name)
	}
	if err = verifySnapshotSymbols(measured.sealed, measured.handle); err != nil {
		t.Fatal(err)
	}
	t.Logf("complete registered-entrypoint mappings=%d; WEAK versioned runtime mappings=%d", len(registered), weak)
}

func TestSnapshotMeasuredSymbolExtentRefusals(t *testing.T) {
	_, original := snapshotCopyFFI(t)
	for _, kind := range []string{"short_dynsym", "hidden_ifunc", "hidden_unique", "wrong_syment", "wrong_section_entsize", "relocation_outside_symbols", "short_versym"} {
		t.Run(kind, func(t *testing.T) {
			b := append([]byte(nil), original...)
			f, err := elf.NewFile(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			symtab := f.SectionByType(elf.SHT_DYNSYM)
			if symtab == nil || symtab.Size < 48 {
				t.Fatal("missing dynamic symbols")
			}
			shoff := binary.LittleEndian.Uint64(b[40:48])
			entsize := uint64(binary.LittleEndian.Uint16(b[58:60]))
			shnum := uint64(binary.LittleEndian.Uint16(b[60:62]))
			sectionHeader := func(kind elf.SectionType) uint64 {
				for i := uint64(0); i < shnum; i++ {
					off := shoff + i*entsize
					if binary.LittleEndian.Uint32(b[off+4:]) == uint32(kind) {
						return off
					}
				}
				t.Fatal("missing section", kind)
				return 0
			}
			switch kind {
			case "short_dynsym", "hidden_ifunc", "hidden_unique":
				header := sectionHeader(elf.SHT_DYNSYM)
				binary.LittleEndian.PutUint64(b[header+32:], symtab.Size-24)
				last := symtab.Offset + symtab.Size - 24
				if kind == "hidden_ifunc" {
					b[last+4] = byte(elf.STB_GLOBAL)<<4 | byte(elf.STT_GNU_IFUNC)
				}
				if kind == "hidden_unique" {
					b[last+4] = 10<<4 | byte(elf.STT_OBJECT)
				}
				// Runtime tables stay byte-identical: only section size and, in the
				// explicit hidden-symbol cases, the final symbol's type/binding change.
				for _, typ := range []elf.SectionType{elf.SHT_DYNAMIC, elf.SHT_HASH, elf.SHT_GNU_HASH, elf.SHT_RELA, elf.SHT_REL} {
					for _, s := range f.Sections {
						if s.Type == typ && !bytes.Equal(original[s.Offset:s.Offset+s.Size], b[s.Offset:s.Offset+s.Size]) {
							t.Fatal("loader table unexpectedly changed", s.Name)
						}
					}
				}
				t.Logf("shortened .dynsym from %d to %d; runtime hash/relocation/dynamic tables unchanged; omitted symbol offset=%d", symtab.Size, symtab.Size-24, last)
			case "wrong_section_entsize":
				header := sectionHeader(elf.SHT_DYNSYM)
				binary.LittleEndian.PutUint64(b[header+56:], 16)
			case "wrong_syment":
				dynamic := f.SectionByType(elf.SHT_DYNAMIC)
				found := false
				for off := dynamic.Offset; off < dynamic.Offset+dynamic.Size; off += 16 {
					if elf.DynTag(binary.LittleEndian.Uint64(b[off:])) == elf.DT_SYMENT {
						binary.LittleEndian.PutUint64(b[off+8:], 16)
						found = true
						break
					}
				}
				if !found {
					t.Fatal("DT_SYMENT missing")
				}
			case "relocation_outside_symbols":
				rel := f.SectionByType(elf.SHT_RELA)
				if rel == nil {
					t.Fatal("RELA missing")
				}
				info := binary.LittleEndian.Uint64(b[rel.Offset+8:])
				binary.LittleEndian.PutUint64(b[rel.Offset+8:], (symtab.Size/24)<<32|info&0xffffffff)
			case "short_versym":
				header := sectionHeader(elf.SHT_GNU_VERSYM)
				s := f.SectionByType(elf.SHT_GNU_VERSYM)
				binary.LittleEndian.PutUint64(b[header+32:], s.Size-2)
			}
			path := filepath.Join(t.TempDir(), kind+".so")
			if err = os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			input, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			if err = inspectSnapshotELF(input, true); err == nil {
				t.Fatal("metadata inspector accepted inconsistent loader symbol view", kind)
			}
			t.Logf("production metadata refusal: %v", err)
		})
	}
}

func TestSnapshotMeasuredLoaderHashVariants(t *testing.T) {
	snapshotTestFFI(t)
	for _, style := range []string{"sysv", "gnu", "both"} {
		t.Run(style, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "hash.c")
			dso := filepath.Join(dir, "hash.so")
			// A real external call guarantees a PLT relocation on both supported
			// architectures; a constant-return function need not produce one.
			if err := os.WriteFile(source, []byte("#include <stdio.h>\nint test_entry(const char *s) { return puts(s); }\n"), 0600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("cc", "-shared", "-fPIC", "-Wl,--hash-style="+style, "-o", dso, source).CombinedOutput()
			if err != nil {
				t.Fatalf("compile actual hash fixture: %v %s", err, out)
			}
			f, err := elf.Open(dso)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err = snapshotELFLoaderMetadata(f, true); err != nil {
				t.Fatal(err)
			}
			t.Logf("actual %s loader hash metadata agrees with complete symbol table", style)
			original, err := os.ReadFile(dso)
			if err != nil {
				t.Fatal(err)
			}
			mutations := []string{"no_hash", "relocation_outside", "plt_outside"}
			if style != "sysv" {
				mutations = append(mutations, "gnu_bucket_before_first", "gnu_unterminated")
			}
			if style != "gnu" {
				mutations = append(mutations, "sysv_index_outside", "sysv_cycle")
			}
			if style == "both" {
				mutations = append(mutations, "conflicting_hash_counts")
			}
			for _, kind := range mutations {
				t.Run(kind, func(t *testing.T) {
					b := append([]byte(nil), original...)
					dynamic := f.SectionByType(elf.SHT_DYNAMIC)
					changeTag := func(tag elf.DynTag, change func(uint64)) bool {
						for off := dynamic.Offset; off < dynamic.Offset+dynamic.Size; off += 16 {
							if elf.DynTag(binary.LittleEndian.Uint64(b[off:])) == tag {
								change(off)
								return true
							}
						}
						return false
					}
					count := f.SectionByType(elf.SHT_DYNSYM).Size / 24
					switch kind {
					case "no_hash":
						for _, tag := range []elf.DynTag{elf.DT_HASH, elf.DT_GNU_HASH} {
							changeTag(tag, func(off uint64) { binary.LittleEndian.PutUint64(b[off:], uint64(elf.DT_DEBUG)) })
						}
					case "relocation_outside", "plt_outside":
						tag := elf.DT_RELA
						if kind == "plt_outside" {
							tag = elf.DT_JMPREL
						}
						values, err := f.DynValue(tag)
						address := uint64(0)
						present := len(values) == 1
						if present {
							address = values[0]
						}
						if err != nil {
							t.Fatal(err)
						}
						if !present {
							t.Fatal("actual relocation table absent", tag)
						}
						t.Logf("mutating actual %s table at 0x%x", tag, address)
						for _, p := range f.Progs {
							if p.Type == elf.PT_LOAD && address >= p.Vaddr && address-p.Vaddr < p.Filesz {
								off := p.Off + address - p.Vaddr
								info := binary.LittleEndian.Uint64(b[off+8:])
								binary.LittleEndian.PutUint64(b[off+8:], count<<32|info&0xffffffff)
								break
							}
						}
					case "gnu_bucket_before_first", "gnu_unterminated":
						s := f.SectionByType(elf.SHT_GNU_HASH)
						off := s.Offset
						nb := uint64(binary.LittleEndian.Uint32(b[off:]))
						first := binary.LittleEndian.Uint32(b[off+4:])
						bloom := uint64(binary.LittleEndian.Uint32(b[off+8:]))
						bucket := off + 16 + bloom*8
						if kind == "gnu_bucket_before_first" {
							if first < 2 {
								t.Fatal("fixture lacks undefined prefix")
							}
							binary.LittleEndian.PutUint32(b[bucket:], first-1)
						} else {
							chain := bucket + nb*4
							for at := chain; at < s.Offset+s.Size; at += 4 {
								binary.LittleEndian.PutUint32(b[at:], binary.LittleEndian.Uint32(b[at:])&^1)
							}
						}
					case "sysv_index_outside", "sysv_cycle", "conflicting_hash_counts":
						s := f.SectionByType(elf.SHT_HASH)
						off := s.Offset
						nb := uint64(binary.LittleEndian.Uint32(b[off:]))
						chains := off + 8 + 4*nb
						switch kind {
						case "sysv_index_outside":
							binary.LittleEndian.PutUint32(b[off+8:], uint32(count))
						case "sysv_cycle":
							binary.LittleEndian.PutUint32(b[chains+4:], 1)
						case "conflicting_hash_counts":
							binary.LittleEndian.PutUint32(b[off+4:], uint32(count+1))
						}
					}
					malformed, err := elf.NewFile(bytes.NewReader(b))
					if err != nil {
						t.Fatal(err)
					}
					if err = snapshotELFLoaderMetadata(malformed, true); err == nil {
						t.Fatal("accepted inconsistent hash/relocation view", kind)
					}
					t.Logf("refused %s: %v", kind, err)
				})
			}
		})
	}
}
