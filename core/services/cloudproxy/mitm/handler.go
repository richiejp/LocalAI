package mitm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mudler/LocalAI/core/schema"
	"github.com/mudler/LocalAI/core/services/routing/pii"
	"github.com/mudler/LocalAI/core/services/routing/piiadapter"
	"github.com/mudler/xlog"
)

// PIIHandlerOptions configures the PII-aware InterceptHandler that
// LocalAI's MITM proxy uses by default. The handler runs the global
// redactor on inbound chat-style requests and the streaming filter
// on outbound SSE responses; everything else (auth, OAuth callback
// endpoints, telemetry) passes through with the upstream's bytes
// unchanged.
type PIIHandlerOptions struct {
	// Redactor is the regex PII redactor. nil disables redaction —
	// the handler then becomes a plain forwarding proxy, useful for
	// observability-only deployments.
	Redactor *pii.Redactor

	// EventStore receives PIIEvent rows. nil discards events.
	EventStore pii.EventStore

	// UpstreamTLS is the tls.Config used when the proxy dials the
	// real upstream. Defaults to a system-trust HTTPS client.
	// Override in tests to trust a self-signed httptest fixture.
	UpstreamTLS *tls.Config

	// CorrelationIDHeader names the request header carrying a
	// caller-supplied correlation ID. Defaults to "X-Correlation-ID";
	// Anthropic clients also send "x-request-id".
	CorrelationIDHeader string

	// DialHost optionally remaps the host used for the outbound
	// upstream URL. Identity by default. Tests inject a httptest
	// listener address here so the handler can keep classifying on
	// the original "api.anthropic.com" name while actually dialing
	// 127.0.0.1:NNNN.
	DialHost func(host string) string
}

// NewPIIHandler returns the InterceptHandler that performs request
// + streaming redaction. The returned handler is the production
// dispatch — tests in this package use the simpler passthrough
// fixture in proxy_test.go.
func NewPIIHandler(opts PIIHandlerOptions) InterceptHandler {
	tlsCfg := opts.UpstreamTLS
	if tlsCfg == nil {
		tlsCfg = &tls.Config{NextProtos: []string{"http/1.1"}}
	}
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   tlsCfg,
			ForceAttemptHTTP2: false,
		},
		// No top-level timeout: streaming responses can run for
		// minutes. Per-request deadline is the client conn's, which
		// the proxy already inherits from the originating CONNECT.
	}

	corrHeader := opts.CorrelationIDHeader
	if corrHeader == "" {
		corrHeader = "X-Correlation-ID"
	}

	dialHost := opts.DialHost
	if dialHost == nil {
		dialHost = func(h string) string { return h }
	}

	return func(w http.ResponseWriter, r *http.Request, host string) {
		dispatchPIIIntercept(w, r, host, dialHost(host), client, opts.Redactor, opts.EventStore, corrHeader)
	}
}

// dispatchPIIIntercept does the per-request work for the PII
// handler: detect the request shape, redact, forward, and stream
// the response. Pulled out as a free function so the handler
// closure stays trivially testable.
func dispatchPIIIntercept(w http.ResponseWriter, r *http.Request, host, dialHost string, client *http.Client, redactor *pii.Redactor, store pii.EventStore, corrHeader string) {
	// Read the inbound body once. We need to parse it for
	// redaction and then re-send the (possibly mutated) bytes to
	// the upstream — http.Request.Body is single-shot.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "mitm: read body: "+err.Error(), http.StatusBadGateway)
		return
	}
	_ = r.Body.Close()

	correlationID := r.Header.Get(corrHeader)
	if correlationID == "" {
		correlationID = r.Header.Get("x-request-id")
	}

	// Decide whether to redact based on the request path. Only
	// chat-style endpoints carry user prose; OAuth, listing, and
	// metadata endpoints get an unmodified passthrough.
	shape := classifyRequestShape(host, r.URL.Path)
	if redactor != nil && shape != shapeUnknown {
		redacted, blocked, err := redactRequest(body, shape, redactor, store, correlationID)
		if err != nil {
			xlog.Debug("mitm: redact request failed; forwarding unchanged", "host", host, "path", r.URL.Path, "error", err)
		} else {
			if blocked {
				writePIIBlocked(w, correlationID)
				return
			}
			body = redacted
		}
	}

	upstreamURL := "https://" + dialHost + r.URL.RequestURI()
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "mitm: build upstream request: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Copy headers from the client request, but drop hop-by-hop
	// ones the proxy must regenerate.
	upstreamReq.Header = cloneHopByHopFiltered(r.Header)
	// Content-Length must reflect the (possibly-mutated) body.
	upstreamReq.ContentLength = int64(len(body))
	upstreamReq.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

	resp, err := client.Do(upstreamReq)
	if err != nil {
		http.Error(w, "mitm: upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		// Skip hop-by-hop headers and transfer-encoding (the
		// connResponseWriter sets its own framing).
		if isHopByHop(k) || strings.EqualFold(k, "Transfer-Encoding") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Streaming responses (SSE) get the StreamFilter treatment.
	// Non-streaming responses are forwarded byte-for-byte.
	if shape != shapeUnknown && redactor != nil && isSSE(resp.Header.Get("Content-Type")) {
		streamWithPII(w, resp.Body, shape, redactor, store, correlationID)
		return
	}

	// Plain copy. SSE responses for unknown shapes also land here.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rErr != nil {
			return
		}
	}
}

// requestShape classifies a host+path pair into a recognised LLM
// API request shape so we can pick the right adapter / streaming
// parser. shapeUnknown means "forward verbatim" — any host or path
// not on the small recognised set passes through, including OAuth,
// usage, and listing endpoints on api.anthropic.com itself.
type requestShape int

const (
	shapeUnknown requestShape = iota
	shapeOpenAIChat
	shapeAnthropicMessages
)

func classifyRequestShape(host, path string) requestShape {
	host = strings.ToLower(host)
	switch {
	case host == "api.openai.com" && strings.HasSuffix(path, "/v1/chat/completions"):
		return shapeOpenAIChat
	case host == "api.anthropic.com" && strings.HasSuffix(path, "/v1/messages"):
		return shapeAnthropicMessages
	}
	return shapeUnknown
}

// redactRequest parses the request body, runs the appropriate
// piiadapter, and re-marshals. blocked=true when the redactor
// returned at least one Block action — the caller short-circuits
// the upstream call and writes a synthetic 400.
func redactRequest(body []byte, shape requestShape, redactor *pii.Redactor, store pii.EventStore, correlationID string) ([]byte, bool, error) {
	var parsed any
	var adapter pii.Adapter
	switch shape {
	case shapeOpenAIChat:
		req := &schema.OpenAIRequest{}
		if err := json.Unmarshal(body, req); err != nil {
			return nil, false, fmt.Errorf("parse openai: %w", err)
		}
		parsed = req
		adapter = piiadapter.OpenAI()
	case shapeAnthropicMessages:
		req := &schema.AnthropicRequest{}
		if err := json.Unmarshal(body, req); err != nil {
			return nil, false, fmt.Errorf("parse anthropic: %w", err)
		}
		parsed = req
		adapter = piiadapter.Anthropic()
	default:
		return body, false, nil
	}

	texts := adapter.Scan(parsed)
	if len(texts) == 0 {
		return body, false, nil
	}

	updates := make([]pii.ScannedText, 0, len(texts))
	blocked := false
	for _, st := range texts {
		if st.Text == "" {
			continue
		}
		res := redactor.RedactWithOverrides(st.Text, nil)
		if len(res.Spans) == 0 {
			continue
		}
		recordEvents(store, res.Spans, correlationID, redactor)
		if res.Blocked {
			blocked = true
		}
		updates = append(updates, pii.ScannedText{Index: st.Index, Text: res.Redacted})
	}

	if len(updates) > 0 {
		adapter.Apply(parsed, updates)
	}

	out, err := json.Marshal(parsed)
	if err != nil {
		return nil, false, fmt.Errorf("re-marshal: %w", err)
	}
	return out, blocked, nil
}

// recordEvents persists one PIIEvent per redaction span. The
// MITM context doesn't have a user (no LocalAI auth header on
// CLI traffic) so UserID is empty — admins can still
// correlate by request ID.
func recordEvents(store pii.EventStore, spans []pii.Span, correlationID string, redactor *pii.Redactor) {
	if store == nil {
		return
	}
	patterns := redactor.Patterns()
	patternAction := make(map[string]pii.Action, len(patterns))
	for _, p := range patterns {
		patternAction[p.ID] = p.Action
	}
	for _, span := range spans {
		ev := pii.PIIEvent{
			ID:            "mitm_" + correlationID + "_" + span.Pattern,
			CorrelationID: correlationID,
			Direction:     pii.DirectionIn,
			PatternID:     span.Pattern,
			ByteOffset:    span.Start,
			Length:        span.End - span.Start,
			HashPrefix:    span.HashPrefix,
			Action:        patternAction[span.Pattern],
		}
		_ = store.Record(context.Background(), ev)
	}
}

// streamWithPII reads SSE events from the upstream, runs each
// content-bearing payload through the streaming filter, and writes
// the (possibly rewritten) bytes to the client. Built directly on
// bufio rather than reusing cloudproxy's scanner to keep the MITM
// package self-contained — the SSE shape is the same on both
// providers and the parser is small.
func streamWithPII(w http.ResponseWriter, src io.Reader, shape requestShape, redactor *pii.Redactor, store pii.EventStore, correlationID string) {
	flusher, _ := w.(http.Flusher)
	filter := pii.NewStreamFilter(redactor, nil, store, correlationID, "")

	provider := "openai"
	if shape == shapeAnthropicMessages {
		provider = "anthropic"
	}

	emit := func(s string) {
		_, _ = w.Write([]byte(s))
		if flusher != nil {
			flusher.Flush()
		}
	}

	scanner := newCloudproxyScanner(src)
	for scanner.Scan() {
		ev := scanner.Event()
		if isTerminalSSE(ev.dataLine, provider) {
			if residual := filter.Drain(); residual != "" {
				emit(synthSSEResidual(provider, residual))
			}
			emit(ev.raw)
			continue
		}
		out := ev.raw
		if ev.dataLine != "" {
			rewritten, drop := rewriteSSEPayload(ev.dataLine, provider, filter)
			if drop {
				continue
			}
			if rewritten != ev.dataLine {
				out = strings.Replace(ev.raw, ev.dataLine, rewritten, 1)
			}
		}
		emit(out)
	}
	if residual := filter.Drain(); residual != "" {
		emit(synthSSEResidual(provider, residual))
	}
}

func writePIIBlocked(w http.ResponseWriter, correlationID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	resp := map[string]any{
		"error": map[string]string{
			"message": "request blocked by LocalAI MITM proxy (sensitive data detected)",
			"type":    "pii_blocked",
		},
		"correlation_id": correlationID,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func isSSE(contentType string) bool {
	return strings.HasPrefix(strings.TrimSpace(contentType), "text/event-stream")
}

// hopByHopHeaders are the request/response headers that must not
// be forwarded by an HTTP proxy per RFC 7230 §6.1. The proxy
// regenerates these as needed.
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailers":            {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

func isHopByHop(name string) bool {
	_, ok := hopByHopHeaders[http.CanonicalHeaderKey(name)]
	return ok
}

func cloneHopByHopFiltered(in http.Header) http.Header {
	out := make(http.Header, len(in))
	for k, vs := range in {
		if isHopByHop(k) {
			continue
		}
		copied := make([]string, len(vs))
		copy(copied, vs)
		out[k] = copied
	}
	return out
}
