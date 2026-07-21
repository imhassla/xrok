package cmd

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

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
}
