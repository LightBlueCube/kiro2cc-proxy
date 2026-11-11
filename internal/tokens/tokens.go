// Package tokens counts prompt tokens the way upstream models count them,
// for agyrelay's own context-window check.
package tokens

import (
	"encoding/base64"
	"image"
	_ "image/gif" // image.DecodeConfig formats
	_ "image/jpeg"
	_ "image/png"
	"strings"

	"github.com/tiktoken-go/tokenizer/codec"
)

// Counter counts prompt parts the way one model family does.
type Counter interface {
	// Count counts the tokens of a text.
	Count(text string) (int, error)
	// Image counts an inline image from its base64 data.
	Image(base64Data string) int
	// Name identifies the counting, e.g. for caches.
	Name() string
}

const (
	// claudePerO200k scales o200k_base counts to Claude's. Measured on
	// claude-sonnet-5-5 and claude-opus-5-5 (equal counts) over 20k- and
	// 40k-character samples: English 1.55-1.57, Chinese 1.48-1.49, Go
	// 1.64-1.66, JSON 1.61. Rounded up so the estimate stays above.
	claudePerO200k = 1.7

	// flatImageTokens is what an image costs where its size does not
	// matter: Gemini 3.8 Flash counted 1202-1204 for 512², 1024², and
	// 2048² PNGs; gpt-oss counted 165.
	flatImageTokens = 1300

	// Claude grows with the pixels: 489 for 512², 1501 for 1024², 4890 for
	// 2048² (Anthropic documents width × height / 750, downscaled past a
	// limit). Counted as pixels / 750 + 200 without the downscaling.
	claudePixelsPerToken = 750
	claudeImageExtra     = 200
	// claudeUnknownImagePixels is assumed when the image's size cannot be
	// read (an unsupported format, e.g. WebP): 2048².
	claudeUnknownImagePixels = 2048 * 2048
)

// Set holds the loaded tokenizers; it is safe for concurrent use.
type Set struct {
	gemma  *gemma
	o200k  o200k
	claude claude
}

// Load loads the tokenizers: about 0.2 s and 55 MiB of heap.
func Load() (*Set, error) {
	g, err := loadGemma()
	if err != nil {
		return nil, err
	}
	// codec.NewO200kBase rather than tokenizer.Get, which links every
	// vocabulary into the binary.
	o := o200k{codec.NewO200kBase()}
	return &Set{gemma: g, o200k: o, claude: claude{base: o}}, nil
}

// For returns the counter for a model id. Measured on 2026-11-11 against
// the prompt tokens agy reported for samples of English, Chinese, Go, and
// JSON text:
//
//	gemini-3.8-flash  Gemma 3's tokenizer, exact (within 2 tokens)
//	gemini-4-argon    0-4% fewer than Gemma 3's tokenizer
//	gpt-oss-120b      o200k_base, exact
//	claude-*          1.48-1.66 × o200k_base, counted as 1.7 ×
//
// Other models count like Claude, the most tokens per character and
// image.
func (s *Set) For(modelID string) Counter {
	switch {
	case strings.HasPrefix(modelID, "gemini-"):
		return s.gemma
	case strings.HasPrefix(modelID, "gpt-oss-"):
		return s.o200k
	default:
		return s.claude
	}
}

type o200k struct{ codec *codec.Codec }

func (o o200k) Count(text string) (int, error) { return o.codec.Count(text) }
func (o o200k) Image(string) int               { return flatImageTokens }
func (o o200k) Name() string                   { return "o200k" }

// claude scales o200k_base counts.
type claude struct{ base o200k }

func (c claude) Count(text string) (int, error) {
	n, err := c.base.Count(text)
	if err != nil {
		return 0, err
	}
	return int(float64(n)*claudePerO200k + 0.999), nil
}

func (c claude) Image(base64Data string) int {
	pixels := claudeUnknownImagePixels
	config, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(base64Data)))
	if err == nil {
		pixels = config.Width * config.Height
	}
	return (pixels+claudePixelsPerToken-1)/claudePixelsPerToken + claudeImageExtra
}

func (c claude) Name() string { return "claude" }
