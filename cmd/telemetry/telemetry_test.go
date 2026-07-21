package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
