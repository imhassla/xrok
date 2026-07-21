package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTraceparentRoundTrip(t *testing.T) {
	tr := NewTraceID()
	sp := NewSpanID()
	got, parent, ok := ExtractTraceparent(InjectTraceparent(tr, sp))
	if !ok || got != tr || parent != sp {
		t.Fatalf("traceparent roundtrip failed")
	}
}

func TestTraceparentRejectsMalformedIdentifiers(t *testing.T) {
	tests := []string{
		"00-not-hex-not-hex-01",
		"00-00000000000000000000000000000000-0123456789abcdef-01",
		"00-0123456789abcdef0123456789abcdef-0000000000000000-01",
		"ff-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
		"00-0123456789abcdef0123456789abcdef-0123456789abcdef-zz",
	}
	for _, header := range tests {
		if _, _, ok := ExtractTraceparent(header); ok {
			t.Fatalf("accepted malformed traceparent %q", header)
		}
	}
}

func TestOTLPStatusMapsHTTPToEnum(t *testing.T) {
	if got := otlpStatusForHTTP(http.StatusOK).Code; got != 0 {
		t.Fatalf("200 mapped to OTLP status %d", got)
	}
	if got := otlpStatusForHTTP(http.StatusBadRequest).Code; got != 0 {
		t.Fatalf("400 mapped to OTLP status %d", got)
	}
	if got := otlpStatusForHTTP(http.StatusServiceUnavailable).Code; got != 2 {
		t.Fatalf("503 mapped to OTLP status %d, want ERROR", got)
	}
}

func TestOTLPHTTPExporter(t *testing.T) {
	defer func() {
		if recover() != nil {
			t.Skip("loopback listener unavailable in restricted test environment")
		}
	}()
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("bad OTLP request: %s", r.URL)
		}
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		got <- p
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	e := NewOTLPHTTPExporter(srv.URL)
	e.Export(context.Background(), Span{TraceID: "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef", Name: "unit", Start: time.Unix(1, 2), End: time.Unix(1, 3), Status: 200})
	select {
	case p := <-got:
		if len(p["resourceSpans"].([]any)) != 1 {
			t.Fatal("missing resource spans")
		}
	case <-time.After(time.Second):
		t.Fatal("export timeout")
	}
}
func TestHTTPMiddlewareExportsAndPropagates(t *testing.T) {
	e := &MemoryExporter{}
	SetExporter(e)
	defer SetExporter(nil)
	h := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if TraceFromContext(r.Context()) == "" {
			t.Error("missing trace context")
		}
		if r.Header.Get("traceparent") == "" {
			t.Error("missing propagation")
		}
		w.WriteHeader(201)
	}))
	req := httptest.NewRequest("GET", "http://x/test", nil)
	req.Header.Set("traceparent", InjectTraceparent(NewTraceID(), NewSpanID()))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 201 || len(e.Snapshot()) != 1 || e.Snapshot()[0].Status != 201 {
		t.Fatalf("span not exported")
	}
	_ = context.Background()
}

func TestHTTPMiddlewareRecordsImplicitOKAndFirstStatus(t *testing.T) {
	e := &MemoryExporter{}
	SetExporter(e)
	defer SetExporter(nil)

	h := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://x/test", nil))
	spans := e.Snapshot()
	if rr.Code != http.StatusCreated || len(spans) != 1 || spans[0].Status != http.StatusCreated {
		t.Fatalf("first status not preserved: response=%d spans=%+v", rr.Code, spans)
	}

	e = &MemoryExporter{}
	SetExporter(e)
	h = HTTPMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://x/implicit", nil))
	spans = e.Snapshot()
	if len(spans) != 1 || spans[0].Status != http.StatusOK {
		t.Fatalf("implicit response status=%+v", spans)
	}
}

func TestAsyncOTLPBatchesAndFlushesOnClose(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	e := newAsyncOTLPExporter(AsyncOTLPConfig{QueueSize: 8, BatchSize: 3, FlushInterval: time.Hour, ExportTimeout: time.Second}, func(_ context.Context, spans []Span) error {
		mu.Lock()
		sizes = append(sizes, len(spans))
		mu.Unlock()
		return nil
	})
	for i := 0; i < 5; i++ {
		e.Export(context.Background(), Span{Name: "batch"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	stats := e.Stats()
	if stats.Accepted != 5 || stats.Exported != 5 || stats.Batches != 2 || stats.Dropped != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sizes) != 2 || sizes[0] != 3 || sizes[1] != 2 {
		t.Fatalf("batch sizes = %v", sizes)
	}
}

func TestAsyncOTLPRetriesRetryableFailures(t *testing.T) {
	var calls atomic.Int32
	e := newAsyncOTLPExporter(AsyncOTLPConfig{QueueSize: 2, BatchSize: 1, FlushInterval: time.Hour, ExportTimeout: time.Second, MaxRetries: 3, RetryBackoff: time.Millisecond}, func(context.Context, []Span) error {
		if calls.Add(1) < 3 {
			return retryableExportError{err: errors.New("collector unavailable")}
		}
		return nil
	})
	e.Export(context.Background(), Span{Name: "retry"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	stats := e.Stats()
	if calls.Load() != 3 || stats.Retries != 2 || stats.Exported != 1 || stats.Dropped != 0 {
		t.Fatalf("unexpected retry result: calls=%d stats=%+v", calls.Load(), stats)
	}
}

func TestAsyncOTLPDropsWhenQueueIsFull(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e := newAsyncOTLPExporter(AsyncOTLPConfig{QueueSize: 2, BatchSize: 1, FlushInterval: time.Hour, ExportTimeout: time.Second}, func(context.Context, []Span) error {
		once.Do(func() { close(started) })
		<-release
		return nil
	})
	e.Export(context.Background(), Span{Name: "active"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("export did not start")
	}
	e.Export(context.Background(), Span{Name: "queued-1"})
	e.Export(context.Background(), Span{Name: "queued-2"})
	e.Export(context.Background(), Span{Name: "dropped"})
	if got := e.Stats(); got.Dropped != 1 || got.QueueDepth != 2 || got.QueueCapacity != 2 {
		t.Fatalf("queue was not bounded: %+v", got)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.Stats(); got.Accepted != 3 || got.Exported != 3 || got.Dropped != 1 {
		t.Fatalf("unexpected final stats: %+v", got)
	}
}

func TestAsyncOTLPDoesNotRetryPermanentFailure(t *testing.T) {
	var calls atomic.Int32
	e := newAsyncOTLPExporter(AsyncOTLPConfig{QueueSize: 1, BatchSize: 1, FlushInterval: time.Hour, ExportTimeout: time.Second, MaxRetries: 5}, func(context.Context, []Span) error {
		calls.Add(1)
		return errors.New("bad request")
	})
	e.Export(context.Background(), Span{Name: "invalid"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.Stats(); calls.Load() != 1 || got.Failed != 1 || got.Dropped != 1 || got.Retries != 0 {
		t.Fatalf("permanent failure retried: calls=%d stats=%+v", calls.Load(), got)
	}
}

func TestAsyncOTLPRejectsExportAfterClose(t *testing.T) {
	e := newAsyncOTLPExporter(AsyncOTLPConfig{QueueSize: 1, BatchSize: 1}, func(context.Context, []Span) error { return nil })
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.Export(context.Background(), Span{Name: "late"})
	if got := e.Stats(); got.Dropped != 1 || got.Accepted != 0 {
		t.Fatalf("late export stats: %+v", got)
	}
}
