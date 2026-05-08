package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/config"
	. "github.com/mudler/LocalAI/core/http/middleware"
	"github.com/mudler/LocalAI/core/schema"
	"github.com/mudler/LocalAI/core/services/routing/router"
	"github.com/mudler/LocalAI/pkg/system"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The middleware is the load-bearing piece of subsystem 2 — it wires
// the classifier output into request rewrites, the depth-1 invariant
// check, the fallback path, and the decision-store record. Bugs here
// silently mis-route or leak router models into the model loader. The
// specs below pin each branch.

var _ = Describe("RouteModel middleware", func() {
	var (
		modelDir  string
		appConfig *config.ApplicationConfig
		loader    *config.ModelConfigLoader
	)

	// writeYAML drops a model YAML into the temp dir and reads it back
	// into the loader cache. The router middleware calls
	// LoadModelConfigFileByNameDefaultOptions which prefers the cache
	// over a file load, so the cache pre-populates everything.
	writeYAML := func(name, body string) *config.ModelConfig {
		path := filepath.Join(modelDir, name+".yaml")
		Expect(os.WriteFile(path, []byte(body), 0o644)).To(Succeed())
		Expect(loader.ReadModelConfig(path)).To(Succeed())
		cfg, err := loader.LoadModelConfigFileByNameDefaultOptions(name, appConfig)
		Expect(err).ToNot(HaveOccurred())
		return cfg
	}

	BeforeEach(func() {
		var err error
		modelDir, err = os.MkdirTemp("", "route-model-test-*")
		Expect(err).ToNot(HaveOccurred())
		appConfig = &config.ApplicationConfig{
			SystemState: &system.SystemState{Model: system.Model{ModelsPath: modelDir}},
		}
		loader = config.NewModelConfigLoader(modelDir)
	})

	AfterEach(func() {
		os.RemoveAll(modelDir)
	})

	// runMiddleware drives the middleware against a synthetic echo
	// context with parsed + cfg pre-populated, the same shape
	// SetModelAndConfig leaves behind. Returns the echo response and
	// the parsed request after rewrite so callers can assert on the
	// model-name swap.
	runMiddleware := func(routerCfg *config.ModelConfig, parsed any, store router.DecisionStore, extractor ProbeExtractor) (*httptest.ResponseRecorder, error) {
		mw := RouteModel(loader, appConfig, store, nil, extractor)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(req, rec)
		c.Set(CONTEXT_LOCALS_KEY_MODEL_CONFIG, routerCfg)
		if parsed != nil {
			c.Set(CONTEXT_LOCALS_KEY_LOCALAI_REQUEST, parsed)
		}
		err := mw(func(c echo.Context) error {
			c.String(http.StatusOK, "ok")
			return nil
		})(c)
		return rec, err
	}

	Describe("classifier success path", func() {
		It("rewrites the model and records a decision", func() {
			writeYAML("qwen-3b", `name: qwen-3b
backend: llama-cpp
`)
			writeYAML("qwen-32b", `name: qwen-32b
backend: llama-cpp
`)
			routerCfg := writeYAML("smart-router", `name: smart-router
backend: llama-cpp
router:
  classifier: feature
  candidates:
    - label: small
      model: qwen-3b
      rules:
        max_prompt_length: 50
    - label: large
      model: qwen-32b
      rules: {}
  fallback: qwen-3b
`)
			parsed := &schema.OpenAIRequest{}
			parsed.Model = "smart-router"
			parsed.Messages = []schema.Message{{Role: "user", Content: "hi"}}

			store := router.NewMemoryDecisionStore(10)
			rec, err := runMiddleware(routerCfg, parsed, store, OpenAIProbe)
			Expect(err).ToNot(HaveOccurred())
			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(parsed.Model).To(Equal("qwen-3b"), "model should be rewritten to the matched candidate")

			decisions, err := store.List(req(), router.DecisionListQuery{Limit: 10})
			Expect(err).ToNot(HaveOccurred())
			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].RouterModel).To(Equal("smart-router"))
			Expect(decisions[0].ServedModel).To(Equal("qwen-3b"))
			Expect(decisions[0].Label).To(Equal("small"))
		})
	})

	Describe("pass-through paths", func() {
		It("passes through when MODEL_CONFIG has no Router block", func() {
			plain := writeYAML("plain", `name: plain
backend: llama-cpp
`)
			parsed := &schema.OpenAIRequest{}
			parsed.Model = "plain"
			rec, err := runMiddleware(plain, parsed, nil, OpenAIProbe)
			Expect(err).ToNot(HaveOccurred())
			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(parsed.Model).To(Equal("plain"), "non-router model must not be rewritten")
		})

		It("passes through when LOCALAI_REQUEST is missing", func() {
			writeYAML("qwen-3b", "name: qwen-3b\nbackend: llama-cpp\n")
			routerCfg := writeYAML("smart-router", `name: smart-router
backend: llama-cpp
router:
  classifier: feature
  candidates:
    - label: small
      model: qwen-3b
      rules: {}
`)
			rec, err := runMiddleware(routerCfg, nil, nil, OpenAIProbe)
			Expect(err).ToNot(HaveOccurred())
			Expect(rec.Code).To(Equal(http.StatusOK))
		})

		It("passes through when the extractor refuses the parsed type", func() {
			writeYAML("qwen-3b", "name: qwen-3b\nbackend: llama-cpp\n")
			routerCfg := writeYAML("smart-router", `name: smart-router
backend: llama-cpp
router:
  candidates:
    - label: small
      model: qwen-3b
`)
			rejecting := func(parsed any) (router.Probe, bool) { return router.Probe{}, false }
			rec, err := runMiddleware(routerCfg, &schema.OpenAIRequest{}, nil, rejecting)
			Expect(err).ToNot(HaveOccurred())
			Expect(rec.Code).To(Equal(http.StatusOK))
		})
	})

	Describe("fallback paths", func() {
		It("falls back to cfg.Router.Fallback when the classifier returns no match", func() {
			writeYAML("qwen-fallback", "name: qwen-fallback\nbackend: llama-cpp\n")
			routerCfg := writeYAML("smart-router", `name: smart-router
backend: llama-cpp
router:
  classifier: feature
  candidates:
    - label: code
      model: qwen-coder
      rules:
        requires_code: true
  fallback: qwen-fallback
`)
			// Prompt has no code fence — feature classifier returns
			// "no candidate rule matched", middleware must use fallback.
			parsed := &schema.OpenAIRequest{}
			parsed.Model = "smart-router"
			parsed.Messages = []schema.Message{{Role: "user", Content: "plain text without backticks"}}

			store := router.NewMemoryDecisionStore(10)
			rec, err := runMiddleware(routerCfg, parsed, store, OpenAIProbe)
			Expect(err).ToNot(HaveOccurred())
			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(parsed.Model).To(Equal("qwen-fallback"))

			decisions, _ := store.List(req(), router.DecisionListQuery{Limit: 10})
			Expect(decisions).To(HaveLen(1))
			Expect(decisions[0].Label).To(Equal("fallback"))
			Expect(decisions[0].ServedModel).To(Equal("qwen-fallback"))
		})

		It("returns 503 when the classifier fails and no fallback is configured", func() {
			routerCfg := writeYAML("smart-router", `name: smart-router
backend: llama-cpp
router:
  classifier: feature
  candidates:
    - label: code
      model: qwen-coder
      rules:
        requires_code: true
`)
			parsed := &schema.OpenAIRequest{}
			parsed.Model = "smart-router"
			parsed.Messages = []schema.Message{{Role: "user", Content: "no code here"}}

			rec, err := runMiddleware(routerCfg, parsed, nil, OpenAIProbe)
			// Echo turns NewHTTPError into a written response (code +
			// JSON body) when handed back from the middleware chain.
			Expect(err).To(HaveOccurred())
			httpErr, ok := err.(*echo.HTTPError)
			Expect(ok).To(BeTrue())
			Expect(httpErr.Code).To(Equal(http.StatusServiceUnavailable))
			// Model must NOT have been rewritten on a hard failure.
			Expect(parsed.Model).To(Equal("smart-router"))
			_ = rec
		})

		It("falls back when an unsupported classifier is named (with fallback)", func() {
			writeYAML("qwen-fallback", "name: qwen-fallback\nbackend: llama-cpp\n")
			routerCfg := writeYAML("smart-router", `name: smart-router
backend: llama-cpp
router:
  classifier: knn
  candidates:
    - label: small
      model: qwen-3b
  fallback: qwen-fallback
`)
			parsed := &schema.OpenAIRequest{}
			parsed.Model = "smart-router"
			rec, err := runMiddleware(routerCfg, parsed, nil, OpenAIProbe)
			Expect(err).ToNot(HaveOccurred())
			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(parsed.Model).To(Equal("qwen-fallback"))
		})
	})

	Describe("depth-1 invariant", func() {
		It("returns 500 when the candidate is itself a router", func() {
			// "inner" is a router model — picking it as a candidate
			// would create a router→router chain. The middleware must
			// reject this at request time even though config validation
			// also rejects it (gallery installs can produce this state
			// after startup).
			writeYAML("qwen-leaf", "name: qwen-leaf\nbackend: llama-cpp\n")
			writeYAML("inner-router", `name: inner-router
backend: llama-cpp
router:
  classifier: feature
  candidates:
    - label: only
      model: qwen-leaf
`)
			routerCfg := writeYAML("outer-router", `name: outer-router
backend: llama-cpp
router:
  classifier: feature
  candidates:
    - label: pick
      model: inner-router
      rules: {}
`)
			parsed := &schema.OpenAIRequest{}
			parsed.Model = "outer-router"
			parsed.Messages = []schema.Message{{Role: "user", Content: "hi"}}

			_, err := runMiddleware(routerCfg, parsed, nil, OpenAIProbe)
			Expect(err).To(HaveOccurred())
			httpErr, ok := err.(*echo.HTTPError)
			Expect(ok).To(BeTrue())
			Expect(httpErr.Code).To(Equal(http.StatusInternalServerError))
			Expect(httpErr.Message).To(ContainSubstring("depth-1"))
		})
	})

	Describe("nil-store path", func() {
		It("rewrites the request even when the decision store is nil", func() {
			// --disable-stats turns off the decision store; the
			// rewrite must still happen so requests complete. Only
			// the audit row is silently dropped.
			writeYAML("qwen-3b", "name: qwen-3b\nbackend: llama-cpp\n")
			routerCfg := writeYAML("smart-router", `name: smart-router
backend: llama-cpp
router:
  classifier: feature
  candidates:
    - label: only
      model: qwen-3b
      rules: {}
`)
			parsed := &schema.OpenAIRequest{}
			parsed.Model = "smart-router"
			parsed.Messages = []schema.Message{{Role: "user", Content: "hi"}}

			rec, err := runMiddleware(routerCfg, parsed, nil, OpenAIProbe)
			Expect(err).ToNot(HaveOccurred())
			Expect(rec.Code).To(Equal(http.StatusOK))
			Expect(parsed.Model).To(Equal("qwen-3b"))
		})
	})
})

var _ = Describe("OpenAIProbe extractor", func() {
	It("concatenates string-form messages with newlines", func() {
		req := &schema.OpenAIRequest{}
		req.Messages = []schema.Message{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: "say hi"},
		}
		probe, ok := OpenAIProbe(req)
		Expect(ok).To(BeTrue())
		Expect(probe.Prompt).To(ContainSubstring("you are helpful"))
		Expect(probe.Prompt).To(ContainSubstring("say hi"))
		Expect(probe.HasCode).To(BeFalse())
	})

	It("walks []any content blocks and detects code fences", func() {
		req := &schema.OpenAIRequest{}
		req.Messages = []schema.Message{
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "look at this:"},
				map[string]any{"type": "text", "text": "```go\nfunc x(){}\n```"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "..."}},
			}},
		}
		probe, ok := OpenAIProbe(req)
		Expect(ok).To(BeTrue())
		Expect(probe.HasCode).To(BeTrue())
		Expect(probe.Prompt).To(ContainSubstring("func x"))
	})

	It("rejects nil and the wrong type", func() {
		_, ok := OpenAIProbe(nil)
		Expect(ok).To(BeFalse())
		_, ok = OpenAIProbe("a string")
		Expect(ok).To(BeFalse())
		_, ok = OpenAIProbe((*schema.OpenAIRequest)(nil))
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("AnthropicProbe extractor", func() {
	It("walks AnthropicRequest message content", func() {
		req := &schema.AnthropicRequest{
			Messages: []schema.AnthropicMessage{
				{Role: "user", Content: "plain question"},
			},
		}
		probe, ok := AnthropicProbe(req)
		Expect(ok).To(BeTrue())
		Expect(probe.Prompt).To(ContainSubstring("plain question"))
		Expect(probe.HasCode).To(BeFalse())
	})

	It("rejects nil and the wrong type", func() {
		_, ok := AnthropicProbe(nil)
		Expect(ok).To(BeFalse())
		_, ok = AnthropicProbe(&schema.OpenAIRequest{})
		Expect(ok).To(BeFalse())
	})
})

// req returns a dummy context for the decision store calls. The store
// implementations ignore it.
func req() context.Context { return context.Background() }
