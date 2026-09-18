//go:build linux

package engine

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
)

// All metadata here is read through actual file-backed PT_LOAD addresses. It
// does not trust a section header to define the loader's symbol-table extent.
// This conservative bound also prevents a malformed local ELF from causing
// unbounded allocation/traversal before its identity can be established.
const snapshotMaxELFMetadata = 64 << 20

func snapshotDynamicValue(f *elf.File, tag elf.DynTag) (uint64, bool, error) {
	values, err := f.DynValue(tag)
	if err != nil {
		return 0, false, err
	}
	if len(values) > 1 {
		return 0, false, fmt.Errorf("engine: duplicate ELF %s", tag)
	}
	if len(values) == 0 {
		return 0, false, nil
	}
	return values[0], true, nil
}
func snapshotLoadBytes(f *elf.File, addr, size uint64) ([]byte, error) {
	if size > snapshotMaxELFMetadata {
		return nil, fmt.Errorf("engine: ELF metadata exceeds supported bound")
	}
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD || addr < p.Vaddr {
			continue
		}
		offset := addr - p.Vaddr
		if offset > p.Filesz || size > p.Filesz-offset {
			continue
		}
		data := make([]byte, int(size))
		if _, err := p.ReadAt(data, int64(offset)); err != nil {
			return nil, fmt.Errorf("engine: read loader metadata: %w", err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("engine: ELF metadata is outside a file-backed LOAD segment")
}
func snapshotGNUHashCount(f *elf.File, addr uint64) (uint64, error) {
	head, err := snapshotLoadBytes(f, addr, 16)
	if err != nil {
		return 0, err
	}
	buckets := uint64(binary.LittleEndian.Uint32(head))
	first := uint64(binary.LittleEndian.Uint32(head[4:]))
	bloom := uint64(binary.LittleEndian.Uint32(head[8:]))
	shift := binary.LittleEndian.Uint32(head[12:])
	if buckets == 0 || bloom == 0 || bloom&(bloom-1) != 0 || shift >= 32 || first == 0 || first > snapshotMaxELFMetadata/24 {
		return 0, fmt.Errorf("engine: unsupported GNU hash header")
	}
	prefix := 16 + bloom*8 + buckets*4
	if prefix > snapshotMaxELFMetadata || addr+prefix < addr {
		return 0, fmt.Errorf("engine: GNU hash metadata overflow")
	}
	table, err := snapshotLoadBytes(f, addr, prefix)
	if err != nil {
		return 0, err
	}
	count := first
	// GNU hash chains are grouped by bucket and terminated by their low bit.
	// Infer the loader's upper extent from every bucket, including the terminal
	// chain, rather than stopping at the section's declared .dynsym size.
	visited := make(map[uint64]bool)
	for i := uint64(0); i < buckets; i++ {
		symbol := uint64(binary.LittleEndian.Uint32(table[16+bloom*8+i*4:]))
		if symbol == 0 {
			continue
		}
		if symbol < first {
			return 0, fmt.Errorf("engine: GNU hash bucket precedes symbol offset")
		}
		for {
			index := symbol - first
			if symbol >= snapshotMaxELFMetadata/24 || index >= snapshotMaxELFMetadata/4 || visited[symbol] {
				return 0, fmt.Errorf("engine: invalid/overlapping GNU hash chain")
			}
			visited[symbol] = true
			offset := prefix + index*4
			if addr+offset < addr {
				return 0, fmt.Errorf("engine: GNU hash chain overflow")
			}
			entry, err := snapshotLoadBytes(f, addr+offset, 4)
			if err != nil {
				return 0, err
			}
			if symbol+1 > count {
				count = symbol + 1
			}
			if binary.LittleEndian.Uint32(entry)&1 != 0 {
				break
			}
			symbol++
		}
	}
	if count > snapshotMaxELFMetadata/24 {
		return 0, fmt.Errorf("engine: excessive loader symbol count")
	}
	return count, nil
}
func snapshotSysVHashCount(f *elf.File, addr uint64) (uint64, error) {
	head, err := snapshotLoadBytes(f, addr, 8)
	if err != nil {
		return 0, err
	}
	buckets := uint64(binary.LittleEndian.Uint32(head))
	count := uint64(binary.LittleEndian.Uint32(head[4:]))
	if buckets == 0 || count == 0 || count > snapshotMaxELFMetadata/24 {
		return 0, fmt.Errorf("engine: invalid SysV hash header")
	}
	size := 8 + 4*(buckets+count)
	table, err := snapshotLoadBytes(f, addr, size)
	if err != nil {
		return 0, err
	}
	// Every bucket and chain index is part of the loader view, even one which
	// is not reached while resolving today's fixed registration names.
	for off := uint64(8); off < size; off += 4 {
		if uint64(binary.LittleEndian.Uint32(table[off:])) >= count {
			return 0, fmt.Errorf("engine: SysV hash index outside symbol table")
		}
	}
	// Reject cycles in the complete chain graph, in linear time.
	state := make([]byte, count)
	for start := uint64(1); start < count; start++ {
		chain := []uint64{}
		index := start
		for index != 0 && state[index] == 0 {
			state[index] = 1
			chain = append(chain, index)
			index = uint64(binary.LittleEndian.Uint32(table[8+4*buckets+index*4:]))
		}
		if index != 0 && state[index] == 1 {
			return 0, fmt.Errorf("engine: cyclic SysV hash chain")
		}
		for _, index := range chain {
			state[index] = 2
		}
	}
	return count, nil
}
func snapshotELFSymbolExtent(f *elf.File, section *elf.Section) error {
	entry, ok, err := snapshotDynamicValue(f, elf.DT_SYMENT)
	if err != nil {
		return err
	}
	if !ok || entry != 24 || section.Entsize != 24 || section.Size%24 != 0 {
		return fmt.Errorf("engine: unsupported/inconsistent ELF symbol entry size")
	}
	var count uint64
	found := false
	for _, tag := range []elf.DynTag{elf.DT_HASH, elf.DT_GNU_HASH} {
		address, present, err := snapshotDynamicValue(f, tag)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		var current uint64
		if tag == elf.DT_HASH {
			current, err = snapshotSysVHashCount(f, address)
		} else {
			current, err = snapshotGNUHashCount(f, address)
		}
		if err != nil {
			return err
		}
		if found && current != count {
			return fmt.Errorf("engine: conflicting loader hash symbol counts")
		}
		count = current
		found = true
	}
	if !found || count == 0 {
		return fmt.Errorf("engine: complete loader symbol extent unavailable")
	}
	if section.Size != count*24 {
		return fmt.Errorf("engine: dynamic symbol extent mismatch: section=%d loader=%d", section.Size/24, count)
	}
	address, ok, err := snapshotDynamicValue(f, elf.DT_SYMTAB)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("engine: missing loader symbol table")
	}
	if _, err = snapshotLoadBytes(f, address, count*24); err != nil {
		return err
	}
	if versions := f.SectionByType(elf.SHT_GNU_VERSYM); versions != nil && (versions.Size != count*2 || versions.Entsize != 2) {
		return fmt.Errorf("engine: incomplete loader symbol version table")
	}
	for _, kind := range []struct {
		address, size, entry elf.DynTag
		width                uint64
	}{{elf.DT_RELA, elf.DT_RELASZ, elf.DT_RELAENT, 24}, {elf.DT_REL, elf.DT_RELSZ, elf.DT_RELENT, 16}} {
		addr, present, err := snapshotDynamicValue(f, kind.address)
		if err != nil {
			return err
		}
		size, sizePresent, err := snapshotDynamicValue(f, kind.size)
		if err != nil {
			return err
		}
		entry, entryPresent, err := snapshotDynamicValue(f, kind.entry)
		if err != nil {
			return err
		}
		if !present && !sizePresent && !entryPresent {
			continue
		}
		if !present || !sizePresent || !entryPresent || entry != kind.width {
			return fmt.Errorf("engine: incomplete loader relocation metadata")
		}
		if err = snapshotRelocationSymbols(f, addr, size, kind.width, count); err != nil {
			return err
		}
	}
	plt, present, err := snapshotDynamicValue(f, elf.DT_JMPREL)
	if err != nil {
		return err
	}
	size, sizePresent, err := snapshotDynamicValue(f, elf.DT_PLTRELSZ)
	if err != nil {
		return err
	}
	kind, kindPresent, err := snapshotDynamicValue(f, elf.DT_PLTREL)
	if err != nil {
		return err
	}
	if present || sizePresent || kindPresent {
		if !present || !sizePresent || !kindPresent {
			return fmt.Errorf("engine: incomplete loader PLT relocation metadata")
		}
		width := uint64(0)
		switch elf.DynTag(kind) {
		case elf.DT_RELA:
			width = 24
		case elf.DT_REL:
			width = 16
		default:
			return fmt.Errorf("engine: unsupported loader PLT relocation format")
		}
		if err = snapshotRelocationSymbols(f, plt, size, width, count); err != nil {
			return err
		}
	}
	return nil
}
func snapshotRelocationSymbols(f *elf.File, addr, size, width, count uint64) error {
	if size%width != 0 {
		return fmt.Errorf("engine: incomplete loader relocation entries")
	}
	entries, err := snapshotLoadBytes(f, addr, size)
	if err != nil {
		return err
	}
	for off := uint64(0); off < size; off += width {
		symbol := binary.LittleEndian.Uint64(entries[off+8:]) >> 32
		if symbol >= count {
			return fmt.Errorf("engine: loader relocation symbol %d outside complete table %d", symbol, count)
		}
	}
	return nil
}
