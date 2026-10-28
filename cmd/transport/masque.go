package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/quicvarint"
	"github.com/yosida95/uritemplate/v3"
)

type MASQUEOptions struct {
	ProxyURL, Target, Token string
	TLSConfig               *tls.Config
	Timeout                 time.Duration
}

// MASQUEConn is a single, fixed-target RFC 9298 HTTP/3 UDP flow. It never
// performs a direct-target dial, even if the proxy rejects or loses the flow.
type MASQUEConn struct {
	stream                      *http3.RequestStream
	conn                        *quic.Conn
	ctx                         context.Context
	cancel                      context.CancelFunc
	mu                          sync.Mutex
	readMu                      sync.Mutex
	readDeadline, writeDeadline time.Time
	readCancel                  context.CancelFunc
	closed                      bool
	once                        sync.Once
	remote                      net.Addr
}

func DialMASQUE(parent context.Context, o MASQUEOptions) (*MASQUEConn, error) {
	u, err := url.Parse(o.ProxyURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("MASQUE proxy must be an HTTPS URL without userinfo, query or fragment")
	}
	if o.Token == "" {
		return nil, fmt.Errorf("MASQUE requires a proxy token")
	}
	host, port, err := net.SplitHostPort(o.Target)
	if err != nil || host == "" {
		return nil, fmt.Errorf("invalid UDP target")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("invalid UDP target port")
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "443")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/.well-known/masque/udp/{target_host}/{target_port}/"
		u.RawPath = ""
	}
	template, err := uritemplate.New(strings.ReplaceAll(strings.ReplaceAll(u.String(), "%7B", "{"), "%7D", "}"))
	if err != nil {
		return nil, fmt.Errorf("invalid MASQUE URI template")
	}
	expanded, err := template.Expand(uritemplate.Values{"target_host": uritemplate.String(host), "target_port": uritemplate.String(port)})
	if err != nil {
		return nil, err
	}
	targetURL, err := url.Parse(expanded)
	if err != nil {
		return nil, err
	}
	budget := o.Timeout
	if budget <= 0 {
		budget = 15 * time.Second
	}
	handshake, finish := context.WithTimeout(parent, budget)
	defer finish()
	cfg := &tls.Config{MinVersion: tls.VersionTLS13}
	if o.TLSConfig != nil {
		cfg = o.TLSConfig.Clone()
	}
	cfg.NextProtos = []string{http3.NextProtoH3}
	conn, err := quic.DialAddr(handshake, u.Host, cfg, &quic.Config{EnableDatagrams: true, InitialPacketSize: 1350, MaxIdleTimeout: 45 * time.Second, KeepAlivePeriod: 15 * time.Second})
	if err != nil {
		return nil, err
	}
	good := false
	defer func() {
		if !good {
			conn.CloseWithError(0, "")
		}
	}()
	stop := context.AfterFunc(handshake, func() { conn.CloseWithError(0, "") })
	defer stop()
	tr := &http3.Transport{EnableDatagrams: true}
	client := tr.NewClientConn(conn)
	select {
	case <-client.ReceivedSettings():
	case <-handshake.Done():
		return nil, handshake.Err()
	case <-client.Context().Done():
		return nil, context.Cause(client.Context())
	}
	if !client.Settings().EnableExtendedConnect || !client.Settings().EnableDatagrams {
		return nil, fmt.Errorf("MASQUE proxy lacks Extended CONNECT/datagrams")
	}
	stream, err := client.OpenRequestStream(handshake)
	if err != nil {
		return nil, err
	}
	if d, ok := handshake.Deadline(); ok {
		stream.SetReadDeadline(d)
		stream.SetWriteDeadline(d)
	}
	err = stream.SendRequestHeader(&http.Request{Method: "CONNECT", Proto: "connect-udp", Host: u.Host, URL: targetURL, Header: http.Header{"Authorization": {"Bearer " + o.Token}, http3.CapsuleProtocolHeader: {"?1"}}})
	if err != nil {
		return nil, err
	}
	response, err := stream.ReadResponse()
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("MASQUE proxy refused flow: HTTP %d", response.StatusCode)
	}
	stream.SetReadDeadline(time.Time{})
	stream.SetWriteDeadline(time.Time{})
	if err := handshake.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	c := &MASQUEConn{stream: stream, conn: conn, ctx: ctx, cancel: cancel, remote: &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: p}}
	good = true
	go func() { <-ctx.Done(); c.Close() }()
	go func() {
		defer c.Close()
		r := quicvarint.NewReader(stream)
		for {
			if _, err := quicvarint.Read(r); err != nil {
				return
			}
			n, err := quicvarint.Read(r)
			if err != nil || n > 65536 {
				return
			}
			if _, err = io.CopyN(io.Discard, stream, int64(n)); err != nil {
				return
			}
		}
	}()
	return c, nil
}

func (c *MASQUEConn) RemoteAddr() net.Addr { return c.remote }
func (c *MASQUEConn) LocalAddr() net.Addr  { return c.conn.LocalAddr() }
func (c *MASQUEConn) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		if c.readCancel != nil {
			c.readCancel()
		}
		c.mu.Unlock()
		c.cancel()
		c.stream.CancelRead(0)
		_ = c.stream.Close()
		_ = c.conn.CloseWithError(0, "")
	})
	return nil
}
func (c *MASQUEConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return 0, nil, net.ErrClosed
		}
		d := c.readDeadline
		if !d.IsZero() && !time.Now().Before(d) {
			c.mu.Unlock()
			return 0, nil, os.ErrDeadlineExceeded
		}
		ctx, cancel := context.WithCancel(c.ctx)
		c.readCancel = cancel
		c.mu.Unlock()
		var timer *time.Timer
		if !d.IsZero() {
			timer = time.AfterFunc(time.Until(d), cancel)
		}
		data, err := c.stream.ReceiveDatagram(ctx)
		if timer != nil {
			timer.Stop()
		}
		cancel()
		c.mu.Lock()
		c.readCancel = nil
		closed := c.closed
		newDeadline := c.readDeadline
		c.mu.Unlock()
		if closed {
			return 0, nil, net.ErrClosed
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				if newDeadline.IsZero() || time.Now().Before(newDeadline) {
					continue
				}
				return 0, nil, os.ErrDeadlineExceeded
			}
			return 0, nil, err
		}
		id, n, err := quicvarint.Parse(data)
		if err != nil {
			return 0, nil, fmt.Errorf("malformed MASQUE datagram")
		}
		if id != 0 {
			continue
		}
		return copy(p, data[n:]), c.remote, nil
	}
}
func (c *MASQUEConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	closed, d := c.closed, c.writeDeadline
	c.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	if !d.IsZero() && !time.Now().Before(d) {
		return 0, os.ErrDeadlineExceeded
	}
	if addr != nil && addr.String() != c.remote.String() {
		return 0, fmt.Errorf("MASQUE flow target is fixed")
	}
	if err := c.stream.SendDatagram(append([]byte{0}, p...)); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (c *MASQUEConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	if c.readCancel != nil {
		c.readCancel()
	}
	return nil
}
func (c *MASQUEConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	return nil
}
func (c *MASQUEConn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}
