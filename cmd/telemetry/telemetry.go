package telemetry

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Span struct {
	TraceID, SpanID, Name string
	Start, End            time.Time
	Attributes            map[string]string
	Status                int
}
type Exporter interface{ Export(context.Context, Span) }
type MemoryExporter struct {
	mu    sync.Mutex
	Spans []Span
}

func (e *MemoryExporter) Export(_ context.Context, s Span) {
	e.mu.Lock()
	e.Spans = append(e.Spans, s)
	e.mu.Unlock()
}
func (e *MemoryExporter) Snapshot() []Span {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Span, len(e.Spans))
	copy(out, e.Spans)
	return out
}

type ctxKey struct{}

var defaultExporter Exporter = &MemoryExporter{}

func SetExporter(e Exporter) {
	if e == nil {
		defaultExporter = &MemoryExporter{}
	} else {
		defaultExporter = e
	}
}
func NewTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", 32)
	}
	return hex.EncodeToString(b)
}
func NewSpanID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", 16)
	}
	return hex.EncodeToString(b)
}
func ExtractTraceparent(h string) (trace, parent string, ok bool) {
	p := strings.Split(strings.TrimSpace(h), "-")
	if len(p) != 4 || len(p[1]) != 32 || len(p[2]) != 16 {
		return "", "", false
	}
	return p[1], p[2], true
}
func InjectTraceparent(trace, span string) string { return "00-" + trace + "-" + span + "-01" }
func ContextWithTrace(ctx context.Context, trace string) context.Context {
	return context.WithValue(ctx, ctxKey{}, trace)
}
func TraceFromContext(ctx context.Context) string { v, _ := ctx.Value(ctxKey{}).(string); return v }

func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace, _, ok := ExtractTraceparent(r.Header.Get("traceparent"))
		if !ok {
			trace = NewTraceID()
		}
		span := NewSpanID()
		r = r.WithContext(ContextWithTrace(r.Context(), trace))
		r.Header.Set("traceparent", InjectTraceparent(trace, span))
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r)
		defaultExporter.Export(r.Context(), Span{TraceID: trace, SpanID: span, Name: r.Method + " " + r.URL.Path, Start: start, End: time.Now(), Status: sw.status, Attributes: map[string]string{"http.method": r.Method, "http.route": r.URL.Path}})
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) WriteHeader(s int) { w.status = s; w.ResponseWriter.WriteHeader(s) }
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}
