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

func TestInjectedTelemetryIgnoresProcessEnvironment(t *testing.T) {
	base := config.DefaultServerConfig().Telemetry
	exporter := &telemetry.MemoryExporter{}
	got, err := telemetryConfigForServer(base, exporter, func(string) string { return "invalid-host-environment" })
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Fatalf("injected runtime telemetry config changed by host env: got=%+v want=%+v", got, base)
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
	if r.telemetry.exporter != nil {
		t.Fatalf("endpoint exporter was constructed before Start: %T", r.telemetry.exporter)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.telemetry.exporter.(*telemetry.AsyncOTLPExporter); !ok {
		t.Fatalf("exporter=%T", r.telemetry.exporter)
	}
	r.Wait()
}

func TestServerRuntimeShutdownBeforeStartClosesOwnedTelemetry(t *testing.T) {
	exporter := &closingTestExporter{closed: make(chan struct{})}
	r, err := NewServerRuntimeWithDeps(config.ServerConfig{Domain: "test.local"}, ServerRuntimeDeps{TelemetryExporter: exporter})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ShutdownContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exporter.closed:
	default:
		t.Fatal("unstarted runtime did not close its owned exporter")
	}
	if err := r.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Start after Shutdown error=%v", err)
	}
	if err := r.WaitContext(context.Background()); err != nil {
		t.Fatal(err)
	}
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
		RunContextWithDeps: func(_ context.Context, _ []string, deps *ServerDeps) error {
			h := telemetry.HTTPMiddlewareWithExporter(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }), deps.Exporter)
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

func TestConcurrentServerRuntimesKeepTelemetryIsolated(t *testing.T) {
	exporterA := &telemetry.MemoryExporter{}
	exporterB := &telemetry.MemoryExporter{}
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	newRuntime := func(name string, exporter telemetry.Exporter) *ServerRuntime {
		r, err := NewServerRuntimeWithDeps(config.ServerConfig{Domain: name + ".test"}, ServerRuntimeDeps{
			TelemetryExporter: exporter,
			RunContextWithDeps: func(_ context.Context, _ []string, deps *ServerDeps) error {
				ready <- struct{}{}
				<-release
				h := telemetry.HTTPMiddlewareWithExporter(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), deps.Exporter)
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/"+name, nil))
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	runtimeA := newRuntime("alpha", exporterA)
	runtimeB := newRuntime("bravo", exporterB)
	if err := runtimeA.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runtimeB.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-ready
	<-ready
	close(release)
	runtimeA.Wait()
	runtimeB.Wait()
	spansA, spansB := exporterA.Snapshot(), exporterB.Snapshot()
	if len(spansA) != 1 || spansA[0].Name != "GET /alpha" {
		t.Fatalf("runtime A spans=%+v", spansA)
	}
	if len(spansB) != 1 || spansB[0].Name != "GET /bravo" {
		t.Fatalf("runtime B spans=%+v", spansB)
	}
}
