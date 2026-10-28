package cmd

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"xrok/cmd/transport"
)

func TestAutoTransportOrderProxyPolicyAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		proxy    bool
		fails    int
		want     string
		attempts string
	}{{false, 0, "quic", "quic"}, {false, 1, "http2", "quic,http2"}, {false, 2, "wss", "quic,http2,wss"}, {true, 0, "http2", "http2"}, {true, 1, "wss", "http2,wss"}} {
		var attempted []string
		choice, err := selectAutoCarrier(context.Background(), []string{"quic", "http2", "wss"}, tc.proxy, func(ctx context.Context, mode string) (selectedCarrier, error) {
			attempted = append(attempted, mode)
			if len(attempted) <= tc.fails {
				return selectedCarrier{}, fmt.Errorf("blocked")
			}
			return selectedCarrier{}, nil
		}, nil)
		if err != nil || choice.Mode != tc.want || strings.Join(attempted, ",") != tc.attempts {
			t.Fatalf("selection=%+v attempts=%v err=%v", choice, attempted, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if _, err := selectAutoCarrier(ctx, []string{"quic"}, false, func(context.Context, string) (selectedCarrier, error) { called = true; return selectedCarrier{}, nil }, nil); err == nil || called {
		t.Fatal("canceled selection dialed")
	}
	if _, err := selectAutoCarrier(context.Background(), nil, false, nil, nil); err == nil {
		t.Fatal("old server silently downgraded")
	}
}

func TestAutoActualHTTP2ToWSSFallback(t *testing.T) {
	ts, info, done := standardCarrierFixture(t)
	state := carrierState(t, ts, info)
	var order []string
	choice, err := selectAutoCarrier(context.Background(), []string{"quic", "http2", "wss"}, false, func(ctx context.Context, mode string) (selectedCarrier, error) {
		order = append(order, mode)
		if mode != "wss" {
			return selectedCarrier{}, fmt.Errorf("injected transport failure")
		}
		conn, err := state.dialStandardCarrier(ctx, mode)
		return selectedCarrier{TCP: conn}, err
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if choice.Mode != "wss" || strings.Join(order, ",") != "quic,http2,wss" {
		t.Fatalf("wrong fallback: %v", order)
	}
	choice.TCP.Close()
	<-done
}

func TestQUICAssociationVersionCompatibilityAndFragmentation(t *testing.T) {
	for _, tc := range []struct {
		first, remaining, id, token string
		v2                          bool
	}{{"XROK:client:secret", "", "client", "secret", false}, {"XROK2:client:secret\n", "", "client", "secret", true}, {"X", "ROK2:client:secret\n", "client", "secret", true}, {"XROK2:client:", "secret\n", "client", "secret", true}} {
		id, token, v2, err := parseQUICAssociation([]byte(tc.first), bufio.NewReader(strings.NewReader(tc.remaining)))
		if err != nil || id != tc.id || token != tc.token || v2 != tc.v2 {
			t.Fatalf("parse %q: %q %q %v %v", tc.first, id, token, v2, err)
		}
	}
	for _, bad := range []string{"bad", "XROK2:a:b", "XROK2:a:", "XROK2:" + strings.Repeat("a", 1100)} {
		if _, _, _, err := parseQUICAssociation([]byte(bad), bufio.NewReader(strings.NewReader(""))); err == nil {
			t.Fatalf("accepted invalid association %q", bad)
		}
	}
	_, _, _, err := parseQUICAssociation([]byte("X"), bufio.NewReader(strings.NewReader("")))
	if err != io.EOF {
		t.Fatal(err)
	}
}

func TestAutoRealBlockedUDPToHTTP2AndCarrierLifetime(t *testing.T) {
	ts, info, done := standardCarrierFixture(t)
	state := carrierState(t, ts, info)
	state.ConnectTimeout = 150 * time.Millisecond
	var tried []string
	choice, err := selectAutoCarrier(context.Background(), []string{"quic", "http2", "wss"}, false, func(ctx context.Context, mode string) (selectedCarrier, error) {
		tried = append(tried, mode)
		if mode == "quic" {
			conn, err := state.dialNativeQUIC(ctx)
			return selectedCarrier{QUIC: conn}, err
		}
		conn, err := state.dialStandardCarrier(ctx, mode)
		return selectedCarrier{TCP: conn}, err
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if choice.Mode != "http2" || strings.Join(tried, ",") != "quic,http2" {
		t.Fatal(choice.Mode, tried)
	}
	// The handshake deadline must not kill a carrier after it succeeds.
	time.Sleep(200 * time.Millisecond)
	exerciseMuxCarrier(t, choice.TCP, info)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP/2 fallback did not close")
	}
}

func TestAutoNativeQUICAcknowledgedAssociation(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			ts, info, _ := standardCarrierFixture(t)
			state := carrierState(t, ts, info)
			if !valid {
				state.SessionToken = "wrong"
			}
			cfg := ts.TLS.Clone()
			cfg.NextProtos = []string{"xrok-quic"}
			cfg.MinVersion = tls.VersionTLS13
			ln, err := transport.NewQUICTransport(cfg).Listen(ts.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				serverQUICPendingSlots <- struct{}{}
				handleQUICServerConnection(ctx, conn.(*transport.QUICConn))
			}()
			conn, err := state.dialNativeQUIC(ctx)
			if !valid {
				if err == nil {
					conn.Close()
					t.Fatal("wrong session token accepted")
				}
				<-done
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			info.connMutex.Lock()
			server := info.quicConn
			info.connMutex.Unlock()
			if server == nil {
				t.Fatal("ack arrived before attachment")
			}
			stream, err := server.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			echo := make(chan error, 1)
			go func() {
				peer, err := conn.AcceptStream(ctx)
				if err != nil {
					echo <- err
					return
				}
				b := make([]byte, 4)
				_, err = io.ReadFull(peer, b)
				if err == nil {
					_, err = peer.Write(b)
				}
				echo <- err
			}()
			_, err = stream.Write([]byte("ping"))
			if err != nil {
				t.Fatal(err)
			}
			body := make([]byte, 4)
			if _, err = io.ReadFull(stream, body); err != nil || string(body) != "ping" {
				t.Fatal(err, string(body))
			}
			if err = <-echo; err != nil {
				t.Fatal(err)
			}
			conn.Close()
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("QUIC session did not close")
			}
		})
	}
}
