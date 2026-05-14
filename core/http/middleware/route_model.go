package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"slices"
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
func RouteModel(loader *config.ModelConfigLoader, appConfig *config.ApplicationConfig, store router.DecisionStore, fallbackUser *auth.User, extractor ProbeExtractor, deps ClassifierDeps) echo.MiddlewareFunc {
	registry := deps.Registry
	if registry == nil {
		registry = router.NewRegistry()
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
				if cfg.Router.Fallback == "" {
					return echo.NewHTTPError(503, "router classifier unavailable and no fallback configured")
				}
				return rewriteRequest(c, parsed, cfg, cfg.Router.Fallback, []string{router.LabelFallback}, router.Decision{Labels: []string{router.LabelFallback}}, router.LabelFallback, store, fallbackUser, loader, appConfig, next)
			}

			start := time.Now()
			decision, err := classifier.Classify(c.Request().Context(), probe)
			if err != nil {
				xlog.Warn("router: classifier returned error — using fallback",
					"router_model", cfg.Name, "error", err, "latency_ms", time.Since(start).Milliseconds())
				if cfg.Router.Fallback == "" {
					return echo.NewHTTPError(503, "router classification failed: "+err.Error())
				}
				return rewriteRequest(c, parsed, cfg, cfg.Router.Fallback, []string{router.LabelFallback}, router.Decision{Labels: []string{router.LabelFallback}, Latency: time.Since(start)}, classifier.Name(), store, fallbackUser, loader, appConfig, next)
			}

			candidate := MatchCandidate(cfg.Router.Candidates, decision.Labels)
			if candidate == "" {
				xlog.Warn("router: no candidate covers active labels — using fallback",
					"router_model", cfg.Name, "labels", decision.Labels)
				if cfg.Router.Fallback == "" {
					return echo.NewHTTPError(500, "no candidate covers active labels: "+strings.Join(decision.Labels, ","))
				}
				candidate = cfg.Router.Fallback
			}

			return rewriteRequest(c, parsed, cfg, candidate, decision.Labels, decision, classifier.Name(), store, fallbackUser, loader, appConfig, next)
		}
	}
}

// rewriteRequest swaps the resolved model from the router to the
// chosen candidate, asserts the depth-1 invariant on the new config,
// records the decision, and continues. Pulled out so the classifier-
// success and fallback paths share one rewrite implementation.
func rewriteRequest(c echo.Context, parsed any, routerCfg *config.ModelConfig, candidateModel string, labels []string, decision router.Decision, classifierName string, store router.DecisionStore, fallbackUser *auth.User, loader *config.ModelConfigLoader, appConfig *config.ApplicationConfig, next echo.HandlerFunc) error {
	candidateCfg, err := loader.LoadModelConfigFileByNameDefaultOptions(candidateModel, appConfig)
	if err != nil || candidateCfg == nil {
		xlog.Error("router: failed to load candidate config",
			"router_model", routerCfg.Name, "candidate", candidateModel, "error", err)
		return echo.NewHTTPError(500, "router candidate not loadable: "+candidateModel)
	}

	// Depth-1 invariant: the resolved candidate must NOT itself be a
	// router. Chained routers turn dispatch into a graph traversal —
	// a configuration we deliberately reject. The check is at runtime
	// because gallery installs can introduce a Router on a previously-
	// flat model after startup.
	if candidateCfg.HasRouter() {
		xlog.Error("router: depth-1 invariant violated — candidate is itself a router",
			"router_model", routerCfg.Name, "candidate", candidateModel)
		return echo.NewHTTPError(500, "router candidate is itself a router (depth-1 invariant)")
	}

	if req, ok := parsed.(schema.LocalAIRequest); ok {
		req.ModelName(&candidateModel)
	}

	c.Set(CONTEXT_LOCALS_KEY_MODEL_CONFIG, candidateCfg)
	c.Set(ContextKeyRequestedModel, routerCfg.Name)
	c.Set(ContextKeyServedModel, candidateModel)

	if store != nil {
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
			RouterModel:     routerCfg.Name,
			RequestedModel:  routerCfg.Name,
			ServedModel:     candidateModel,
			Classifier:      classifierName,
			Label:           strings.Join(labels, ","),
			Score:           decision.Score,
			LatencyMs:       decision.Latency.Milliseconds(),
			Cached:          decision.Cached,
			CacheSimilarity: decision.CacheSimilarity,
			CreatedAt:       time.Now().UTC(),
		})
	}

	return next(c)
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
	classifier := rc.Classifier
	if classifier == "" {
		classifier = router.ClassifierScore
	}
	if classifier != router.ClassifierScore {
		return nil, fmt.Errorf("router: unknown classifier %q (only %q is supported)", classifier, router.ClassifierScore)
	}
	if rc.ClassifierModel == "" {
		return nil, fmt.Errorf("router classifier score requires classifier_model")
	}
	if deps.Scorer == nil {
		return nil, fmt.Errorf("router classifier score unavailable: no scorer factory wired")
	}
	scorer := deps.Scorer(rc.ClassifierModel)
	if scorer == nil {
		return nil, fmt.Errorf("router classifier score: classifier_model %q not loadable", rc.ClassifierModel)
	}
	if len(rc.Policies) == 0 {
		return nil, fmt.Errorf("router classifier score requires at least one policy")
	}
	policies := make([]router.ScorePolicy, 0, len(rc.Policies))
	for _, p := range rc.Policies {
		if p.Label == "" {
			return nil, fmt.Errorf("router classifier score: policy with empty label")
		}
		if p.Description == "" {
			return nil, fmt.Errorf("router classifier score: policy %q has no description", p.Label)
		}
		policies = append(policies, router.ScorePolicy{
			Label:       p.Label,
			Description: p.Description,
		})
	}
	// Validate that every label referenced by a candidate is declared
	// as a policy — otherwise the classifier would emit labels no
	// candidate covers, and the routing always falls back.
	policyLabels := make(map[string]struct{}, len(policies))
	for _, p := range policies {
		policyLabels[p.Label] = struct{}{}
	}
	for _, c := range rc.Candidates {
		if c.Model == "" {
			return nil, fmt.Errorf("router classifier score: candidate has empty model field")
		}
		if len(c.Labels) == 0 {
			return nil, fmt.Errorf("router classifier score: candidate %q has no labels", c.Model)
		}
		for _, l := range c.Labels {
			if _, ok := policyLabels[l]; !ok {
				return nil, fmt.Errorf("router classifier score: candidate %q references unknown label %q (not in policies)", c.Model, l)
			}
		}
	}
	cacheCap := rc.ClassifierCacheSize
	if cacheCap == 0 {
		cacheCap = 1024
	}
	score := router.NewScoreClassifier(policies, scorer, cacheCap, rc.ActivationThreshold)

	if rc.EmbeddingCache == nil {
		return score, nil
	}
	wrapped, err := wrapWithEmbeddingCache(cfg, score, deps)
	if err != nil {
		// Caching plumbing problems must not break routing — log,
		// drop the cache layer, and return the uncached score
		// classifier. The admin UI surfaces the warning via the
		// classifier-build error path used elsewhere.
		xlog.Warn("router: embedding cache disabled",
			"router_model", cfg.Name, "error", err)
		return score, nil
	}
	return wrapped, nil
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

// MatchCandidate picks the FIRST candidate whose Labels are a
// superset of the active label set. Admins order the candidates list
// smallest → largest, so a request that needs one label routes to
// the smallest capable model and one that needs multiple falls to
// the first bigger candidate that covers them all. Returns empty
// string when no candidate matches; the caller falls back.
//
// Exported so the /api/router/decide oracle endpoint can run the same
// label-set → candidate-model resolution as the in-band middleware.
func MatchCandidate(candidates []config.RouterCandidate, active []string) string {
	if len(active) == 0 {
		return ""
	}
	for _, c := range candidates {
		if labelSetCovers(c.Labels, active) {
			return c.Model
		}
	}
	return ""
}

// labelSetCovers returns true when every element of needed appears
// in have. Label sets are typically <10 entries so the linear scan
// is fine.
func labelSetCovers(have, needed []string) bool {
	for _, n := range needed {
		if !slices.Contains(have, n) {
			return false
		}
	}
	return true
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

