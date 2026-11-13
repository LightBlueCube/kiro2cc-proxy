package agy

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// LogFile is one of agy's process logs or crash reports.
type LogFile struct {
	Name string
	// Kind is the process kind of a log ("front", "worker", "server",
	// "login"), "crash" for a crash report, "" for any other file.
	Kind string
	// PID is the process the log belongs to, 0 when the name has none.
	PID      int
	Running  bool
	Size     int64
	Modified time.Time

	dir string
}

// LogQuery selects log entries.
type LogQuery struct {
	// File is one file's name; "" searches them all.
	File string
	// MinLevel is the lowest glog severity shown ("I", "W", "E", "F");
	// "" shows every entry, the lines before a file's first entry too.
	MinLevel string
	// Contains keeps entries holding this text, ignoring case.
	Contains string
	// Limit (> 0) is how many of the newest matching entries are returned.
	Limit int
}

// LogEntry is one glog entry: its header line and the lines that follow
// it up to the next header.
type LogEntry struct {
	File  string
	Level string
	Text  string
}

// LogResult is the newest matching entries, oldest first.
type LogResult struct {
	Entries []LogEntry
	// Matched counts every matching entry, also those beyond Limit.
	Matched int
	// Partial names the files only the end of which was read.
	Partial []string
}

// ErrNoLogFile means the queried file is not among the account's logs.
var ErrNoLogFile = errors.New("no such agy log file")

// ErrLogLevel means the query's MinLevel is not a glog severity.
var ErrLogLevel = errors.New("level must be one of I, W, E, F")

// maxLogRead bounds how much of one file is read; longer files are read
// from that far before their end.
const maxLogRead = 4 << 20

// glogHeader starts a glog entry: "E1113 08:51:32.578620     103 file.go:820] ".
var glogHeader = regexp.MustCompile(`^([IWEF])\d{4} \d{2}:\d{2}:\d{2}\.\d{6}\s+\d+ \S+:\d+\] `)

var logKind = regexp.MustCompile(`^acp-([a-z]+)-`)

// crashName is a crash report, crash_<pid>_<uuid>.log.
var crashName = regexp.MustCompile(`^crash_(\d+)_`)

const glogSeverities = "IWEF"

// LogFiles lists agy's logs and crash reports below geminiDir, newest
// first.
func LogFiles(geminiDir string) ([]LogFile, error) {
	files := []LogFile{}
	for _, dir := range []string{logDir, crashDir} {
		crashes := dir == crashDir
		entries, err := os.ReadDir(filepath.Join(geminiDir, dir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue // pruned meanwhile
			}
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			files = append(files, describeLog(filepath.Join(geminiDir, dir), info, crashes))
		}
	}
	slices.SortFunc(files, func(a, b LogFile) int { return b.Modified.Compare(a.Modified) })
	return files, nil
}

func describeLog(dir string, info fs.FileInfo, crashes bool) LogFile {
	f := LogFile{Name: info.Name(), Size: info.Size(), Modified: info.ModTime(), dir: dir}
	switch {
	case crashes:
		f.Kind = "crash"
		if m := crashName.FindStringSubmatch(f.Name); m != nil {
			f.PID, _ = strconv.Atoi(m[1])
		}
	case logName.MatchString(f.Name):
		f.Kind = logKind.FindStringSubmatch(f.Name)[1]
		f.PID, _ = strconv.Atoi(logName.FindStringSubmatch(f.Name)[1])
		f.Running = ofRunningProcess(f.Name)
	}
	return f
}

// ReadLogs returns the entries of agy's logs below geminiDir that q
// selects. Files are read oldest first, so the newest entry comes last.
func ReadLogs(geminiDir string, q LogQuery) (LogResult, error) {
	if q.MinLevel != "" && (len(q.MinLevel) != 1 || !strings.Contains(glogSeverities, q.MinLevel)) {
		return LogResult{}, ErrLogLevel
	}
	files, err := LogFiles(geminiDir)
	if err != nil {
		return LogResult{}, err
	}
	if q.File != "" {
		i := slices.IndexFunc(files, func(f LogFile) bool { return f.Name == q.File })
		if i < 0 {
			return LogResult{}, ErrNoLogFile
		}
		files = files[i : i+1]
	}
	slices.Reverse(files)

	result := LogResult{Entries: []LogEntry{}, Partial: []string{}}
	match := entryFilter(q)
	for _, f := range files {
		partial, err := scanLog(f, func(e LogEntry) {
			if !match(e) {
				return
			}
			result.Matched++
			result.Entries = append(result.Entries, e)
			if len(result.Entries) > q.Limit {
				result.Entries = result.Entries[1:]
			}
		})
		if errors.Is(err, fs.ErrNotExist) {
			continue // pruned meanwhile
		}
		if err != nil {
			return LogResult{}, err
		}
		if partial {
			result.Partial = append(result.Partial, f.Name)
		}
	}
	return result, nil
}

func entryFilter(q LogQuery) func(LogEntry) bool {
	needle := strings.ToLower(q.Contains)
	return func(e LogEntry) bool {
		if q.MinLevel != "" && strings.Index(glogSeverities, e.Level) < strings.Index(glogSeverities, q.MinLevel) {
			return false
		}
		return needle == "" || strings.Contains(strings.ToLower(e.Text), needle)
	}
}

// scanLog passes each entry of f to emit and reports whether only the end
// of the file was read.
func scanLog(f LogFile, emit func(LogEntry)) (bool, error) {
	file, err := os.Open(filepath.Join(f.dir, f.Name))
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	reader := bufio.NewReader(file)
	partial := info.Size() > maxLogRead
	if partial {
		if _, err := file.Seek(info.Size()-maxLogRead, io.SeekStart); err != nil {
			return false, err
		}
		reader.Reset(file)
		// The read starts inside a line.
		if _, err := reader.ReadString('\n'); err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
	}

	// A file's lines before its first header form an entry of level "".
	var text strings.Builder
	level := ""
	flush := func() {
		if text.Len() > 0 {
			emit(LogEntry{File: f.Name, Level: level, Text: strings.TrimSuffix(text.String(), "\n")})
		}
		text.Reset()
	}
	for {
		line, err := reader.ReadString('\n')
		if m := glogHeader.FindStringSubmatch(line); m != nil {
			flush()
			level = m[1]
		}
		text.WriteString(line)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, err
		}
	}
	flush()
	return partial, nil
}
