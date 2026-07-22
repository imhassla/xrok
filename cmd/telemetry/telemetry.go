package telemetry

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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
	return &OTLPHTTPExporter{Endpoint: normalizeOTLPTraceEndpoint(endpoint), Client: &http.Client{Timeout: 5 * time.Second}}
}

func normalizeOTLPTraceEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	// A host/root value is the conventional OTLP base endpoint. A non-root
	// path is already a per-signal endpoint and must be preserved verbatim,
	// including any query used by an authenticated collector gateway.
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/traces"
	}
	return u.String()
}

func (e *OTLPHTTPExporter) Export(ctx context.Context, s Span) {
	_ = e.exportBatch(ctx, []Span{s})
}

func (e *OTLPHTTPExporter) exportBatch(ctx context.Context, spans []Span) error {
	if e == nil || e.Endpoint == "" {
		return errors.New("telemetry: OTLP endpoint is empty")
	}
	otlpSpans := make([]otlpSpan, 0, len(spans))
	for _, s := range spans {
		otlpSpans = append(otlpSpans, otlpSpan{TraceID: s.TraceID, SpanID: s.SpanID, Name: s.Name, StartTimeUnixNano: s.Start.UnixNano(), EndTimeUnixNano: s.End.UnixNano(), Status: otlpStatusForHTTP(s.Status), Attributes: otlpAttributes(s.Attributes)})
	}
	p := otlpTracePayload{ResourceSpans: []otlpResourceSpans{{ScopeSpans: []otlpScopeSpans{{Spans: otlpSpans}}}}}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return retryableExportError{err: err}
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	err = fmt.Errorf("telemetry: collector returned HTTP %d", resp.StatusCode)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return retryableExportError{err: err}
	}
	return err
}

type retryableExportError struct{ err error }

func (e retryableExportError) Error() string { return e.err.Error() }
func (e retryableExportError) Unwrap() error { return e.err }

type retryable interface{ Retryable() bool }

func (e retryableExportError) Retryable() bool { return true }

// AsyncOTLPConfig bounds every resource used by AsyncOTLPExporter. A slow or
// unavailable collector can therefore never apply backpressure to tunnel
// request handling or grow memory without limit.
type AsyncOTLPConfig struct {
	QueueSize     int
	BatchSize     int
	FlushInterval time.Duration
	ExportTimeout time.Duration
	MaxRetries    int
	RetryBackoff  time.Duration
}

func DefaultAsyncOTLPConfig() AsyncOTLPConfig {
	return AsyncOTLPConfig{QueueSize: 2048, BatchSize: 128, FlushInterval: time.Second, ExportTimeout: 5 * time.Second, MaxRetries: 3, RetryBackoff: 100 * time.Millisecond}
}

type AsyncOTLPStats struct {
	Accepted      uint64
	Exported      uint64
	Dropped       uint64
	Failed        uint64
	Retries       uint64
	Batches       uint64
	QueueDepth    int
	QueueCapacity int
}

type batchExportFunc func(context.Context, []Span) error

// AsyncOTLPExporter batches OTLP spans in a bounded in-memory queue. Export is
// deliberately non-blocking; QueueFull drops are counted rather than delaying
// the data plane. Close flushes pending spans until its context expires.
type AsyncOTLPExporter struct {
	cfg       AsyncOTLPConfig
	queue     chan Span
	export    batchExportFunc
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	lifecycle sync.RWMutex
	closed    atomic.Bool
	accepted  atomic.Uint64
	exported  atomic.Uint64
	dropped   atomic.Uint64
	failed    atomic.Uint64
	retries   atomic.Uint64
	batches   atomic.Uint64
}

func NewAsyncOTLPHTTPExporter(endpoint string, cfg AsyncOTLPConfig) *AsyncOTLPExporter {
	httpExporter := NewOTLPHTTPExporter(endpoint)
	return newAsyncOTLPExporter(cfg, httpExporter.exportBatch)
}

func newAsyncOTLPExporter(cfg AsyncOTLPConfig, export batchExportFunc) *AsyncOTLPExporter {
	defaults := DefaultAsyncOTLPConfig()
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaults.QueueSize
	} else if cfg.QueueSize > 65536 {
		cfg.QueueSize = 65536
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaults.BatchSize
	}
	if cfg.BatchSize > cfg.QueueSize {
		cfg.BatchSize = cfg.QueueSize
	}
	if cfg.BatchSize > 4096 {
		cfg.BatchSize = 4096
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = defaults.FlushInterval
	} else if cfg.FlushInterval > time.Hour {
		cfg.FlushInterval = time.Hour
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = defaults.ExportTimeout
	} else if cfg.ExportTimeout > time.Minute {
		cfg.ExportTimeout = time.Minute
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	} else if cfg.MaxRetries > 8 {
		cfg.MaxRetries = 8
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = defaults.RetryBackoff
	} else if cfg.RetryBackoff > time.Minute {
		cfg.RetryBackoff = time.Minute
	}
	if export == nil {
		export = func(context.Context, []Span) error { return errors.New("telemetry: nil batch exporter") }
	}
	e := &AsyncOTLPExporter{cfg: cfg, queue: make(chan Span, cfg.QueueSize), export: export, stop: make(chan struct{}), done: make(chan struct{})}
	go e.run()
	return e
}

func (e *AsyncOTLPExporter) Export(_ context.Context, span Span) {
	if e == nil {
		return
	}
	e.lifecycle.RLock()
	defer e.lifecycle.RUnlock()
	if e.closed.Load() {
		e.dropped.Add(1)
		return
	}
	select {
	case e.queue <- span:
		e.accepted.Add(1)
	default:
		e.dropped.Add(1)
	}
}

func (e *AsyncOTLPExporter) Stats() AsyncOTLPStats {
	if e == nil {
		return AsyncOTLPStats{}
	}
	return AsyncOTLPStats{Accepted: e.accepted.Load(), Exported: e.exported.Load(), Dropped: e.dropped.Load(), Failed: e.failed.Load(), Retries: e.retries.Load(), Batches: e.batches.Load(), QueueDepth: len(e.queue), QueueCapacity: cap(e.queue)}
}

func (e *AsyncOTLPExporter) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.closeOnce.Do(func() {
		e.lifecycle.Lock()
		e.closed.Store(true)
		close(e.stop)
		e.lifecycle.Unlock()
	})
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *AsyncOTLPExporter) run() {
	defer close(e.done)
	ticker := time.NewTicker(e.cfg.FlushInterval)
	defer ticker.Stop()
	batch := make([]Span, 0, e.cfg.BatchSize)
	flush := func() {
		if len(batch) > 0 {
			e.send(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case span := <-e.queue:
			batch = append(batch, span)
			if len(batch) >= e.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-e.stop:
			for {
				select {
				case span := <-e.queue:
					batch = append(batch, span)
					if len(batch) >= e.cfg.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (e *AsyncOTLPExporter) send(batch []Span) {
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), e.cfg.ExportTimeout)
		err := e.export(ctx, batch)
		cancel()
		if err == nil {
			e.exported.Add(uint64(len(batch)))
			e.batches.Add(1)
			return
		}
		var r retryable
		if attempt >= e.cfg.MaxRetries || !errors.As(err, &r) || !r.Retryable() {
			e.failed.Add(uint64(len(batch)))
			e.dropped.Add(uint64(len(batch)))
			return
		}
		e.retries.Add(1)
		delay := e.cfg.RetryBackoff << attempt
		if delay <= 0 || delay > time.Minute {
			delay = time.Minute
		}
		select {
		case <-time.After(delay):
		case <-e.stop:
			// Close is a flush request: skip retry sleeps but retain the bounded
			// retry budget so transient collector failures do not drop accepted
			// spans during graceful shutdown.
		}
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

func otlpStatusForHTTP(status int) otlpStatus {
	// OTLP StatusCode is an enum (UNSET=0, OK=1, ERROR=2), not an HTTP
	// status code. Server spans mark 5xx responses as errors; 4xx describes
	// a client-side request failure and remains unset per HTTP conventions.
	if status >= http.StatusInternalServerError {
		return otlpStatus{Code: 2}
	}
	return otlpStatus{}
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
	if len(p) != 4 || len(p[0]) != 2 || len(p[1]) != 32 || len(p[2]) != 16 || len(p[3]) != 2 {
		return "", "", false
	}
	version, versionErr := hex.DecodeString(p[0])
	traceBytes, traceErr := hex.DecodeString(p[1])
	parentBytes, parentErr := hex.DecodeString(p[2])
	_, flagsErr := hex.DecodeString(p[3])
	if versionErr != nil || traceErr != nil || parentErr != nil || flagsErr != nil || version[0] == 0xff || allZero(traceBytes) || allZero(parentBytes) {
		return "", "", false
	}
	return p[1], p[2], true
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}
func InjectTraceparent(trace, span string) string { return "00-" + trace + "-" + span + "-01" }
func ContextWithTrace(ctx context.Context, trace string) context.Context {
	return context.WithValue(ctx, ctxKey{}, trace)
}
func TraceFromContext(ctx context.Context) string { v, _ := ctx.Value(ctxKey{}).(string); return v }

// HTTPMiddlewareWithExporter records one server span into the explicitly
// supplied exporter. A nil exporter disables recording while preserving trace
// propagation, which keeps runtimes isolated without process-global state.
func HTTPMiddlewareWithExporter(next http.Handler, exporter Exporter) http.Handler {
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
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		if exporter != nil {
			exporter.Export(r.Context(), Span{TraceID: trace, SpanID: span, Name: r.Method + " " + r.URL.Path, Start: start, End: time.Now(), Status: status, Attributes: map[string]string{"http.method": r.Method, "http.route": r.URL.Path}})
		}
	})
}

// HTTPMiddleware preserves the pre-DI middleware API. It propagates trace
// context but intentionally performs no process-global export. New serving
// paths should use HTTPMiddlewareWithExporter.
func HTTPMiddleware(next http.Handler) http.Handler {
	return HTTPMiddlewareWithExporter(next, nil)
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

func (w *statusWriter) WriteHeader(s int) {
	if w.status != 0 {
		return
	}
	w.status = s
	w.ResponseWriter.WriteHeader(s)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}
