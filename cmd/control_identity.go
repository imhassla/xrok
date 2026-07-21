package cmd

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"xrok/cmd/config"
)

const (
	scopeAdmin        = "admin"
	scopeClient       = "client"
	scopeP2P          = "p2p"
	scopeP2PRead      = "p2p:read"
	scopeP2PAdmin     = "p2p:admin"
	scopeP2PSignal    = "p2p:signal"
	legacyPrincipalID = "*"
)

type controlPrincipal struct {
	ClientID string
	Scopes   map[string]struct{}
	Legacy   bool
}

func (p controlPrincipal) allows(scope string) bool {
	if p.Legacy {
		return true
	}
	if _, ok := p.Scopes["*"]; ok {
		return true
	}
	if _, ok := p.Scopes[scope]; ok {
		return true
	}
	if i := strings.IndexByte(scope, ':'); i > 0 {
		_, ok := p.Scopes[scope[:i]]
		return ok
	}
	return false
}

type controlIdentityPolicy struct {
	credentials []controlCredential
}

type controlCredential struct {
	token     string
	principal controlPrincipal
}

func newControlIdentityPolicy(configs []config.ClientIdentityConfig) *controlIdentityPolicy {
	p := &controlIdentityPolicy{credentials: make([]controlCredential, 0, len(configs))}
	for _, identity := range configs {
		scopes := make(map[string]struct{}, len(identity.Scopes))
		for _, scope := range identity.Scopes {
			scopes[scope] = struct{}{}
		}
		// An omitted scope list is intentionally least-privilege: the identity
		// can establish its own client session but cannot use P2P endpoints.
		if len(scopes) == 0 {
			scopes[scopeClient] = struct{}{}
		}
		p.credentials = append(p.credentials, controlCredential{
			token:     identity.Token,
			principal: controlPrincipal{ClientID: identity.ID, Scopes: scopes},
		})
	}
	return p
}

func (p *controlIdentityPolicy) authenticate(token string) (controlPrincipal, bool) {
	if p == nil || token == "" {
		return controlPrincipal{}, false
	}
	// Scan all credentials to avoid revealing the position of a matching ID.
	match := -1
	for i := range p.credentials {
		if subtle.ConstantTimeCompare([]byte(token), []byte(p.credentials[i].token)) == 1 {
			match = i
		}
	}
	if match < 0 {
		return controlPrincipal{}, false
	}
	return p.credentials[match].principal, true
}

type controlPrincipalContextKey struct{}

func withControlPrincipal(ctx context.Context, p controlPrincipal) context.Context {
	return context.WithValue(ctx, controlPrincipalContextKey{}, p)
}

func controlPrincipalFromRequest(r *http.Request) (controlPrincipal, bool) {
	p, ok := r.Context().Value(controlPrincipalContextKey{}).(controlPrincipal)
	return p, ok
}

func requestControlToken(r *http.Request) string {
	if token := r.Header.Get("X-API-Token"); token != "" {
		return token
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

func authenticateControlRequest(r *http.Request) (controlPrincipal, bool) {
	token := requestControlToken(r)
	if principal, ok := serverClientIdentityPolicy.authenticate(token); ok {
		return principal, true
	}
	if serverAuthEnabled && subtle.ConstantTimeCompare([]byte(token), []byte(serverApiToken)) == 1 {
		return controlPrincipal{ClientID: legacyPrincipalID, Legacy: true}, true
	}
	if !serverAuthEnabled && len(serverClientIdentityPolicy.credentials) == 0 {
		return controlPrincipal{ClientID: legacyPrincipalID, Legacy: true}, true
	}
	return controlPrincipal{}, false
}

func serverAuthScopeMiddleware(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := authenticateControlRequest(r)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if !principal.allows(scope) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(withControlPrincipal(r.Context(), principal)))
	}
}

func requirePrincipalClient(w http.ResponseWriter, r *http.Request, requested string) (string, bool) {
	principal, ok := controlPrincipalFromRequest(r)
	if !ok || principal.Legacy {
		return requested, true
	}
	if requested == "" {
		return principal.ClientID, true
	}
	if requested != principal.ClientID {
		http.Error(w, "credential is not valid for requested client", http.StatusForbidden)
		return "", false
	}
	return requested, true
}
