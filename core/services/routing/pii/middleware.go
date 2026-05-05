package pii

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/http/auth"
	"github.com/mudler/LocalAI/core/services/routing/contract"
	"github.com/mudler/xlog"
)

// Echo context keys this middleware reads from / writes to. The string
// values must match the constants in core/http/middleware/context_keys.go;
// kept in sync by hand because echoing constants across packages would
// drag the http/middleware package into pii's import graph and create
// a cycle (http/middleware will import this one).
const (
	ctxKeyCorrelationID    = "routing.correlation_id"
	ctxKeyPIIEventID       = "routing.pii_event_id"
	ctxKeyLocalOnly        = "routing.local_only"
	ctxKeyParsedRequest    = "LOCALAI_REQUEST"
)

// ScannedText is one piece of user text from the request. Index is
// opaque to the middleware — the Adapter implementation uses it to
// put the redacted version back in the right place.
type ScannedText struct {
	Index int
	Text  string
}

// Adapter pulls scannable text out of a parsed request and writes
// redacted text back. Provided as a per-API-shape function rather
// than an interface on the request type so the schema package does
// not have to depend on pii. Each route registration passes the
// adapter that knows its request format.
//
// The middleware calls Scan once per request and Apply once with
// every span the redactor returned. updates are guaranteed to share
// indices the adapter previously returned from Scan; the adapter
// must not assume input order matches scan order.
type Adapter struct {
	Scan  func(parsed any) []ScannedText
	Apply func(parsed any, updates []ScannedText)
}

// RequestMiddleware applies the regex PII tier to incoming chat
// requests. If the parsed request is not a MessageScanner (e.g.,
// non-chat endpoints registered against the same group later), the
// middleware passes through.
//
//   - On match with action=block: the request is rejected with 400 and
//     a PIIEvent is recorded. The matched value is never echoed back
//     to the client.
//   - On match with action=mask: the redacted text replaces the
//     original on the parsed request. PIIEvents are recorded.
//   - On match with action=route_local: the original text is left
//     intact, but the echo context is annotated so the (future) router
//     middleware refuses cloud-proxy candidates.
//
// recorder is the Recorder on which to record events; nil disables
// recording (the redaction still happens). fallbackUser supplies the
// no-auth identity. The middleware writes ctxKeyPIIEventID on the echo
// context so the usage middleware can later cross-reference the event
// with the UsageRecord.
func RequestMiddleware(redactor *Redactor, store EventStore, adapter Adapter, fallbackUser *auth.User) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if redactor == nil || len(redactor.Patterns()) == 0 || adapter.Scan == nil {
				return next(c)
			}

			parsed := c.Get(ctxKeyParsedRequest)
			if parsed == nil {
				return next(c)
			}

			user := auth.GetUser(c)
			if user == nil {
				user = fallbackUser
			}
			userID := ""
			if user != nil {
				userID = user.ID
			}
			correlationID, _ := c.Get(ctxKeyCorrelationID).(string)

			texts := adapter.Scan(parsed)
			updates := make([]ScannedText, 0, len(texts))
			var blocked bool
			var localOnly bool
			var firstEventID string

			for _, st := range texts {
				if st.Text == "" {
					continue
				}
				res := redactor.Redact(st.Text)
				if len(res.Spans) == 0 {
					continue
				}

				// Persist one event per span so admins can see exactly
				// which patterns fired in which positions.
				for _, span := range res.Spans {
					action := actionForPattern(redactor.Patterns(), span.Pattern)
					ev := PIIEvent{
						ID:            newEventID(),
						CorrelationID: correlationID,
						UserID:        userID,
						Direction:     DirectionIn,
						PatternID:     span.Pattern,
						ByteOffset:    span.Start,
						Length:        span.End - span.Start,
						HashPrefix:    span.HashPrefix,
						Action:        action,
						CreatedAt:     time.Now().UTC(),
					}
					if firstEventID == "" {
						firstEventID = ev.ID
					}
					if store != nil {
						if err := store.Record(context.Background(), ev); err != nil {
							xlog.Error("pii: failed to record event", "error", err, "pattern", span.Pattern)
						}
					}
					// Contract: every span must produce an event.
					contract.Invariant(
						"pii.event_per_span",
						span.Pattern != "" && ev.PatternID != "",
						"correlation", correlationID, "pattern", span.Pattern,
					)
				}

				if res.Blocked {
					blocked = true
				}
				if res.LocalOnly {
					localOnly = true
				}
				updates = append(updates, ScannedText{Index: st.Index, Text: res.Redacted})
			}

			if blocked {
				return c.JSON(http.StatusBadRequest, map[string]any{
					"error": map[string]string{
						"message": "request blocked by content policy (sensitive data detected)",
						"type":    "pii_blocked",
					},
					"correlation_id": correlationID,
					"pii_event_id":   firstEventID,
				})
			}

			if len(updates) > 0 && adapter.Apply != nil {
				adapter.Apply(parsed, updates)
			}
			if firstEventID != "" {
				c.Set(ctxKeyPIIEventID, firstEventID)
			}
			if localOnly {
				c.Set(ctxKeyLocalOnly, true)
			}

			return next(c)
		}
	}
}

func actionForPattern(patterns []Pattern, id string) Action {
	for _, p := range patterns {
		if p.ID == id {
			return p.Action
		}
	}
	return ActionMask
}

func newEventID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "pii_" + hex.EncodeToString(b[:])
}
