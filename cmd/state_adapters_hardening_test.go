package cmd

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRESPRejectsOversizedBulkBeforeAllocation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		_, _ = io.CopyN(io.Discard, server, 22)
		_, _ = io.WriteString(server, "$999999999\r\n")
	}()
	_, err := resp(client, bufio.NewReader(client), "GET", "key")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err=%v, want bounded bulk response error", err)
	}
}

func TestRESPRejectsMalformedLengthAndTerminator(t *testing.T) {
	tests := []struct {
		name, response, contains string
	}{
		{"length", "$nope\r\n", "invalid redis bulk length"},
		{"terminator", "$3\r\nabcXX", "invalid redis bulk terminator"},
		{"type", "?wat\r\n", "unsupported redis response type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			go func() {
				_, _ = io.CopyN(io.Discard, server, 22)
				_, _ = io.WriteString(server, tt.response)
			}()
			_, err := resp(client, bufio.NewReader(client), "GET", "key")
			if err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("err=%v, want %q", err, tt.contains)
			}
		})
	}
}

func TestRedisCommandAppliesTimeoutToResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable: %v", err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
	}()

	store := NewRedisStateStore(listener.Addr().String())
	store.Timeout = 40 * time.Millisecond
	started := time.Now()
	_, _, err = store.Load(context.Background(), "blocked")
	if err == nil {
		t.Fatal("expected response deadline error")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("response timeout took %s", elapsed)
	}
	select {
	case <-accepted:
	default:
		t.Fatal("test server did not accept connection")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("err=%T %v, want network timeout", err, err)
	}
}

func TestRedisLoadRejectsEmptyID(t *testing.T) {
	store := NewRedisStateStore("127.0.0.1:1")
	if _, _, err := store.Load(context.Background(), ""); err == nil {
		t.Fatal("expected empty id validation error")
	}
}
