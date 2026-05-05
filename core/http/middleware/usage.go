package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/http/auth"
	"github.com/mudler/LocalAI/core/services/routing/billing"
	"github.com/mudler/xlog"
)

// usageResponseBody is the minimal structure we need from the response JSON.
type usageResponseBody struct {
	Model string `json:"model"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// UsageMiddleware extracts token usage from OpenAI-compatible response
// JSON and records it via the billing.Recorder. Unlike the pre-routing
// version, this middleware does not short-circuit when auth is off: a
// no-auth single-user box still records under the synthetic fallback
// user so dashboards and `/api/usage` work out of the box.
//
// recorder being nil disables recording entirely (e.g., --disable-stats)
// — the middleware then becomes a transparent pass-through.
//
// fallbackUser is used when auth.GetUser(c) returns nil. It must have a
// non-empty ID; the billing invariant assertion catches accidental empty
// IDs that would otherwise cluster all usage under a blank user.
func UsageMiddleware(recorder *billing.Recorder, fallbackUser *auth.User) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if recorder == nil {
				return next(c)
			}

			startTime := time.Now()

			// Wrap response writer to capture body so we can parse the
			// OpenAI/Anthropic usage block at the end of the response.
			resBody := new(bytes.Buffer)
			origWriter := c.Response().Writer
			mw := &bodyWriter{
				ResponseWriter: origWriter,
				body:           resBody,
			}
			c.Response().Writer = mw

			handlerErr := next(c)

			c.Response().Writer = origWriter

			if c.Response().Status < 200 || c.Response().Status >= 300 {
				return handlerErr
			}

			user := auth.GetUser(c)
			if user == nil {
				user = fallbackUser
			}
			if user == nil || user.ID == "" {
				// Both real auth and fallback are absent — nothing to attribute.
				return handlerErr
			}

			responseBytes := resBody.Bytes()
			if len(responseBytes) == 0 {
				return handlerErr
			}

			ct := c.Response().Header().Get("Content-Type")
			isJSON := ct == "" || ct == "application/json" || bytes.HasPrefix([]byte(ct), []byte("application/json"))
			isSSE := bytes.HasPrefix([]byte(ct), []byte("text/event-stream"))
			if !isJSON && !isSSE {
				return handlerErr
			}

			var resp usageResponseBody
			if isSSE {
				last, ok := lastSSEData(responseBytes)
				if !ok {
					return handlerErr
				}
				if err := json.Unmarshal(last, &resp); err != nil {
					return handlerErr
				}
			} else {
				if err := json.Unmarshal(responseBytes, &resp); err != nil {
					return handlerErr
				}
			}

			if resp.Usage == nil {
				return handlerErr
			}

			// Pull the routing-extension fields off the echo context if
			// upstream middleware (router, PII filter) populated them.
			// Each helper falls back to the legacy field when not set, so
			// records produced before those middlewares land still
			// validate cleanly.
			requested, served := modelsFromContext(c, resp.Model)
			pre, post := promptTokensFromContext(c, resp.Usage.PromptTokens)

			record := &auth.UsageRecord{
				UserID:                 user.ID,
				UserName:               user.Name,
				Model:                  resp.Model,
				Endpoint:               c.Request().URL.Path,
				PromptTokens:           resp.Usage.PromptTokens,
				CompletionTokens:       resp.Usage.CompletionTokens,
				TotalTokens:            resp.Usage.TotalTokens,
				Duration:               time.Since(startTime).Milliseconds(),
				CreatedAt:              startTime,
				RequestedModel:         requested,
				ServedModel:            served,
				PreFilterPromptTokens:  pre,
				PostFilterPromptTokens: post,
				CorrelationID:          correlationIDFromContext(c),
			}

			if err := recorder.Record(context.Background(), record); err != nil {
				xlog.Error("usage middleware: recorder.Record failed", "error", err, "user", user.ID, "model", resp.Model)
			}

			return handlerErr
		}
	}
}

// modelsFromContext returns (requested, served) using context-set values
// when present, falling back to the response-reported model for both.
// The router middleware (subsystem 2 of the routing plan) populates
// these; until it lands they are equal.
func modelsFromContext(c echo.Context, fallback string) (string, string) {
	requested := fallback
	served := fallback
	if v, ok := c.Get(ContextKeyRequestedModel).(string); ok && v != "" {
		requested = v
	}
	if v, ok := c.Get(ContextKeyServedModel).(string); ok && v != "" {
		served = v
	}
	return requested, served
}

func promptTokensFromContext(c echo.Context, fallback int64) (int64, int64) {
	pre := fallback
	post := fallback
	if v, ok := c.Get(ContextKeyPreFilterPromptTokens).(int64); ok && v > 0 {
		pre = v
	}
	if v, ok := c.Get(ContextKeyPostFilterPromptTokens).(int64); ok && v > 0 {
		post = v
	}
	return pre, post
}

func correlationIDFromContext(c echo.Context) string {
	if v, ok := c.Get(ContextKeyCorrelationID).(string); ok {
		return v
	}
	// X-Correlation-ID header is set by trace.go middleware; read it as a
	// fallback if the echo-context binding hasn't been populated yet.
	return c.Response().Header().Get("X-Correlation-ID")
}

// lastSSEData returns the payload of the last "data: " line whose content is not "[DONE]".
func lastSSEData(b []byte) ([]byte, bool) {
	prefix := []byte("data: ")
	var last []byte
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, prefix) {
			payload := line[len(prefix):]
			if !bytes.Equal(payload, []byte("[DONE]")) {
				last = payload
			}
		}
	}
	return last, last != nil
}
