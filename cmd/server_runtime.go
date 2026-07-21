package cmd

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"xrok/cmd/config"
)

// ServerRuntime owns one server process lifecycle.  It provides an embeddable
// API for tests and supervisors while preserving the legacy RunServer CLI.
// The underlying handlers remain compatible with the existing global registry;
// lifecycle state is reset for each runtime instance.
type ServerRuntime struct {
	cfg        config.ServerConfig
	mu         sync.Mutex
	start      bool
	stop       sync.Once
	done       chan struct{}
	run        func([]string)
	runContext func(context.Context, []string) error
	cancel     context.CancelFunc
	shutdown   chan struct{}
	legacy     bool
	runErr     error
	store      RuntimeStateStore
	drain      DrainController
	ha         HAConfig
}

func NewServerRuntime(cfg config.ServerConfig) (*ServerRuntime, error) {
	return NewServerRuntimeWithDeps(cfg, ServerRuntimeDeps{})
}

func newServerRuntime(cfg config.ServerConfig, deps ServerRuntimeDeps) (*ServerRuntime, error) {
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
	return &ServerRuntime{
		cfg: cfg, done: make(chan struct{}), shutdown: make(chan struct{}),
		ha: ha, store: store, run: RunServer, legacy: true,
	}, nil
}

// ServerRuntimeDeps allows embedders and tests to inject the serving loop.
// When Run is nil, the legacy CLI runner is used for backwards compatibility.
type ServerRuntimeDeps struct {
	Run func([]string)
	// RunContext is the preferred embeddable runner. It receives a runtime-owned
	// context which is cancelled by Shutdown. When set, it takes precedence over
	// Run and never touches the legacy process-global shutdown channel.
	RunContext   func(context.Context, []string) error
	Store        RuntimeStateStore
	StoreFactory StoreFactoryDeps
}

func NewServerRuntimeWithDeps(cfg config.ServerConfig, deps ServerRuntimeDeps) (*ServerRuntime, error) {
	r, err := newServerRuntime(cfg, deps)
	if err != nil {
		return nil, err
	}
	r.run = deps.Run
	r.runContext = deps.RunContext
	if deps.Run != nil || deps.RunContext != nil {
		r.legacy = false
	}
	if r.run == nil && r.runContext == nil {
		r.run = RunServer
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
	if r.start {
		r.mu.Unlock()
		return fmt.Errorf("server runtime already started")
	}
	if r.legacy && !acquireLegacyServerRuntime(r) {
		r.mu.Unlock()
		return fmt.Errorf("another legacy server runtime is already active")
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
			if r.legacy {
				releaseLegacyServerRuntime(r)
			}
			close(r.done)
		}()
		if r.runContext != nil {
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
// Calling it before Start is a no-op and cannot affect another runtime.
func (r *ServerRuntime) ShutdownContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("server shutdown context is nil")
	}
	r.mu.Lock()
	if !r.start {
		r.mu.Unlock()
		return nil
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
	a := []string{"--domain", c.Domain, "--port", strconv.Itoa(c.ProxyPort)}
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
