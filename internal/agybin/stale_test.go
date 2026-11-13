package agybin

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRemoveStale(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"agy-aaaa-1111", "agy-aaaa-1111.sha256", "agy-aaaa-2222", "agy-aaaa-2222.sha256", "agy-aaaa-1111.tmp123", "notes"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveStale(dir, filepath.Join(dir, "agy-aaaa-1111")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"agy-aaaa-1111", "agy-aaaa-1111.sha256", "notes"}; !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
}
