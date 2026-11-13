// Package agybin derives the agy executable the relay runs: a pinned agy
// release with the relay's patches applied, cached under the data
// directory. Every patch locates its function by name in the Go function
// table and its instructions by a signature that must match exactly once,
// so a changed build fails instead of being patched at the wrong place.
package agybin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Build is an agy release the patches are verified against.
type Build struct {
	Version string
	SHA256  string
}

// pinned lists the accepted agy executables (linux/amd64).
var pinned = []Build{
	{Version: "1.5.3", SHA256: "7ca212748ca0dfda718efbc39a776a104472e7e9344d4b6ea147c543684f96a3"},
}

// SiteResult is the outcome of one patch site.
type SiteResult struct {
	Patch    string
	Function string
	// Matches counts the signature inside the function; -1 when the
	// function is missing.
	Matches int
	Err     error
}

// Report describes an agy executable and how the patches apply to it.
type Report struct {
	SHA256 string
	// Build is nil when the executable is not pinned.
	Build *Build
	Sites []SiteResult
}

// Version names the build for logs.
func (r *Report) Version() string {
	if r.Build == nil {
		return "unpinned"
	}
	return r.Build.Version
}

// Err is nil when the executable is pinned and every site patched.
func (r *Report) Err() error {
	var errs []error
	if r.Build == nil {
		var accepted []string
		for _, b := range pinned {
			accepted = append(accepted, b.Version+" "+b.SHA256)
		}
		errs = append(errs, fmt.Errorf("agy sha256 %s is not a pinned build (pinned: %s)", r.SHA256, strings.Join(accepted, ", ")))
	}
	for _, s := range r.Sites {
		if s.Err != nil {
			errs = append(errs, fmt.Errorf("patch %s in %s: %w", s.Patch, s.Function, s.Err))
		}
	}
	return errors.Join(errs...)
}

// Check reports how the patches apply to the executable at path, pinned or
// not. The error covers only unreadable or unsupported files.
func Check(path string) (*Report, error) {
	report, _, err := inspect(path)
	return report, err
}

// Derive returns the path of the patched copy of the pinned agy at src,
// writing it under dir unless an intact copy is there. src is never
// modified. On a failed check the report says which sites failed.
func Derive(src, dir string) (string, *Report, error) {
	report, derived, err := inspect(src)
	if err != nil {
		return "", nil, err
	}
	if err := report.Err(); err != nil {
		return "", report, err
	}
	sum := sha256.Sum256(derived)
	derivedSum := hex.EncodeToString(sum[:])
	path := filepath.Join(dir, fmt.Sprintf("agy-%s-%s", report.SHA256[:16], derivedSum[:16]))
	if intact(path, derivedSum) {
		return path, report, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", report, err
	}
	if err := writeAtomic(path, derived, 0o700); err != nil {
		return "", report, err
	}
	if err := writeAtomic(path+".sha256", []byte(derivedSum+"\n"), 0o600); err != nil {
		return "", report, err
	}
	return path, report, nil
}

// RemoveStale deletes the copies in dir other than current, left by other
// agy versions or patch sets (each about 250 MB), with their checksums.
func RemoveStale(dir, current string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		ours := strings.HasPrefix(entry.Name(), "agy-")
		if !ours || path == current || path == current+".sha256" {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// inspect reads the executable and patches it in memory.
func inspect(path string) (*Report, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(data)
	report := &Report{SHA256: hex.EncodeToString(sum[:])}
	for i := range pinned {
		if pinned[i].SHA256 == report.SHA256 {
			report.Build = &pinned[i]
		}
	}
	im, err := loadImage(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, p := range patches {
		for _, s := range p.sites {
			report.Sites = append(report.Sites, applySite(im, p.name, s))
		}
	}
	return report, data, nil
}

func applySite(im *image, patchName string, s site) SiteResult {
	result := SiteResult{Patch: patchName, Function: s.function, Matches: -1}
	fn, err := im.function(s.function)
	if err != nil {
		result.Err = err
		return result
	}
	matches := s.signature.find(fn.body)
	result.Matches = len(matches)
	switch {
	case len(matches) != 1:
		result.Err = fmt.Errorf("%d signature matches, want 1", len(matches))
	case s.atEntry && matches[0] != 0:
		result.Err = fmt.Errorf("signature matches at +%#x, want the function entry", matches[0])
	default:
		result.Err = s.rewrite(im, fn, matches[0])
	}
	return result
}

// intact reports whether path holds the derived executable: its recorded
// and actual hashes both equal sum.
func intact(path, sum string) bool {
	recorded, err := os.ReadFile(path + ".sha256")
	if err != nil || strings.TrimSpace(string(recorded)) != sum {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == sum
}

// writeAtomic replaces path with data: a reader sees the old file or the
// complete new one.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // fails harmlessly after the rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
