package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTraceparentRoundTrip(t *testing.T) {
	tr := NewTraceID()
	sp := NewSpanID()
	got, parent, ok := ExtractTraceparent(InjectTraceparent(tr, sp))
	if !ok || got != tr || parent != sp {
		t.Fatalf("traceparent roundtrip failed")
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
