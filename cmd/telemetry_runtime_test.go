package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xrok/cmd/config"
	"xrok/cmd/telemetry"
)

type closingTestExporter struct {
	exports atomic.Int64
	closed  chan struct{}
}

func (e *closingTestExporter) Export(context.Context, telemetry.Span) { e.exports.Add(1) }
func (e *closingTestExporter) Close(context.Context) error {
	select {
	case <-e.closed:
	default:
		close(e.closed)
	}
	return nil
}

func TestTelemetryConfigFromEnvironment(t *testing.T) {
	values := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":        "http://collector:4318",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://traces:4318/v1/traces",
		"XROK_OTLP_QUEUE_SIZE":               "64", "XROK_OTLP_BATCH_SIZE": "8",
		"XROK_OTLP_FLUSH_INTERVAL": "20ms", "XROK_OTLP_MAX_RETRIES": "4",
	}
	c := config.DefaultServerConfig().Telemetry
	got, err := telemetryConfigFromEnvironment(c, func(k string) string { return values[k] })
	if err != nil {
		t.Fatal(err)
	}
	if got.Endpoint != values["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] || got.QueueSize != 64 || got.BatchSize != 8 || got.FlushInterval != 20*time.Millisecond || got.MaxRetries != 4 {
		t.Fatalf("unexpected env config: %#v", got)
	}
	values["XROK_OTLP_QUEUE_SIZE"] = "not-an-int"
	if _, err := telemetryConfigFromEnvironment(c, func(k string) string { return values[k] }); err == nil || !strings.Contains(err.Error(), "XROK_OTLP_QUEUE_SIZE") {
		t.Fatalf("invalid env error=%v", err)
	}
}

func TestServerRuntimeDrainsInjectedTelemetryOnNaturalExit(t *testing.T) {
	exporter := &closingTestExporter{closed: make(chan struct{})}
	r, err := NewServerRuntimeWithDeps(config.ServerConfig{Domain: "test.local"}, ServerRuntimeDeps{
		TelemetryExporter: exporter,
		RunContext: func(context.Context, []string) error {
			exporter.Export(context.Background(), telemetry.Span{Name: "runtime-test"})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	select {
	case <-exporter.closed:
	default:
		t.Fatal("telemetry exporter was not drained")
	}
	if exporter.exports.Load() != 1 {
		t.Fatalf("exports=%d", exporter.exports.Load())
	}
}

func TestServerRuntimeAcceptsFinalSpanAfterShutdownCancellation(t *testing.T) {
	exporter := &closingTestExporter{closed: make(chan struct{})}
	r, err := NewServerRuntimeWithDeps(config.ServerConfig{Domain: "test.local"}, ServerRuntimeDeps{
		TelemetryExporter: exporter,
		RunContext: func(ctx context.Context, _ []string) error {
			<-ctx.Done()
			exporter.Export(context.Background(), telemetry.Span{Name: "shutdown-final"})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.ShutdownContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if exporter.exports.Load() != 1 {
		t.Fatalf("final shutdown span was lost; exports=%d", exporter.exports.Load())
	}
	select {
	case <-exporter.closed:
	default:
		t.Fatal("telemetry exporter was not closed after serving loop exit")
	}
}

func TestServerRuntimeBuildsConfiguredAsyncExporter(t *testing.T) {
	cfg := config.ServerConfig{Domain: "test.local", Telemetry: config.TelemetryConfig{Endpoint: "http://127.0.0.1:4318"}}
	r, err := NewServerRuntimeWithDeps(cfg, ServerRuntimeDeps{RunContext: func(context.Context, []string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.telemetry.exporter.(*telemetry.AsyncOTLPExporter); !ok {
		t.Fatalf("exporter=%T", r.telemetry.exporter)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Wait()
}

func TestServerRuntimeOTLPCollectorIntegration(t *testing.T) {
	endpoint := os.Getenv("XROK_OTLP_INTEGRATION_ENDPOINT")
	if endpoint == "" {
		t.Skip("XROK_OTLP_INTEGRATION_ENDPOINT is not set")
	}
	cfg := *config.DefaultServerConfig()
	cfg.Domain = "test.local"
	cfg.Telemetry.Endpoint = endpoint
	cfg.Telemetry.FlushInterval = 10 * time.Millisecond
	r, err := NewServerRuntimeWithDeps(cfg, ServerRuntimeDeps{
		RunContext: func(context.Context, []string) error {
			h := telemetry.HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/collector-integration", nil))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	exporter := r.telemetry.exporter.(*telemetry.AsyncOTLPExporter)
	if stats := exporter.Stats(); stats.Exported != 1 || stats.Dropped != 0 {
		t.Fatalf("runtime exporter did not drain: %+v", stats)
	}
}
