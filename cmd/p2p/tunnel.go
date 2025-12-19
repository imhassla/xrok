package p2p

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// P2PTunnel provides HTTP tunneling over P2P connection
type P2PTunnel struct {
	conn       net.Conn
	peerID     string
	localProxy string // local target to proxy to
	streamID   uint32
	streams    map[uint32]*P2PStream
	streamsMu  sync.RWMutex
	nextID     uint32
	closed     int32
}

// P2PStream represents a multiplexed stream over P2P
type P2PStream struct {
	id       uint32
	tunnel   *P2PTunnel
	readBuf  *bytes.Buffer
	readCond *sync.Cond
	readMu   sync.Mutex
	closed   int32
}

// P2P tunnel message header (8 bytes)
// [0:4] - stream ID (uint32 big endian)
// [4:8] - payload length (uint32 big endian)
const (
	p2pHeaderSize   = 8
	p2pMaxPayload   = 65000 // Max UDP payload
	p2pMagic        = 0x5852 // "XR" - xrok P2P
)

// Message types for tunnel protocol
const (
	msgTypeData  byte = 0x01
	msgTypeClose byte = 0x02
	msgTypeAck   byte = 0x03
)

// NewP2PTunnel creates a new P2P tunnel over a connection
func NewP2PTunnel(conn net.Conn, peerID, localProxy string) *P2PTunnel {
	t := &P2PTunnel{
		conn:       conn,
		peerID:     peerID,
		localProxy: localProxy,
		streams:    make(map[uint32]*P2PStream),
		nextID:     1,
	}
	go t.readLoop()
	return t
}

// OpenStream opens a new multiplexed stream
func (t *P2PTunnel) OpenStream() (*P2PStream, error) {
	if atomic.LoadInt32(&t.closed) != 0 {
		return nil, fmt.Errorf("tunnel closed")
	}

	id := atomic.AddUint32(&t.nextID, 1)
	stream := &P2PStream{
		id:      id,
		tunnel:  t,
		readBuf: bytes.NewBuffer(nil),
	}
	stream.readCond = sync.NewCond(&stream.readMu)

	t.streamsMu.Lock()
	t.streams[id] = stream
	t.streamsMu.Unlock()

	return stream, nil
}

// AcceptStream waits for and returns the next incoming stream
func (t *P2PTunnel) AcceptStream() (*P2PStream, error) {
	// Streams are created on-demand when data arrives
	// This is a placeholder - in practice, streams are created in readLoop
	return nil, fmt.Errorf("use readLoop for incoming streams")
}

// readLoop reads from the connection and dispatches to streams
func (t *P2PTunnel) readLoop() {
	buf := make([]byte, p2pMaxPayload+p2pHeaderSize+1)
	for {
		if atomic.LoadInt32(&t.closed) != 0 {
			return
		}

		t.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := t.conn.Read(buf)
		if err != nil {
			if atomic.LoadInt32(&t.closed) == 0 {
				// Connection error, close tunnel
				t.Close()
			}
			return
		}

		if n < p2pHeaderSize+1 {
			continue
		}

		msgType := buf[0]
		streamID := binary.BigEndian.Uint32(buf[1:5])
		payloadLen := binary.BigEndian.Uint32(buf[5:9])

		if int(payloadLen) > n-p2pHeaderSize-1 {
			continue
		}

		payload := buf[p2pHeaderSize+1 : p2pHeaderSize+1+payloadLen]

		t.streamsMu.RLock()
		stream, exists := t.streams[streamID]
		t.streamsMu.RUnlock()

		if !exists && msgType == msgTypeData {
			// New incoming stream - create it and handle
			stream = &P2PStream{
				id:      streamID,
				tunnel:  t,
				readBuf: bytes.NewBuffer(nil),
			}
			stream.readCond = sync.NewCond(&stream.readMu)

			t.streamsMu.Lock()
			t.streams[streamID] = stream
			t.streamsMu.Unlock()

			// Handle new stream (proxy request)
			go t.handleIncomingStream(stream, payload)
			continue
		}

		if stream == nil {
			continue
		}

		switch msgType {
		case msgTypeData:
			stream.readMu.Lock()
			stream.readBuf.Write(payload)
			stream.readCond.Signal()
			stream.readMu.Unlock()

		case msgTypeClose:
			stream.Close()
		}
	}
}

// handleIncomingStream handles an incoming P2P tunnel request
func (t *P2PTunnel) handleIncomingStream(stream *P2PStream, initialData []byte) {
	defer stream.Close()

	if t.localProxy == "" {
		return
	}

	// Connect to local target
	targetConn, err := net.DialTimeout("tcp", t.localProxy, 5*time.Second)
	if err != nil {
		return
	}
	defer targetConn.Close()

	// Write initial data to target
	if len(initialData) > 0 {
		targetConn.Write(initialData)
	}

	// Bidirectional copy
	done := make(chan struct{}, 2)

	// P2P -> Target
	go func() {
		buf := make([]byte, 32*1024)
		for {
			stream.readMu.Lock()
			for stream.readBuf.Len() == 0 && atomic.LoadInt32(&stream.closed) == 0 {
				stream.readCond.Wait()
			}
			if atomic.LoadInt32(&stream.closed) != 0 && stream.readBuf.Len() == 0 {
				stream.readMu.Unlock()
				break
			}
			n, _ := stream.readBuf.Read(buf)
			stream.readMu.Unlock()

			if n > 0 {
				targetConn.Write(buf[:n])
			}
		}
		done <- struct{}{}
	}()

	// Target -> P2P
	go func() {
		buf := make([]byte, 32*1024)
		for {
			targetConn.SetReadDeadline(time.Now().Add(30 * time.Second))
			n, err := targetConn.Read(buf)
			if err != nil {
				break
			}
			if n > 0 {
				stream.Write(buf[:n])
			}
		}
		done <- struct{}{}
	}()

	<-done
}

// Close closes the tunnel
func (t *P2PTunnel) Close() error {
	if !atomic.CompareAndSwapInt32(&t.closed, 0, 1) {
		return nil
	}

	t.streamsMu.Lock()
	for _, stream := range t.streams {
		stream.Close()
	}
	t.streamsMu.Unlock()

	return t.conn.Close()
}

// P2PStream methods

func (s *P2PStream) Read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()

	for s.readBuf.Len() == 0 {
		if atomic.LoadInt32(&s.closed) != 0 {
			return 0, io.EOF
		}
		s.readCond.Wait()
	}

	return s.readBuf.Read(p)
}

func (s *P2PStream) Write(p []byte) (int, error) {
	if atomic.LoadInt32(&s.closed) != 0 {
		return 0, fmt.Errorf("stream closed")
	}

	// Fragment into max payload chunks
	written := 0
	for written < len(p) {
		end := written + p2pMaxPayload - p2pHeaderSize - 1
		if end > len(p) {
			end = len(p)
		}

		chunk := p[written:end]
		if err := s.writeFrame(msgTypeData, chunk); err != nil {
			return written, err
		}
		written = end
	}
	return len(p), nil
}

func (s *P2PStream) writeFrame(msgType byte, payload []byte) error {
	frame := make([]byte, p2pHeaderSize+1+len(payload))
	frame[0] = msgType
	binary.BigEndian.PutUint32(frame[1:5], s.id)
	binary.BigEndian.PutUint32(frame[5:9], uint32(len(payload)))
	copy(frame[p2pHeaderSize+1:], payload)

	_, err := s.tunnel.conn.Write(frame)
	return err
}

func (s *P2PStream) Close() error {
	if !atomic.CompareAndSwapInt32(&s.closed, 0, 1) {
		return nil
	}

	s.writeFrame(msgTypeClose, nil)

	s.readMu.Lock()
	s.readCond.Broadcast()
	s.readMu.Unlock()

	s.tunnel.streamsMu.Lock()
	delete(s.tunnel.streams, s.id)
	s.tunnel.streamsMu.Unlock()

	return nil
}

// DoHTTPRequest sends an HTTP request over P2P and returns the response
func (t *P2PTunnel) DoHTTPRequest(req *http.Request) (*http.Response, error) {
	stream, err := t.OpenStream()
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	// Write request
	var reqBuf bytes.Buffer
	if err := req.Write(&reqBuf); err != nil {
		return nil, fmt.Errorf("failed to serialize request: %w", err)
	}

	if _, err := stream.Write(reqBuf.Bytes()); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Read response
	reader := bufio.NewReader(&streamReader{stream: stream})
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return resp, nil
}

// streamReader wraps P2PStream for buffered reading
type streamReader struct {
	stream *P2PStream
}

func (r *streamReader) Read(p []byte) (int, error) {
	return r.stream.Read(p)
}
