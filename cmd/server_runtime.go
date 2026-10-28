package cmd

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"xrok/cmd/config"
	"xrok/cmd/telemetry"
)

// ServerRuntime owns one server process lifecycle.  It provides an embeddable
// API for tests and supervisors while preserving the legacy RunServer CLI.
// The underlying handlers remain compatible with the existing global registry;
// lifecycle state is reset for each runtime instance.
type ServerRuntime struct {
	cfg                config.ServerConfig
	mu                 sync.Mutex
	start              bool
	closed             bool
	stop               sync.Once
	doneOnce           sync.Once
	done               chan struct{}
	run                func([]string)
	runContext         func(context.Context, []string) error
	runContextWithDeps func(context.Context, []string, *ServerDeps) error
	cancel             context.CancelFunc
	shutdown           chan struct{}
	legacy             bool
	runErr             error
	store              RuntimeStateStore
	drain              DrainController
	ha                 HAConfig
	telemetry          runtimeTelemetry
	telemetryConfig    config.TelemetryConfig
	serverDeps         *ServerDeps
}

func NewServerRuntime(cfg config.ServerConfig) (*ServerRuntime, error) {
	return NewServerRuntimeWithDeps(cfg, ServerRuntimeDeps{})
}

func newServerRuntime(cfg config.ServerConfig, deps ServerRuntimeDeps) (*ServerRuntime, error) {
	config.MigrateServerConfig(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Derive backend settings from the versioned ServerConfig. Environment
	// variables remain a CLI compatibility fallback, but embedded runtimes must
	// not silently select a different backend from process-global environment.
	ha, err := HAConfigFromServerConfig(cfg)
	if err != nil {
		return nil, err
	}
	var store RuntimeStateStore
	if deps.Store != nil {
		store = deps.Store
	} else {
		store, err = NewRuntimeStateStore(context.Background(), cfg, deps.StoreFactory)
		if err != nil {
			return nil, err
		}
	}
	injectedExporter := deps.TelemetryExporter
	if injectedExporter == nil && deps.ServerDeps != nil {
		injectedExporter = deps.ServerDeps.Exporter
	}
	var runtimeTelemetry runtimeTelemetry
	if injectedExporter != nil {
		runtimeTelemetry, err = newRuntimeTelemetry(cfg.Telemetry, injectedExporter)
		if err != nil {
			return nil, err
		}
	} else {
		// Endpoint exporters own a batching goroutine. Construct them only after
		// Start has acquired the runtime lifecycle, so an unstarted runtime or a
		// retryable legacy-owner conflict cannot leak that goroutine.
		if err := validateTelemetryConfig(cfg.Telemetry); err != nil {
			return nil, err
		}
		runtimeTelemetry.timeout = cfg.Telemetry.ShutdownTimeout
	}
	serverDeps := deps.ServerDeps
	if serverDeps == nil {
		serverDeps = NewServerDeps(cfg.Domain, time.Now(), nil)
	} else {
		// Preserve caller-owned registries while keeping per-runtime scalar
		// dependencies (notably Exporter) independently assignable.
		copy := *serverDeps
		serverDeps = &copy
	}
	serverDeps.Exporter = runtimeTelemetry.exporter
	r := &ServerRuntime{
		cfg: cfg, done: make(chan struct{}), shutdown: make(chan struct{}),
		ha: ha, store: store, legacy: true, telemetry: runtimeTelemetry,
		telemetryConfig: cfg.Telemetry, serverDeps: serverDeps,
	}
	r.run = func(args []string) { runServerWithConfig(args, &r.cfg, r.telemetry.exporter) }
	return r, nil
}

// ServerRuntimeDeps allows embedders and tests to inject the serving loop.
// When no runner is supplied, the legacy CLI runner is used for compatibility.
type ServerRuntimeDeps struct {
	Run func([]string)
	// RunContext is the compatibility embeddable runner. It receives a
	// runtime-owned context which is cancelled by Shutdown.
	RunContext func(context.Context, []string) error
	// RunContextWithDeps is the instance-safe serving hook. Handlers should use
	// the supplied ServerDeps, including its runtime-owned telemetry exporter.
	RunContextWithDeps func(context.Context, []string, *ServerDeps) error
	Store              RuntimeStateStore
	StoreFactory       StoreFactoryDeps
	// TelemetryExporter overrides endpoint-based construction for embedders and
	// tests. Closers implementing Close(context.Context) join graceful drain.
	TelemetryExporter telemetry.Exporter
	ServerDeps        *ServerDeps
}

func NewServerRuntimeWithDeps(cfg config.ServerConfig, deps ServerRuntimeDeps) (*ServerRuntime, error) {
	r, err := newServerRuntime(cfg, deps)
	if err != nil {
		return nil, err
	}
	if deps.Run != nil {
		r.run = deps.Run
	}
	r.runContext = deps.RunContext
	r.runContextWithDeps = deps.RunContextWithDeps
	if deps.Run != nil || deps.RunContext != nil || deps.RunContextWithDeps != nil {
		r.legacy = false
	}
	if deps.Store != nil {
		r.store = deps.Store
	}
	return r, nil
}

// Start launches the server asynchronously. Cancellation of ctx requests
// Shutdown; readiness is reported by the serving implementation, not Start.
func (r *ServerRuntime) Start(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("server runtime context is nil")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("server runtime is closed")
	}
	if r.start {
		r.mu.Unlock()
		return fmt.Errorf("server runtime already started")
	}
	if r.legacy && !acquireLegacyServerRuntime(r) {
		r.mu.Unlock()
		return fmt.Errorf("another legacy server runtime is already active")
	}
	if r.telemetry.exporter == nil && r.telemetryConfig.Endpoint != "" {
		runtimeTelemetry, err := newRuntimeTelemetry(r.telemetryConfig, nil)
		if err != nil {
			if r.legacy {
				releaseLegacyServerRuntime(r)
			}
			r.mu.Unlock()
			return err
		}
		r.telemetry = runtimeTelemetry
		r.serverDeps.Exporter = runtimeTelemetry.exporter
	}
	r.start = true
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	// The CLI runner still consumes a process-global channel. Only the active
	// legacy runtime may publish its owned channel there; injected runtimes are
	// fully isolated from it.
	if r.legacy {
		serverShutdownChan = r.shutdown
	}
	r.mu.Unlock()

	args := r.args()
	go func() {
		defer func() {
			drainTimeout := r.telemetry.timeout
			if drainTimeout <= 0 {
				drainTimeout = 10 * time.Second
			}
			drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
			_ = r.drain.Drain(drainCtx)
			// Keep telemetry accepting spans until the serving loop and its
			// handlers have stopped. Draining it from ShutdownContext would race
			// with final request/shutdown spans emitted after cancellation.
			_ = r.telemetry.drain(drainCtx)
			cancel()
			if r.legacy {
				releaseLegacyServerRuntime(r)
			}
			r.mu.Lock()
			r.closed = true
			r.mu.Unlock()
			r.doneOnce.Do(func() { close(r.done) })
		}()
		if r.runContextWithDeps != nil {
			err := r.runContextWithDeps(runCtx, args, r.serverDeps)
			r.mu.Lock()
			r.runErr = err
			r.mu.Unlock()
		} else if r.runContext != nil {
			err := r.runContext(runCtx, args)
			r.mu.Lock()
			r.runErr = err
			r.mu.Unlock()
		} else {
			r.run(args)
		}
	}()
	go func() {
		select {
		case <-runCtx.Done():
			r.Shutdown()
		case <-r.done:
		}
	}()
	return nil
}

func (r *ServerRuntime) Shutdown() {
	_ = r.ShutdownContext(context.Background())
}

// ShutdownContext stops this runtime only, then runs registered drain hooks.
// Calling it before Start closes runtime-owned resources without affecting
// another runtime; a shut down runtime cannot subsequently be started.
func (r *ServerRuntime) ShutdownContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("server shutdown context is nil")
	}
	r.mu.Lock()
	if !r.start {
		if r.closed {
			r.mu.Unlock()
			return nil
		}
		r.closed = true
		r.mu.Unlock()
		err := r.drain.Drain(ctx)
		if telemetryErr := r.telemetry.drain(ctx); err == nil {
			err = telemetryErr
		}
		r.doneOnce.Do(func() { close(r.done) })
		return err
	}
	cancel := r.cancel
	legacy := r.legacy
	shutdown := r.shutdown
	r.mu.Unlock()
	r.stop.Do(func() {
		if cancel != nil {
			cancel()
		}
		if legacy {
			closeServerShutdown(shutdown)
		}
	})
	return r.drain.Drain(ctx)
}

func (r *ServerRuntime) StateStore() RuntimeStateStore { return r.store }
func (r *ServerRuntime) HAConfig() HAConfig            { return r.ha }
func (r *ServerRuntime) ServerDeps() *ServerDeps       { return r.serverDeps }
func (r *ServerRuntime) AddDrainHook(h DrainHook)      { r.drain.Add(h) }

func (r *ServerRuntime) Wait() { <-r.done }

// WaitContext waits for completion without forcing callers to block forever
// when a serving implementation fails to honor cancellation.
func (r *ServerRuntime) WaitContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("server wait context is nil")
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitErr waits for completion and reports an error returned by RunContext.
func (r *ServerRuntime) WaitErr() error {
	r.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runErr
}

var legacyServerRuntimeOwner struct {
	sync.Mutex
	runtime *ServerRuntime
}

func acquireLegacyServerRuntime(r *ServerRuntime) bool {
	legacyServerRuntimeOwner.Lock()
	defer legacyServerRuntimeOwner.Unlock()
	if legacyServerRuntimeOwner.runtime != nil {
		return false
	}
	legacyServerRuntimeOwner.runtime = r
	return true
}

func releaseLegacyServerRuntime(r *ServerRuntime) {
	legacyServerRuntimeOwner.Lock()
	if legacyServerRuntimeOwner.runtime == r {
		legacyServerRuntimeOwner.runtime = nil
	}
	legacyServerRuntimeOwner.Unlock()
}

func (r *ServerRuntime) args() []string {
	c := r.cfg
	a := []string{"--domain", c.Domain, "--port", strconv.Itoa(c.ProxyPort), "--public-port", strconv.Itoa(c.PublicPort)}
	if c.TLSCertPath != "" {
		a = append(a, "--tls-cert", c.TLSCertPath)
	}
	if c.TLSKeyPath != "" {
		a = append(a, "--tls-key", c.TLSKeyPath)
	}
	if c.StateFile != "" {
		a = append(a, "--state-file", c.StateFile)
	}
	if c.LogFile != "" {
		a = append(a, "--log-file", c.LogFile)
	}
	if c.Debug {
		a = append(a, "--debug")
	}
	if c.RequestLog {
		a = append(a, "--request-log")
	}
	if c.ClientCAPath != "" {
		a = append(a, "--client-ca", c.ClientCAPath)
	}
	return a
}
