package telemetry

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestOTLPCollectorIntegration is activated by the local reference-lab hook.
// Normal unit and CI runs remain hermetic.
func TestOTLPCollectorIntegration(t *testing.T) {
	endpoint := os.Getenv("XROK_OTLP_INTEGRATION_ENDPOINT")
	if endpoint == "" {
		t.Skip("XROK_OTLP_INTEGRATION_ENDPOINT is not set")
	}
	exporter := NewAsyncOTLPHTTPExporter(endpoint, AsyncOTLPConfig{
		QueueSize: 16, BatchSize: 2, FlushInterval: 20 * time.Millisecond,
		ExportTimeout: 2 * time.Second, MaxRetries: 2, RetryBackoff: 20 * time.Millisecond,
	})
	now := time.Now()
	exporter.Export(context.Background(), Span{TraceID: "00112233445566778899aabbccddeeff", SpanID: "0011223344556677", Name: "xrok.collector.integration", Start: now, End: now.Add(time.Millisecond), Status: 200})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exporter.Close(ctx); err != nil {
		t.Fatal(err)
	}
	stats := exporter.Stats()
	if stats.Exported != 1 || stats.Dropped != 0 {
		t.Fatalf("unexpected exporter stats: %+v", stats)
	}
}
