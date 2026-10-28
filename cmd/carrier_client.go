package cmd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/hashicorp/yamux"
)

// Client-initiated egress currently uses yamux; native QUIC serves ingress.
func (s *clientState) requiresTCPCarrier() bool {
	return s.HTTPProxy != "" || s.StdioMode || s.UseProxy || s.Socks5Addr != "" || len(s.UDPTunnels) != 0
}

func connectMuxWithDisconnect(ctx context.Context, state *clientState, disconnected chan<- struct{}) {
	defer func() {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	}()
	carrier, err := state.dialStandardCarrier(ctx, state.Transport)
	if err != nil {
		logCarrierError(state, err)
		return
	}
	runClientMuxCarrier(ctx, state, carrier)
}

func connectQUICWithDisconnect(ctx context.Context, state *clientState, disconnected chan<- struct{}) {
	defer func() {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	}()
	if state.requiresTCPCarrier() {
		logCarrierError(state, fmt.Errorf("configured proxy or client-initiated streams require HTTP/2 or WSS"))
		return
	}
	qc, err := state.dialNativeQUIC(ctx)
	if err != nil {
		logCarrierError(state, err)
		return
	}
	defer qc.Close()
	clientStateMutex.Lock()
	state.QUICConn = qc
	clientStateMutex.Unlock()
	defer func() {
		clientStateMutex.Lock()
		if state.QUICConn == qc {
			state.QUICConn = nil
		}
		clientStateMutex.Unlock()
	}()
	for {
		stream, err := qc.AcceptStream(ctx)
		if err != nil {
			return
		}
		go handleQUICStream(ctx, stream, state)
	}
}

func dialStdioStream(ctx context.Context, s *clientState) (net.Conn, func(), error) {
	if len(s.StdioTarget) == 0 || len(s.StdioTarget) > 255 {
		return nil, nil, fmt.Errorf("invalid stdio target length")
	}
	var carrier net.Conn
	var err error
	if s.Transport == "auto" {
		choice, e := selectAutoCarrier(ctx, s.SupportedTransports, true, func(ctx context.Context, mode string) (selectedCarrier, error) {
			c, e := s.dialStandardCarrier(ctx, mode)
			return selectedCarrier{TCP: c}, e
		}, nil)
		carrier, err = choice.TCP, e
	} else if s.Transport == "http2" || s.Transport == "wss" {
		carrier, err = s.dialStandardCarrier(ctx, s.Transport)
	} else {
		err = fmt.Errorf("stdio requires auto, http2 or wss")
	}
	if err != nil {
		return nil, nil, err
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = os.Stderr
	session, err := yamux.Client(carrier, cfg)
	if err != nil {
		carrier.Close()
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { session.Close() })
	cleanup := func() { stop(); session.Close(); carrier.Close() }
	stream, err := session.Open()
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	header := append([]byte{0xFF, byte(len(s.StdioTarget))}, []byte(s.StdioTarget)...)
	if _, err = stream.Write(header); err != nil {
		cleanup()
		return nil, nil, err
	}
	return stream, cleanup, nil
}

func bridgeStdio(ctx context.Context, stream net.Conn, input io.Reader, output io.Writer) error {
	done := make(chan error, 2)
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	go func() {
		_, err := io.Copy(stream, input)
		if err != nil {
			stream.Close()
			done <- err
			return
		}
		closeStreamWrite(stream)
	}()
	go func() { _, err := io.Copy(output, stream); done <- err }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runStdioMode(state *clientState, token string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !registerClient(ctx, state, token) {
		return fmt.Errorf("registration failed")
	}
	stream, cleanup, err := dialStdioStream(ctx, state)
	if err != nil {
		return err
	}
	defer cleanup()
	return bridgeStdio(ctx, stream, os.Stdin, os.Stdout)
}
