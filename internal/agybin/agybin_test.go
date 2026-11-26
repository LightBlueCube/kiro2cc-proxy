package agybin

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hexBytes(t *testing.T, spec string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(spec, " ", ""))
	if err != nil {
		t.Fatalf("bad hex %q: %v", spec, err)
	}
	return b
}

func TestJccToJmpKeepsTarget(t *testing.T) {
	// test rdx,rdx; jle +0x10; filler.
	body := append(hexBytes(t, "48 85 d2 0f 8e 10 00 00 00"), make([]byte, 0x20)...)
	b, ok := decodeJcc(body, 3, ccLE)
	if !ok {
		t.Fatal("decodeJcc did not match")
	}
	if err := rewriteJccToJmp(body, b); err != nil {
		t.Fatal(err)
	}
	if body[3] != opJmpRel32 || body[8] != opNOP {
		t.Fatalf("bytes % x", body[:9])
	}
	// jle target: 3+6+0x10; jmp target: 3+5+rel.
	if rel := int32(binary.LittleEndian.Uint32(body[4:])); 3+5+int(rel) != 3+6+0x10 {
		t.Fatalf("jmp rel %d lands elsewhere", rel)
	}
}

func TestJccToJmpRejectsOutsideTarget(t *testing.T) {
	body := hexBytes(t, "48 85 d2 0f 8e 00 01 00 00")
	b, ok := decodeJcc(body, 3, ccLE)
	if !ok {
		t.Fatal("decodeJcc did not match")
	}
	if err := rewriteJccToJmp(body, b); err == nil {
		t.Fatal("want an error for a target outside the function")
	}
}

func TestShortSymbolName(t *testing.T) {
	cases := map[string]string{
		"google3/third_party/jetski/cli/model/model.SelectorModel.offersContextWindow": "model.SelectorModel.offersContextWindow",
		"runtime.selectgo":              "runtime.selectgo",
		"google3/a/b.Fn[google3/c/d.T]": "b.Fn[google3/c/d.T]",
	}
	for in, want := range cases {
		if got := shortSymbolName(in); got != want {
			t.Errorf("shortSymbolName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOriginalSnippetsMatch verifies the structural locators match the exact
// agy 1.5.3 instruction snippets once. End-to-end compatibility across agy
// builds is verified against real executables (TestPinnedBuild / check-agy).
func TestOriginalSnippetsMatch(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		locate func(*image, function) ([]func() error, error)
	}{
		{
			name:   "mcp-call-timeout-call",
			body:   "48 b9 00 08 d6 e8 29 00 00 00",
			locate: locateImm64(defaultMCPCallTimeout.Nanoseconds(), patchedMCPCallTimeout.Nanoseconds()),
		},
		{
			name:   "mcp-call-timeout-error",
			body:   "48 ba 00 08 d6 e8 29 00 00 00",
			locate: locateImm64(defaultMCPCallTimeout.Nanoseconds(), patchedMCPCallTimeout.Nanoseconds()),
		},
		{
			name:   "full-tool-output",
			body:   "48 85 d2 0f 8e 05 00 00 00 90 90 90 90 90 c9 c3",
			locate: locateOutputTruncation,
		},
		{
			name:   "context-gate-1",
			body:   "49 89 e4 49 81 ec 68 3d 00 00 0f 82 00 00 00 00 c9 c3",
			locate: locateReturnTrue,
		},
		{
			name:   "context-gate-3a",
			body:   "81 fa 00 e8 03 00 be 00 e8 03 00 0f 42 f2",
			locate: locateClamp256K,
		},
		{
			name:   "context-gate-4a",
			body:   "83 f8 08 74 05 83 f8 13 75 08 90 90 90 90 90 90 90 90 48 89 c8 c9 c3",
			locate: locateUserInputMaxTokens,
		},
		{
			name:   "context-gate-4b",
			body:   "83 fa 0e 75 0a 48 8b bc 24 18 02 00 00 eb 11 83 fa 17 75 04 31 ff eb 08 48 8b bc 24 10 02 00 00 48 85 ff 7f 01 90 c3",
			locate: locateStepUserInputLimit,
		},
		{
			name:   "full-step-output",
			body:   "83 fa 0e 75 0a 48 8b bc 24 18 02 00 00 eb 11 83 fa 17 75 04 31 ff eb 08 48 8b bc 24 10 02 00 00 48 85 ff 7f 01 90 c3",
			locate: locateStepDefaultLimit,
		},
		{
			name:   "acp-media-mime",
			body:   "49 3b 66 10 76 3c 55 48 89 e5 48 83 ec 10 48 89 44 24 20 e8 48 00 00 00 90 48 83 fb 06 7c 1f 8b 08 0f b7 50 04 48 c1 e2 20 48 09 d1 48 ba 69 6d 61 67 65 2f 00 00 48 39 ca 0f 94 c0 eb 02 31 c0 c9 c3",
			locate: locateReturnTrue,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn := function{body: hexBytes(t, tc.body)}
			hits, err := tc.locate(nil, fn)
			if err != nil {
				t.Fatalf("locate error: %v", err)
			}
			if len(hits) != 1 {
				t.Fatalf("got %d matches, want 1", len(hits))
			}
			if err := hits[0](); err != nil {
				t.Fatalf("rewrite error: %v", err)
			}
		})
	}
}

func TestMatchGateKindsAroundConcat(t *testing.T) {
	// Exact snippet from agy 1.5.3 customizations.gateKinds:
	// test rcx,rcx; je +0x39; xor eax,eax; lea rbx,[rip+...]; mov ecx,0x1a;
	// mov rdi,[rsp+0x50]; mov rsi,[rsp+0x30]; call concatstring2 (at offset 0x1d);
	// ...; call rdx; test al,al.
	body := hexBytes(t,
		"48 85 c9 74 39 "+
			"31 c0 "+
			"48 8d 1d 00 00 00 00 "+
			"b9 1a 00 00 00 "+
			"48 8b 7c 24 50 "+
			"48 8b 74 24 30 "+
			"e8 00 00 00 00 "+
			"48 8b 94 24 00 01 00 00 "+
			"48 8b 52 20 "+
			"48 89 d9 "+
			"48 89 c3 "+
			"48 8b 84 24 08 01 00 00 "+
			"ff d2 "+
			"84 c0",
	)
	const concatCallAt = 0x1d
	hits := matchGateKindsAroundConcat(body, concatCallAt)
	if len(hits) != 1 {
		t.Fatalf("got %d matches, want 1", len(hits))
	}
	if err := hits[0](); err != nil {
		t.Fatalf("rewrite error: %v", err)
	}
	if body[3] != opJmpRel8 || body[4] != 0x39 {
		t.Fatalf("rewritten branch = %x %x, want eb 39", body[3], body[4])
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
	entries, err := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("cache holds %d files, want the binary and its .sha256", len(entries))
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("derived %s outside %s", path, dir)
	}
}
