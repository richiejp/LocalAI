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
	"golang.org/x/net/http2"
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

// handleIntercept terminates TLS for the requested host using a
// CA-signed leaf, negotiates the application protocol via ALPN
// (preferring h2, falling back to http/1.1), and serves the
// plaintext stream with the matching parser. h2 is the primary
// path — modern clients negotiate it and Anthropic / OpenAI APIs
// require it for keep-alive multiplexing. h1.1 stays as a fallback
// because the ALPN spec mandates an h1 fallback option.
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
		// h2 first so modern clients (Claude Code, Codex, anything
		// built on Go/Node since 2018) get HTTP/2. h1.1 stays as a
		// fallback for the rare client that doesn't speak h2.
		NextProtos: []string{"h2", "http/1.1"},
	})
	defer tlsConn.Close()

	// The deadline below is for the handshake only; we clear it
	// before serving so long-running streams aren't killed at 30s.
	if err := tlsConn.SetDeadline(time.Now().Add(s.connectTimeout)); err == nil {
		if err := tlsConn.Handshake(); err != nil {
			xlog.Debug("mitm: TLS handshake failed", "host", host, "error", err)
			return
		}
		_ = tlsConn.SetDeadline(time.Time{})
	}

	// Wrap the InterceptHandler as a standard http.Handler so both
	// the h2 server and the h1 loop can dispatch through the same
	// adapter. Closure captures `host` so the handler still receives
	// the per-host context the InterceptHandler signature expects.
	handler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		req.URL.Scheme = "https"
		if req.URL.Host == "" {
			req.URL.Host = req.Host
		}
		s.handler(rw, req, host)
	})

	switch tlsConn.ConnectionState().NegotiatedProtocol {
	case "h2":
		// http2.Server takes the already-TLS-terminated conn and
		// runs the framing layer + multiplexing internally. We
		// pass our intercept handler unchanged — h2 streams map
		// 1:1 to http.Request, just like h1, so the handler shape
		// doesn't have to know which protocol it's serving.
		h2srv := &http2.Server{}
		h2srv.ServeConn(tlsConn, &http2.ServeConnOpts{
			Handler: handler,
			Context: r.Context(),
		})
	default:
		// "http/1.1" or empty NegotiatedProtocol (older clients
		// that don't send ALPN at all). Fall back to the manual
		// keep-alive loop with the in-package response writer.
		s.serveHTTP1(tlsConn, handler, host)
	}
}

// serveHTTP1 reads HTTP/1.1 requests from a TLS-terminated conn and
// dispatches each through handler until the client closes or
// signals Connection: close. Lives separately from the h2 path
// because http2.Server.ServeConn handles its own request loop;
// h1.1 has to be done by hand on a hijacked conn.
func (s *Server) serveHTTP1(tlsConn *tls.Conn, handler http.Handler, host string) {
	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				xlog.Debug("mitm: read request", "host", host, "error", err)
			}
			return
		}
		rw := newConnResponseWriter(tlsConn, req)
		handler.ServeHTTP(rw, req)
		rw.finish()
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
