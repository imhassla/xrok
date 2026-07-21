package cmd

import (
	"net/http"
	"testing"
	"time"
)

func TestServerDepsIsolatedRegistries(t *testing.T) {
	a := NewServerDeps("a.example", time.Now(), nil)
	b := NewServerDeps("b.example", time.Now(), nil)
	a.Clients.Put("a", &serverClientInfo{})
	if _, ok := b.Clients.Get("a"); ok {
		t.Fatal("server dependency registries must be isolated")
	}
}

func TestServerDepsInjectionAvoidsLegacyFallback(t *testing.T) {
	before := LegacyServerDepsFallbacks()
	deps := NewServerDeps("test.example", time.Now(), nil)
	r := httptestWithDeps(deps)
	if got := serverDepsFromRequest(r); got != deps {
		t.Fatal("request-scoped dependencies were not returned")
	}
	if got := LegacyServerDepsFallbacks(); got != before {
		t.Fatalf("injected handler incremented legacy fallback counter: %d -> %d", before, got)
	}
}

func httptestWithDeps(deps *ServerDeps) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://example.test", nil)
	return r.WithContext(WithServerDeps(r.Context(), deps))
}
