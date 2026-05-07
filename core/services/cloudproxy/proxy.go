// Package cloudproxy forwards LocalAI requests to external
// provider APIs (Backend = "proxy-*") wire-format-faithfully — it
// does not translate request shapes between providers.
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
	"github.com/mudler/LocalAI/core/services/cloudproxy/ssewire"
	"github.com/mudler/LocalAI/core/services/routing/pii"
	"github.com/mudler/xlog"
)

// Backend names recognised by the proxy. Any backend starting with
// "proxy-" goes through Forward; these two are the wire formats the
// auth + SSE rewriter knows. Unknown proxy-* backends fall back to
// OpenAI auth.
const (
	BackendProxyOpenAI    = "proxy-openai"
	BackendProxyAnthropic = "proxy-anthropic"
)

// transport is overridable in tests; production uses http.DefaultTransport.
var transport http.RoundTripper = http.DefaultTransport

// SetTransport swaps the HTTP transport used by every Forward call.
// Test-only.
func SetTransport(rt http.RoundTripper) func() {
	prev := transport
	transport = rt
	return func() { transport = prev }
}

// providerName classifies a backend string into one of the
// supported upstreams. Unknown "proxy-*" names fall back to
// openai-shaped auth since most third-party providers mirror it.
func providerName(backend string) string {
	switch backend {
	case BackendProxyAnthropic:
		return "anthropic"
	default:
		return "openai"
	}
}

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

func httpClient(cfg *config.ModelConfig) *http.Client {
	c := &http.Client{Transport: transport}
	if cfg.Proxy.RequestTimeoutSeconds > 0 {
		c.Timeout = time.Duration(cfg.Proxy.RequestTimeoutSeconds) * time.Second
	}
	return c
}

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

func streaming(body []byte) bool {
	var probe struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Stream
}

// Forward proxies a chat-style request to the upstream and writes
// the response to the client, applying filter to per-token text
// extracted from streaming SSE responses.
func Forward(c echo.Context, cfg *config.ModelConfig, body []byte, filter *pii.StreamFilter) error {
	body, err := rewriteModel(body, cfg.Proxy.UpstreamModel)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	req, err := buildHTTPRequest(c.Request().Context(), cfg, body)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	isStream := streaming(body)
	xlog.Debug("cloudproxy: forwarding",
		"model", cfg.Name,
		"backend", cfg.Backend,
		"upstream", cfg.Proxy.UpstreamURL,
		"stream", isStream,
	)

	resp, err := httpClient(cfg).Do(req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "cloudproxy: upstream request failed: "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return passthroughError(c, resp)
	}

	if isStream {
		return forwardStream(c, resp, providerName(cfg.Backend), filter)
	}
	return forwardBuffered(c, resp)
}

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

func forwardBuffered(c echo.Context, resp *http.Response) error {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Response().Header().Set("Content-Type", ct)
	}
	c.Response().WriteHeader(resp.StatusCode)
	_, err := io.Copy(c.Response().Writer, resp.Body)
	return err
}

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
		if line := ssewire.SynthResidualEvent(ssewire.Provider(provider), residual); line != "" {
			_ = emit(line)
		}
	}

	prov := ssewire.Provider(provider)
	scanner := ssewire.NewScanner(resp.Body)
	for scanner.Scan() {
		ev := scanner.Event()
		if ssewire.IsTerminalMarker(ev.DataLine, prov) {
			flushResidual()
			_ = emit(ev.Raw)
			continue
		}
		out := ev.Raw
		if filter != nil && ev.DataLine != "" {
			rewritten, drop := ssewire.RewritePayload(ev.DataLine, prov, filter)
			if drop {
				continue
			}
			if rewritten != ev.DataLine {
				// strings.Replace with n=1 touches only the data
				// line, preserving any "event:"/"id:" preamble.
				out = strings.Replace(ev.Raw, ev.DataLine, rewritten, 1)
			}
		}
		if err := emit(out); err != nil {
			return nil
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		xlog.Debug("cloudproxy: stream read error", "error", err)
	}
	flushResidual()
	return nil
}
