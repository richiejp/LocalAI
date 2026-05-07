package routes

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/application"
	"github.com/mudler/LocalAI/core/http/auth"
	"github.com/mudler/LocalAI/core/services/routing/pii"
)

// RegisterPIIRoutes wires the read-only routing-PII endpoints. They
// surface (a) the active pattern set so admins can verify what is
// being filtered, (b) the recent PIIEvent log so they can audit what
// has been redacted, and (c) a dry-run "test" endpoint so an admin
// can paste candidate text and see what the redactor would do without
// sending a real request.
//
// The redactor itself runs from the chat middleware in routes/openai.go;
// these endpoints are observation- and configuration-side only.
func RegisterPIIRoutes(e *echo.Echo, app *application.Application) {
	if app.PIIRedactor() == nil {
		stub := func(c echo.Context) error {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{
				"error": "PII filter is disabled (--disable-pii)",
			})
		}
		e.GET("/api/pii/patterns", stub)
		e.GET("/api/pii/events", stub)
		e.POST("/api/pii/test", stub)
		return
	}

	// GetPIIPatternsEndpoint godoc
	// @Summary List the active PII patterns
	// @Description Returns the configured pattern set with their actions. Available without auth.
	// @Tags pii
	// @Produce json
	// @Success 200 {object} map[string]interface{}
	// @Router /api/pii/patterns [get]
	e.GET("/api/pii/patterns", func(c echo.Context) error {
		patterns := app.PIIRedactor().Patterns()
		out := make([]map[string]any, 0, len(patterns))
		for _, p := range patterns {
			out = append(out, map[string]any{
				"id":               p.ID,
				"description":      p.Description,
				"action":           string(p.Action),
				"max_match_length": p.MaxMatchLength,
			})
		}
		return c.JSON(http.StatusOK, map[string]any{"patterns": out})
	})

	// GetPIIEventsEndpoint godoc
	// @Summary List recent middleware events
	// @Description The event log is shared between the PII filter and the MITM proxy: PII redactions, proxy_connect (intercept decisions), and proxy_traffic (per-request byte counts) all flow through the same store. Filter by kind to narrow the view. Admin-only when auth is on; available to the local user in single-user mode.
	// @Tags pii
	// @Produce json
	// @Param correlation_id query string false "Correlation ID join key"
	// @Param user_id query string false "User id"
	// @Param pattern_id query string false "Pattern id (e.g. email, ssn)"
	// @Param kind query string false "Event kind: pii | proxy_connect | proxy_traffic"
	// @Param limit query int false "Max events" default(100)
	// @Success 200 {object} map[string]interface{}
	// @Router /api/pii/events [get]
	e.GET("/api/pii/events", func(c echo.Context) error {
		viewer := resolveUsageUser(c, app)
		if viewer == nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
		}
		// Admin-only when auth is enabled. Local user has Role: admin.
		if viewer.Role != auth.RoleAdmin {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "admin access required"})
		}

		limit := 100
		if v := c.QueryParam("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		events, err := app.PIIEvents().List(c.Request().Context(), pii.ListQuery{
			CorrelationID: c.QueryParam("correlation_id"),
			UserID:        c.QueryParam("user_id"),
			PatternID:     c.QueryParam("pattern_id"),
			Kind:          pii.EventKind(c.QueryParam("kind")),
			Limit:         limit,
		})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to list events"})
		}
		return c.JSON(http.StatusOK, map[string]any{"events": events})
	})

	// PostPIITestEndpoint godoc
	// @Summary Dry-run the PII redactor against text
	// @Description Useful for admins tuning patterns. Returns the redacted text, matched spans, and whether the input would have been blocked.
	// @Tags pii
	// @Accept json
	// @Produce json
	// @Param body body map[string]string true "JSON {\"text\":\"...\"}"
	// @Success 200 {object} map[string]interface{}
	// @Router /api/pii/test [post]
	e.POST("/api/pii/test", func(c echo.Context) error {
		var body struct {
			Text string `json:"text"`
		}
		if err := c.Bind(&body); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		}
		res := app.PIIRedactor().Redact(body.Text)
		return c.JSON(http.StatusOK, map[string]any{
			"redacted":   res.Redacted,
			"spans":      res.Spans,
			"blocked":    res.Blocked,
			"local_only": res.LocalOnly,
		})
	})

	// PutPIIPatternActionEndpoint godoc
	// @Summary Change a pattern's action in-process
	// @Description Mutates the named pattern's action (mask|block|route_local). Transient — restored to YAML defaults on restart. Admin-only.
	// @Tags pii
	// @Accept json
	// @Produce json
	// @Param id path string true "Pattern id"
	// @Param body body map[string]string true "JSON {\"action\":\"mask|block|route_local\"}"
	// @Success 200 {object} map[string]interface{}
	// @Router /api/pii/patterns/{id} [put]
	e.PUT("/api/pii/patterns/:id", func(c echo.Context) error {
		viewer := resolveUsageUser(c, app)
		if viewer == nil {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
		}
		if viewer.Role != auth.RoleAdmin {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "admin access required"})
		}

		id := c.Param("id")
		if id == "" {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "pattern id is required"})
		}
		var body struct {
			Action string `json:"action"`
		}
		if err := c.Bind(&body); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		}
		if err := app.PIIRedactor().SetAction(id, pii.Action(body.Action)); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusOK, map[string]any{
			"id":     id,
			"action": body.Action,
			// Transient by design — admins relying on persistence should
			// edit --pii-config YAML and restart instead.
			"persisted": false,
		})
	})
}
