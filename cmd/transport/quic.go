package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// QUICTransport handles QUIC connections
type QUICTransport struct {
	tlsConfig  *tls.Config
	quicConfig *quic.Config
}

// NewQUICTransport creates a new QUIC transport
func NewQUICTransport(tlsConfig *tls.Config) *QUICTransport {
	return &QUICTransport{
		tlsConfig: tlsConfig,
		quicConfig: &quic.Config{
			MaxIdleTimeout:        5 * time.Minute,
			KeepAlivePeriod:       30 * time.Second,
			MaxIncomingStreams:    1000,
			MaxIncomingUniStreams: 1000,
			EnableDatagrams:       true,
		},
	}
}

// QUICListener wraps quic.Listener to provide net.Listener interface
type QUICListener struct {
	listener *quic.Listener
	ctx      context.Context
	cancel   context.CancelFunc
}

// Accept accepts a new QUIC connection and returns it as net.Conn
func (l *QUICListener) Accept() (net.Conn, error) {
	conn, err := l.listener.Accept(l.ctx)
	if err != nil {
		return nil, err
	}
	return &QUICConn{conn: conn}, nil
}

// Close closes the listener
func (l *QUICListener) Close() error {
	l.cancel()
	return l.listener.Close()
}

// Addr returns the listener address
func (l *QUICListener) Addr() net.Addr {
	return l.listener.Addr()
}

// Listen creates a QUIC listener
func (t *QUICTransport) Listen(addr string) (net.Listener, error) {
	ctx, cancel := context.WithCancel(context.Background())
	listener, err := quic.ListenAddr(addr, t.tlsConfig, t.quicConfig)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create QUIC listener: %w", err)
	}
	return &QUICListener{
		listener: listener,
		ctx:      ctx,
		cancel:   cancel,
	}, nil
}

// Dial connects to a QUIC server
func (t *QUICTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := quic.DialAddr(ctx, addr, t.tlsConfig, t.quicConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to dial QUIC: %w", err)
	}
	return &QUICConn{conn: conn}, nil
}

// QUICConn wraps quic.Conn to provide net.Conn-like interface
type QUICConn struct {
	conn   *quic.Conn
	stream *quic.Stream
	mu     sync.Mutex
}

// ensureStream opens a stream if not already open
func (c *QUICConn) ensureStream() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stream == nil {
		stream, err := c.conn.OpenStreamSync(context.Background())
		if err != nil {
			return err
		}
		c.stream = stream
	}
	return nil
}

// Read reads data from the QUIC stream
func (c *QUICConn) Read(b []byte) (n int, err error) {
	if err := c.ensureStream(); err != nil {
		return 0, err
	}
	return c.stream.Read(b)
}

// Write writes data to the QUIC stream
func (c *QUICConn) Write(b []byte) (n int, err error) {
	if err := c.ensureStream(); err != nil {
		return 0, err
	}
	return c.stream.Write(b)
}

// Close closes the QUIC connection
func (c *QUICConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stream != nil {
		c.stream.Close()
	}
	return c.conn.CloseWithError(0, "closed")
}

// LocalAddr returns the local address
func (c *QUICConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote address
func (c *QUICConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// SetDeadline sets read and write deadlines
func (c *QUICConn) SetDeadline(t time.Time) error {
	if c.stream != nil {
		c.stream.SetDeadline(t)
	}
	return nil
}

// SetReadDeadline sets the read deadline
func (c *QUICConn) SetReadDeadline(t time.Time) error {
	if c.stream != nil {
		c.stream.SetReadDeadline(t)
	}
	return nil
}

// SetWriteDeadline sets the write deadline
func (c *QUICConn) SetWriteDeadline(t time.Time) error {
	if c.stream != nil {
		c.stream.SetWriteDeadline(t)
	}
	return nil
}

// OpenStream opens a new stream on the QUIC connection
func (c *QUICConn) OpenStream() (*quic.Stream, error) {
	return c.conn.OpenStreamSync(context.Background())
}

// AcceptStream accepts an incoming stream
func (c *QUICConn) AcceptStream(ctx context.Context) (*quic.Stream, error) {
	return c.conn.AcceptStream(ctx)
}

// Connection returns the underlying QUIC connection
func (c *QUICConn) Connection() *quic.Conn {
	return c.conn
}
