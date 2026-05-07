package cloudproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/core/services/routing/pii"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// fakeUpstream returns an httptest.Server whose handler captures the
// inbound request and replies with the given status/body. It is the
// shared fixture for proxy integration tests.
func fakeUpstream(status int, body string, captured **http.Request, capturedBody *[]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			cloned := r.Clone(context.Background())
			*captured = cloned
		}
		if capturedBody != nil {
			b, _ := io.ReadAll(r.Body)
			*capturedBody = b
		}
		// Mirror the upstream's content-type so the proxy passes it
		// through faithfully. Tests for streaming explicitly set
		// text/event-stream; tests for buffered set application/json.
		ct := r.Header.Get("X-Test-Response-Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func newEchoCtx(method, path string, body string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	return c, rec
}

var _ = Describe("Forward", func() {
	It("buffered passthrough", func() {
		var capturedBody []byte
		var capturedReq *http.Request
		upstream := fakeUpstream(200, `{"id":"abc","choices":[{"message":{"content":"hi"}}]}`, &capturedReq, &capturedBody)
		defer upstream.Close()

		cfg := &config.ModelConfig{
			Backend: "proxy-openai",
			Proxy: config.ProxyConfig{
				UpstreamURL:   upstream.URL,
				UpstreamModel: "gpt-4o-mini",
			},
		}
		cfg.Name = "alias"

		c, rec := newEchoCtx("POST", "/v1/chat/completions", `{"model":"alias","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

		body := `{"model":"alias","stream":false,"messages":[{"role":"user","content":"hi"}]}`
		Expect(Forward(c, cfg, []byte(body), nil)).To(Succeed())

		Expect(rec.Code).To(Equal(200))
		Expect(rec.Body.String()).To(ContainSubstring(`"content":"hi"`))
		// Model rewrite must have replaced the alias.
		var sent map[string]any
		Expect(json.Unmarshal(capturedBody, &sent)).To(Succeed())
		Expect(sent["model"]).To(Equal("gpt-4o-mini"))
	})

	It("openai auth header", func() {
		var captured *http.Request
		upstream := fakeUpstream(200, `{}`, &captured, nil)
		defer upstream.Close()

		GinkgoT().Setenv("PROXY_TEST_KEY", "sk-secret-xyz")
		cfg := &config.ModelConfig{
			Backend: "proxy-openai",
			Proxy: config.ProxyConfig{
				UpstreamURL: upstream.URL,
				APIKeyEnv:   "PROXY_TEST_KEY",
			},
		}
		c, _ := newEchoCtx("POST", "/v1/chat/completions", `{}`)
		Expect(Forward(c, cfg, []byte(`{"model":"x"}`), nil)).To(Succeed())
		Expect(captured.Header.Get("Authorization")).To(Equal("Bearer sk-secret-xyz"))
		Expect(captured.Header.Get("x-api-key")).To(BeEmpty(), "x-api-key leaked on openai backend")
	})

	It("anthropic auth header", func() {
		var captured *http.Request
		upstream := fakeUpstream(200, `{}`, &captured, nil)
		defer upstream.Close()

		GinkgoT().Setenv("PROXY_TEST_KEY", "ant-secret")
		cfg := &config.ModelConfig{
			Backend: "proxy-anthropic",
			Proxy: config.ProxyConfig{
				UpstreamURL: upstream.URL,
				APIKeyEnv:   "PROXY_TEST_KEY",
			},
		}
		c, _ := newEchoCtx("POST", "/v1/messages", `{}`)
		Expect(Forward(c, cfg, []byte(`{"model":"x"}`), nil)).To(Succeed())
		Expect(captured.Header.Get("x-api-key")).To(Equal("ant-secret"))
		Expect(captured.Header.Get("anthropic-version")).NotTo(BeEmpty(), "anthropic-version header missing")
		Expect(captured.Header.Get("Authorization")).To(BeEmpty(), "Authorization leaked on anthropic backend")
	})

	It("upstream error passthrough", func() {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit"}}`)
		}))
		defer upstream.Close()

		cfg := &config.ModelConfig{
			Backend: "proxy-openai",
			Proxy:   config.ProxyConfig{UpstreamURL: upstream.URL},
		}
		c, rec := newEchoCtx("POST", "/v1/chat/completions", `{}`)
		Expect(Forward(c, cfg, []byte(`{"model":"x"}`), nil)).To(Succeed())
		Expect(rec.Code).To(Equal(429))
		Expect(rec.Body.String()).To(ContainSubstring("rate_limit"))
	})

	It("openai stream passthrough", func() {
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"hello "}}]}`,
			``,
			`data: {"choices":[{"delta":{"content":"world"}}]}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n")
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, stream)
		}))
		defer upstream.Close()

		cfg := &config.ModelConfig{
			Backend: "proxy-openai",
			Proxy:   config.ProxyConfig{UpstreamURL: upstream.URL},
		}
		c, rec := newEchoCtx("POST", "/v1/chat/completions", `{"stream":true}`)
		Expect(Forward(c, cfg, []byte(`{"model":"x","stream":true}`), nil)).To(Succeed())
		out := rec.Body.String()
		Expect(out).To(ContainSubstring(`"content":"hello "`))
		Expect(out).To(ContainSubstring(`"content":"world"`))
		Expect(out).To(ContainSubstring("[DONE]"))
	})

	It("openai stream PII rewrite", func() {
		// The redactor masks email addresses by default (see DefaultPatterns).
		// Splitting "alice@example.com" across two chunks proves the
		// streaming filter buffers correctly when the PII spans events.
		stream := strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"contact me at alice@"}}]}`,
			``,
			`data: {"choices":[{"delta":{"content":"example.com please"}}]}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n")
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, stream)
		}))
		defer upstream.Close()

		patterns, err := pii.Compile(pii.DefaultPatterns())
		Expect(err).NotTo(HaveOccurred())
		r := pii.NewRedactor(patterns)
		store := pii.NewMemoryEventStore(16)
		filter := pii.NewStreamFilter(r, nil, store, "corr-1", "user-1")

		cfg := &config.ModelConfig{
			Backend: "proxy-openai",
			Proxy:   config.ProxyConfig{UpstreamURL: upstream.URL},
		}
		c, rec := newEchoCtx("POST", "/v1/chat/completions", `{"stream":true}`)
		Expect(Forward(c, cfg, []byte(`{"model":"x","stream":true}`), filter)).To(Succeed())
		out := rec.Body.String()
		Expect(out).NotTo(ContainSubstring("alice@example.com"), "email leaked through stream")
		Expect(out).To(ContainSubstring("[REDACTED:email]"))
		Expect(out).To(ContainSubstring("[DONE]"))
	})

	It("anthropic stream PII rewrite", func() {
		stream := strings.Join([]string{
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"reach me at bob@"}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"example.org any time"}}`,
			``,
		}, "\n")
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, stream)
		}))
		defer upstream.Close()

		patterns, err := pii.Compile(pii.DefaultPatterns())
		Expect(err).NotTo(HaveOccurred())
		r := pii.NewRedactor(patterns)
		store := pii.NewMemoryEventStore(16)
		filter := pii.NewStreamFilter(r, nil, store, "corr-1", "user-1")

		cfg := &config.ModelConfig{
			Backend: "proxy-anthropic",
			Proxy:   config.ProxyConfig{UpstreamURL: upstream.URL},
		}
		c, rec := newEchoCtx("POST", "/v1/messages", `{"stream":true}`)
		Expect(Forward(c, cfg, []byte(`{"model":"x","stream":true}`), filter)).To(Succeed())
		out := rec.Body.String()
		Expect(out).NotTo(ContainSubstring("bob@example.org"), "email leaked through anthropic stream")
		Expect(out).To(ContainSubstring("[REDACTED:email]"))
		// Event-name preamble must survive the rewrite — Anthropic SDKs
		// route on it.
		Expect(out).To(ContainSubstring("event: content_block_delta"))
	})
})

var _ = Describe("rewriteModel", func() {
	It("is a no-op when upstream model is empty", func() {
		body := []byte(`{"model":"x","stream":false}`)
		out, err := rewriteModel(body, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal(string(body)))
	})

	It("replaces the model", func() {
		body := []byte(`{"model":"alias","stream":false}`)
		out, err := rewriteModel(body, "real-model-id")
		Expect(err).NotTo(HaveOccurred())
		var m map[string]any
		Expect(json.Unmarshal(out, &m)).To(Succeed())
		Expect(m["model"]).To(Equal("real-model-id"))
	})
})

var _ = Describe("providerName", func() {
	cases := map[string]string{
		"proxy-openai":    "openai",
		"proxy-anthropic": "anthropic",
		"proxy-grok":      "openai", // unknowns fall back to openai
		"":                "openai",
	}
	for backend, want := range cases {
		It("maps "+backend, func() {
			Expect(providerName(backend)).To(Equal(want))
		})
	}
})

var _ = Describe("streaming", func() {
	It("detects stream=true", func() {
		Expect(streaming([]byte(`{"stream":true}`))).To(BeTrue())
	})
	It("detects stream=false", func() {
		Expect(streaming([]byte(`{"stream":false}`))).To(BeFalse())
	})
	It("returns false when stream key absent", func() {
		Expect(streaming([]byte(`{}`))).To(BeFalse())
	})
})
