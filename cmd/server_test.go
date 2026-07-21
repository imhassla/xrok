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
