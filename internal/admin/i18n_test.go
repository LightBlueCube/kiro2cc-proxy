package admin

import (
	"encoding/json"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// UI text keys in the static app: t("key") / tx("key") calls, lookup("group",
// value) groups, and data-i18n attributes. The catalogs must hold exactly the
// keys used, so every key passed to t and tx must be a literal.
var (
	textCall     = regexp.MustCompile(`\btx?\(`)
	textCallKey  = regexp.MustCompile(`^\btx?\("([^"]+)"`)
	lookupCall   = regexp.MustCompile(`\blookup\(`)
	lookupGroup  = regexp.MustCompile(`^\blookup\("([^"]+)"`)
	pageKey      = regexp.MustCompile(`data-i18n="([^"]+)"`)
	placeholders = regexp.MustCompile(`\{(\w+)\}`)
)

var pluralForms = []string{"zero", "one", "two", "few", "many", "other"}

func readStatic(t *testing.T, name string) string {
	t.Helper()
	raw, err := fs.ReadFile(staticFiles, path.Join("static", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// scanCalls returns the literal first arguments of the calls call matches
// in src; with strict, a call without a literal key fails the test.
func scanCalls(t *testing.T, file, src string, call, literal *regexp.Regexp, strict bool) []string {
	t.Helper()
	var keys []string
	for _, loc := range call.FindAllStringIndex(src, -1) {
		if strings.HasSuffix(src[:loc[0]], "function ") {
			continue
		}
		m := literal.FindStringSubmatch(src[loc[0]:])
		if m == nil {
			if strict {
				line := strings.Count(src[:loc[0]], "\n") + 1
				t.Errorf("%s:%d: %s without a literal key", file, line, strings.TrimSuffix(src[loc[0]:loc[1]], "("))
			}
			continue
		}
		keys = append(keys, m[1])
	}
	return keys
}

// catalog loads locales/<lang>.json as key → forms (one form for plain text).
func catalog(t *testing.T, lang string) map[string]map[string]string {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readStatic(t, "locales/"+lang+".json")), &raw); err != nil {
		t.Fatalf("%s.json: %v", lang, err)
	}
	out := make(map[string]map[string]string, len(raw))
	for key, value := range raw {
		var text string
		if json.Unmarshal(value, &text) == nil {
			out[key] = map[string]string{"": text}
			continue
		}
		var forms map[string]string
		if err := json.Unmarshal(value, &forms); err != nil {
			t.Errorf("%s.json %s: neither text nor plural forms: %v", lang, key, err)
			continue
		}
		if _, ok := forms["other"]; !ok {
			t.Errorf("%s.json %s: plural forms without other", lang, key)
		}
		for form := range forms {
			if !slices.Contains(pluralForms, form) {
				t.Errorf("%s.json %s: unknown plural form %q", lang, key, form)
			}
		}
		out[key] = forms
	}
	return out
}

func placeholderSet(forms map[string]string) []string {
	var names []string
	for _, text := range forms {
		for _, m := range placeholders.FindAllStringSubmatch(text, -1) {
			names = append(names, m[1])
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func TestUITextKeys(t *testing.T) {
	app := readStatic(t, "app.js")
	used := map[string]bool{}
	for _, key := range scanCalls(t, "app.js", app, textCall, textCallKey, true) {
		used[key] = true
	}
	// i18n.js passes the page's data-i18n keys on, checked below.
	for _, key := range scanCalls(t, "i18n.js", readStatic(t, "i18n.js"), textCall, textCallKey, false) {
		used[key] = true
	}
	for _, m := range pageKey.FindAllStringSubmatch(readStatic(t, "index.html"), -1) {
		used[m[1]] = true
	}
	groups := scanCalls(t, "app.js", app, lookupCall, lookupGroup, true)

	en := catalog(t, "en")
	for key := range used {
		if _, ok := en[key]; !ok {
			t.Errorf("en.json lacks %q", key)
		}
	}
	for key := range en {
		looked := slices.ContainsFunc(groups, func(g string) bool { return strings.HasPrefix(key, g+".") })
		if !used[key] && !looked {
			t.Errorf("en.json has unused %q", key)
		}
	}
}

// TestUITextCatalogs checks that every catalog has exactly English's keys
// with the same placeholders, since the app has no per-key fallback.
func TestUITextCatalogs(t *testing.T) {
	entries, err := fs.ReadDir(staticFiles, "static/locales")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		files = append(files, `"`+strings.TrimSuffix(entry.Name(), ".json")+`"`)
	}
	languages := regexp.MustCompile(`const LANGUAGES = \[([^\]]*)\]`).FindStringSubmatch(readStatic(t, "i18n.js"))
	if languages == nil {
		t.Fatal("i18n.js: no LANGUAGES list")
	}
	listed := strings.Split(strings.ReplaceAll(languages[1], " ", ""), ",")
	if listed[0] != `"en"` || !slices.Equal(slices.Sorted(slices.Values(listed)), files) {
		t.Errorf("i18n.js LANGUAGES %v must start with en and match locales/ %v", listed, files)
	}

	en := catalog(t, "en")
	for _, entry := range entries {
		lang := strings.TrimSuffix(entry.Name(), ".json")
		if lang == "en" {
			continue
		}
		other := catalog(t, lang)
		if missing := slices.DeleteFunc(slices.Collect(maps.Keys(en)), func(k string) bool { _, ok := other[k]; return ok }); len(missing) > 0 {
			slices.Sort(missing)
			t.Errorf("%s.json lacks %v", lang, missing)
		}
		for key, forms := range other {
			base, ok := en[key]
			if !ok {
				t.Errorf("%s.json has %q, which en.json does not", lang, key)
				continue
			}
			if got, want := placeholderSet(forms), placeholderSet(base); !slices.Equal(got, want) {
				t.Errorf("%s.json %s: placeholders %v, en.json has %v", lang, key, got, want)
			}
		}
	}
}
