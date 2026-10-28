package cmd

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"

	"xrok/cmd/transport"
)

type selectedCarrier struct {
	Mode string
	TCP  net.Conn
	QUIC *transport.QUICConn
}

// Selection happens before application streams exist. Reconnecting selects a new
// carrier; it never replays application requests or migrates existing TCP streams.
func selectAutoCarrier(ctx context.Context, supported []string, requireTCP bool, dial func(context.Context, string) (selectedCarrier, error), report func(string, error)) (selectedCarrier, error) {
	var failures []error
	for _, mode := range []string{"quic", "http2", "wss"} {
		if err := ctx.Err(); err != nil {
			return selectedCarrier{}, err
		}
		if mode == "quic" && requireTCP {
			if report != nil {
				report(mode, fmt.Errorf("configured proxy or client-initiated streams require TCP carrier"))
			}
			continue
		}
		if !containsTransport(supported, mode) {
			continue
		}
		carrier, err := dial(ctx, mode)
		if err == nil {
			carrier.Mode = mode
			return carrier, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", mode, err))
		if report != nil {
			report(mode, err)
		}
	}
	if len(failures) == 0 {
		return selectedCarrier{}, fmt.Errorf("server advertises no permitted automatic transport")
	}
	return selectedCarrier{}, errors.Join(failures...)
}

func (s *clientState) dialNativeQUIC(ctx context.Context) (*transport.QUICConn, error) {
	if s.HTTPProxy != "" {
		return nil, fmt.Errorf("QUIC cannot bypass a configured TCP upstream proxy")
	}
	port := s.ServerPort
	if port == 0 {
		port = 443
	}
	addr := net.JoinHostPort(s.ServerAddr, fmt.Sprint(port))
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.TLSConfig != nil {
		cfg = s.TLSConfig.Clone()
	}
	cfg.NextProtos = []string{"xrok-quic"}
	handshake, cancel := context.WithTimeout(ctx, s.connectionBudget())
	defer cancel()
	conn, err := transport.NewQUICTransport(cfg).Dial(handshake, addr)
	if err != nil {
		return nil, err
	}
	qc := conn.(*transport.QUICConn)
	success := false
	defer func() {
		if !success {
			_ = qc.Close()
		}
	}()
	stop := context.AfterFunc(handshake, func() { _ = qc.Close() })
	defer stop()
	stream, err := qc.Connection().OpenStreamSync(handshake)
	if err != nil {
		return nil, err
	}
	if d, ok := handshake.Deadline(); ok {
		_ = stream.SetDeadline(d)
	}
	if _, err = fmt.Fprintf(stream, "XROK2:%s:%s\n", s.ClientID, s.SessionToken); err != nil {
		return nil, err
	}
	if err = stream.Close(); err != nil {
		return nil, err
	}
	line, err := bufio.NewReaderSize(stream, 128).ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	if string(line) != "OK\n" {
		return nil, fmt.Errorf("QUIC association refused")
	}
	if err = handshake.Err(); err != nil {
		return nil, err
	}
	success = true
	return qc, nil
}

func connectAutoWithDisconnect(ctx context.Context, state *clientState, disconnected chan<- struct{}) {
	defer func() {
		select {
		case disconnected <- struct{}{}:
		default:
		}
	}()
	choice, err := selectAutoCarrier(ctx, state.SupportedTransports, state.requiresTCPCarrier(), func(ctx context.Context, mode string) (selectedCarrier, error) {
		if mode == "quic" {
			conn, err := state.dialNativeQUIC(ctx)
			return selectedCarrier{QUIC: conn}, err
		}
		conn, err := state.dialStandardCarrier(ctx, mode)
		return selectedCarrier{TCP: conn}, err
	}, func(mode string, err error) {
		if clientLog != nil {
			clientLog.Warn("Transport %s unavailable: %s", mode, state.redactRouteSecrets(err.Error()))
		}
	})
	if err != nil {
		logCarrierError(state, err)
		return
	}
	if clientLog != nil {
		clientLog.Info("Transport selected: %s", choice.Mode)
	}
	if choice.QUIC == nil {
		runClientMuxCarrier(ctx, state, choice.TCP)
		return
	}
	defer choice.QUIC.Close()
	clientStateMutex.Lock()
	state.QUICConn = choice.QUIC
	clientStateMutex.Unlock()
	defer func() {
		clientStateMutex.Lock()
		if state.QUICConn == choice.QUIC {
			state.QUICConn = nil
		}
		clientStateMutex.Unlock()
	}()
	for {
		stream, err := choice.QUIC.AcceptStream(ctx)
		if err != nil {
			return
		}
		go handleQUICStream(ctx, stream, state)
	}
}

// parseQUICAssociation accepts the historic one-write form and the v2
// newline-delimited, acknowledged form without logging credentials.
func parseQUICAssociation(first []byte, remaining *bufio.Reader) (string, string, bool, error) {
	message := string(first)
	for len(message) < 6 && strings.HasPrefix("XROK2:", message) {
		b, err := remaining.ReadByte()
		if err != nil {
			return "", "", true, err
		}
		message += string(b)
	}
	v2 := strings.HasPrefix(message, "XROK2:")
	if v2 && !strings.Contains(message, "\n") {
		for len(message) < 1024 {
			b, err := remaining.ReadByte()
			if err != nil {
				return "", "", true, err
			}
			message += string(b)
			if b == '\n' {
				break
			}
		}
	}
	if v2 {
		if !strings.HasSuffix(message, "\n") || len(message) > 1024 {
			return "", "", true, fmt.Errorf("invalid QUIC association")
		}
		message = strings.TrimSuffix(message, "\n")
	}
	parts := strings.SplitN(message, ":", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" || (!v2 && parts[0] != "XROK") {
		return "", "", v2, fmt.Errorf("invalid QUIC association")
	}
	return parts[1], parts[2], v2, nil
}
