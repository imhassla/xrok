package cmd

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	socks5Version = 0x05

	// Authentication methods
	socks5NoAuth       = 0x00
	socks5UserPassAuth = 0x02
	socks5NoAcceptable = 0xFF

	// Commands
	socks5CmdConnect = 0x01
	socks5CmdBind    = 0x02
	socks5CmdUDP     = 0x03

	// Address types
	socks5AddrIPv4   = 0x01
	socks5AddrDomain = 0x03
	socks5AddrIPv6   = 0x04

	// Reply codes
	socks5RepSuccess         = 0x00
	socks5RepServerFailure   = 0x01
	socks5RepNotAllowed      = 0x02
	socks5RepNetUnreachable  = 0x03
	socks5RepHostUnreachable = 0x04
	socks5RepConnRefused     = 0x05
	socks5RepTTLExpired      = 0x06
	socks5RepCmdNotSupported = 0x07
	socks5RepAddrNotSupported = 0x08
)

// SOCKS5Server represents a SOCKS5 proxy server
type SOCKS5Server struct {
	listener   net.Listener
	clientConn func(target string) (net.Conn, error) // Function to establish connection through tunnel
	wg         sync.WaitGroup
	done       chan struct{}
}

// NewSOCKS5Server creates a new SOCKS5 server
func NewSOCKS5Server(addr string, dialFunc func(target string) (net.Conn, error)) (*SOCKS5Server, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to start SOCKS5 server on %s: %w", addr, err)
	}

	return &SOCKS5Server{
		listener:   listener,
		clientConn: dialFunc,
		done:       make(chan struct{}),
	}, nil
}

// Serve starts accepting SOCKS5 connections
func (s *SOCKS5Server) Serve() error {
	log.Printf("SOCKS5 proxy server listening on %s", s.listener.Addr())

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
				log.Printf("SOCKS5 accept error: %v", err)
				continue
			}
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConnection(conn)
		}()
	}
}

// Close shuts down the SOCKS5 server
func (s *SOCKS5Server) Close() error {
	close(s.done)
	err := s.listener.Close()
	s.wg.Wait()
	return err
}

func (s *SOCKS5Server) handleConnection(conn net.Conn) {
	defer conn.Close()

	// Set deadlines for handshake
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// 1. Read version and auth methods
	buf := make([]byte, 256)
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		log.Printf("SOCKS5: failed to read version: %v", err)
		return
	}

	if buf[0] != socks5Version {
		log.Printf("SOCKS5: unsupported version: %d", buf[0])
		return
	}

	numMethods := int(buf[1])
	if _, err := io.ReadFull(conn, buf[:numMethods]); err != nil {
		log.Printf("SOCKS5: failed to read auth methods: %v", err)
		return
	}

	// 2. Respond with no-auth (we don't require authentication for local use)
	if _, err := conn.Write([]byte{socks5Version, socks5NoAuth}); err != nil {
		log.Printf("SOCKS5: failed to write auth response: %v", err)
		return
	}

	// 3. Read request
	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		log.Printf("SOCKS5: failed to read request: %v", err)
		return
	}

	if buf[0] != socks5Version {
		log.Printf("SOCKS5: unsupported version in request: %d", buf[0])
		return
	}

	cmd := buf[1]
	// buf[2] is reserved
	addrType := buf[3]

	if cmd != socks5CmdConnect {
		s.sendReply(conn, socks5RepCmdNotSupported, nil)
		log.Printf("SOCKS5: unsupported command: %d", cmd)
		return
	}

	// 4. Read target address
	var targetAddr string
	var targetPort uint16

	switch addrType {
	case socks5AddrIPv4:
		if _, err := io.ReadFull(conn, buf[:4]); err != nil {
			log.Printf("SOCKS5: failed to read IPv4 address: %v", err)
			return
		}
		targetAddr = net.IP(buf[:4]).String()

	case socks5AddrIPv6:
		if _, err := io.ReadFull(conn, buf[:16]); err != nil {
			log.Printf("SOCKS5: failed to read IPv6 address: %v", err)
			return
		}
		targetAddr = net.IP(buf[:16]).String()

	case socks5AddrDomain:
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			log.Printf("SOCKS5: failed to read domain length: %v", err)
			return
		}
		domainLen := int(buf[0])
		if _, err := io.ReadFull(conn, buf[:domainLen]); err != nil {
			log.Printf("SOCKS5: failed to read domain: %v", err)
			return
		}
		targetAddr = string(buf[:domainLen])

	default:
		s.sendReply(conn, socks5RepAddrNotSupported, nil)
		log.Printf("SOCKS5: unsupported address type: %d", addrType)
		return
	}

	// Read port
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		log.Printf("SOCKS5: failed to read port: %v", err)
		return
	}
	targetPort = binary.BigEndian.Uint16(buf[:2])

	target := net.JoinHostPort(targetAddr, strconv.Itoa(int(targetPort)))

	// 5. Connect through tunnel
	remoteConn, err := s.clientConn(target)
	if err != nil {
		log.Printf("SOCKS5: failed to connect to %s: %v", target, err)
		s.sendReply(conn, socks5RepHostUnreachable, nil)
		return
	}
	defer remoteConn.Close()

	// 6. Send success reply
	// Get local address for reply
	localAddr := conn.LocalAddr().(*net.TCPAddr)
	s.sendReply(conn, socks5RepSuccess, localAddr)

	// Clear deadline for data transfer
	conn.SetDeadline(time.Time{})

	// 7. Start bidirectional copy
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(remoteConn, conn)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(conn, remoteConn)
		done <- struct{}{}
	}()

	<-done
}

func (s *SOCKS5Server) sendReply(conn net.Conn, rep byte, addr *net.TCPAddr) {
	var reply []byte

	if addr == nil {
		// Use all zeros for address
		reply = []byte{
			socks5Version, rep, 0x00, socks5AddrIPv4,
			0, 0, 0, 0, // IPv4 0.0.0.0
			0, 0, // Port 0
		}
	} else {
		ip := addr.IP.To4()
		if ip == nil {
			ip = addr.IP.To16()
			reply = make([]byte, 22)
			reply[0] = socks5Version
			reply[1] = rep
			reply[2] = 0x00
			reply[3] = socks5AddrIPv6
			copy(reply[4:20], ip)
			binary.BigEndian.PutUint16(reply[20:], uint16(addr.Port))
		} else {
			reply = make([]byte, 10)
			reply[0] = socks5Version
			reply[1] = rep
			reply[2] = 0x00
			reply[3] = socks5AddrIPv4
			copy(reply[4:8], ip)
			binary.BigEndian.PutUint16(reply[8:], uint16(addr.Port))
		}
	}

	conn.Write(reply)
}
