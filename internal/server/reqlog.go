package server

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// maxLoggedBody bounds how much of each request/response body the request log
// keeps in memory and writes to disk. A DAV PUT or multiget response can be
// large; this is a debugging aid, not an audit trail, so truncate rather than
// let logging reproduce the very memory-exhaustion the export/fan-out caps
// guard against.
const maxLoggedBody = 1 << 20 // 1 MiB

// requestLogger writes one file per request — method, path, headers, and
// (bounded) request and response bodies — to a directory, for debugging DAV
// clients. It runs as outer http middleware so it can capture the response
// too.
// Authorization headers are never written.
type requestLogger struct {
	dir string
	seq atomic.Uint64
}

// WrapRequestLog wraps next so every request is dumped to a file under dir.
// An empty dir disables logging and returns next unchanged. Creating the
// directory once up front means a misconfigured path fails loudly at startup.
func WrapRequestLog(next http.Handler, dir string) http.Handler {
	if dir == "" {
		return next
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		slog.Error("request log: cannot create directory; logging disabled", "dir", dir, "err", err)
		return next
	}
	l := &requestLogger{dir: dir}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody bytes.Buffer
		if r.Body != nil {
			r.Body = &capturingBody{rc: r.Body, buf: &reqBody, max: maxLoggedBody}
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK, body: &bytes.Buffer{}, max: maxLoggedBody}
		start := time.Now()
		// Deferred so a panicking handler is still logged before it unwinds.
		defer func() { l.write(r, &reqBody, rec, time.Since(start)) }()
		next.ServeHTTP(rec, r)
	})
}

func (l *requestLogger) write(r *http.Request, reqBody *bytes.Buffer, rec *statusRecorder, dur time.Duration) {
	n := l.seq.Add(1)
	name := fmt.Sprintf("%s-%06d-%s.log",
		time.Now().UTC().Format("20060102-150405.000"), n, sanitizeMethod(r.Method))
	path := filepath.Join(l.dir, name)

	var b strings.Builder
	fmt.Fprintf(&b, "--- REQUEST ---\n")
	fmt.Fprintf(&b, "Time:     %s\n", time.Now().UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(&b, "Method:   %s\n", r.Method)
	fmt.Fprintf(&b, "Path:     %s\n", r.URL.RequestURI())
	fmt.Fprintf(&b, "Remote:   %s\n", r.RemoteAddr)
	fmt.Fprintf(&b, "Duration: %s\n", dur)
	b.WriteString("\n--- REQUEST HEADERS ---\n")
	writeHeaders(&b, r.Header)
	b.WriteString("\n--- REQUEST BODY ---\n")
	writeBody(&b, reqBody.Bytes(), r.ContentLength)

	fmt.Fprintf(&b, "\n--- RESPONSE ---\n")
	fmt.Fprintf(&b, "Status: %d\n", rec.status)
	b.WriteString("\n--- RESPONSE HEADERS ---\n")
	writeHeaders(&b, rec.Header())
	b.WriteString("\n--- RESPONSE BODY ---\n")
	writeBody(&b, rec.body.Bytes(), int64(rec.written))

	if err := os.WriteFile(path, []byte(b.String()), 0o640); err != nil {
		slog.Warn("request log: write failed", "path", path, "err", err)
	}
}

// writeHeaders prints headers sorted for stable diffs, redacting Authorization
// so Basic passwords never land in a log file.
func writeHeaders(b *strings.Builder, h http.Header) {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") {
			fmt.Fprintf(b, "%s: [redacted]\n", name)
			continue
		}
		fmt.Fprintf(b, "%s: %s\n", name, strings.Join(h[name], ", "))
	}
}

// writeBody appends the captured body, noting truncation when the full body
// exceeded the in-memory cap.
func writeBody(b *strings.Builder, body []byte, fullLen int64) {
	b.Write(body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		b.WriteByte('\n')
	}
	if fullLen > int64(len(body)) {
		fmt.Fprintf(b, "[... truncated at %d bytes; full length %d ...]\n", len(body), fullLen)
	} else if len(body) >= maxLoggedBody {
		fmt.Fprintf(b, "[... truncated at %d bytes ...]\n", len(body))
	}
}

func sanitizeMethod(m string) string {
	if m == "" {
		return "UNKNOWN"
	}
	for _, c := range m {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return "OTHER"
		}
	}
	return m
}

// capturingBody tees the request body into buf (up to max bytes) as the
// handler reads it, so logging never consumes the body the handler needs.
type capturingBody struct {
	rc  io.ReadCloser
	buf *bytes.Buffer
	max int
}

func (c *capturingBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		if room := c.max - c.buf.Len(); room > 0 {
			if n < room {
				room = n
			}
			c.buf.Write(p[:room])
		}
	}
	return n, err
}

func (c *capturingBody) Close() error { return c.rc.Close() }
