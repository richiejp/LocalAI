package pii

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/http/auth"
)

// fakeRequest is the simplest possible parsed-request shape: a list of
// strings that the adapter scans and writes back. Lets us drive the
// middleware without dragging the real schema package in.
type fakeRequest struct {
	Messages []string
}

func fakeAdapter() Adapter {
	return Adapter{
		Scan: func(parsed any) []ScannedText {
			r, ok := parsed.(*fakeRequest)
			if !ok {
				return nil
			}
			out := make([]ScannedText, len(r.Messages))
			for i, m := range r.Messages {
				out[i] = ScannedText{Index: i, Text: m}
			}
			return out
		},
		Apply: func(parsed any, updates []ScannedText) {
			r, ok := parsed.(*fakeRequest)
			if !ok {
				return
			}
			for _, u := range updates {
				r.Messages[u.Index] = u.Text
			}
		},
	}
}

func setRequestOnContext(req *fakeRequest) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(ctxKeyParsedRequest, req)
			return next(c)
		}
	}
}

func newTestRedactor(t *testing.T, ids ...string) *Redactor {
	t.Helper()
	patterns, err := Compile(pick(DefaultPatterns(), ids))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return NewRedactor(patterns)
}

func TestRequestMiddlewareMasksEmail(t *testing.T) {
	red := newTestRedactor(t, "email")
	store := NewMemoryEventStore(0)
	defer store.Close()
	user := &auth.User{ID: "user-1", Name: "alice"}

	body := &fakeRequest{Messages: []string{"contact me at alice@example.com"}}
	mw := RequestMiddleware(red, store, fakeAdapter(), nil)

	e := echo.New()
	e.POST("/chat", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"ok": "yes"})
	}, setRequestOnContext(body), mw, func(next echo.HandlerFunc) echo.HandlerFunc {
		// Inject the user as if upstream auth ran.
		return func(c echo.Context) error {
			c.Set("auth_user", user)
			return next(c)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(body.Messages[0], "alice@example.com") {
		t.Errorf("request body should be redacted in place, got %q", body.Messages[0])
	}
	if !strings.Contains(body.Messages[0], "[REDACTED:email]") {
		t.Errorf("expected mask placeholder, got %q", body.Messages[0])
	}

	events, err := store.List(context.Background(), ListQuery{Limit: 100})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 event recorded, got %d", len(events))
	}
	if events[0].PatternID != "email" || events[0].Direction != DirectionIn {
		t.Errorf("event mismatch: %+v", events[0])
	}
}

func TestRequestMiddlewareBlocksApiKey(t *testing.T) {
	red := newTestRedactor(t, "api_key_prefix")
	store := NewMemoryEventStore(0)
	defer store.Close()

	body := &fakeRequest{Messages: []string{"my key is sk-abcdefghijklmnopqrstuvwxyz0123456789"}}
	mw := RequestMiddleware(red, store, fakeAdapter(), nil)

	e := echo.New()
	handlerCalled := false
	e.POST("/chat", func(c echo.Context) error {
		handlerCalled = true
		return c.JSON(http.StatusOK, map[string]string{"ok": "yes"})
	}, setRequestOnContext(body), mw)

	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on block, got %d; body=%s", w.Code, w.Body.String())
	}
	if handlerCalled {
		t.Errorf("handler must not run when request is blocked")
	}
	// Ensure the matched value never appears in the response body.
	if strings.Contains(w.Body.String(), "abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Errorf("blocked response leaks the matched value: %s", w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errBlock, ok := resp["error"].(map[string]any)
	if !ok || errBlock["type"] != "pii_blocked" {
		t.Errorf("expected pii_blocked error type, got %v", resp)
	}
}

func TestRequestMiddlewareRouteLocalSetsContextFlag(t *testing.T) {
	patterns, _ := Compile([]Pattern{{
		ID: "email", Description: "Email", Action: ActionRouteLocal, MaxMatchLength: 254,
	}})
	red := NewRedactor(patterns)
	store := NewMemoryEventStore(0)
	defer store.Close()

	body := &fakeRequest{Messages: []string{"hi at alice@example.com"}}
	mw := RequestMiddleware(red, store, fakeAdapter(), nil)

	e := echo.New()
	var observedLocalOnly bool
	e.POST("/chat", func(c echo.Context) error {
		v, _ := c.Get(ctxKeyLocalOnly).(bool)
		observedLocalOnly = v
		return c.JSON(http.StatusOK, map[string]string{"ok": "yes"})
	}, setRequestOnContext(body), mw)

	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if !observedLocalOnly {
		t.Errorf("ctxKeyLocalOnly should be true on route_local match")
	}
	// route_local does NOT mutate the body — the model still sees the email.
	if !strings.Contains(body.Messages[0], "alice@example.com") {
		t.Errorf("route_local should leave text intact, got %q", body.Messages[0])
	}
}

func TestRequestMiddlewareNoMatchPassesThrough(t *testing.T) {
	red := newTestRedactor(t)
	store := NewMemoryEventStore(0)
	defer store.Close()

	body := &fakeRequest{Messages: []string{"perfectly innocent text"}}
	mw := RequestMiddleware(red, store, fakeAdapter(), nil)

	e := echo.New()
	e.POST("/chat", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"ok": "yes"})
	}, setRequestOnContext(body), mw)

	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if body.Messages[0] != "perfectly innocent text" {
		t.Errorf("body should be untouched, got %q", body.Messages[0])
	}
	events, _ := store.List(context.Background(), ListQuery{Limit: 100})
	if len(events) != 0 {
		t.Errorf("expected 0 events on no-match input, got %d", len(events))
	}
}

func TestRequestMiddlewareNilRedactorIsPassthrough(t *testing.T) {
	body := &fakeRequest{Messages: []string{"alice@example.com"}}
	mw := RequestMiddleware(nil, nil, fakeAdapter(), nil)

	e := echo.New()
	e.POST("/chat", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"ok": "yes"})
	}, setRequestOnContext(body), mw)

	req := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	if body.Messages[0] != "alice@example.com" {
		t.Errorf("nil redactor must be a no-op, got %q", body.Messages[0])
	}
}
