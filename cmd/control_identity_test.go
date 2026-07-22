package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"xrok/cmd/config"
	"xrok/cmd/p2p"
)

func TestControlIdentityScopesAndClientBinding(t *testing.T) {
	oldPolicy, oldEnabled, oldToken := serverClientIdentityPolicy, serverAuthEnabled, serverApiToken
	t.Cleanup(func() {
		serverClientIdentityPolicy, serverAuthEnabled, serverApiToken = oldPolicy, oldEnabled, oldToken
	})
	serverClientIdentityPolicy = newControlIdentityPolicy([]config.ClientIdentityConfig{{ID: "alice", Token: "alice-secret", Scopes: []string{"client", "p2p:signal"}}})
	serverAuthEnabled, serverApiToken = false, ""

	called := false
	h := serverAuthScopeMiddleware(scopeP2PSignal, func(w http.ResponseWriter, r *http.Request) {
		id, ok := requirePrincipalClient(w, r, r.URL.Query().Get("client_id"))
		called = ok && id == "alice"
	})
	req := httptest.NewRequest(http.MethodGet, "/?client_id=alice", nil)
	req.Header.Set("X-API-Token", "alice-secret")
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusOK || !called {
		t.Fatalf("valid scoped identity rejected: code=%d", w.Code)
	}

	called = false
	req = httptest.NewRequest(http.MethodGet, "/?client_id=bob", nil)
	req.Header.Set("X-API-Token", "alice-secret")
	w = httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("cross-client impersonation accepted: code=%d", w.Code)
	}
}

func TestRegistrationBindsP2PIdentityToAuthenticatedClient(t *testing.T) {
	info := &p2p.PeerInfo{}
	if err := bindRegistrationP2PIdentity("alice", info); err != nil || info.PeerID != "alice" {
		t.Fatalf("empty P2P identity was not normalized: info=%#v err=%v", info, err)
	}
	poisoned := &p2p.PeerInfo{PeerID: "bob"}
	if err := bindRegistrationP2PIdentity("alice", poisoned); err == nil {
		t.Fatal("cross-client P2P identity poisoning accepted")
	}
}

func TestControlIdentityRejectsMissingScopeAndWrongToken(t *testing.T) {
	oldPolicy, oldEnabled, oldToken := serverClientIdentityPolicy, serverAuthEnabled, serverApiToken
	t.Cleanup(func() {
		serverClientIdentityPolicy, serverAuthEnabled, serverApiToken = oldPolicy, oldEnabled, oldToken
	})
	serverClientIdentityPolicy = newControlIdentityPolicy([]config.ClientIdentityConfig{{ID: "alice", Token: "alice-secret", Scopes: []string{"client"}}})
	serverAuthEnabled, serverApiToken = false, ""
	h := serverAuthScopeMiddleware(scopeP2PAdmin, func(http.ResponseWriter, *http.Request) { t.Fatal("unauthorized handler called") })
	for name, token := range map[string]string{"wrong token": "wrong", "missing scope": "alice-secret"} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h(w, req)
			want := http.StatusUnauthorized
			if name == "missing scope" {
				want = http.StatusForbidden
			}
			if w.Code != want {
				t.Fatalf("code=%d want=%d", w.Code, want)
			}
		})
	}
}

func TestLegacyAPITokenRetainsAdminCompatibility(t *testing.T) {
	oldPolicy, oldEnabled, oldToken := serverClientIdentityPolicy, serverAuthEnabled, serverApiToken
	t.Cleanup(func() {
		serverClientIdentityPolicy, serverAuthEnabled, serverApiToken = oldPolicy, oldEnabled, oldToken
	})
	serverClientIdentityPolicy = newControlIdentityPolicy(nil)
	serverAuthEnabled, serverApiToken = true, "legacy-admin"
	called := false
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("X-API-Token", "legacy-admin")
	w := httptest.NewRecorder()
	serverAuthMiddleware(func(http.ResponseWriter, *http.Request) { called = true })(w, req)
	if w.Code != http.StatusOK || !called {
		t.Fatalf("legacy administrator token rejected: code=%d", w.Code)
	}
}
