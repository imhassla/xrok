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
	cfg   config.ServerConfig
	mu    sync.Mutex
	start bool
	stop  sync.Once
	done  chan struct{}
	run   func([]string)
	store RuntimeStateStore
	drain DrainController
	ha    HAConfig
}

func NewServerRuntime(cfg config.ServerConfig) (*ServerRuntime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ha, err := LoadHAConfig(nil)
	if err != nil {
		return nil, err
	}
	return &ServerRuntime{cfg: cfg, done: make(chan struct{}), ha: ha}, nil
}

// ServerRuntimeDeps allows embedders and tests to inject the serving loop.
// When Run is nil, the legacy CLI runner is used for backwards compatibility.
type ServerRuntimeDeps struct {
	Run   func([]string)
	Store RuntimeStateStore
}

func NewServerRuntimeWithDeps(cfg config.ServerConfig, deps ServerRuntimeDeps) (*ServerRuntime, error) {
	r, err := NewServerRuntime(cfg)
	if err != nil {
		return nil, err
	}
	r.run = deps.Run
	r.store = deps.Store
	return r, nil
}

// Start launches the server asynchronously and returns when listeners are
// initialized by the legacy runner. Cancellation of ctx requests Shutdown.
func (r *ServerRuntime) Start(ctx context.Context) error {
	r.mu.Lock()
	if r.start {
		r.mu.Unlock()
		return fmt.Errorf("server runtime already started")
	}
	r.start = true
	// A fresh channel allows repeated runtimes in the same test process.
	serverShutdownChan = make(chan struct{})
	r.mu.Unlock()

	args := r.args()
	go func() {
		if r.run != nil {
			r.run(args)
		} else {
			RunServer(args)
		}
		close(r.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			r.Shutdown()
		case <-r.done:
		}
	}()
	return nil
}

func (r *ServerRuntime) Shutdown() {
	_ = r.drain.Drain(context.Background())
	r.stop.Do(func() {
		select {
		case <-serverShutdownChan:
		default:
			close(serverShutdownChan)
		}
	})
}

func (r *ServerRuntime) StateStore() RuntimeStateStore { return r.store }
func (r *ServerRuntime) HAConfig() HAConfig            { return r.ha }
func (r *ServerRuntime) AddDrainHook(h DrainHook)      { r.drain.Add(h) }

func (r *ServerRuntime) Wait() { <-r.done }

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
