package mitm

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mudler/xlog"
)

// Server is an HTTPS forward proxy that selectively MITMs traffic
// for hosts in its intercept allowlist. Hosts outside the allowlist
// get a plain TCP CONNECT tunnel — the proxy reads the bytes once
// and never again, so OAuth flows, telemetry, and unrelated HTTPS
// keep working without depending on the CA being trusted.
//
// Server is safe for concurrent use; each accepted connection runs
// on its own goroutine.
type Server struct {
	addr            string
	ca              *CA
	interceptHosts  map[string]bool
	handler         InterceptHandler
	connectTimeout  time.Duration
	dialTimeout     time.Duration
	upstreamTLS     *tls.Config

	listener net.Listener
	srv      *http.Server

	wg       sync.WaitGroup
	stopOnce sync.Once
	stopped  chan struct{}
}

// InterceptHandler runs after the proxy has terminated TLS for an
// allowlisted host. It receives a fully-formed plaintext request
// (host header set to the original target) plus the upstream TLS
// config to use when dialing the real server. The handler is
// responsible for forwarding the response bytes to w.
//
// Implemented in handler.go for the PII redaction case. Decoupled
// from the proxy core so tests can swap in a no-op handler that
// just echoes upstream responses.
type InterceptHandler func(w http.ResponseWriter, r *http.Request, upstreamHost string)

// Config is the constructor input. addr is the plaintext address the
// proxy listens on (clients use it as HTTPS_PROXY). InterceptHosts
// is the lowercased hostname allowlist; CONNECTs to other hosts
// pass through as TCP tunnels. Handler runs intercepted requests.
type Config struct {
	Addr           string
	CA             *CA
	InterceptHosts []string
	Handler        InterceptHandler
}

// NewServer wires up the proxy. It does NOT start listening — call
// Start (or ListenAndServe) afterwards. Splitting construction from
// listening lets callers run the test fixture against a chosen port
// before tearing it down.
func NewServer(cfg Config) (*Server, error) {
	if cfg.CA == nil {
		return nil, errors.New("mitm: NewServer: CA is required")
	}
	if cfg.Handler == nil {
		return nil, errors.New("mitm: NewServer: Handler is required")
	}
	hosts := make(map[string]bool, len(cfg.InterceptHosts))
	for _, h := range cfg.InterceptHosts {
		hosts[strings.ToLower(strings.TrimSpace(h))] = true
	}
	return &Server{
		addr:           cfg.Addr,
		ca:             cfg.CA,
		interceptHosts: hosts,
		handler:        cfg.Handler,
		connectTimeout: 30 * time.Second,
		dialTimeout:    15 * time.Second,
		// Upstream TLS uses the system trust store — we trust the
		// real api.anthropic.com cert chain like any HTTPS client
		// would. No pinning, no MITM-of-MITM.
		upstreamTLS: &tls.Config{NextProtos: []string{"http/1.1"}},
		stopped:     make(chan struct{}),
	}, nil
}

// Start begins listening on the configured address. Returns once
// the listener is bound; serving runs in a background goroutine
// until Stop. The bound address is exposed via Addr() so tests can
// pick a free port (Addr ":0") and discover where it landed.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("mitm: listen %q: %w", s.addr, err)
	}
	s.listener = ln
	s.srv = &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: 30 * time.Second,
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := s.srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			xlog.Error("mitm: serve error", "error", err)
		}
	}()
	xlog.Info("mitm: listening", "addr", ln.Addr().String(), "intercept_hosts", len(s.interceptHosts))
	return nil
}

// Addr returns the bound listener address. Useful when Start was
// called with ":0" — the kernel picks a port and tests need to
// discover which.
func (s *Server) Addr() string {
	if s.listener == nil {
		return s.addr
	}
	return s.listener.Addr().String()
}

// Stop closes the listener and waits for in-flight handlers to
// drain. Idempotent — safe to call multiple times.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		if s.srv != nil {
			_ = s.srv.Close()
		}
		s.wg.Wait()
	})
}

// handle is the top-level dispatch. The proxy only speaks HTTP/1.1
// on its listener side (clients always send CONNECT, never speak
// HTTPS to the proxy itself). Method != CONNECT is rejected so
// unconfigured clients get a clear error.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "this proxy only supports HTTPS via CONNECT", http.StatusMethodNotAllowed)
		return
	}

	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		// Tolerate clients that send "api.anthropic.com" without
		// the port — defaults to 443 below.
		host = r.Host
	}
	host = strings.ToLower(host)

	if !s.shouldIntercept(host) {
		s.handleTunnel(w, r)
		return
	}
	s.handleIntercept(w, r, host)
}

// shouldIntercept consults the allowlist. Empty allowlist means
// "tunnel everything" — useful when a deployment wants the proxy
// purely for observability without TLS termination.
func (s *Server) shouldIntercept(host string) bool {
	if len(s.interceptHosts) == 0 {
		return false
	}
	return s.interceptHosts[host]
}

// handleTunnel implements plain CONNECT pass-through. Standard
// pattern: dial the upstream, write 200 to the client, then copy
// bytes both directions until either side closes.
func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	upstream, err := net.DialTimeout("tcp", normalizeHostPort(r.Host), s.dialTimeout)
	if err != nil {
		http.Error(w, "mitm: tunnel dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "mitm: hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "mitm: hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}

	pipe(clientConn, upstream)
}

// pipe relays bytes in both directions concurrently, ending when
// either copy finishes (peer closed or error). The goroutine
// without WaitGroup synchronisation is intentional: once one side
// closes, the other side's blocking Read or Write will fail, and we
// don't care which finishes first as long as we close both ends on
// return.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(a, b)
		_ = a.SetDeadline(time.Now())
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(b, a)
		_ = b.SetDeadline(time.Now())
		done <- struct{}{}
	}()
	<-done
}

// handleIntercept terminates TLS using a CA-signed leaf for the
// requested host, then reads HTTP/1.1 requests off the plaintext
// stream and dispatches each to the configured handler. Loops until
// the client closes (Connection: close, EOF, or error) so a single
// CONNECT can carry multiple requests (HTTP keep-alive).
func (s *Server) handleIntercept(w http.ResponseWriter, r *http.Request, host string) {
	leaf, err := s.ca.IssueLeaf(host)
	if err != nil {
		http.Error(w, "mitm: leaf issuance failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "mitm: hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "mitm: hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}

	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		// HTTP/1.1 only in the MVP. h2 is doable but adds the
		// golang.org/x/net/http2 dependency for ServeConn and
		// changes the request-handling model — deferred until we
		// observe a measurable perf hit on long streaming sessions.
		NextProtos: []string{"http/1.1"},
	})
	defer tlsConn.Close()

	if err := tlsConn.SetDeadline(time.Now().Add(s.connectTimeout)); err == nil {
		// The deadline above is for the handshake; we clear it
		// before the request loop so long-running streams aren't
		// killed at 30s.
		if err := tlsConn.Handshake(); err != nil {
			xlog.Debug("mitm: TLS handshake failed", "host", host, "error", err)
			return
		}
		_ = tlsConn.SetDeadline(time.Time{})
	}

	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				xlog.Debug("mitm: read request", "host", host, "error", err)
			}
			return
		}
		// http.ReadRequest sets req.URL.Scheme="" and Host from
		// the request line; populate Scheme so handler code can
		// build the upstream URL without guessing.
		req.URL.Scheme = "https"
		if req.URL.Host == "" {
			req.URL.Host = req.Host
		}
		// Wrap the connection in a minimal ResponseWriter so the
		// handler can stream the upstream response back.
		rw := newConnResponseWriter(tlsConn, req)
		s.handler(rw, req, host)
		rw.finish()
		// If the client (or upstream) signaled close, drop out of
		// the keep-alive loop.
		if req.Close || rw.closeAfter {
			return
		}
	}
}

// normalizeHostPort returns host:port — if the caller already has
// a port, returns the input unchanged; otherwise appends :443. We
// see hosts without ports when a non-RFC-7230 client sends just the
// authority component.
func normalizeHostPort(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return host + ":443"
}
