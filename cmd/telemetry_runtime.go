package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"xrok/cmd/config"
	"xrok/cmd/telemetry"
)

// telemetryConfigFromEnvironment applies process-level OTLP overrides. The
// standard endpoint variable is supported alongside xrok-specific resource
// bounds. Empty variables preserve the versioned config values.
func telemetryConfigFromEnvironment(c config.TelemetryConfig, getenv func(string) string) (config.TelemetryConfig, error) {
	if getenv == nil {
		return c, nil
	}
	if v := getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"); v != "" {
		c.Endpoint = v
	} else if v := getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		c.Endpoint = v
	}
	ints := []struct {
		name string
		dst  *int
	}{{"XROK_OTLP_QUEUE_SIZE", &c.QueueSize}, {"XROK_OTLP_BATCH_SIZE", &c.BatchSize}, {"XROK_OTLP_MAX_RETRIES", &c.MaxRetries}}
	for _, item := range ints {
		if v := getenv(item.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return c, fmt.Errorf("invalid %s: %w", item.name, err)
			}
			*item.dst = n
		}
	}
	durations := []struct {
		name string
		dst  *time.Duration
	}{{"XROK_OTLP_FLUSH_INTERVAL", &c.FlushInterval}, {"XROK_OTLP_EXPORT_TIMEOUT", &c.ExportTimeout}, {"XROK_OTLP_RETRY_BACKOFF", &c.RetryBackoff}, {"XROK_OTLP_SHUTDOWN_TIMEOUT", &c.ShutdownTimeout}}
	for _, item := range durations {
		if v := getenv(item.name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return c, fmt.Errorf("invalid %s: %w", item.name, err)
			}
			*item.dst = d
		}
	}
	return c, validateTelemetryConfig(c)
}

// telemetryConfigForServer keeps injected runtimes independent from host
// process environment. Only the direct CLI path consumes OTEL_* overrides.
func telemetryConfigForServer(c config.TelemetryConfig, injected telemetry.Exporter, getenv func(string) string) (config.TelemetryConfig, error) {
	if injected != nil {
		return c, nil
	}
	return telemetryConfigFromEnvironment(c, getenv)
}

func validateTelemetryConfig(c config.TelemetryConfig) error {
	if strings.TrimSpace(c.Endpoint) == "" {
		return nil
	}
	probe := config.ServerConfig{Domain: "telemetry.validation", StateBackend: "memory", Telemetry: c}
	return probe.Validate()
}

func asyncOTLPConfig(c config.TelemetryConfig) telemetry.AsyncOTLPConfig {
	return telemetry.AsyncOTLPConfig{
		QueueSize: c.QueueSize, BatchSize: c.BatchSize,
		FlushInterval: c.FlushInterval, ExportTimeout: c.ExportTimeout,
		MaxRetries: c.MaxRetries, RetryBackoff: c.RetryBackoff,
	}
}

type runtimeTelemetry struct {
	exporter telemetry.Exporter
	close    func(context.Context) error
	timeout  time.Duration
}

func newRuntimeTelemetry(c config.TelemetryConfig, injected telemetry.Exporter) (runtimeTelemetry, error) {
	if err := validateTelemetryConfig(c); err != nil {
		return runtimeTelemetry{}, err
	}
	if injected != nil {
		r := runtimeTelemetry{exporter: injected, timeout: c.ShutdownTimeout}
		if closer, ok := injected.(interface{ Close(context.Context) error }); ok {
			r.close = closer.Close
		}
		return r, nil
	}
	if c.Endpoint == "" {
		return runtimeTelemetry{}, nil
	}
	exporter := telemetry.NewAsyncOTLPHTTPExporter(c.Endpoint, asyncOTLPConfig(c))
	return runtimeTelemetry{exporter: exporter, close: exporter.Close, timeout: c.ShutdownTimeout}, nil
}

func (r runtimeTelemetry) drain(ctx context.Context) error {
	if r.close == nil {
		return nil
	}
	if r.timeout <= 0 {
		return r.close(ctx)
	}
	drainCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.close(drainCtx)
}
