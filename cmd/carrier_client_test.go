package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestStdioStandardCarriersPreserveHalfClose(t *testing.T) {
	old := serverAllowPrivateEgress
	serverAllowPrivateEgress = true
	defer func() { serverAllowPrivateEgress = old }()
	for _, mode := range []string{"auto", "http2", "wss"} {
		t.Run(mode, func(t *testing.T) {
			target, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			backendDone := make(chan struct{})
			go func() {
				defer close(backendDone)
				c, err := target.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				b, err := io.ReadAll(c)
				if err == nil {
					c.Write(append([]byte("reply:"), b...))
				}
			}()
			ts, info, done := standardCarrierFixture(t)
			state := carrierState(t, ts, info)
			state.Transport = mode
			state.StdioMode = true
			state.StdioTarget = target.Addr().String()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, cleanup, err := dialStdioStream(ctx, state)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			input := bytes.Repeat([]byte("stdio-payload"), 20000)
			var output bytes.Buffer
			if err := bridgeStdio(ctx, stream, bytes.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(output.Bytes(), append([]byte("reply:"), input...)) {
				t.Fatalf("stdio response truncated: %d bytes", output.Len())
			}
			cleanup()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("carrier did not close")
			}
			<-backendDone
		})
	}
}

func TestAutoRequiresTCPForClientInitiatedStreams(t *testing.T) {
	for _, s := range []*clientState{{HTTPProxy: "http://proxy:8080"}, {StdioMode: true}, {UseProxy: true}, {Socks5Addr: "127.0.0.1:1080"}, {UDPTunnels: []UDPTunnelConfig{{}}}} {
		if !s.requiresTCPCarrier() {
			t.Fatalf("TCP requirement missing: %+v", s)
		}
		choice, err := selectAutoCarrier(context.Background(), []string{"quic", "http2", "wss"}, s.requiresTCPCarrier(), func(_ context.Context, mode string) (selectedCarrier, error) {
			if mode == "quic" {
				t.Fatal("QUIC bypassed required TCP carrier")
			}
			return selectedCarrier{}, nil
		}, nil)
		if err != nil || choice.Mode != "http2" {
			t.Fatal(choice.Mode, err)
		}
	}
	if (&clientState{}).requiresTCPCarrier() {
		t.Fatal("ordinary ingress should allow QUIC")
	}
}
