package transport

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocketConn carries a byte stream in RFC 6455 binary messages. Message
// boundaries are invisible to yamux; large writes are split into bounded frames.
type WebSocketConn struct {
	ws      *websocket.Conn
	read    io.Reader
	writeMu sync.Mutex
}

func NewWebSocketConn(ws *websocket.Conn) *WebSocketConn {
	ws.SetReadLimit(256 << 10)
	return &WebSocketConn{ws: ws}
}
func (c *WebSocketConn) Read(p []byte) (int, error) {
	for {
		if c.read != nil {
			n, err := c.read.Read(p)
			if err == io.EOF {
				c.read = nil
				if n > 0 {
					return n, nil
				}
				continue
			}
			return n, err
		}
		kind, r, err := c.ws.NextReader()
		if err != nil {
			return 0, err
		}
		if kind != websocket.BinaryMessage {
			return 0, fmt.Errorf("tunnel requires binary WebSocket messages")
		}
		c.read = r
	}
}
func (c *WebSocketConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	sent := 0
	for len(p) > 0 {
		n := len(p)
		if n > 64<<10 {
			n = 64 << 10
		}
		if err := c.ws.WriteMessage(websocket.BinaryMessage, p[:n]); err != nil {
			return sent, err
		}
		sent += n
		p = p[n:]
	}
	return sent, nil
}
func (c *WebSocketConn) Close() error                      { return c.ws.Close() }
func (c *WebSocketConn) LocalAddr() net.Addr               { return c.ws.LocalAddr() }
func (c *WebSocketConn) RemoteAddr() net.Addr              { return c.ws.RemoteAddr() }
func (c *WebSocketConn) SetReadDeadline(t time.Time) error { return c.ws.SetReadDeadline(t) }

// Carrier writes are canceled by Close. Individual yamux streams own their
// deadlines; callers must not infer support for a carrier write deadline.
func (c *WebSocketConn) SetWriteDeadline(time.Time) error { return errors.ErrUnsupported }
func (c *WebSocketConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
