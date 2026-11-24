// Package openai implements the OpenAI Chat Completions and Responses wire
// formats on top of the protocol-neutral conv model.
//
// Reasoning travels in the fields clients already round-trip: Chat uses
// reasoning_content plus OpenRouter-style reasoning_details, Responses uses
// reasoning items. The gateway's anchor rides as the "encrypted" reasoning
// data (reasoning.encrypted / encrypted_content); it is not encrypted, the
// field is only where clients keep opaque reasoning state.
package openai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"agyrelay/internal/conv"
)

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Tools         []chatTool    `json:"tools"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	ReasoningEffort string `json:"reasoning_effort"`
	// Reasoning is OpenRouter's reasoning control.
	Reasoning *struct {
		Effort  string `json:"effort"`
		Exclude bool   `json:"exclude"`
	} `json:"reasoning"`
}

func (r chatRequest) effort() string {
	if r.ReasoningEffort == "" && r.Reasoning != nil {
		return r.Reasoning.Effort
	}
	return r.ReasoningEffort
}

// hideReasoning reports whether the client asked to omit reasoning text.
// The anchor is still sent: it is state, not reasoning.
func (r chatRequest) hideReasoning() bool { return r.Reasoning != nil && r.Reasoning.Exclude }

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	// ReasoningDetails stays raw: other providers' entries have other
	// shapes and must not fail the request.
	ReasoningDetails json.RawMessage `json:"reasoning_details"`
}

// OpenRouter reasoning_details entry types.
const (
	detailText      = "reasoning.text"
	detailEncrypted = "reasoning.encrypted"
	detailFormat    = "agyrelay-v1"
)

// reasoningDetail is one reasoning_details entry. Index orders entries of
// one message; stream deltas with the same index continue an entry.
type reasoningDetail struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	Data   string `json:"data,omitempty"`
	Format string `json:"format,omitempty"`
	Index  int    `json:"index"`
}

type chatToolCall struct {
	// Index is set only in stream deltas.
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name string `json:"name"`
	// Arguments is a JSON object encoded as a string.
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type contentPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	Refusal    string          `json:"refusal"`
	ImageURL   json.RawMessage `json:"image_url"`
	InputAudio json.RawMessage `json:"input_audio"`
	File       *chatFilePart   `json:"file"`
}

type chatFilePart struct {
	FileID   string `json:"file_id"`
	FileURL  string `json:"file_url"`
	FileData string `json:"file_data"`
	Filename string `json:"filename"`
}

// decodeRequest converts a Chat Completions request. Errors describe the
// offending field for a 400 response.
func decodeRequest(req chatRequest) (conv.Request, error) {
	if req.Model == "" {
		return conv.Request{}, errors.New("model is required")
	}
	out := conv.Request{Model: req.Model, Effort: req.effort()}
	for i, tool := range req.Tools {
		if tool.Type != "function" {
			return conv.Request{}, fmt.Errorf("tools[%d]: only function tools are supported", i)
		}
		out.Tools = append(out.Tools, conv.ToolSpec{Name: tool.Function.Name, Description: tool.Function.Description, Parameters: tool.Function.Parameters})
	}
	// Tool messages do not name their tool; the assistant call does.
	callNames := map[string]string{}
	for i, m := range req.Messages {
		msg, err := decodeMessage(m, callNames)
		if err != nil {
			return conv.Request{}, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, msg)
	}
	return out, nil
}

func decodeMessage(m chatMessage, callNames map[string]string) (conv.Message, error) {
	text, media, err := decodeContent(m.Content)
	if err != nil {
		return conv.Message{}, err
	}
	switch m.Role {
	case "system", "developer":
		if len(media) > 0 {
			return conv.Message{}, errors.New("system messages cannot carry media")
		}
		return conv.Message{Role: conv.RoleSystem, Text: text}, nil
	case "user":
		return conv.Message{Role: conv.RoleUser, Text: text, Media: media}, nil
	case "assistant":
		if len(media) > 0 {
			return conv.Message{}, errors.New("assistant messages cannot carry media")
		}
		msg := conv.Message{Role: conv.RoleAssistant, Text: text, Anchors: detailAnchors(m.ReasoningDetails)}
		for j, call := range m.ToolCalls {
			args, err := decodeArguments(call.Function.Arguments)
			if err != nil {
				return conv.Message{}, fmt.Errorf("tool_calls[%d]: %w", j, err)
			}
			callNames[call.ID] = call.Function.Name
			msg.ToolCalls = append(msg.ToolCalls, conv.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: args})
		}
		return msg, nil
	case "tool":
		if m.ToolCallID == "" {
			return conv.Message{}, errors.New("tool messages need tool_call_id")
		}
		return conv.Message{Role: conv.RoleTool, Text: text, Media: media, ToolCallID: m.ToolCallID, ToolName: callNames[m.ToolCallID]}, nil
	}
	return conv.Message{}, fmt.Errorf("unsupported role %q", m.Role)
}

// detailAnchors collects the data of reasoning.encrypted entries. Entries
// it cannot read belong to someone else and are skipped.
func detailAnchors(raw json.RawMessage) []string {
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	var anchors []string
	for _, entry := range entries {
		var detail struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}
		if json.Unmarshal(entry, &detail) == nil && detail.Type == detailEncrypted && detail.Data != "" {
			anchors = append(anchors, detail.Data)
		}
	}
	return anchors
}

// decodeArguments accepts the string-encoded JSON object; models and
// clients sometimes send an empty string for no arguments.
func decodeArguments(raw string) (json.RawMessage, error) {
	if strings.TrimSpace(raw) == "" {
		return json.RawMessage("{}"), nil
	}
	if !json.Valid([]byte(raw)) {
		return nil, errors.New("function.arguments is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

// decodeContent reads a string or an array of text and inline media parts.
func decodeContent(raw json.RawMessage) (string, []conv.Media, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil
	}
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, errors.New("content must be a string or an array of parts")
	}
	var texts []string
	var media []conv.Media
	for i, part := range parts {
		switch part.Type {
		case "text":
			texts = append(texts, part.Text)
		case "refusal":
			texts = append(texts, part.Refusal)
		case "image_url":
			item, err := decodeMediaURL(part.ImageURL)
			if err != nil {
				return "", nil, fmt.Errorf("content[%d]: %w", i, err)
			}
			media = append(media, item)
		case "input_audio":
			item, err := decodeInputAudio(part.InputAudio)
			if err != nil {
				return "", nil, fmt.Errorf("content[%d]: %w", i, err)
			}
			media = append(media, item)
		case "file":
			if part.File == nil {
				return "", nil, fmt.Errorf("content[%d]: file object is required", i)
			}
			item, err := decodeInlineFile(part.File.FileID, part.File.FileURL, part.File.FileData, part.File.Filename)
			if err != nil {
				return "", nil, fmt.Errorf("content[%d]: %w", i, err)
			}
			media = append(media, item)
		default:
			return "", nil, fmt.Errorf("content[%d]: unsupported part type %q", i, part.Type)
		}
	}
	return strings.Join(texts, "\n"), media, nil
}

const dataURLBase64Suffix = ";base64"

var audioFormatMimes = map[string]string{
	"wav":   "audio/wav",
	"x-wav": "audio/wav",
	"wave":  "audio/wav",
	"mp3":   "audio/mp3",
	"mpeg":  "audio/mpeg",
	"mpga":  "audio/mpeg",
	"aiff":  "audio/aiff",
	"aac":   "audio/aac",
	"ogg":   "audio/ogg",
	"oga":   "audio/ogg",
	"opus":  "audio/opus",
	"flac":  "audio/flac",
	"m4a":   "audio/m4a",
	"x-m4a": "audio/x-m4a",
	"mp4":   "audio/mp4",
	"webm":  "audio/webm",
	"amr":   "audio/amr",
	"pcm16": "audio/l16",
	"pcm":   "audio/l16",
	"l16":   "audio/l16",
}

var extMimes = map[string]string{
	".pdf":      "application/pdf",
	".rtf":      "application/rtf",
	".doc":      "application/msword",
	".docx":     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":      "application/vnd.ms-excel",
	".xlsx":     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":      "application/vnd.ms-powerpoint",
	".pptx":     "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt":      "application/vnd.oasis.opendocument.text",
	".ods":      "application/vnd.oasis.opendocument.spreadsheet",
	".odp":      "application/vnd.oasis.opendocument.presentation",
	".txt":      "text/plain",
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".csv":      "text/csv",
	".html":     "text/html",
	".htm":      "text/html",
	".css":      "text/css",
	".xml":      "text/xml",
	".json":     "application/json",
	".ipynb":    "application/x-ipynb+json",
	".js":       "text/javascript",
	".mjs":      "text/javascript",
	".cjs":      "text/javascript",
	".ts":       "text/x-typescript",
	".tsx":      "text/x-typescript",
	".py":       "text/x-python",
	".png":      "image/png",
	".jpg":      "image/jpeg",
	".jpeg":     "image/jpeg",
	".webp":     "image/webp",
	".gif":      "image/gif",
	".bmp":      "image/bmp",
	".tif":      "image/tiff",
	".tiff":     "image/tiff",
	".heic":     "image/heic",
	".heif":     "image/heif",
	".ico":      "image/x-icon",
	".wav":      "audio/wav",
	".mp3":      "audio/mp3",
	".m4a":      "audio/m4a",
	".aac":      "audio/aac",
	".flac":     "audio/flac",
	".ogg":      "audio/ogg",
	".oga":      "audio/ogg",
	".opus":     "audio/opus",
	".amr":      "audio/amr",
	".mp4":      "video/mp4",
	".m4v":      "video/mp4",
	".webm":     "video/webm",
	".mov":      "video/quicktime",
	".3gp":      "video/3gpp",
}

// decodeMediaURL accepts {url} or a bare url string. Only base64 data: URLs
// are accepted: the relay never fetches remote URLs on a client's behalf.
func decodeMediaURL(raw json.RawMessage) (conv.Media, error) {
	var url string
	if json.Unmarshal(raw, &url) != nil {
		var object struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &object); err != nil {
			return conv.Media{}, errors.New("image_url must be a string or {url}")
		}
		url = object.URL
	}
	media, err := parseDataURL(url)
	if err != nil {
		return conv.Media{}, fmt.Errorf("image_url: %w", err)
	}
	return media, nil
}

func decodeInputAudio(raw json.RawMessage) (conv.Media, error) {
	var payload struct {
		Data   string `json:"data"`
		Format string `json:"format"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return conv.Media{}, errors.New("input_audio must be {data, format}")
	}
	if strings.HasPrefix(payload.Data, "data:") {
		media, err := parseDataURL(payload.Data)
		if err != nil {
			return conv.Media{}, fmt.Errorf("input_audio: %w", err)
		}
		if !strings.HasPrefix(media.MimeType, "audio/") {
			return conv.Media{}, fmt.Errorf("input_audio: expected audio/* MIME type, got %q", media.MimeType)
		}
		return media, nil
	}
	mimeType, ok := resolveAudioFormat(payload.Format)
	if !ok {
		return conv.Media{}, fmt.Errorf("input_audio: unsupported audio format %q", payload.Format)
	}
	_, canonical, err := decodeBase64(payload.Data)
	if err != nil {
		return conv.Media{}, fmt.Errorf("input_audio: %w", err)
	}
	return conv.Media{Data: canonical, MimeType: mimeType}, nil
}

func decodeInlineFile(fileID, fileURL, fileData, filename string) (conv.Media, error) {
	if fileID != "" {
		return conv.Media{}, errors.New("file_id is not supported; send inline file_data")
	}
	if fileURL != "" {
		if !strings.HasPrefix(fileURL, "data:") {
			return conv.Media{}, errors.New("remote file_url is not supported; send inline file_data")
		}
		if fileData == "" {
			fileData = fileURL
		}
	}
	if fileData == "" {
		return conv.Media{}, errors.New("file_data is required")
	}
	if strings.HasPrefix(fileData, "data:") {
		return parseDataURL(fileData)
	}
	decoded, canonical, err := decodeBase64(fileData)
	if err != nil {
		return conv.Media{}, fmt.Errorf("file_data: %w", err)
	}
	mimeType := extMimes[strings.ToLower(filepath.Ext(strings.TrimSpace(filename)))]
	if mimeType == "" {
		mimeType = sniffMediaMime(decoded)
	}
	if mimeType == "" {
		return conv.Media{}, errors.New("cannot determine file MIME type; pass a data:<mime>;base64,... URL or a filename with a known extension")
	}
	return conv.Media{Data: canonical, MimeType: mimeType}, nil
}

func parseDataURL(url string) (conv.Media, error) {
	if !strings.HasPrefix(url, "data:") {
		return conv.Media{}, errors.New("must be a base64 data: URL")
	}
	header, encoded, hasComma := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	hasBase64 := len(header) >= len(dataURLBase64Suffix) &&
		strings.EqualFold(header[len(header)-len(dataURLBase64Suffix):], dataURLBase64Suffix)
	if !hasComma || !hasBase64 {
		return conv.Media{}, errors.New("must be a base64 data: URL")
	}
	mimeType := strings.ToLower(strings.TrimSpace(header[:len(header)-len(dataURLBase64Suffix)]))
	if !validMimeType(mimeType) {
		return conv.Media{}, errors.New("data: URL is missing a valid MIME type")
	}
	_, canonical, err := decodeBase64(encoded)
	if err != nil {
		return conv.Media{}, err
	}
	return conv.Media{Data: canonical, MimeType: mimeType}, nil
}

func resolveAudioFormat(format string) (string, bool) {
	clean := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(format)), ".")
	if mimeType, ok := audioFormatMimes[clean]; ok {
		return mimeType, true
	}
	if strings.HasPrefix(clean, "audio/") && validMimeType(clean) {
		return clean, true
	}
	return "", false
}

func validMimeType(mimeType string) bool {
	base, _, _ := strings.Cut(mimeType, ";")
	base = strings.TrimSpace(base)
	kind, sub, hasSlash := strings.Cut(base, "/")
	return hasSlash && kind != "" && sub != "" && !strings.ContainsAny(base, " \t\r\n")
}

func decodeBase64(encoded string) ([]byte, string, error) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		default:
			return r
		}
	}, encoded)
	if clean == "" {
		return nil, "", errors.New("empty base64 media data")
	}
	decoded, err := base64.StdEncoding.DecodeString(clean)
	if err != nil && !strings.HasSuffix(clean, "=") {
		decoded, err = base64.RawStdEncoding.DecodeString(clean)
	}
	if err != nil || len(decoded) == 0 {
		return nil, "", errors.New("invalid base64 media data")
	}
	return decoded, base64.StdEncoding.EncodeToString(decoded), nil
}

func sniffMediaMime(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return "application/pdf"
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE")):
		return "audio/wav"
	case bytes.HasPrefix(data, []byte("ID3")),
		len(data) >= 2 && data[0] == 0xff && (data[1]&0xe0) == 0xe0:
		return "audio/mp3"
	case bytes.HasPrefix(data, []byte("fLaC")):
		return "audio/flac"
	case bytes.HasPrefix(data, []byte("OggS")):
		return "audio/ogg"
	case len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")):
		if bytes.Equal(data[8:12], []byte("M4A ")) {
			return "audio/m4a"
		}
		return "video/mp4"
	}
	return ""
}
