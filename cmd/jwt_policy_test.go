package cmd

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func testRS256Token(t *testing.T, key *rsa.PrivateKey, kid, issuer, audience string, exp time.Time) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid})
	claims, _ := json.Marshal(map[string]interface{}{"iss": issuer, "aud": audience, "exp": exp.Unix()})
	signingInput := enc(header) + "." + enc(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + enc(sig)
}

func TestOIDCVerifierRejectsMalformedAndEmptyIssuer(t *testing.T) {
	v := NewOIDCVerifier("", "api", nil, 0)
	if err := v.Verify(context.Background(), "a.b.c", time.Now()); err == nil {
		t.Fatal("expected malformed token rejection")
	}
	v = NewOIDCVerifier("http://127.0.0.1", "api", nil, 0)
	if err := v.Verify(context.Background(), "eyJhbGciOiJSUzI1NiIsImtpZCI6Im1pc3MifQ.eA.eA", time.Now()); err == nil {
		t.Fatal("expected invalid signature rejection")
	}
}

func testJWT(secret string, exp int64, aud string) string {
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	head := enc([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := enc([]byte(fmt.Sprintf(`{"exp":%d,"aud":"%s"}`, exp, aud)))
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(head + "." + body))
	return head + "." + body + "." + enc(m.Sum(nil))
}
func TestVerifyJWTCapability(t *testing.T) {
	now := time.Unix(1700000000, 0)
	tok := testJWT("s3cret", now.Add(time.Hour).Unix(), "api")
	if !verifyJWTCapability(tok, "s3cret", "api", now) {
		t.Fatal("valid token rejected")
	}
	if verifyJWTCapability(tok, "s3cret", "other", now) {
		t.Fatal("audience bypass")
	}
	if verifyJWTCapability(testJWT("s3cret", now.Add(-time.Second).Unix(), "api"), "s3cret", "api", now) {
		t.Fatal("expired token accepted")
	}
	if verifyJWTCapability(tok, "wrong", "api", now) {
		t.Fatal("bad signature accepted")
	}
	if verifyJWTCapability(strings.Repeat("a", maxJWTSize+1), "s3cret", "api", now) {
		t.Fatal("oversized token accepted")
	}
}

func TestOIDCVerifierValidatesDiscoveryAndThrottlesUnknownKID(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const issuer = "https://issuer.test"
	exponent := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes())
	modulus := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	jwks := fmt.Sprintf(`{"keys":[{"kid":"current","kty":"RSA","use":"sig","alg":"RS256","n":%q,"e":%q}]}`, modulus, exponent)
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		switch r.URL.String() {
		case issuer + "/.well-known/openid-configuration":
			return jsonResponse(`{"issuer":"https://issuer.test","jwks_uri":"https://issuer.test/keys"}`), nil
		case issuer + "/keys":
			return jsonResponse(jwks), nil
		default:
			return nil, fmt.Errorf("unexpected URL %s", r.URL)
		}
	})}
	v := NewOIDCVerifier(issuer, "api", client, time.Minute)
	now := time.Now()
	if err := v.Verify(context.Background(), testRS256Token(t, key, "current", issuer, "api", now.Add(time.Hour)), now); err != nil {
		t.Fatalf("valid OIDC token rejected: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("initial discovery calls=%d, want 2", got)
	}
	unknown := testRS256Token(t, key, "attacker-controlled", issuer, "api", now.Add(time.Hour))
	for i := 0; i < 20; i++ {
		if err := v.Verify(context.Background(), unknown, now); err == nil {
			t.Fatal("unknown kid accepted")
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("unknown kid caused outbound refresh storm: calls=%d", got)
	}
}

func TestOIDCVerifierRejectsDiscoveryIssuerMismatch(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(`{"issuer":"https://evil.test","jwks_uri":"https://issuer.test/keys"}`), nil
	})}
	v := NewOIDCVerifier("https://issuer.test", "api", client, time.Minute)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	token := testRS256Token(t, key, "current", "https://issuer.test", "api", time.Now().Add(time.Hour))
	if err := v.Verify(context.Background(), token, time.Now()); err == nil || !strings.Contains(err.Error(), "issuer mismatch") {
		t.Fatalf("discovery issuer mismatch not rejected: %v", err)
	}
}

func TestOIDCVerifierRejectsOversizedDiscovery(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(strings.Repeat(" ", maxOIDCBodySize+1)), nil
	})}
	v := NewOIDCVerifier("https://issuer.test", "api", client, time.Minute)
	if err := v.refresh(context.Background(), "kid"); err == nil {
		t.Fatal("oversized discovery response accepted")
	}
}
