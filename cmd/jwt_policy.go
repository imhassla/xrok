package cmd

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// verifyJWTCapability validates a compact HS256 JWT. The configured secret is
// supplied per tunnel; audience is optional but, when set, must match aud.
func verifyJWTCapability(token, secret, audience string, now time.Time) bool {
	p := strings.Split(token, ".")
	if len(p) != 3 || secret == "" {
		return false
	}
	dec := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
	header, err := dec(p[0])
	if err != nil {
		return false
	}
	var h struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if json.Unmarshal(header, &h) != nil || h.Alg != "HS256" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(p[0] + "." + p[1]))
	sig, err := dec(p[2])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	payload, err := dec(p[1])
	if err != nil {
		return false
	}
	var c struct {
		Exp int64       `json:"exp"`
		Nbf int64       `json:"nbf"`
		Aud interface{} `json:"aud"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return false
	}
	ts := now.Unix()
	if c.Exp == 0 || ts >= c.Exp || (c.Nbf > 0 && ts < c.Nbf) {
		return false
	}
	if audience != "" {
		switch a := c.Aud.(type) {
		case string:
			if a != audience {
				return false
			}
		case []interface{}:
			ok := false
			for _, v := range a {
				if v == audience {
					ok = true
				}
			}
			if !ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

var errInvalidJWT = errors.New("invalid jwt capability")

// OIDCVerifier validates RS256 tokens using the issuer's discovery document and
// JWKS. Keys are cached and transparently refreshed when an unknown kid appears.
type OIDCVerifier struct {
	issuer, audience string
	client           *http.Client
	ttl              time.Duration
	mu               sync.RWMutex
	keys             map[string]*rsa.PublicKey
	fetched          time.Time
	jwksURL          string
}

func NewOIDCVerifier(issuer, audience string, client *http.Client, ttl time.Duration) *OIDCVerifier {
	if client == nil {
		client = http.DefaultClient
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &OIDCVerifier{issuer: strings.TrimRight(issuer, "/"), audience: audience, client: client, ttl: ttl, keys: make(map[string]*rsa.PublicKey)}
}
func (v *OIDCVerifier) refresh(ctx context.Context) error {
	if v.issuer == "" {
		return errors.New("oidc issuer is empty")
	}
	dreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, v.issuer+"/.well-known/openid-configuration", nil)
	resp, err := v.client.Do(dreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("oidc discovery: %s", resp.Status)
	}
	var d struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&d); err != nil || d.JWKSURI == "" {
		return errors.New("oidc discovery missing jwks_uri")
	}
	jreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, d.JWKSURI, nil)
	jr, err := v.client.Do(jreq)
	if err != nil {
		return err
	}
	defer jr.Body.Close()
	if jr.StatusCode/100 != 2 {
		return fmt.Errorf("jwks: %s", jr.Status)
	}
	b, err := io.ReadAll(jr.Body)
	if err != nil {
		return err
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if json.Unmarshal(b, &set) != nil {
		return errors.New("invalid jwks")
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, e1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, e2 := base64.RawURLEncoding.DecodeString(k.E)
		if e1 != nil || e2 != nil {
			continue
		}
		ei := 0
		for _, x := range eb {
			ei = ei*256 + int(x)
		}
		if ei == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: ei}
	}
	if len(keys) == 0 {
		return errors.New("jwks has no rsa keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.fetched = time.Now()
	v.jwksURL = d.JWKSURI
	v.mu.Unlock()
	return nil
}
func (v *OIDCVerifier) Verify(ctx context.Context, token string, now time.Time) error {
	p := strings.Split(token, ".")
	if len(p) != 3 {
		return errInvalidJWT
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	dec := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
	hb, e := dec(p[0])
	if e != nil || json.Unmarshal(hb, &h) != nil || h.Alg != "RS256" || h.Kid == "" {
		return errInvalidJWT
	}
	v.mu.RLock()
	key := v.keys[h.Kid]
	stale := time.Since(v.fetched) > v.ttl
	v.mu.RUnlock()
	if key == nil || stale {
		if err := v.refresh(ctx); err != nil {
			return err
		}
		v.mu.RLock()
		key = v.keys[h.Kid]
		v.mu.RUnlock()
	}
	if key == nil {
		return errInvalidJWT
	}
	sig, e := dec(p[2])
	if e != nil {
		return errInvalidJWT
	}
	digest := sha256.Sum256([]byte(p[0] + "." + p[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return errInvalidJWT
	}
	var c struct {
		Exp int64       `json:"exp"`
		Nbf int64       `json:"nbf"`
		Iss string      `json:"iss"`
		Aud interface{} `json:"aud"`
	}
	pb, e := dec(p[1])
	if e != nil || json.Unmarshal(pb, &c) != nil {
		return errInvalidJWT
	}
	ts := now.Unix()
	if c.Exp == 0 || ts >= c.Exp || (c.Nbf > 0 && ts < c.Nbf) || c.Iss != v.issuer {
		return errInvalidJWT
	}
	if v.audience != "" {
		ok := false
		switch a := c.Aud.(type) {
		case string:
			ok = subtle.ConstantTimeCompare([]byte(a), []byte(v.audience)) == 1
		case []interface{}:
			for _, x := range a {
				if s, yes := x.(string); yes && s == v.audience {
					ok = true
				}
			}
		}
		if !ok {
			return errInvalidJWT
		}
	}
	return nil
}
