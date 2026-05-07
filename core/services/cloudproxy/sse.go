package cloudproxy

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent is one SSE event as the upstream sent it. raw is the
// exact wire bytes (including the trailing blank line that
// terminates the event in the SSE grammar) so the scanner can
// re-emit it byte-for-byte when the PII filter doesn't touch the
// data line. dataLine is the inner JSON of the first "data:" line
// when the event has one — both providers emit one data line per
// event today, so a slice isn't needed yet.
type sseEvent struct {
	raw      string
	dataLine string
}

// sseScanner is a minimal SSE event reader that yields one event
// per Scan() call. SSE events are blank-line delimited; the
// scanner accumulates lines until it hits an empty line, then
// surfaces the accumulated buffer as raw, plus the parsed inner
// data payload for the rewriter to inspect.
//
// We don't use the off-the-shelf stdlib scanner because we need
// to preserve the exact byte sequence (including the line
// separator the upstream chose) for pass-through and at the same
// time pull out the data payload. A custom scanner is ~30 lines
// and keeps both invariants explicit.
type sseScanner struct {
	r   *bufio.Reader
	ev  sseEvent
	err error
}

func newSSEScanner(r io.Reader) *sseScanner {
	return &sseScanner{r: bufio.NewReaderSize(r, 64*1024)}
}

// Scan reads the next event into Event(). Returns false on EOF or
// error; callers should check Err() to distinguish.
func (s *sseScanner) Scan() bool {
	var raw strings.Builder
	var dataLine string
	for {
		line, err := s.r.ReadString('\n')
		if line != "" {
			raw.WriteString(line)
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				// Event terminator. If we accumulated nothing,
				// keep reading — leading blank lines between
				// events are a no-op in SSE.
				if raw.Len() == len(line) {
					raw.Reset()
					continue
				}
				s.ev = sseEvent{raw: raw.String(), dataLine: dataLine}
				return true
			}
			if strings.HasPrefix(trimmed, "data:") {
				// "data:" with optional single-space prefix per
				// the SSE spec. We capture only the first data
				// line per event because both providers we
				// support today emit single-line JSON payloads.
				if dataLine == "" {
					payload := strings.TrimPrefix(trimmed, "data:")
					payload = strings.TrimPrefix(payload, " ")
					dataLine = payload
				}
			}
		}
		if err != nil {
			s.err = err
			if raw.Len() > 0 {
				// Surface a final partial event so the proxy
				// flushes any in-flight data before EOF.
				s.ev = sseEvent{raw: raw.String(), dataLine: dataLine}
				return true
			}
			return false
		}
	}
}

func (s *sseScanner) Event() sseEvent { return s.ev }
func (s *sseScanner) Err() error      { return s.err }
