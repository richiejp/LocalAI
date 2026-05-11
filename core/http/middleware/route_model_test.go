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
		return runMiddlewareWithFactories(loader, appConfig, store, extractor, nil, nil, nil, routerCfg, parsed)
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

// runMiddlewareWithFactories is the explicit-deps variant of
// runMiddleware. The KNN/LLM specs need to inject stub embedder,
// LLM-caller, and vector-store factories; the closure form was
// getting unwieldy once those parameters joined.
func runMiddlewareWithFactories(loader *config.ModelConfigLoader, appConfig *config.ApplicationConfig, store router.DecisionStore, extractor ProbeExtractor, embedFactory EmbedderFactory, llmFactory LLMCallerFactory, vsFactory VectorStoreFactory, routerCfg *config.ModelConfig, parsed any) (*httptest.ResponseRecorder, error) {
	mw := RouteModel(loader, appConfig, store, nil, extractor, embedFactory, llmFactory, vsFactory)
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

// stubKNNEmbedder maps text → vector by substring match. Mirrors the
// router-package stub but lives here because middleware_test is in
// a different package.
type stubKNNEmbedder struct {
	mappings map[string][]float32
}

func (s *stubKNNEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	for needle, vec := range s.mappings {
		if strings.Contains(text, needle) {
			out := make([]float32, len(vec))
			copy(out, vec)
			return out, nil
		}
	}
	return []float32{0, 0, 1}, nil
}

// stubVectorStore is an in-memory dot-product KNN used by the
// middleware-level integration test. Mirrors the router-package
// stub — keeps the test free of a real local-store backend process.
type stubVectorStore struct {
	keys   [][]float32
	values [][]byte
}

func (s *stubVectorStore) Set(_ context.Context, keys [][]float32, values [][]byte) error {
	s.keys = append(s.keys, keys...)
	s.values = append(s.values, values...)
	return nil
}

func (s *stubVectorStore) Find(_ context.Context, query []float32, topK int) ([][]byte, []float32, error) {
	type item struct {
		sim float32
		val []byte
	}
	items := make([]item, 0, len(s.keys))
	for i, k := range s.keys {
		var dot float64
		for j := range k {
			dot += float64(k[j]) * float64(query[j])
		}
		items = append(items, item{sim: float32(dot), val: s.values[i]})
	}
	// simple insertion sort by descending sim — n is tiny.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].sim > items[j-1].sim; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	if topK > len(items) {
		topK = len(items)
	}
	values := make([][]byte, topK)
	sims := make([]float32, topK)
	for i := 0; i < topK; i++ {
		values[i] = items[i].val
		sims[i] = items[i].sim
	}
	return values, sims, nil
}

var _ = Describe("RouteModel KNN classifier integration", func() {
	// Pins the KNN branch end-to-end: the middleware builds a KNN
	// classifier from the YAML config, the embedder factory wires
	// in a stub, the classifier picks the matching label, and the
	// model swap + decision-store record happen as for the
	// feature classifier. Without this, the only KNN coverage was
	// the in-package router unit tests — leaving buildClassifier's
	// "knn" arm unverified end-to-end.
	var (
		modelDir  string
		appConfig *config.ApplicationConfig
		loader    *config.ModelConfigLoader
	)
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
		modelDir, err = os.MkdirTemp("", "knn-route-test-*")
		Expect(err).ToNot(HaveOccurred())
		appConfig = &config.ApplicationConfig{
			SystemState: &system.SystemState{Model: system.Model{ModelsPath: modelDir}},
		}
		loader = config.NewModelConfigLoader(modelDir)
	})
	AfterEach(func() { os.RemoveAll(modelDir) })

	It("rewrites the model based on nearest exemplar and records the decision", func() {
		writeYAML("qwen-coder", "name: qwen-coder\nbackend: llama-cpp\n")
		writeYAML("qwen-chat", "name: qwen-chat\nbackend: llama-cpp\n")
		// embedding-anchor stub config so the loader can resolve it
		// when buildClassifier asks the factory.
		writeYAML("text-embedding", "name: text-embedding\nbackend: llama-cpp\n")

		routerCfg := writeYAML("smart-router", `name: smart-router
backend: llama-cpp
router:
  classifier: knn
  embedding_model: text-embedding
  candidates:
    - label: code
      model: qwen-coder
      rules:
        examples:
          - "fix the bug in this function"
    - label: chat
      model: qwen-chat
      rules:
        examples:
          - "hello there"
`)
		embedFactory := func(name string) router.Embedder {
			return &stubKNNEmbedder{mappings: map[string][]float32{
				"bug":   {1, 0, 0},
				"fix":   {1, 0, 0},
				"hello": {0, 1, 0},
				"there": {0, 1, 0},
			}}
		}
		parsed := &schema.OpenAIRequest{}
		parsed.Model = "smart-router"
		parsed.Messages = []schema.Message{{Role: "user", Content: "fix this bug please"}}

		store := router.NewMemoryDecisionStore(10)
		vsFactory := func(_, _ string) router.VectorStore { return &stubVectorStore{} }
		rec, err := runMiddlewareWithFactories(loader, appConfig, store, OpenAIProbe, embedFactory, nil, vsFactory, routerCfg, parsed)
		Expect(err).ToNot(HaveOccurred())
		Expect(rec.Code).To(Equal(http.StatusOK))
		Expect(parsed.Model).To(Equal("qwen-coder"), "KNN should route bug-fix to coder via nearest exemplar")

		decisions, _ := store.List(req(), router.DecisionListQuery{Limit: 10})
		Expect(decisions).To(HaveLen(1))
		Expect(decisions[0].Classifier).To(Equal(router.ClassifierKNN))
		Expect(decisions[0].Label).To(Equal("code"))
		Expect(decisions[0].ServedModel).To(Equal("qwen-coder"))
	})

	It("falls back when embedding_model is missing", func() {
		writeYAML("qwen-fb", "name: qwen-fb\nbackend: llama-cpp\n")
		routerCfg := writeYAML("smart-router", `name: smart-router
backend: llama-cpp
router:
  classifier: knn
  candidates:
    - label: code
      model: qwen-fb
      rules:
        examples: ["x"]
  fallback: qwen-fb
`)
		parsed := &schema.OpenAIRequest{}
		parsed.Model = "smart-router"
		parsed.Messages = []schema.Message{{Role: "user", Content: "anything"}}

		// nil EmbedderFactory mimics --disable-stats / no embedding wiring.
		_, err := runMiddlewareWithFactories(loader, appConfig, nil, OpenAIProbe, nil, nil, nil, routerCfg, parsed)
		Expect(err).ToNot(HaveOccurred(), "should gracefully fall back rather than 503")
		Expect(parsed.Model).To(Equal("qwen-fb"))
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
