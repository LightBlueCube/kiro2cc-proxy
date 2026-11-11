package agybin

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestPatternFind(t *testing.T) {
	p := mustPattern("48 ?? d2")
	got := p.find([]byte{0x48, 0x85, 0xd2, 0x48, 0x00, 0xd2, 0x48, 0x85})
	if len(got) != 2 || got[0] != 0 || got[1] != 3 {
		t.Fatalf("matches %v, want [0 3]", got)
	}
}

func TestJccToJmpKeepsTarget(t *testing.T) {
	// test rdx,rdx; jle +0x10; then filler.
	body := append(mustBytes("48 85 d2 0f 8e 10 00 00 00"), make([]byte, 0x20)...)
	if err := jccToJmp(3)(nil, function{body: body}, 0); err != nil {
		t.Fatal(err)
	}
	if body[3] != 0xe9 || body[8] != 0x90 {
		t.Fatalf("bytes % x", body[:9])
	}
	// jle target: 3+6+0x10; jmp target: 3+5+rel.
	if rel := int32(binary.LittleEndian.Uint32(body[4:])); 3+5+int(rel) != 3+6+0x10 {
		t.Fatalf("jmp rel %d lands elsewhere", rel)
	}
}

func TestJccToJmpRejectsOutsideTarget(t *testing.T) {
	body := mustBytes("48 85 d2 0f 8e 00 01 00 00")
	if err := jccToJmp(3)(nil, function{body: body}, 0); err == nil {
		t.Fatal("want an error for a target outside the function")
	}
}

// TestPinnedBuild runs against a real agy: AGYBIN_TEST_BIN=/path/antigravity.
func TestPinnedBuild(t *testing.T) {
	src := os.Getenv("AGYBIN_TEST_BIN")
	if src == "" {
		t.Skip("AGYBIN_TEST_BIN not set")
	}
	dir := t.TempDir()
	path, report, err := Derive(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range report.Sites {
		if s.Matches != 1 || s.Err != nil {
			t.Errorf("%s %s: %d matches, %v", s.Patch, s.Function, s.Matches, s.Err)
		}
	}
	original, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(derived) != len(original) || bytes.Equal(derived, original) {
		t.Fatal("derived copy must differ from the source in content only")
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	before := stat.ModTime()
	again, _, err := Derive(src, dir)
	if err != nil || again != path {
		t.Fatalf("second derive: %s, %v", again, err)
	}
	if stat, _ := os.Stat(again); !stat.ModTime().Equal(before) {
		t.Fatal("an intact cached copy must be reused, not rewritten")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("cache holds %d files, want the binary and its .sha256", len(entries))
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("derived %s outside %s", path, dir)
	}
}
