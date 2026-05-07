package mitm

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net/http"
	"strconv"
)

// connResponseWriter is a minimal http.ResponseWriter that writes
// directly to the hijacked TLS connection. We can't use the
// standard library's http.Server response machinery because the
// TLS conn was already extracted via hijack; the response side has
// to be hand-rolled.
//
// Supports:
//   - Header(): the headers buffered until the first write
//   - WriteHeader(int): emits the status line + headers + blank line
//   - Write([]byte): chunked transfer when no Content-Length is set,
//     identity otherwise
//   - Flusher: streaming responses (SSE) flush the underlying
//     buffered writer immediately
//
// Does NOT support:
//   - HTTP/2 trailers (HTTP/1.1 only in the MVP)
//   - Hijack on the response side (the TLS conn is already hijacked
//     once)
type connResponseWriter struct {
	conn *tls.Conn
	bw   *bufio.Writer
	req  *http.Request

	header        http.Header
	wroteHeader   bool
	chunked       bool
	contentLength int64
	written       int64
	closeAfter    bool
}

func newConnResponseWriter(conn *tls.Conn, req *http.Request) *connResponseWriter {
	return &connResponseWriter{
		conn:          conn,
		bw:            bufio.NewWriter(conn),
		req:           req,
		header:        make(http.Header),
		contentLength: -1,
	}
}

func (w *connResponseWriter) Header() http.Header { return w.header }

func (w *connResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	// Detect whether to send Content-Length or chunked. SSE
	// upstreams omit Content-Length and use Transfer-Encoding:
	// chunked or just keep the connection open with neither (HTTP
	// streaming with implicit framing). We mirror that: if the
	// caller set Content-Length, use it; else use chunked.
	if cl := w.header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			w.contentLength = n
		}
	}
	if w.contentLength < 0 {
		// SSE / streaming response — chunked is the right framing
		// for HTTP/1.1.
		w.chunked = true
		w.header.Set("Transfer-Encoding", "chunked")
		w.header.Del("Content-Length")
	}

	// HTTP/1.1 keep-alive is the default; honour upstream's
	// "Connection: close" hint and propagate it to the next
	// iteration of the request-read loop.
	if conn := w.header.Get("Connection"); conn != "" {
		for _, v := range w.header.Values("Connection") {
			if v == "close" {
				w.closeAfter = true
			}
		}
	}

	fmt.Fprintf(w.bw, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	_ = w.header.Write(w.bw)
	_, _ = w.bw.WriteString("\r\n")
}

func (w *connResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.chunked {
		// Chunked framing: <hex-size>\r\n<data>\r\n
		if _, err := fmt.Fprintf(w.bw, "%x\r\n", len(p)); err != nil {
			return 0, err
		}
		n, err := w.bw.Write(p)
		if err != nil {
			return n, err
		}
		if _, err := w.bw.WriteString("\r\n"); err != nil {
			return n, err
		}
		w.written += int64(n)
		return n, nil
	}
	n, err := w.bw.Write(p)
	w.written += int64(n)
	return n, err
}

// Flush forces buffered output to the wire. SSE clients depend on
// this — without it, intermediate buffers hold tokens until the
// stream ends, which defeats the whole point of streaming.
func (w *connResponseWriter) Flush() {
	_ = w.bw.Flush()
}

// finish closes the chunked stream (if any) and flushes the
// buffered writer. Called by the proxy core once the handler
// returns.
func (w *connResponseWriter) finish() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.chunked {
		// Empty terminating chunk + final CRLF.
		_, _ = w.bw.WriteString("0\r\n\r\n")
	}
	_ = w.bw.Flush()
}
