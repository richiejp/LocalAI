package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/config"
	"github.com/mudler/LocalAI/core/http/auth"
	"github.com/mudler/LocalAI/core/schema"
	"github.com/mudler/LocalAI/core/services/routing/router"
	"github.com/mudler/xlog"
)

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
func RouteModel(loader *config.ModelConfigLoader, appConfig *config.ApplicationConfig, store router.DecisionStore, fallbackUser *auth.User, extractor ProbeExtractor) echo.MiddlewareFunc {
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

			classifier, classifierErr := buildClassifier(cfg.Router)
			if classifierErr != nil {
				xlog.Warn("router: unsupported classifier — falling back",
					"router_model", cfg.Name, "classifier", cfg.Router.Classifier, "error", classifierErr)
				if cfg.Router.Fallback == "" {
					return echo.NewHTTPError(503, "router classifier unavailable and no fallback configured")
				}
				return rewriteRequest(c, parsed, cfg, cfg.Router.Fallback, "fallback", router.Decision{Label: "fallback"}, "fallback", store, fallbackUser, loader, appConfig, next)
			}

			start := time.Now()
			decision, err := classifier.Classify(c.Request().Context(), probe)
			if err != nil {
				xlog.Warn("router: classifier returned error — using fallback",
					"router_model", cfg.Name, "error", err, "latency_ms", time.Since(start).Milliseconds())
				if cfg.Router.Fallback == "" {
					return echo.NewHTTPError(503, "router classification failed: "+err.Error())
				}
				return rewriteRequest(c, parsed, cfg, cfg.Router.Fallback, "fallback", router.Decision{Label: "fallback", Latency: time.Since(start)}, classifier.Name(), store, fallbackUser, loader, appConfig, next)
			}

			candidate := matchCandidate(cfg.Router.Candidates, decision.Label)
			if candidate == "" {
				xlog.Warn("router: classifier label not in candidates — using fallback",
					"router_model", cfg.Name, "label", decision.Label)
				if cfg.Router.Fallback == "" {
					return echo.NewHTTPError(500, "classifier produced unknown label: "+decision.Label)
				}
				candidate = cfg.Router.Fallback
			}

			return rewriteRequest(c, parsed, cfg, candidate, decision.Label, decision, classifier.Name(), store, fallbackUser, loader, appConfig, next)
		}
	}
}

// rewriteRequest swaps the resolved model from the router to the
// chosen candidate, asserts the depth-1 invariant on the new config,
// records the decision, and continues. Pulled out so the classifier-
// success and fallback paths share one rewrite implementation.
func rewriteRequest(c echo.Context, parsed any, routerCfg *config.ModelConfig, candidateModel, label string, decision router.Decision, classifierName string, store router.DecisionStore, fallbackUser *auth.User, loader *config.ModelConfigLoader, appConfig *config.ApplicationConfig, next echo.HandlerFunc) error {
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
			ID:             newDecisionID(),
			CorrelationID:  correlationID,
			UserID:         userID,
			RouterModel:    routerCfg.Name,
			RequestedModel: routerCfg.Name,
			ServedModel:    candidateModel,
			Classifier:     classifierName,
			Label:          label,
			Score:          decision.Score,
			LatencyMs:      decision.Latency.Milliseconds(),
			CreatedAt:      time.Now().UTC(),
		})
	}

	return next(c)
}

func buildClassifier(rc config.RouterConfig) (router.Classifier, error) {
	switch rc.Classifier {
	case "", "feature":
		// Empty defaults to "feature" — the only shipped classifier in
		// this slice. KNN/LLM land in follow-ups behind the same
		// interface; the YAML field already accepts those values.
		cands := make([]router.FeatureCandidate, 0, len(rc.Candidates))
		for _, c := range rc.Candidates {
			cands = append(cands, router.FeatureCandidate{
				Label: c.Label,
				Rule: router.CandidateRule{
					MaxPromptLength: c.Rules.MaxPromptLength,
					MinPromptLength: c.Rules.MinPromptLength,
					RequiresCode:    c.Rules.RequiresCode,
				},
			})
		}
		if len(cands) == 0 {
			return nil, errClassifierUnavailable
		}
		return router.NewFeatureClassifier(cands), nil
	default:
		return nil, errClassifierUnavailable
	}
}

func matchCandidate(candidates []config.RouterCandidate, label string) string {
	for _, c := range candidates {
		if c.Label == label {
			return c.Model
		}
	}
	return ""
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
	prompt := b.String()
	return router.Probe{
		Prompt:  prompt,
		HasCode: strings.Contains(prompt, "```"),
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
	prompt := b.String()
	return router.Probe{
		Prompt:  prompt,
		HasCode: strings.Contains(prompt, "```"),
	}, true
}

// errClassifierUnavailable is returned when the classifier type isn't
// yet implemented or candidates are empty. Surfaced as a typed
// sentinel so the middleware can choose between fallback and HTTP 503.
var errClassifierUnavailable = classifierUnavailableError{}

type classifierUnavailableError struct{}

func (classifierUnavailableError) Error() string {
	return "classifier unavailable (only 'feature' is supported in this build)"
}
