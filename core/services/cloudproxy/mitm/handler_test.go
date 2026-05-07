package mitm

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mudler/LocalAI/core/services/routing/pii"
)

// startPIITestRig is the same shape as startMITMTestRig but plugs
// in the production PII handler instead of the passthrough fixture.
// The "host" the client thinks it's reaching is forced to
// api.anthropic.com so the request shape classifier matches.
func startPIITestRig(t *testing.T, upstream http.Handler) (*http.Client, string, *fakeStore, func()) {
	t.Helper()

	// Upstream fake — plays the role of api.anthropic.com.
	ts := httptest.NewTLSServer(upstream)
	upstreamCertPool := x509.NewCertPool()
	upstreamCertPool.AddCert(ts.Certificate())
	upstreamURL, _ := url.Parse(ts.URL)

	// Compiled patterns required for the redactor to actually fire
	// (DefaultPatterns alone returns Pattern structs without regex).
	patterns, err := pii.Compile(pii.DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	redactor := pii.NewRedactor(patterns)
	store := &fakeStore{}

	ca, err := NewInMemoryCA()
	if err != nil {
		t.Fatal(err)
	}

	// DialHost remaps the upstream dial target to the httptest
	// fake while leaving the classifier-facing host
	// ("api.anthropic.com") untouched. ServerName=example.com is
	// what httptest.NewTLSServer issues its cert for.
	upstreamHost := upstreamURL.Host
	prodHandler := NewPIIHandler(PIIHandlerOptions{
		Redactor:   redactor,
		EventStore: store,
		UpstreamTLS: &tls.Config{
			RootCAs:    upstreamCertPool,
			ServerName: "example.com",
		},
		DialHost: func(_ string) string { return upstreamHost },
	})

	srv, err := NewServer(Config{
		Addr:           "127.0.0.1:0",
		CA:             ca,
		InterceptHosts: []string{"api.anthropic.com"},
		Handler:        prodHandler,
		EventStore:     store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}

	clientPool := x509.NewCertPool()
	clientPool.AddCert(ca.Cert())
	proxyURL, _ := url.Parse("http://" + srv.Addr())
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: clientPool},
		},
	}

	cleanup := func() {
		srv.Stop()
		ts.Close()
	}
	// We point requests at api.anthropic.com so classifyRequestShape
	// matches; the wrappedHandler retargets to the upstream fake.
	return client, "https://api.anthropic.com", store, cleanup
}

type fakeStore struct{ events []pii.PIIEvent }

func (s *fakeStore) Record(_ context.Context, ev pii.PIIEvent) error {
	s.events = append(s.events, ev)
	return nil
}

func (s *fakeStore) List(_ context.Context, _ pii.ListQuery) ([]pii.PIIEvent, error) {
	return s.events, nil
}

func (s *fakeStore) Count(_ context.Context) (int, error) { return len(s.events), nil }
func (s *fakeStore) Close() error                          { return nil }

func (s *fakeStore) recorded() int { return len(s.events) }

func TestPIIHandler_RedactsRequestEmail(t *testing.T) {
	var receivedBody []byte
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_x","content":[{"type":"text","text":"ok"}]}`)
	})

	client, base, store, cleanup := startPIITestRig(t, upstream)
	defer cleanup()

	body := `{"model":"claude-3-5-sonnet","max_tokens":100,"messages":[{"role":"user","content":"my email is alice@example.com please reply"}]}`
	resp, err := client.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("client.Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	if strings.Contains(string(receivedBody), "alice@example.com") {
		t.Errorf("upstream received unredacted body: %s", receivedBody)
	}
	if !strings.Contains(string(receivedBody), "[REDACTED:email]") {
		t.Errorf("upstream did not see redaction marker: %s", receivedBody)
	}
	if store.recorded() == 0 {
		t.Error("no PIIEvent recorded for the email match")
	}
}

func TestPIIHandler_BlocksApiKeyInRequest(t *testing.T) {
	upstreamCalled := false
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(200)
	})

	client, base, _, cleanup := startPIITestRig(t, upstream)
	defer cleanup()

	body := `{"model":"claude-3-5-sonnet","max_tokens":100,"messages":[{"role":"user","content":"my key is sk-abcdefghijklmnopqrstuvwxyz1234"}]}`
	resp, err := client.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("client.Post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (api_key_prefix has Block default)", resp.StatusCode)
	}
	if upstreamCalled {
		t.Error("upstream was called despite block — proxy should short-circuit")
	}
	body2, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body2), "pii_blocked") {
		t.Errorf("response missing pii_blocked marker: %s", body2)
	}
}

func TestPIIHandler_StreamingRedaction(t *testing.T) {
	// Anthropic-shape SSE; "alice@" + "example.com" splits the
	// email across chunks so the StreamFilter has to buffer.
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		chunks := []string{
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"contact me at alice@"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"example.com any time"}}`,
			`{"type":"message_stop"}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", "content_block_delta", c)
			flusher.Flush()
		}
	})

	client, base, _, cleanup := startPIITestRig(t, upstream)
	defer cleanup()

	body := `{"model":"claude-3-5-sonnet","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := client.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	outStr := string(out)
	if strings.Contains(outStr, "alice@example.com") {
		t.Errorf("email leaked through MITM stream: %s", outStr)
	}
	if !strings.Contains(outStr, "[REDACTED:email]") {
		t.Errorf("redaction marker missing from MITM stream: %s", outStr)
	}
}

func TestPIIHandler_NonChatPathPassesThrough(t *testing.T) {
	// A path the classifier doesn't recognise (e.g. an OAuth
	// callback) must forward the body verbatim, no PII parsing.
	var receivedBody []byte
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})

	client, base, _, cleanup := startPIITestRig(t, upstream)
	defer cleanup()

	body := `{"email":"alice@example.com"}`
	resp, err := client.Post(base+"/oauth/callback", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if string(receivedBody) != body {
		t.Errorf("body forwarded with mutation: got %q want %q", receivedBody, body)
	}
}

func TestRedactRequest_AnthropicShape(t *testing.T) {
	patterns, _ := pii.Compile(pii.DefaultPatterns())
	r := pii.NewRedactor(patterns)
	body := []byte(`{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"reach me at bob@example.org"}]}`)

	d := &piiDispatcher{redactor: r, patternAction: map[string]pii.Action{}}
	out, blocked, err := d.redactRequest(body, shapeAnthropicMessages, "corr-1")
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Error("email is mask, not block — blocked should be false")
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	msgs := parsed["messages"].([]any)
	first := msgs[0].(map[string]any)
	content, _ := first["content"].(string)
	if strings.Contains(content, "bob@example.org") {
		t.Errorf("redaction did not run: %q", content)
	}
}

func TestProxy_EmitsConnectAndTrafficEvents(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_x","content":[{"type":"text","text":"ok"}]}`)
	})

	client, base, store, cleanup := startPIITestRig(t, upstream)
	defer cleanup()

	body := `{"model":"claude-3-5-sonnet","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := client.Post(base+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("client.Post: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	var connect, traffic *pii.PIIEvent
	for i := range store.events {
		ev := &store.events[i]
		switch ev.ResolvedKind() {
		case pii.KindProxyConnect:
			connect = ev
		case pii.KindProxyTraffic:
			traffic = ev
		}
	}

	if connect == nil {
		t.Fatal("no proxy_connect event recorded")
	}
	if connect.Host != "api.anthropic.com" {
		t.Errorf("connect.Host = %q, want api.anthropic.com", connect.Host)
	}
	if connect.Intercepted == nil || !*connect.Intercepted {
		t.Error("connect.Intercepted should be true for an allowlisted host")
	}

	if traffic == nil {
		t.Fatal("no proxy_traffic event recorded")
	}
	if traffic.Host != "api.anthropic.com" {
		t.Errorf("traffic.Host = %q, want api.anthropic.com", traffic.Host)
	}
	if traffic.BytesSent <= 0 {
		t.Errorf("traffic.BytesSent = %d, want > 0", traffic.BytesSent)
	}
	if traffic.BytesReceived <= 0 {
		t.Errorf("traffic.BytesReceived = %d, want > 0", traffic.BytesReceived)
	}
	if traffic.StatusCode != 200 {
		t.Errorf("traffic.StatusCode = %d, want 200", traffic.StatusCode)
	}
}

func TestClassifyRequestShape(t *testing.T) {
	cases := []struct {
		host string
		path string
		want requestShape
	}{
		{"api.anthropic.com", "/v1/messages", shapeAnthropicMessages},
		{"api.openai.com", "/v1/chat/completions", shapeOpenAIChat},
		{"api.anthropic.com", "/v1/oauth/token", shapeUnknown},
		{"api.openai.com", "/v1/embeddings", shapeUnknown},
		{"example.com", "/v1/messages", shapeUnknown},
	}
	for _, c := range cases {
		got := classifyRequestShape(c.host, c.path)
		if got != c.want {
			t.Errorf("classify(%q, %q) = %v, want %v", c.host, c.path, got, c.want)
		}
	}
}
