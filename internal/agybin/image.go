package agybin

import (
	"bytes"
	"debug/elf"
	"debug/gosym"
	"errors"
	"fmt"
)

// pclntabMagic opens a Go 1.20+ function table: magic, two pad bytes,
// instruction size quantum 1, pointer size 8.
var pclntabMagic = []byte{0xf1, 0xff, 0xff, 0xff, 0x00, 0x00, 0x01, 0x08}

// textAnchor is a function exported in the dynamic symbol table of every
// cgo binary. Its address calibrates the function table, whose own text
// start field is 0 in agy.
const textAnchor = "crosscall2"

// image is a linux/amd64 Go executable held in memory. Function bodies are
// views into data, so rewriting a body rewrites the image.
type image struct {
	data      []byte
	file      *elf.File
	table     *gosym.Table
	textStart uint64
}

// function is one Go function of the image.
type function struct {
	name string
	addr uint64
	body []byte
}

func loadImage(data []byte) (*image, error) {
	file, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("not an ELF executable: %w", err)
	}
	if file.Class != elf.ELFCLASS64 || file.Machine != elf.EM_X86_64 {
		return nil, fmt.Errorf("unsupported executable: %v %v, want ELFCLASS64 EM_X86_64", file.Class, file.Machine)
	}
	pclntab, err := findPclntab(file, data)
	if err != nil {
		return nil, err
	}
	table, err := gosym.NewTable(nil, gosym.NewLineTable(pclntab, 0))
	if err != nil {
		return nil, fmt.Errorf("go function table: %w", err)
	}
	im := &image{data: data, file: file, table: table}
	if im.textStart, err = im.calibrate(); err != nil {
		return nil, err
	}
	return im, nil
}

// findPclntab returns the function table of the main program. agy also
// embeds a second Go program as data, so tables in writable segments are
// not the executable's own.
func findPclntab(file *elf.File, data []byte) ([]byte, error) {
	var found []int
	for _, prog := range file.Progs {
		if prog.Type != elf.PT_LOAD || prog.Flags&elf.PF_W != 0 {
			continue
		}
		segment := data[prog.Off : prog.Off+prog.Filesz]
		for from := 0; ; {
			at := bytes.Index(segment[from:], pclntabMagic)
			if at < 0 {
				break
			}
			found = append(found, int(prog.Off)+from+at)
			from += at + 1
		}
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("found %d Go function tables in read-only segments, want 1", len(found))
	}
	return data[found[0]:], nil
}

func (im *image) calibrate() (uint64, error) {
	symbols, err := im.file.DynamicSymbols()
	if err != nil {
		return 0, fmt.Errorf("dynamic symbols: %w", err)
	}
	fn := im.table.LookupFunc(textAnchor)
	if fn == nil {
		return 0, fmt.Errorf("%s missing from the Go function table", textAnchor)
	}
	for _, sym := range symbols {
		if sym.Name == textAnchor && elf.ST_TYPE(sym.Info) == elf.STT_FUNC {
			return sym.Value - fn.Entry, nil
		}
	}
	return 0, fmt.Errorf("%s missing from the dynamic symbols", textAnchor)
}

var errNoFunction = errors.New("function not found")

// function looks up a function by its full package-qualified name.
func (im *image) function(name string) (function, error) {
	fn := im.table.LookupFunc(name)
	if fn == nil {
		return function{}, errNoFunction
	}
	addr, end := im.textStart+fn.Entry, im.textStart+fn.End
	for _, prog := range im.file.Progs {
		inside := prog.Type == elf.PT_LOAD && prog.Flags&elf.PF_X != 0 &&
			addr >= prog.Vaddr && end <= prog.Vaddr+prog.Filesz
		if !inside {
			continue
		}
		off := prog.Off + addr - prog.Vaddr
		return function{name: name, addr: addr, body: im.data[off : off+end-addr]}, nil
	}
	return function{}, fmt.Errorf("function %#x-%#x is outside executable segments", addr, end)
}
