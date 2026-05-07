package routes

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/application"
	"github.com/mudler/LocalAI/core/http/auth"
	"github.com/mudler/LocalAI/core/services/routing/router"
)

// RegisterMiddlewareRoutes wires the routing-module admin surface that
// powers the /app/middleware React page. Two endpoints:
//
//   - GET /api/middleware/status — single round-trip aggregator. Lists
//     PII patterns with current actions, each model's resolved
//     enabled/override state, recent event count, and a router status
//     stub (until subsystem 2 lands).
//   - GET /api/router/status — placeholder that the page renders for
//     the Routing tab. Returns { configured: false, models: [] } today;
//     subsystem 2 fills it in.
//
// Both are admin-only when auth is on. In single-user (no-auth) mode
// the synthetic local user has Role: admin so the page works without
// extra config — same gating shape as the existing /api/usage/all.
func RegisterMiddlewareRoutes(e *echo.Echo, app *application.Application) {
	e.GET("/api/middleware/status", func(c echo.Context) error {
		viewer := resolveUsageUser(c, app)
		if viewer == nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
		}
		if viewer.Role != auth.RoleAdmin {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "admin access required"})
		}

		piiSection := buildPIIStatus(app)
		routerSection := buildRouterStatus(app)
		mitmSection := buildMITMStatus(app)

		return c.JSON(http.StatusOK, map[string]any{
			"pii":    piiSection,
			"router": routerSection,
			"mitm":   mitmSection,
		})
	})

	e.GET("/api/router/status", func(c echo.Context) error {
		// Read-only — admins want to see classifier configurations
		// without authenticating, same as /api/pii/patterns.
		return c.JSON(http.StatusOK, buildRouterStatus(app))
	})

	e.GET("/api/middleware/proxy-ca.crt", func(c echo.Context) error {
		// The CA cert is the public half — safe to expose without
		// auth so clients can curl it during initial setup. The
		// private key never leaves disk and is mode 0600. Returning
		// 404 (rather than 500) when MITM is disabled keeps the
		// endpoint a clean "is this feature available?" probe.
		ca := app.MITMCA()
		if ca == nil {
			return c.JSON(http.StatusNotFound, map[string]string{
				"error": "mitm proxy is not enabled (set --mitm-listen to start it)",
			})
		}
		c.Response().Header().Set("Content-Type", "application/x-pem-file")
		c.Response().Header().Set("Content-Disposition", `attachment; filename="localai-mitm-ca.crt"`)
		return c.Blob(http.StatusOK, "application/x-pem-file", ca.PublicCertPEM())
	})

	e.GET("/api/router/decisions", func(c echo.Context) error {
		viewer := resolveUsageUser(c, app)
		if viewer == nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
		}
		// Decision logs may include user ids — admin-only when auth is
		// on; the synthetic local user has admin so single-user mode
		// works.
		if viewer.Role != auth.RoleAdmin {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "admin access required"})
		}

		store := app.RouterDecisions()
		if store == nil {
			return c.JSON(http.StatusOK, map[string]any{"decisions": []any{}})
		}

		limit := 100
		if v := c.QueryParam("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		decisions, err := store.List(c.Request().Context(), router.DecisionListQuery{
			CorrelationID: c.QueryParam("correlation_id"),
			UserID:        c.QueryParam("user_id"),
			RouterModel:   c.QueryParam("router_model"),
			Limit:         limit,
		})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to list decisions"})
		}
		return c.JSON(http.StatusOK, map[string]any{"decisions": decisions})
	})
}

// buildRouterStatus inventories every model that declares a Router
// block and reports their classifiers + candidate tables. Reads from
// the same loader the RouteModel middleware uses so the admin page
// agrees with what's actually live in the request path.
func buildRouterStatus(app *application.Application) map[string]any {
	models := []map[string]any{}
	hasAny := false
	for _, cfg := range app.ModelConfigLoader().GetAllModelsConfigs() {
		if !cfg.HasRouter() {
			continue
		}
		hasAny = true
		candidates := make([]map[string]any, 0, len(cfg.Router.Candidates))
		for _, ca := range cfg.Router.Candidates {
			candidates = append(candidates, map[string]any{
				"label": ca.Label,
				"model": ca.Model,
				"rules": map[string]any{
					"max_prompt_length": ca.Rules.MaxPromptLength,
					"min_prompt_length": ca.Rules.MinPromptLength,
					"requires_code":     ca.Rules.RequiresCode,
				},
			})
		}
		classifier := cfg.Router.Classifier
		if classifier == "" {
			classifier = "feature"
		}
		models = append(models, map[string]any{
			"name":       cfg.Name,
			"classifier": classifier,
			"candidates": candidates,
			"fallback":   cfg.Router.Fallback,
		})
	}

	recentCount := 0
	if store := app.RouterDecisions(); store != nil {
		if n, err := store.Count(context.Background()); err == nil {
			recentCount = n
		}
	}

	out := map[string]any{
		"configured":          hasAny,
		"models":              models,
		"recent_decision_count": recentCount,
		"available_classifiers": []string{"feature"},
	}
	if !hasAny {
		out["note"] = "No router models configured. Add a `router:` block to a model YAML to enable intelligent routing."
	}
	return out
}

func buildMITMStatus(app *application.Application) map[string]any {
	srv := app.MITMServer()
	ca := app.MITMCA()
	cfg := app.ApplicationConfig()

	out := map[string]any{
		"running":         srv != nil,
		"listen_addr":     "",
		"configured_addr": cfg.MITMListen,
		"intercept_hosts": cfg.MITMInterceptHosts,
		"ca_available":    ca != nil,
		"ca_cert_url":     "",
	}
	if srv != nil {
		out["listen_addr"] = srv.Addr()
	}
	if ca != nil {
		out["ca_cert_url"] = "/api/middleware/proxy-ca.crt"
	}
	return out
}

// buildPIIStatus builds the pii section of /api/middleware/status. It
// reads the live redactor, walks every model config, and reports the
// resolved enabled state plus any per-pattern overrides — that's what
// the admin page renders side-by-side so the operator can see at a
// glance which models are protected.
//
// Returns a sentinel "disabled" payload when the redactor is nil
// (--disable-pii), letting the page show "filter switched off" rather
// than a confusing empty state.
func buildPIIStatus(app *application.Application) map[string]any {
	redactor := app.PIIRedactor()
	if redactor == nil {
		return map[string]any{
			"enabled_globally": false,
			"reason":           "--disable-pii",
			"patterns":         []any{},
			"models":           []any{},
		}
	}

	patterns := redactor.Patterns()
	patternList := make([]map[string]any, 0, len(patterns))
	for _, p := range patterns {
		patternList = append(patternList, map[string]any{
			"id":               p.ID,
			"description":      p.Description,
			"action":           string(p.Action),
			"max_match_length": p.MaxMatchLength,
		})
	}

	models := []map[string]any{}
	for _, cfg := range app.ModelConfigLoader().GetAllModelsConfigs() {
		entry := map[string]any{
			"name":      cfg.Name,
			"backend":   cfg.Backend,
			"enabled":   cfg.PIIIsEnabled(),
			"overrides": cfg.PIIPatternOverrides(),
		}
		// explicit-set tells the UI whether the resolved state came
		// from the YAML or the backend-prefix default. Helps admins
		// understand "why is this on?" without reading source.
		entry["explicit"] = cfg.PII.Enabled != nil
		entry["default_for_backend"] = strings.HasPrefix(cfg.Backend, "proxy-")
		models = append(models, entry)
	}

	recentCount := 0
	if app.PIIEvents() != nil {
		if n, err := app.PIIEvents().Count(context.Background()); err == nil {
			recentCount = n
		}
	}

	return map[string]any{
		"enabled_globally":             true,
		"default_enabled_for_backends": []string{"proxy-*"},
		"patterns":                     patternList,
		"models":                       models,
		"recent_event_count":           recentCount,
	}
}
