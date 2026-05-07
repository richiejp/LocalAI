package mitm

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"github.com/mudler/LocalAI/core/services/routing/pii"
)

// sseEvent is one SSE event with its exact wire bytes preserved
// in raw (so unmodified events round-trip byte-for-byte) and the
// extracted JSON payload from the data: line in dataLine.
type sseEvent struct {
	raw      string
	dataLine string
}

type sseScanner struct {
	r   *bufio.Reader
	ev  sseEvent
	err error
}

// newCloudproxyScanner returns an SSE scanner with the same shape
// as the one in core/services/cloudproxy. Duplicated here so the
// mitm package doesn't import cloudproxy (which imports schema —
// keeping mitm small and dep-light is worth ~80 lines of
// duplication).
func newCloudproxyScanner(r io.Reader) *sseScanner {
	return &sseScanner{r: bufio.NewReaderSize(r, 64*1024)}
}

func (s *sseScanner) Scan() bool {
	var raw strings.Builder
	var dataLine string
	for {
		line, err := s.r.ReadString('\n')
		if line != "" {
			raw.WriteString(line)
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				if raw.Len() == len(line) {
					raw.Reset()
					continue
				}
				s.ev = sseEvent{raw: raw.String(), dataLine: dataLine}
				return true
			}
			if strings.HasPrefix(trimmed, "data:") {
				if dataLine == "" {
					payload := strings.TrimPrefix(trimmed, "data:")
					payload = strings.TrimPrefix(payload, " ")
					dataLine = payload
				}
			}
		}
		if err != nil {
			s.err = err
			if raw.Len() > 0 {
				s.ev = sseEvent{raw: raw.String(), dataLine: dataLine}
				return true
			}
			return false
		}
	}
}

func (s *sseScanner) Event() sseEvent { return s.ev }

// rewriteSSEPayload mutates the data line of one SSE event by
// running its content-bearing field through the streaming filter.
// drop=true tells the caller to suppress the event entirely
// because the filter buffered the whole token.
func rewriteSSEPayload(dataLine, provider string, filter *pii.StreamFilter) (string, bool) {
	if strings.TrimSpace(dataLine) == "[DONE]" {
		return dataLine, false
	}
	switch provider {
	case "anthropic":
		return rewriteAnthropic(dataLine, filter)
	default:
		return rewriteOpenAI(dataLine, filter)
	}
}

func rewriteOpenAI(dataLine string, filter *pii.StreamFilter) (string, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(dataLine), &m); err != nil {
		return dataLine, false
	}
	choices, ok := m["choices"].([]any)
	if !ok || len(choices) == 0 {
		return dataLine, false
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return dataLine, false
	}
	delta, ok := first["delta"].(map[string]any)
	if !ok {
		return dataLine, false
	}
	content, ok := delta["content"].(string)
	if !ok || content == "" {
		return dataLine, false
	}
	rewritten := filter.Push(content)
	if rewritten == "" {
		return "", true
	}
	if rewritten == content {
		return dataLine, false
	}
	delta["content"] = rewritten
	out, err := json.Marshal(m)
	if err != nil {
		return dataLine, false
	}
	return string(out), false
}

func rewriteAnthropic(dataLine string, filter *pii.StreamFilter) (string, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(dataLine), &m); err != nil {
		return dataLine, false
	}
	if t, _ := m["type"].(string); t != "content_block_delta" {
		return dataLine, false
	}
	delta, ok := m["delta"].(map[string]any)
	if !ok {
		return dataLine, false
	}
	if dt, _ := delta["type"].(string); dt != "text_delta" {
		return dataLine, false
	}
	text, ok := delta["text"].(string)
	if !ok || text == "" {
		return dataLine, false
	}
	rewritten := filter.Push(text)
	if rewritten == "" {
		return "", true
	}
	if rewritten == text {
		return dataLine, false
	}
	delta["text"] = rewritten
	out, err := json.Marshal(m)
	if err != nil {
		return dataLine, false
	}
	return string(out), false
}

func isTerminalSSE(dataLine, provider string) bool {
	if dataLine == "" {
		return false
	}
	if strings.TrimSpace(dataLine) == "[DONE]" {
		return true
	}
	if provider == "anthropic" {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(dataLine), &probe); err == nil {
			return probe.Type == "message_stop"
		}
	}
	return false
}

// synthSSEResidual builds a provider-shaped SSE event carrying the
// PII filter's drained tail. Same shape the cloudproxy package
// uses for its own residual flush.
func synthSSEResidual(provider, text string) string {
	switch provider {
	case "anthropic":
		payload := map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]string{"type": "text_delta", "text": text},
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return ""
		}
		return "event: content_block_delta\ndata: " + string(b) + "\n\n"
	default:
		payload := map[string]any{
			"object": "chat.completion.chunk",
			"choices": []map[string]any{
				{"index": 0, "delta": map[string]string{"content": text}},
			},
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return ""
		}
		return "data: " + string(b) + "\n\n"
	}
}
