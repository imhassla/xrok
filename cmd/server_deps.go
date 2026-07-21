package cmd

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"
)

// ServerDeps contains the mutable services a server handler needs.  New
// handlers should receive this value (directly or through ServerContext)
// instead of reaching for package globals.  The legacy CLI is bridged by
// serverDepsFromGlobals while it is being migrated incrementally.
type ServerDeps struct {
	Clients   *ClientRegistry
	Routes    *RouteRegistry
	Logger    *log.Logger
	Domain    string
	StartTime time.Time
}

type ClientRegistry struct {
	mu sync.RWMutex
	m  map[string]*serverClientInfo
}

func (r *ClientRegistry) Snapshot() map[string]*serverClientInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*serverClientInfo, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}

func NewClientRegistry() *ClientRegistry {
	return &ClientRegistry{m: make(map[string]*serverClientInfo)}
}
func (r *ClientRegistry) Get(id string) (*serverClientInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.m[id]
	return c, ok
}
func (r *ClientRegistry) Put(id string, c *serverClientInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[id] = c
}
func (r *ClientRegistry) Delete(id string) { r.mu.Lock(); defer r.mu.Unlock(); delete(r.m, id) }
func (r *ClientRegistry) Len() int         { r.mu.RLock(); defer r.mu.RUnlock(); return len(r.m) }

type RouteRegistry struct {
	mu sync.RWMutex
	m  map[string]*TunnelRoute
}

func NewRouteRegistry() *RouteRegistry { return &RouteRegistry{m: make(map[string]*TunnelRoute)} }
func (r *RouteRegistry) Get(name string) (*TunnelRoute, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.m[name]
	return v, ok
}
func (r *RouteRegistry) Put(name string, v *TunnelRoute) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[name] = v
}
func (r *RouteRegistry) Delete(name string) { r.mu.Lock(); defer r.mu.Unlock(); delete(r.m, name) }
func (r *RouteRegistry) Snapshot() []*TunnelRoute {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*TunnelRoute, 0, len(r.m))
	for _, v := range r.m {
		out = append(out, v)
	}
	return out
}

type serverDepsContextKey struct{}

func WithServerDeps(ctx context.Context, deps *ServerDeps) context.Context {
	return context.WithValue(ctx, serverDepsContextKey{}, deps)
}

func ServerDepsFromContext(ctx context.Context) (*ServerDeps, bool) {
	deps, ok := ctx.Value(serverDepsContextKey{}).(*ServerDeps)
	return deps, ok
}

func serverDepsFromRequest(r *http.Request) *ServerDeps {
	if d, ok := ServerDepsFromContext(r.Context()); ok && d != nil && d.Clients != nil && d.Routes != nil {
		// Domain and StartTime are optional for embedded runtimes; preserve the
		// legacy defaults when callers only inject registries.
		if d.Domain == "" || d.StartTime.IsZero() {
			copy := *d
			if copy.Domain == "" {
				copy.Domain = serverDomain
			}
			if copy.StartTime.IsZero() {
				copy.StartTime = serverStartTime
			}
			return &copy
		}
		return d
	}
	return serverDepsFromGlobals()
}

// serverDepsFromGlobals is a compatibility bridge for legacy CLI handlers.
func serverDepsFromGlobals() *ServerDeps {
	return &ServerDeps{Clients: &ClientRegistry{m: serverClients}, Routes: &RouteRegistry{m: serverTunnelRoutes}, Logger: log.Default(), Domain: serverDomain, StartTime: serverStartTime}
}
