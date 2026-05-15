package cloudproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mudler/LocalAI/core/backend"
	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/core/schema"
	"github.com/mudler/LocalAI/core/services/cloudproxy/ssewire"
	"github.com/mudler/xlog"
)

// StreamCallback receives content deltas as they arrive from the
// upstream. Returning false signals "stop streaming" — the underlying
// HTTP connection is then closed and the request is treated as
// cancelled. Mirrors the per-token callback shape backend.ModelInference
// uses so callers can re-use the same closure for local and cloud
// dispatch.
type StreamCallback func(token string, usage backend.TokenUsage) bool

// StreamTokens forwards an OpenAI-shaped chat-completion request to a
// proxy-* cloud backend and yields each content delta to onToken. The
// request body is forced to stream=true (so the upstream sends SSE)
// and the response is parsed event-by-event without ever touching the
// HTTP response writer — making this callable from the realtime
// pipeline, an MCP tool, or any other non-HTTP entry point.
//
// On upstream error (4xx/5xx, transport failure, malformed stream)
// StreamTokens returns an error whose message preserves the upstream's
// original body so callers can faithfully relay it to their client —
// the same fidelity contract Forward already honours for HTTP traffic.
//
// Only proxy-openai upstreams are supported in this first pass:
// Anthropic requires translating the outbound OpenAI body to Anthropic
// shape, which is a separate change.
func StreamTokens(ctx context.Context, cfg *config.ModelConfig, req *schema.OpenAIRequest, onToken StreamCallback) (backend.LLMResponse, error) {
	if cfg == nil {
		return backend.LLMResponse{}, fmt.Errorf("cloudproxy: StreamTokens called with nil config")
	}
	if !cfg.IsCloudProxy() {
		return backend.LLMResponse{}, fmt.Errorf("cloudproxy: StreamTokens called on non-proxy backend %q", cfg.Backend)
	}
	if providerName(cfg.Backend) != "openai" {
		return backend.LLMResponse{}, fmt.Errorf("cloudproxy: StreamTokens only supports proxy-openai upstreams (got %q)", cfg.Backend)
	}

	// Force streaming so we can deliver deltas to the callback. The
	// caller's req may or may not have set Stream=true; we want it on
	// regardless for realtime LLM dispatch.
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return backend.LLMResponse{}, fmt.Errorf("cloudproxy: marshal request: %w", err)
	}
	body, err = rewriteModel(body, cfg.Proxy.UpstreamModel)
	if err != nil {
		return backend.LLMResponse{}, err
	}

	httpReq, err := buildHTTPRequest(ctx, cfg, body)
	if err != nil {
		return backend.LLMResponse{}, err
	}

	resp, err := httpClient(cfg).Do(httpReq)
	if err != nil {
		return backend.LLMResponse{}, fmt.Errorf("cloudproxy: upstream request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		const maxErrBody = 1 << 20
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return backend.LLMResponse{}, &UpstreamError{
			StatusCode:  resp.StatusCode,
			ContentType: resp.Header.Get("Content-Type"),
			Body:        errBody,
		}
	}

	return consumeOpenAIStream(resp, onToken)
}

// UpstreamError carries the upstream's status + body verbatim so the
// caller can surface the cloud's original error to its own client (the
// realtime handler turns this into an error event; an MCP tool turns
// it into a tool_result). Pointer-equality-friendly so callers can
// errors.As(err, &UpstreamError{}) to detect "the cloud said no".
type UpstreamError struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("cloudproxy upstream returned %d: %s", e.StatusCode, string(e.Body))
}

// consumeOpenAIStream reads an OpenAI-shaped SSE response, calling
// onToken with each content delta and accumulating the final usage +
// full text into an LLMResponse. Mirrors the behaviour of the local
// gRPC streaming path so the realtime caller can swap dispatch without
// changing how it consumes tokens.
func consumeOpenAIStream(resp *http.Response, onToken StreamCallback) (backend.LLMResponse, error) {
	prov := ssewire.Provider("openai")
	scanner := ssewire.NewScanner(resp.Body)

	var fullText strings.Builder
	usage := backend.TokenUsage{}

	for scanner.Scan() {
		ev := scanner.Event()
		if ssewire.IsTerminalMarker(ev.DataLine, prov) {
			break
		}
		if ev.DataLine == "" {
			continue
		}
		// Strip the "data: " prefix that ssewire preserves on the raw
		// line; the scanner's DataLine field already contains the JSON
		// payload only when the data: prefix was present.
		payload := strings.TrimSpace(ev.DataLine)
		payload = strings.TrimPrefix(payload, "data:")
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var chunk openAIChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// Tolerate non-JSON keep-alive comments; log debug and
			// continue. Hard errors come from the scanner.
			xlog.Debug("cloudproxy: stream chunk parse error", "error", err, "payload", payload)
			continue
		}

		// Surface usage when the upstream emits it (final chunk on
		// stream_options.include_usage=true).
		if chunk.Usage != nil {
			usage.Prompt = chunk.Usage.PromptTokens
			usage.Completion = chunk.Usage.CompletionTokens
		}

		for _, choice := range chunk.Choices {
			content := choice.Delta.Content
			if content == "" {
				continue
			}
			fullText.WriteString(content)
			if onToken != nil && !onToken(content, usage) {
				// Caller cancelled; close upstream by returning early.
				// resp.Body.Close() is deferred in StreamTokens.
				return backend.LLMResponse{
					Response: fullText.String(),
					Usage:    usage,
				}, nil
			}
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return backend.LLMResponse{}, fmt.Errorf("cloudproxy: stream read error: %w", err)
	}

	return backend.LLMResponse{
		Response: fullText.String(),
		Usage:    usage,
	}, nil
}

// openAIChunk mirrors the subset of the OpenAI streaming chunk shape
// we care about. Kept local to this file rather than exposed publicly
// because consumers should call StreamTokens, not parse chunks
// themselves.
type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}
