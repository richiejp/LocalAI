package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/core/http/auth"
	"github.com/mudler/LocalAI/core/schema"
	"github.com/mudler/LocalAI/core/services/routing/router"
	"github.com/mudler/xlog"
	"gopkg.in/yaml.v3"
)

// ScorerFactory returns a router.Scorer bound to a named classifier
// model. The Score classifier uses it to compute joint log-prob of
// every policy label against the routing prompt and read off the
// softmax distribution.
type ScorerFactory func(modelName string) router.Scorer

// EmbedderFactory returns a router.Embedder bound to a named model.
// Used by the L2 embedding cache. Returning nil signals "model not
// loadable" — the middleware then falls back to the uncached
// classifier so routing still happens.
type EmbedderFactory func(modelName string) router.Embedder

// VectorStoreFactory returns a router.VectorStore bound to a named
// collection. Each router model's cache lives in its own collection
// (see ClassifierDeps.storeNameFor) so two routers can't poison each
// other's hits.
type VectorStoreFactory func(storeName string) router.VectorStore

// RerankerFactory returns a router.Reranker bound to a named model.
// Used by the colbert classifier to score policy descriptions against
// the prompt via LocalAI's rerankers backend. Returning nil signals
// "model not loadable" — buildClassifier reports a config error.
type RerankerFactory func(modelName string) router.Reranker

// ClassifierDeps bundles the backend factories the router middleware
// needs to build a classifier and its optional L2 cache. Bundled into
// one struct because RouteModel already takes many positional
// arguments — additions to the dependency surface go here instead of
// growing the signature.
//
// Embedder and VectorStore are optional: when both are non-nil and the
// router config declares an embedding_cache block, the score
// classifier is wrapped in EmbeddingCacheClassifier. Otherwise the
// score classifier runs unwrapped and the embedding-cache YAML is
// ignored with a warning.
type ClassifierDeps struct {
	Scorer      ScorerFactory
	Embedder    EmbedderFactory
	VectorStore VectorStoreFactory
	Reranker    RerankerFactory

	// Registry is the shared classifier cache. Both the OpenAI and
	// Anthropic routes pass the same registry so the admin stats
	// endpoint sees every live classifier. Nil falls back to a local
	// registry — tests that don't need cross-route stats use this.
	Registry *router.Registry
}

// ProbeExtractor pulls the prompt content out of a parsed request so
// the classifier can inspect it without taking a dependency on the
// schema package. One extractor per request shape — wired by the
// route registration site (mirrors the piiadapter pattern).
//
// Returns ok=false when the parsed value isn't the expected type — the
// middleware then passes through without engaging the router.
type ProbeExtractor func(parsed any) (router.Probe, bool)

// RouteModel runs after SetModelAndConfig and the schema-specific
// SetXRequest, looks at the resolved model's Router config, and (when
// present) reclassifies the request to one of the candidates.
//
// The middleware:
//
//  1. Loads MODEL_CONFIG from the echo context. If nil or HasRouter()
//     is false, passes through.
//  2. Extracts the probe via the supplied ProbeExtractor.
//  3. Invokes the classifier matching cfg.Router.Classifier. Today
//     only "feature" is supported; unknown classifiers fall back to
//     cfg.Router.Fallback (or fail when none is set).
//  4. Resolves the chosen candidate to its model name. Reloads the
//     ModelConfig for that model and asserts depth-1 (the candidate
//     must NOT itself have a Router). Violation returns 500 — config
//     bug, not a request bug.
//  5. Updates input.Model in place, replaces MODEL_CONFIG with the
//     candidate's config, and stamps RequestedModel/ServedModel on the
//     context so UsageMiddleware records the routing.
//  6. Writes a DecisionRecord to the store for the admin page.
//
// store may be nil when --disable-stats turns off the routing log;
// classification still runs.
//
// Composition with SmartRouter (distributed mode): this middleware
// only does *model* selection. Node selection still happens in
// SmartRouter.Route() downstream of this middleware.
// RouteModel wires the router middleware. source is the value written to
// DecisionRecord.Source (router.SourceChat / SourceAnthropic / ...) so
// the admin page can split decisions by entry point. Pass
// router.SourceChat for the OpenAI chat endpoint, router.SourceAnthropic
// for the Anthropic messages endpoint.
func RouteModel(loader *config.ModelConfigLoader, appConfig *config.ApplicationConfig, store router.DecisionStore, fallbackUser *auth.User, extractor ProbeExtractor, source string, deps ClassifierDeps) echo.MiddlewareFunc {
	registry := deps.Registry
	if registry == nil {
		registry = router.NewRegistry()
	}
	candidateLoader := func(name string) (*config.ModelConfig, error) {
		return loader.LoadModelConfigFileByNameDefaultOptions(name, appConfig)
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			cfg, ok := c.Get(CONTEXT_LOCALS_KEY_MODEL_CONFIG).(*config.ModelConfig)
			if !ok || cfg == nil || !cfg.HasRouter() {
				return next(c)
			}

			parsed := c.Get(CONTEXT_LOCALS_KEY_LOCALAI_REQUEST)
			if parsed == nil {
				return next(c)
			}

			probe, probeOK := extractor(parsed)
			if !probeOK {
				return next(c)
			}

			classifier, classifierErr := GetOrBuildClassifier(registry, cfg, deps)
			if classifierErr != nil {
				xlog.Warn("router: classifier unavailable — falling back",
					"router_model", cfg.Name, "classifier", cfg.Router.Classifier, "error", classifierErr)
				// classifier == nil pushes Resolve straight to the
				// fallback path; if no fallback is configured Resolve
				// returns a terminal error that we surface as 503.
				classifier = nil
			}

			result, err := router.Resolve(c.Request().Context(), cfg, classifier, candidateLoader, probe)
			if err != nil {
				xlog.Warn("router: resolve failed", "router_model", cfg.Name, "error", err)
				// classifier-unavailable + no-fallback maps to 503;
				// candidate-not-loadable + depth-1 violations map to
				// 500. Resolve embeds enough context in the error to
				// tell them apart by string match, but the historical
				// behaviour returned 503 only for the classifier-side
				// failures — preserve that.
				if classifierErr != nil {
					return echo.NewHTTPError(503, "router classifier unavailable and no fallback configured")
				}
				return echo.NewHTTPError(500, err.Error())
			}

			if req, ok := parsed.(schema.LocalAIRequest); ok {
				chosen := result.ChosenModel
				req.ModelName(&chosen)
			}

			c.Set(CONTEXT_LOCALS_KEY_MODEL_CONFIG, result.ChosenConfig)
			c.Set(ContextKeyRequestedModel, result.RouterModel)
			c.Set(ContextKeyServedModel, result.ChosenModel)

			if store != nil {
				recordHTTPDecision(c, store, result, fallbackUser, source)
			}
			return next(c)
		}
	}
}

// recordHTTPDecision writes the resolved decision to the store with
// HTTP-shaped audit metadata (correlation id from header, user from
// auth middleware, fallback to the synthetic local user). Realtime
// has its own recorder that supplies session-derived metadata
// instead.
func recordHTTPDecision(c echo.Context, store router.DecisionStore, result *router.ResolveResult, fallbackUser *auth.User, source string) {
	correlationID, _ := c.Get(ContextKeyCorrelationID).(string)
	if correlationID == "" {
		correlationID = c.Response().Header().Get("X-Correlation-ID")
	}
	userID := ""
	if u := auth.GetUser(c); u != nil {
		userID = u.ID
	} else if fallbackUser != nil {
		userID = fallbackUser.ID
	}
	_ = store.Record(context.Background(), router.DecisionRecord{
		ID:              newDecisionID(),
		CorrelationID:   correlationID,
		UserID:          userID,
		RouterModel:     result.RouterModel,
		RequestedModel:  result.RouterModel,
		ServedModel:     result.ChosenModel,
		Classifier:      result.ClassifierName,
		Label:           strings.Join(result.Labels, ","),
		Score:           result.Decision.Score,
		LatencyMs:       result.Decision.Latency.Milliseconds(),
		Cached:          result.Decision.Cached,
		CacheSimilarity: result.Decision.CacheSimilarity,
		Source:          source,
		CreatedAt:       time.Now().UTC(),
	})
}


// GetOrBuildClassifier looks up a built Classifier for the named router
// model in the registry and builds it on miss. Exported so the
// /api/router/decide decision-oracle endpoint can share the same
// build-once cache that the in-band RouteModel middleware uses.
func GetOrBuildClassifier(registry *router.Registry, cfg *config.ModelConfig, deps ClassifierDeps) (router.Classifier, error) {
	fp := routerConfigFingerprint(cfg.Router)
	if cached, ok := registry.Get(cfg.Name, fp); ok {
		return cached, nil
	}
	c, err := buildClassifier(cfg, deps)
	if err != nil {
		return nil, err
	}
	registry.Put(cfg.Name, fp, c)
	return c, nil
}

// routerConfigFingerprint is a stable cache key for a RouterConfig.
// FNV-64 over the YAML form — equality-only, not cryptographic.
// YAML-marshal picks up any future field added to RouterConfig
// without this function needing to be touched.
func routerConfigFingerprint(rc config.RouterConfig) uint64 {
	bytes, err := yaml.Marshal(rc)
	if err != nil {
		// Marshalling a value type can't fail in practice; fall
		// back to a hash that varies per call so we don't quietly
		// share a cache entry across distinct configs.
		return uint64(time.Now().UnixNano())
	}
	h := fnv.New64a()
	h.Write(bytes)
	return h.Sum64()
}

func buildClassifier(cfg *config.ModelConfig, deps ClassifierDeps) (router.Classifier, error) {
	rc := cfg.Router
	name := rc.Classifier
	if name == "" {
		name = router.ClassifierScore
	}
	policies, err := validateRouterPolicies(name, rc)
	if err != nil {
		return nil, err
	}
	cacheCap := rc.ClassifierCacheSize
	if cacheCap == 0 {
		cacheCap = 1024
	}

	var inner router.Classifier
	switch name {
	case router.ClassifierScore:
		if deps.Scorer == nil {
			return nil, fmt.Errorf("router classifier score unavailable: no scorer factory wired")
		}
		scorer := deps.Scorer(rc.ClassifierModel)
		if scorer == nil {
			return nil, fmt.Errorf("router classifier score: classifier_model %q not loadable", rc.ClassifierModel)
		}
		inner = router.NewScoreClassifier(policies, scorer, cacheCap, rc.ActivationThreshold)
	case router.ClassifierColbert:
		if deps.Reranker == nil {
			return nil, fmt.Errorf("router classifier colbert unavailable: no reranker factory wired")
		}
		reranker := deps.Reranker(rc.ClassifierModel)
		if reranker == nil {
			return nil, fmt.Errorf("router classifier colbert: classifier_model %q not loadable", rc.ClassifierModel)
		}
		inner = router.NewRerankClassifier(policies, reranker, cacheCap, rc.ActivationThreshold)
	default:
		return nil, fmt.Errorf("router: unknown classifier %q (supported: %s)", name, strings.Join([]string{router.ClassifierScore, router.ClassifierColbert}, ", "))
	}

	if rc.EmbeddingCache == nil {
		return inner, nil
	}
	wrapped, err := wrapWithEmbeddingCache(cfg, inner, deps)
	if err != nil {
		// Caching plumbing problems must not break routing — log,
		// drop the cache layer, and return the uncached classifier.
		// The admin UI surfaces the warning via the classifier-build
		// error path used elsewhere.
		xlog.Warn("router: embedding cache disabled",
			"router_model", cfg.Name, "error", err)
		return inner, nil
	}
	return wrapped, nil
}

// validateRouterPolicies checks the shared invariants both classifiers
// rely on (non-empty policies, every candidate label declared as a
// policy, every candidate has a model + at least one label) and
// returns the parsed []ScorePolicy. Both Score and Rerank classifiers
// take the same policy shape.
func validateRouterPolicies(classifierName string, rc config.RouterConfig) ([]router.ScorePolicy, error) {
	if rc.ClassifierModel == "" {
		return nil, fmt.Errorf("router classifier %s requires classifier_model", classifierName)
	}
	if len(rc.Policies) == 0 {
		return nil, fmt.Errorf("router classifier %s requires at least one policy", classifierName)
	}
	policies := make([]router.ScorePolicy, 0, len(rc.Policies))
	for _, p := range rc.Policies {
		if p.Label == "" {
			return nil, fmt.Errorf("router classifier %s: policy with empty label", classifierName)
		}
		if p.Description == "" {
			return nil, fmt.Errorf("router classifier %s: policy %q has no description", classifierName, p.Label)
		}
		policies = append(policies, router.ScorePolicy{Label: p.Label, Description: p.Description})
	}
	policyLabels := make(map[string]struct{}, len(policies))
	for _, p := range policies {
		policyLabels[p.Label] = struct{}{}
	}
	for _, c := range rc.Candidates {
		if c.Model == "" {
			return nil, fmt.Errorf("router classifier %s: candidate has empty model field", classifierName)
		}
		if len(c.Labels) == 0 {
			return nil, fmt.Errorf("router classifier %s: candidate %q has no labels", classifierName, c.Model)
		}
		for _, l := range c.Labels {
			if _, ok := policyLabels[l]; !ok {
				return nil, fmt.Errorf("router classifier %s: candidate %q references unknown label %q (not in policies)", classifierName, c.Model, l)
			}
		}
	}
	return policies, nil
}

func wrapWithEmbeddingCache(cfg *config.ModelConfig, inner router.Classifier, deps ClassifierDeps) (router.Classifier, error) {
	ec := cfg.Router.EmbeddingCache
	if ec.EmbeddingModel == "" {
		return nil, fmt.Errorf("embedding_cache requires embedding_model")
	}
	if deps.Embedder == nil || deps.VectorStore == nil {
		return nil, fmt.Errorf("embedding cache factories not wired")
	}
	embedder := deps.Embedder(ec.EmbeddingModel)
	if embedder == nil {
		return nil, fmt.Errorf("embedding_model %q not loadable", ec.EmbeddingModel)
	}
	storeName := ec.StoreName
	if storeName == "" {
		storeName = "router-cache-" + cfg.Name
	}
	vstore := deps.VectorStore(storeName)
	if vstore == nil {
		return nil, fmt.Errorf("vector store %q not loadable", storeName)
	}
	return router.NewEmbeddingCacheClassifier(inner, embedder, vstore, ec.SimilarityThreshold, ec.ConfidenceThreshold), nil
}

func newDecisionID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "rd_" + hex.EncodeToString(b[:])
}

// OpenAIProbe extracts a router.Probe from a parsed *schema.OpenAIRequest.
// Concatenates message contents (string-form or text blocks of the
// structured `[]any` content) so the classifier sees a single corpus
// for length and content-shape rules. Image blocks are skipped — a
// future multimodal classifier can take a different route.
func OpenAIProbe(parsed any) (router.Probe, bool) {
	req, ok := parsed.(*schema.OpenAIRequest)
	if !ok || req == nil {
		return router.Probe{}, false
	}
	return OpenAIProbeFromRequest(req), true
}

// OpenAIProbeFromRequest is the typed counterpart of OpenAIProbe — same
// extraction logic, but takes the request struct directly. Realtime and
// other non-HTTP callers use it to feed a probe to router.Resolve
// without going through an echo.Context first.
func OpenAIProbeFromRequest(req *schema.OpenAIRequest) router.Probe {
	if req == nil {
		return router.Probe{}
	}
	var b strings.Builder
	for i := range req.Messages {
		switch ct := req.Messages[i].Content.(type) {
		case string:
			b.WriteString(ct)
			b.WriteByte('\n')
		case []any:
			for _, block := range ct {
				if bm, ok := block.(map[string]any); ok && bm["type"] == "text" {
					if t, ok := bm["text"].(string); ok {
						b.WriteString(t)
						b.WriteByte('\n')
					}
				}
			}
		}
	}
	return router.Probe{Prompt: b.String()}
}

// AnthropicProbe is the AnthropicRequest analogue of OpenAIProbe.
func AnthropicProbe(parsed any) (router.Probe, bool) {
	req, ok := parsed.(*schema.AnthropicRequest)
	if !ok || req == nil {
		return router.Probe{}, false
	}
	var b strings.Builder
	for i := range req.Messages {
		switch ct := req.Messages[i].Content.(type) {
		case string:
			b.WriteString(ct)
			b.WriteByte('\n')
		case []any:
			for _, block := range ct {
				if bm, ok := block.(map[string]any); ok && bm["type"] == "text" {
					if t, ok := bm["text"].(string); ok {
						b.WriteString(t)
						b.WriteByte('\n')
					}
				}
			}
		}
	}
	return router.Probe{
		Prompt: b.String(),
	}, true
}

