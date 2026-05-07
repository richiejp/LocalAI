// Package cloudproxy forwards LocalAI requests to external provider
// APIs without going through the local gRPC backend pipeline. It is
// the dispatch backend for any ModelConfig with Backend = "proxy-*"
// and a non-empty Proxy.UpstreamURL.
//
// Wire-format faithfulness is the design contract: the proxy does NOT
// translate request shapes between providers in the MVP. A client
// posting to /v1/chat/completions on a model whose backend is
// "proxy-openai" forwards an OpenAI chat-completions body to the
// configured upstream; the same client posting to a "proxy-anthropic"
// chat-completions endpoint will get a confused upstream. Cross-shape
// translation is a deliberately deferred follow-up — it would need to
// solve tool-call argument round-tripping and reasoning-content
// passthrough, both of which are subtle enough to deserve their own
// review. The provider mapping in this package only chooses how the
// upstream is *authenticated* and how its response stream is parsed
// for the per-token PII filter.
package cloudproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/core/services/routing/pii"
	"github.com/mudler/xlog"
)

// transport is overridable in tests so httptest fakes can intercept
// upstream calls without monkey-patching DefaultClient. Production
// always uses http.DefaultTransport.
var transport http.RoundTripper = http.DefaultTransport

// SetTransport swaps the HTTP transport used by every Forward call.
// Test-only — production code should never call this.
func SetTransport(rt http.RoundTripper) func() {
	prev := transport
	transport = rt
	return func() { transport = prev }
}

// providerName classifies a backend string into one of the supported
// upstream providers. The MVP recognises "proxy-openai" and
// "proxy-anthropic"; anything else falls back to openai-shaped
// authentication, which is the more common case (most third-party
// providers ape OpenAI's wire format and Bearer-token auth).
func providerName(backend string) string {
	switch backend {
	case "proxy-anthropic":
		return "anthropic"
	default:
		return "openai"
	}
}

// buildHTTPRequest constructs the upstream HTTP request with the
// correct authentication headers for the resolved provider. The
// body is the raw JSON to forward (after model-name swap). The
// returned request has the caller's context so cancellation
// propagates from the originating echo request.
func buildHTTPRequest(ctx context.Context, cfg *config.ModelConfig, body []byte) (*http.Request, error) {
	if cfg.Proxy.UpstreamURL == "" {
		return nil, fmt.Errorf("cloudproxy: proxy.upstream_url is empty for model %q", cfg.Name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Proxy.UpstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")

	apiKey := ""
	if cfg.Proxy.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.Proxy.APIKeyEnv)
	}

	switch providerName(cfg.Backend) {
	case "anthropic":
		// Anthropic uses x-api-key plus a required version header.
		// 2023-06-01 is the stable wire we already speak in
		// /v1/messages — bumping it is a separate decision.
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
		}
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}
	return req, nil
}

// httpClient builds the per-request client with the configured
// timeout. A zero RequestTimeoutSeconds disables the client-level
// deadline — the request still ends when the echo context cancels.
func httpClient(cfg *config.ModelConfig) *http.Client {
	c := &http.Client{Transport: transport}
	if cfg.Proxy.RequestTimeoutSeconds > 0 {
		c.Timeout = time.Duration(cfg.Proxy.RequestTimeoutSeconds) * time.Second
	}
	return c
}

// rewriteModel replaces the "model" field at the top level of the
// JSON body with cfg.Proxy.UpstreamModel when set. It uses
// generic-map round-tripping rather than reflection over the schema
// type so a single helper covers both OpenAIRequest and
// AnthropicRequest. Returns the original bytes when no rewrite is
// needed.
func rewriteModel(body []byte, upstreamModel string) ([]byte, error) {
	if upstreamModel == "" {
		return body, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("cloudproxy: parse request body: %w", err)
	}
	m["model"] = upstreamModel
	return json.Marshal(m)
}

// streaming reports whether a request body asks for SSE streaming.
// Both OpenAI and Anthropic accept top-level "stream": true with the
// same semantics, so a single boolean check covers both shapes.
func streaming(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream
}

// Forward proxies a chat-style request to the configured upstream
// and writes the response back to the client. The body is forwarded
// verbatim apart from a top-level model rewrite. When streaming is
// requested, SSE chunks are decoded just enough to extract the
// per-token text for the PII filter (when filter != nil); the wire
// envelope is otherwise preserved.
//
// Forward is the single entry point used by both the OpenAI chat
// handler and the Anthropic messages handler. The provider-specific
// logic is the SSE text extractor selected by Backend.
func Forward(c echo.Context, cfg *config.ModelConfig, body []byte, filter *pii.StreamFilter) error {
	body, err := rewriteModel(body, cfg.Proxy.UpstreamModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	req, err := buildHTTPRequest(c.Request().Context(), cfg, body)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	xlog.Debug("cloudproxy: forwarding",
		"model", cfg.Name,
		"backend", cfg.Backend,
		"upstream", cfg.Proxy.UpstreamURL,
		"stream", streaming(body),
	)

	resp, err := httpClient(cfg).Do(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "cloudproxy: upstream request failed: "+err.Error())
	}
	defer resp.Body.Close()

	// Forward upstream non-2xx responses to the caller as-is. We
	// preserve the upstream content-type so error envelopes (which
	// providers return as JSON, not SSE, even on a streaming
	// request) reach the client unmolested.
	if resp.StatusCode >= 400 {
		return passthroughError(c, resp)
	}

	if streaming(body) {
		return forwardStream(c, resp, providerName(cfg.Backend), filter)
	}
	return forwardBuffered(c, resp)
}

// passthroughError relays a non-2xx upstream response to the client
// without rewriting its body. We copy the content-type so JSON
// error envelopes deserialise correctly on the client side. The
// response is capped at 1 MiB to avoid an unbounded copy when the
// upstream misbehaves.
func passthroughError(c echo.Context, resp *http.Response) error {
	const maxErrBody = 1 << 20
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Response().Header().Set("Content-Type", ct)
	}
	c.Response().WriteHeader(resp.StatusCode)
	_, _ = c.Response().Writer.Write(body)
	return nil
}

// forwardBuffered is the non-streaming path: read the full upstream
// response and write it back. We don't run the PII filter on
// non-streaming responses today — the request-side middleware
// already redacts inputs, and the streaming filter is what catches
// model output. Adding output-side PII for buffered responses is a
// follow-up that needs the redactor not just the stream filter.
func forwardBuffered(c echo.Context, resp *http.Response) error {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Response().Header().Set("Content-Type", ct)
	}
	c.Response().WriteHeader(resp.StatusCode)
	_, err := io.Copy(c.Response().Writer, resp.Body)
	return err
}

// forwardStream copies an SSE stream from upstream to client,
// extracting per-token text from each event so the PII filter can
// observe and rewrite it. The filter is wire-format-agnostic: the
// extractor below knows the JSON shape of each provider's chunks
// and pulls out / writes back the text content; everything else
// passes through unchanged.
//
// Streaming PII does block→mask remapping internally (see
// pii.StreamFilter docs) — we never reject mid-stream because the
// HTTP response is already on the wire.
func forwardStream(c echo.Context, resp *http.Response, provider string, filter *pii.StreamFilter) error {
	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	c.Response().WriteHeader(http.StatusOK)

	emit := func(line string) error {
		_, err := fmt.Fprint(c.Response().Writer, line)
		if err != nil {
			return err
		}
		c.Response().Flush()
		return nil
	}

	flushResidual := func() {
		if filter == nil {
			return
		}
		residual := filter.Drain()
		if residual == "" {
			return
		}
		if line := synthResidualEvent(provider, residual); line != "" {
			_ = emit(line)
		}
	}

	scanner := newSSEScanner(resp.Body)
	for scanner.Scan() {
		ev := scanner.Event()
		// A terminal marker (OpenAI [DONE], Anthropic message_stop)
		// signals "no further text". Clients stop reading after it,
		// so we MUST flush the PII filter's held-back residue
		// before forwarding the marker, or the tail of the response
		// is lost.
		if isTerminalMarker(ev.dataLine, provider) {
			flushResidual()
			_ = emit(ev.raw)
			continue
		}
		out := ev.raw
		if filter != nil && ev.dataLine != "" {
			rewritten, drop := rewriteSSEData(ev.dataLine, provider, filter)
			if drop {
				continue
			}
			if rewritten != ev.dataLine {
				// Splice the rewritten payload into the original
				// envelope so any "event: foo" / "id: bar" lines
				// the upstream emitted survive verbatim. We rely
				// on the fact that strings.Replace with n=1
				// touches the first match — the data line — and
				// leaves the rest of the event alone.
				out = strings.Replace(ev.raw, ev.dataLine, rewritten, 1)
			}
		}
		if err := emit(out); err != nil {
			return nil
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		xlog.Debug("cloudproxy: stream read error", "error", err)
	}

	// Final safety net: if the upstream closed the stream without a
	// terminal marker (or for providers that don't emit one),
	// flush whatever the filter is still holding.
	flushResidual()
	return nil
}

// isTerminalMarker identifies the per-provider end-of-stream
// sentinel. We treat these specially so the PII residual flushes
// before the client stops reading.
func isTerminalMarker(dataLine, provider string) bool {
	if dataLine == "" {
		return false
	}
	if strings.TrimSpace(dataLine) == "[DONE]" {
		return true
	}
	if provider == "anthropic" {
		// Anthropic uses message_stop as the final event. We don't
		// treat content_block_stop as terminal because tool calls
		// emit one mid-stream.
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(dataLine), &probe); err == nil {
			return probe.Type == "message_stop"
		}
	}
	return false
}

// synthResidualEvent builds an SSE event line that carries the
// PII filter's drained residual. The shape mirrors the provider's
// content-bearing chunk so a client decoder accepts it.
func synthResidualEvent(provider, text string) string {
	switch provider {
	case "anthropic":
		// Anthropic's text_delta event. We omit the "event:" name
		// because the data line type field already carries the
		// discriminator; clients we've tested accept both forms.
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
		// OpenAI chat-completion chunk shape.
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

// rewriteSSEData decodes a single SSE data payload, runs any
// content-bearing field through the PII filter, and returns the
// rewritten data line. The drop flag instructs the caller to
// suppress the entire SSE event when the filter held back the
// whole text (a common mid-stream case while a pattern boundary
// is buffered).
func rewriteSSEData(dataLine, provider string, filter *pii.StreamFilter) (string, bool) {
	// "[DONE]" is the OpenAI sentinel — pass through unchanged.
	if strings.TrimSpace(dataLine) == "[DONE]" {
		return dataLine, false
	}
	switch provider {
	case "anthropic":
		return rewriteAnthropicChunk(dataLine, filter)
	default:
		return rewriteOpenAIChunk(dataLine, filter)
	}
}

// rewriteOpenAIChunk handles a single chat.completion.chunk by
// rewriting the first choice's delta.content field. We only touch
// content (not reasoning_content or tool_calls) because the PII
// filter is a regex matcher over user-visible text; tool-call
// arguments are JSON strings whose mid-redaction would break
// schema validation downstream.
func rewriteOpenAIChunk(dataLine string, filter *pii.StreamFilter) (string, bool) {
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
		// Filter buffered the whole token — drop the event entirely.
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

// rewriteAnthropicChunk handles content_block_delta events whose
// delta is a text_delta. Other deltas (input_json_delta on a tool
// block, ping, message_start) pass through.
func rewriteAnthropicChunk(dataLine string, filter *pii.StreamFilter) (string, bool) {
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
