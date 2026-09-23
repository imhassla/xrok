package cmd

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestVerifyBasicAuth(t *testing.T) {
	credential := base64.StdEncoding.EncodeToString([]byte("user:secret"))
	request := []byte("GET / HTTP/1.1\r\nAuthorization: Basic " + credential + "\r\n\r\n")
	if !verifyBasicAuth(request, "user:secret") {
		t.Fatal("valid basic auth rejected")
	}
	if verifyBasicAuth(request, "user:other") {
		t.Fatal("invalid basic auth accepted")
	}
}

func TestInjectHeadersReplacesExistingValue(t *testing.T) {
	request := []byte("GET / HTTP/1.1\r\nX-Env: old\r\nHost: app.localhost\r\n\r\n")
	result := string(injectHeaders(request, map[string]string{"X-Env": "test", "X-New": "yes"}))
	if strings.Count(strings.ToLower(result), "x-env:") != 1 || !strings.Contains(result, "X-Env: test") {
		t.Fatalf("header not replaced safely:\n%s", result)
	}
	if !strings.Contains(result, "X-New: yes") {
		t.Fatalf("new header missing:\n%s", result)
	}
}

func TestInjectHeadersDoesNotRewriteBody(t *testing.T) {
	request := []byte("POST / HTTP/1.1\r\nX-Env: old\r\nHost: app.localhost\r\nContent-Length: 11\r\n\r\nX-Env: old")
	result := string(injectHeaders(request, map[string]string{"X-Env": "test"}))
	if !strings.HasSuffix(result, "\r\n\r\nX-Env: old") {
		t.Fatalf("body was modified:\n%s", result)
	}
}

func BenchmarkInjectHeaders(b *testing.B) {
	request := []byte("POST /benchmark HTTP/1.1\r\nHost: app.localhost\r\nContent-Length: 7\r\n\r\npayload")
	headers := map[string]string{"X-Trace-ID": "bench", "X-Environment": "test"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = injectHeaders(request, headers)
	}
}

func TestRestoredClientWithoutSessionTokenCannotAttach(t *testing.T) {
	restored := &serverClientInfo{}
	if restored.validSessionToken("") {
		t.Fatal("empty presented token matched a client restored without a session token")
	}
	live := &serverClientInfo{sessionToken: "s3cret"}
	if !live.validSessionToken("s3cret") || live.validSessionToken("") || live.validSessionToken("other") {
		t.Fatal("session token comparison is incorrect for a live client")
	}
}

func TestServerConfigPathAcceptsBothFlagForms(t *testing.T) {
	cases := map[string][]string{
		"/a.yaml": {"--config", "/a.yaml"},
		"/b.yaml": {"--config=/b.yaml"},
		"/c.yaml": {"--domain", "x", "-config=/c.yaml"},
		"/d.yaml": {"-config", "/d.yaml", "--debug"},
	}
	for want, args := range cases {
		if got, ok := serverConfigPath(args); !ok || got != want {
			t.Fatalf("serverConfigPath(%q) = %q, %v; want %q", args, got, ok, want)
		}
	}
	if _, ok := serverConfigPath([]string{"--", "--config=/e.yaml"}); ok {
		t.Fatal("config after the -- terminator must be ignored")
	}
}
