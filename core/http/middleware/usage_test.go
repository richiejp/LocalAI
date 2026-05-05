package middleware_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/mudler/LocalAI/core/http/auth"
	httpMiddleware "github.com/mudler/LocalAI/core/http/middleware"
	"github.com/mudler/LocalAI/core/services/routing/billing"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// captureBackend collects records the recorder forwards. We assert on
// it directly rather than going through StatsBackend.Aggregate because
// these tests verify the middleware -> recorder hop, not aggregation
// (which has its own tests in routing/billing).
type captureBackend struct {
	records []*auth.UsageRecord
}

func (c *captureBackend) Record(_ context.Context, r *auth.UsageRecord) error {
	c.records = append(c.records, r)
	return nil
}
func (c *captureBackend) Aggregate(_ context.Context, _ billing.AggregateQuery) ([]auth.UsageBucket, error) {
	return nil, nil
}
func (c *captureBackend) Close() error { return nil }

var _ = Describe("UsageMiddleware", func() {
	mockChat := func(usage string) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Set("Content-Type", "application/json")
			body := fmt.Sprintf(`{"model":"qwen-7b","usage":%s}`, usage)
			return c.String(http.StatusOK, body)
		}
	}

	It("records under the synthetic local user when auth is off", func() {
		cap := &captureBackend{}
		rec := billing.NewRecorder(cap)
		fallback := &auth.User{ID: "local-uuid", Name: "local", Provider: auth.ProviderLocal}

		e := echo.New()
		e.POST("/v1/chat/completions",
			mockChat(`{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}`),
			httpMiddleware.UsageMiddleware(rec, fallback),
		)

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)

		Expect(w.Code).To(Equal(http.StatusOK))
		Expect(cap.records).To(HaveLen(1))
		r := cap.records[0]
		Expect(r.UserID).To(Equal("local-uuid"))
		Expect(r.UserName).To(Equal("local"))
		Expect(r.Model).To(Equal("qwen-7b"))
		Expect(r.PromptTokens).To(Equal(int64(12)))
		Expect(r.CompletionTokens).To(Equal(int64(8)))
		Expect(r.TotalTokens).To(Equal(int64(20)))
	})

	It("does nothing when recorder is nil (--disable-stats)", func() {
		fallback := &auth.User{ID: "local-uuid", Name: "local"}
		e := echo.New()
		e.POST("/v1/chat/completions",
			mockChat(`{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}`),
			httpMiddleware.UsageMiddleware(nil, fallback),
		)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		Expect(w.Code).To(Equal(http.StatusOK))
		// no panic, no record — recorder=nil is the disable-stats path
	})

	It("skips when neither auth nor fallback user is available", func() {
		cap := &captureBackend{}
		rec := billing.NewRecorder(cap)

		e := echo.New()
		e.POST("/v1/chat/completions",
			mockChat(`{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}`),
			httpMiddleware.UsageMiddleware(rec, nil),
		)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		Expect(w.Code).To(Equal(http.StatusOK))
		Expect(cap.records).To(BeEmpty())
	})

	It("ignores 5xx responses (no usage to attribute)", func() {
		cap := &captureBackend{}
		rec := billing.NewRecorder(cap)
		fallback := &auth.User{ID: "local-uuid", Name: "local"}

		e := echo.New()
		e.POST("/v1/chat/completions",
			func(c echo.Context) error {
				return c.String(http.StatusInternalServerError, `{"error":"boom"}`)
			},
			httpMiddleware.UsageMiddleware(rec, fallback),
		)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)
		Expect(w.Code).To(Equal(http.StatusInternalServerError))
		Expect(cap.records).To(BeEmpty())
	})

	It("populates RequestedModel/ServedModel from echo context when set", func() {
		cap := &captureBackend{}
		rec := billing.NewRecorder(cap)
		fallback := &auth.User{ID: "local-uuid", Name: "local"}

		// A pre-handler stand-in for the future router middleware: it
		// rewrites Served and remembers the original Requested. Once the
		// real router lands, this is exactly the contract it must keep.
		setRouterContext := func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				c.Set(httpMiddleware.ContextKeyRequestedModel, "auto")
				c.Set(httpMiddleware.ContextKeyServedModel, "qwen-7b")
				return next(c)
			}
		}

		e := echo.New()
		e.POST("/v1/chat/completions",
			mockChat(`{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}`),
			httpMiddleware.UsageMiddleware(rec, fallback),
			setRouterContext,
		)

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, req)

		Expect(w.Code).To(Equal(http.StatusOK))
		Expect(cap.records).To(HaveLen(1))
		Expect(cap.records[0].RequestedModel).To(Equal("auto"))
		Expect(cap.records[0].ServedModel).To(Equal("qwen-7b"))
	})
})
