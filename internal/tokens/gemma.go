package tokens

import (
	"bytes"
	_ "embed"
	"strings"

	"github.com/eliben/go-sentencepiece"
)

// gemmaModel is Gemma 3's SentencePiece model (Apache-2.0, see
// gemma3.spiece.model.LICENSE), from github.com/google/gemma_pytorch
// tokenizer/gemma3_cleaned_262144_v2.spiece.model.
//
//go:embed gemma3.spiece.model
var gemmaModel []byte

// maxMemoBytes bounds the chunks one Count call remembers; longer chunks
// rarely repeat.
const maxMemoBytes = 64

type gemma struct{ sp *sentencepiece.Processor }

func loadGemma() (*gemma, error) {
	sp, err := sentencepiece.NewProcessor(bytes.NewReader(gemmaModel))
	if err != nil {
		return nil, err
	}
	return &gemma{sp: sp}, nil
}

func (g *gemma) Name() string { return "gemma3" }

func (g *gemma) Image(string) int { return flatImageTokens }

// Count encodes the text in chunks: it splits before every whitespace run
// that follows a non-space character. Gemma 3 has no piece with
// whitespace after a non-space character except ">▁</", so no merge
// crosses such a split (except after '>', where it does not split) and
// the sum equals the whole text's count; on 3.8 MB of English, Chinese,
// code, HTML, JSON, and CRLF text it matched exactly. Chunks make the
// merge queues small (about 3× faster) and let repeated words be counted
// once.
func (g *gemma) Count(text string) (int, error) {
	text = strings.ReplaceAll(text, " ", "▁")
	memo := map[string]int{}
	total, start := 0, 0
	prev := rune(-1)
	for i, r := range text {
		split := isGemmaSpace(r) && prev >= 0 && !isGemmaSpace(prev) && prev != '>'
		if split {
			total += g.countChunk(text[start:i], memo)
			start = i
		}
		prev = r
	}
	return total + g.countChunk(text[start:], memo), nil
}

// isGemmaSpace reports whether the vocabulary treats r as whitespace.
func isGemmaSpace(r rune) bool {
	return r == '▁' || r == '\n' || r == '\t' || r == '\r'
}

func (g *gemma) countChunk(chunk string, memo map[string]int) int {
	if chunk == "" {
		return 0
	}
	if n, ok := memo[chunk]; ok {
		return n
	}
	n := len(g.sp.Encode(chunk))
	if len(chunk) <= maxMemoBytes {
		memo[chunk] = n
	}
	return n
}
