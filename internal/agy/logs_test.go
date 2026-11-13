package agy

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeLog(t *testing.T, path, text string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// Lines as agy 1.5.3 writes them; the 403 body is shortened.
const workerLog = `I1113 08:51:32.100000      46 server.go:313] Creating CLI server backend
W1113 08:51:32.200000     114 cache.go:157] Cache(loadCodeAssistResponse): Singleflight refresh failed: PERMISSION_DENIED (code 403): Verify your account to continue.
I1113 08:51:32.578620     103 http_helpers.go:820] Failed to make code assist backend request (http://127.0.0.1:41319/main/v1internal:loadCodeAssist): {"error": {"code": 403,
  "details": [{"metadata": {"validation_url": "https://accounts.google.com/signin/continue?plt=X"}}]}}
E1113 08:51:33.000000     103 errorreport.go:273] PERMISSION_DENIED (code 403): Verify your account to continue.
`

func TestReadLogsSelectsEntries(t *testing.T) {
	dir := t.TempDir()
	older := time.Now().Add(-time.Hour)
	writeLog(t, filepath.Join(dir, logDir, "acp-front-20261113_085132-999999998.log"), "I1113 08:51:31.000000       1 options.go:467] Starting ACP front\n", older)
	writeLog(t, filepath.Join(dir, logDir, "acp-worker-20261113_085132-999999999.log"), "panic: boom\ngoroutine 1\n"+workerLog, time.Now())
	writeLog(t, filepath.Join(dir, crashDir, "crash_6123_77c67d78-d233-4ccb-aeb2-c98795a40958.log"), "", older.Add(-time.Hour))

	files, err := LogFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, f := range files {
		kinds = append(kinds, f.Kind)
	}
	if !slices.Equal(kinds, []string{"worker", "front", "crash"}) || files[0].PID != 999999999 || files[0].Running || files[2].PID != 6123 {
		t.Fatalf("files = %+v", files)
	}

	all, err := ReadLogs(dir, LogQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if all.Matched != 6 || all.Entries[0].File != files[1].Name || all.Entries[1].Text != "panic: boom\ngoroutine 1" || all.Entries[1].Level != "" {
		t.Fatalf("all = %+v", all)
	}

	link, err := ReadLogs(dir, LogQuery{Contains: "VALIDATION_URL", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if link.Matched != 1 || !strings.HasSuffix(link.Entries[0].Text, `"https://accounts.google.com/signin/continue?plt=X"}}]}}`) {
		t.Fatalf("a multi-line entry is searched whole: %+v", link)
	}

	warnings, err := ReadLogs(dir, LogQuery{File: files[0].Name, MinLevel: "W", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if warnings.Matched != 2 || len(warnings.Entries) != 1 || warnings.Entries[0].Level != "E" {
		t.Fatalf("the newest of the W+ entries: %+v", warnings)
	}

	if _, err := ReadLogs(dir, LogQuery{File: "../../etc/passwd", Limit: 1}); !errors.Is(err, ErrNoLogFile) {
		t.Fatalf("a name outside the listing: %v", err)
	}
}

func TestReadLogsReadsTheEndOfLongFiles(t *testing.T) {
	dir := t.TempDir()
	line := "I1113 08:51:32.100000      46 server.go:313] " + strings.Repeat("x", 99) + "\n"
	text := strings.Repeat(line, maxLogRead/len(line)+10) + "E1113 08:51:33.000000     103 errorreport.go:273] last\n"
	writeLog(t, filepath.Join(dir, logDir, "acp-worker-20261113_085132-999999999.log"), text, time.Now())

	got, err := ReadLogs(dir, LogQuery{Limit: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Partial) != 1 || got.Entries[len(got.Entries)-1].Text != "E1113 08:51:33.000000     103 errorreport.go:273] last" {
		t.Fatalf("partial %v, last %q", got.Partial, got.Entries[len(got.Entries)-1].Text)
	}
	if got.Entries[0].Text != strings.TrimSuffix(line, "\n") {
		t.Fatalf("the read starts at a whole line: %q", got.Entries[0].Text)
	}
}
