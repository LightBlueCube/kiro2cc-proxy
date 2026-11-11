package tokens

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"sync"
	"testing"
)

var samples = []string{
	"Hello, world!",
	"func main() {\n\tfmt.Println(\"hi\")\r\n}\n\n\n    indented   spaces\t\ttabs",
	"<div>\n  <p>a</p> </div><a> </a>> </x>",
	"中华人民共和国是位于东亚的国家。 混合 English 和中文。",
	`{"key": [1, 2, 3], "nested": {"a": "b  c"}}`,
	"   leading spaces and trailing   ",
	"emoji 🙂🚀 and accents: café naïve",
	"",
}

func TestGemmaChunksMatchWholeText(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(samples, "\n")
	for _, text := range append(samples, all, strings.Repeat(all, 50)) {
		got, err := set.gemma.Count(text)
		if err != nil {
			t.Fatal(err)
		}
		if want := len(set.gemma.sp.Encode(text)); got != want {
			t.Errorf("Count(%.40q) = %d, whole text encodes to %d", text, got, want)
		}
	}
}

func TestFor(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{
		"gemini-3.8-flash": "gemma3", "gemini-4-argon": "gemma3", "gpt-oss-120b": "o200k",
		"claude-opus-5-5": "claude", "some-new-model": "claude",
	} {
		if got := set.For(model).Name(); got != want {
			t.Errorf("For(%s) = %s, want %s", model, got, want)
		}
	}

	text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20)
	o, err := set.o200k.Count(text)
	if err != nil {
		t.Fatal(err)
	}
	c, err := set.claude.Count(text)
	if err != nil {
		t.Fatal(err)
	}
	if want := int(float64(o)*claudePerO200k + 0.999); c != want || c <= o {
		t.Fatalf("claude = %d, want %d (o200k %d)", c, want, o)
	}
}

func TestImages(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1024, 1024))); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())

	// Measured: 1501 tokens on Claude, 1203 on Gemini.
	if got, want := set.claude.Image(data), 1024*1024/750+1+claudeImageExtra; got != want {
		t.Errorf("claude 1024² = %d, want %d", got, want)
	}
	if got, want := set.claude.Image("UklGRg=="), claudeUnknownImagePixels/750+1+claudeImageExtra; got != want {
		t.Errorf("claude unreadable = %d, want %d", got, want)
	}
	if got := set.gemma.Image(data); got != flatImageTokens {
		t.Errorf("gemma = %d", got)
	}
}

func TestConcurrentCounts(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(samples, "\n")
	want := map[string]int{}
	for _, model := range []string{"gemini-3.8-flash", "gpt-oss-120b", "claude-opus-5-5"} {
		n, err := set.For(model).Count(text)
		if err != nil {
			t.Fatal(err)
		}
		want[model] = n
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for model, n := range want {
				if got, err := set.For(model).Count(text); err != nil || got != n {
					t.Errorf("%s: %d, %v; want %d", model, got, err, n)
				}
			}
		})
	}
	wg.Wait()
}
