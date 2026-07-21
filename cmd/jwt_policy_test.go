package cmd

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"
	"time"
)

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
