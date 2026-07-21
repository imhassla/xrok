package telemetry

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

// OTLPHTTPExporter exports spans using the OTLP/HTTP JSON protocol. It is
// intentionally dependency-free so the runtime can emit traces to a local
// OpenTelemetry Collector without pulling the full SDK into binaries.
type OTLPHTTPExporter struct {
	Endpoint string
	Client   *http.Client
}

func NewOTLPHTTPExporter(endpoint string) *OTLPHTTPExporter {
	endpoint = strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(endpoint, "/v1/traces") {
		endpoint += "/v1/traces"
	}
	return &OTLPHTTPExporter{Endpoint: endpoint, Client: &http.Client{Timeout: 5 * time.Second}}
}

func (e *OTLPHTTPExporter) Export(ctx context.Context, s Span) {
	if e == nil || e.Endpoint == "" {
		return
	}
	p := otlpTracePayload{ResourceSpans: []otlpResourceSpans{{ScopeSpans: []otlpScopeSpans{{Spans: []otlpSpan{{TraceID: s.TraceID, SpanID: s.SpanID, Name: s.Name, StartTimeUnixNano: s.Start.UnixNano(), EndTimeUnixNano: s.End.UnixNano(), Status: otlpStatus{Code: s.Status}, Attributes: otlpAttributes(s.Attributes)}}}}}}}
	b, err := json.Marshal(p)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint, strings.NewReader(string(b)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

type otlpTracePayload struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}
type otlpResourceSpans struct {
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}
type otlpScopeSpans struct {
	Spans []otlpSpan `json:"spans"`
}
type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	Name              string          `json:"name"`
	StartTimeUnixNano int64           `json:"startTimeUnixNano,string"`
	EndTimeUnixNano   int64           `json:"endTimeUnixNano,string"`
	Attributes        []otlpAttribute `json:"attributes,omitempty"`
	Status            otlpStatus      `json:"status"`
}
type otlpAttribute struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}
type otlpValue struct {
	StringValue string `json:"stringValue,omitempty"`
}
type otlpStatus struct {
	Code int `json:"code,omitempty"`
}

func otlpAttributes(in map[string]string) []otlpAttribute {
	out := make([]otlpAttribute, 0, len(in))
	for k, v := range in {
		out = append(out, otlpAttribute{Key: k, Value: otlpValue{StringValue: v}})
	}
	return out
}

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
var exporterMu sync.RWMutex

func SetExporter(e Exporter) {
	exporterMu.Lock()
	defer exporterMu.Unlock()
	if e == nil {
		defaultExporter = &MemoryExporter{}
	} else {
		defaultExporter = e
	}
}

func currentExporter() Exporter {
	exporterMu.RLock()
	defer exporterMu.RUnlock()
	return defaultExporter
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
		currentExporter().Export(r.Context(), Span{TraceID: trace, SpanID: span, Name: r.Method + " " + r.URL.Path, Start: start, End: time.Now(), Status: sw.status, Attributes: map[string]string{"http.method": r.Method, "http.route": r.URL.Path}})
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
