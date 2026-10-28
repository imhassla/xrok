package cmd

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/quic-go/quic-go"
)

func closeStreamWrite(c io.Closer) {
	if half, ok := c.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
		return
	}
	switch c.(type) {
	case *yamux.Stream, *quic.Stream:
		_ = c.Close()
	}
}

// relayLocalApplication preserves half-close: a browser can finish its request
// stream and still receive the backend's response. Cancellation closes both sides.
func relayLocalApplication(ctx context.Context, stream io.ReadWriteCloser, target io.ReadWriteCloser) {
	closeAll := func() {
		for _, c := range []io.ReadWriteCloser{stream, target} {
			if d, ok := c.(interface{ SetDeadline(time.Time) error }); ok {
				_ = d.SetDeadline(time.Now())
			}
			_ = c.Close()
		}
	}
	stop := context.AfterFunc(ctx, closeAll)
	defer stop()
	defer closeAll()
	done := make(chan struct{}, 2)
	copySide := func(dst io.ReadWriteCloser, src io.Reader) {
		_, err := io.Copy(dst, src)
		if err != nil {
			closeAll()
		} else {
			closeStreamWrite(dst)
		}
		done <- struct{}{}
	}
	go copySide(target, stream)
	go copySide(stream, target)
	<-done
	<-done
}

func writeUDPFrame(w io.Writer, p []byte) error {
	if len(p) > 1200 {
		return fmt.Errorf("UDP payload too large")
	}
	frame := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(frame, uint16(len(p)))
	copy(frame[2:], p)
	n, err := w.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return err
}
func readUDPFrame(r io.Reader) ([]byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(h[:]))
	if n > 1200 {
		return nil, fmt.Errorf("UDP frame too large")
	}
	p := make([]byte, n)
	_, err := io.ReadFull(r, p)
	return p, err
}

func openVisitorUDPStream(ctx context.Context, client *serverClientInfo, name string) (net.Conn, error) {
	if name == "" || len(name) > 255 {
		return nil, fmt.Errorf("invalid UDP tunnel name")
	}
	conn, err := openClientTransportStream(ctx, client)
	if err != nil {
		return nil, err
	}
	if _, err = conn.Write(append([]byte{0xFC, byte(len(name))}, []byte(name)...)); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func handleVisitorUDPStream(ctx context.Context, stream io.ReadWriteCloser, state *clientState) {
	defer stream.Close()
	var length [1]byte
	if _, err := io.ReadFull(stream, length[:]); err != nil {
		return
	}
	name := make([]byte, int(length[0]))
	if _, err := io.ReadFull(stream, name); err != nil {
		return
	}
	target, ok := state.TunnelMap[string(name)]
	if !ok {
		return
	}
	allowed := false
	for _, t := range state.Tunnels {
		if (t.Name == string(name) || (t.Name == "" && t.Target == target)) && t.Datagrams && t.WebTransport {
			allowed = true
			break
		}
	}
	if !allowed {
		return
	}
	udp, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "udp", target)
	if err != nil {
		return
	}
	defer udp.Close()
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan struct{}, 2)
	closeAll := func() {
		udp.Close()
		if d, ok := stream.(interface{ SetDeadline(time.Time) error }); ok {
			_ = d.SetDeadline(time.Now())
		}
		stream.Close()
	}
	stop := context.AfterFunc(ctx, closeAll)
	defer stop()
	defer closeAll()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			p, err := readUDPFrame(stream)
			if err != nil {
				return
			}
			last.Store(time.Now().UnixNano())
			if _, err = udp.Write(p); err != nil {
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		b := make([]byte, 1201)
		for {
			n, err := udp.Read(b)
			if err != nil {
				return
			}
			if n > 1200 {
				continue
			}
			last.Store(time.Now().UnixNano())
			if err = writeUDPFrame(stream, b[:n]); err != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			closeAll()
			<-done
			return
		case <-ctx.Done():
			closeAll()
			<-done
			<-done
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, last.Load())) > time.Minute {
				closeAll()
				<-done
				<-done
				return
			}
		}
	}
}
