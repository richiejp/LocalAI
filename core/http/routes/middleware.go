package routes

import (
	"context"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/application"
	"github.com/mudler/LocalAI/core/http/auth"
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
		routerSection := map[string]any{
			"configured": false,
			"models":     []any{},
			"note":       "Intelligent routing is not yet implemented.",
		}

		return c.JSON(http.StatusOK, map[string]any{
			"pii":    piiSection,
			"router": routerSection,
		})
	})

	e.GET("/api/router/status", func(c echo.Context) error {
		// Anonymous read is fine for the placeholder — no sensitive
		// data leaks. Will tighten to admin-only when subsystem 2
		// surfaces decision logs.
		return c.JSON(http.StatusOK, map[string]any{
			"configured": false,
			"models":     []any{},
			"note":       "Intelligent routing is not yet implemented.",
		})
	})
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
