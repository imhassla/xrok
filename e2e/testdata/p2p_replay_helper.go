package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"xrok/cmd/p2p"
)

const (
	localToken  = "local-process-e2e-capability"
	remoteToken = "remote-process-e2e-capability"
	remoteID    = "remote-process-peer"
	remoteSess  = "remote-process-session"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	stunConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return fmt.Errorf("listen STUN fixture: %w", err)
	}
	defer stunConn.Close()
	go serveOneSTUNBinding(stunConn)

	manager := p2p.NewP2PManagerWithToken("local-process-peer", stunConn.LocalAddr().String(), localToken)
	localInfo, err := manager.DiscoverPublicAddr()
	if err != nil {
		return fmt.Errorf("discover through local STUN fixture: %w", err)
	}
	if localInfo.SessionID == "" {
		return fmt.Errorf("P2P manager returned an empty session")
	}

	peerConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		return fmt.Errorf("listen peer fixture: %w", err)
	}
	defer peerConn.Close()
	peer := &p2p.PeerInfo{
		PeerID:     remoteID,
		PublicAddr: peerConn.LocalAddr().String(),
		AuthToken:  remoteToken,
		SessionID:  remoteSess,
	}

	replayed := authenticatedPunch(localToken, remoteID, localInfo.SessionID, "fixed-replay-nonce", time.Now())
	firstSent := make(chan error, 1)
	go replyToNextPunch(peerConn, replayed, firstSent)
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 2*time.Second)
	first, err := manager.PunchHole(firstCtx, peer)
	firstCancel()
	if err != nil {
		return fmt.Errorf("first authenticated punch rejected: %w", err)
	}
	_ = first.Conn.Close()
	if err := <-firstSent; err != nil {
		return err
	}

	secondSent := make(chan error, 1)
	go replyToNextPunch(peerConn, replayed, secondSent)
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 650*time.Millisecond)
	_, replayErr := manager.PunchHole(secondCtx, peer)
	secondCancel()
	if err := <-secondSent; err != nil {
		return err
	}
	if replayErr == nil {
		return fmt.Errorf("replayed authenticated punch was accepted")
	}
	return nil
}

func replyToNextPunch(conn *net.UDPConn, response []byte, result chan<- error) {
	buf := make([]byte, 2048)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		result <- fmt.Errorf("set peer deadline: %w", err)
		return
	}
	_, source, err := conn.ReadFromUDP(buf)
	if err != nil {
		result <- fmt.Errorf("read outgoing punch: %w", err)
		return
	}
	_, err = conn.WriteToUDP(response, source)
	if err != nil {
		result <- fmt.Errorf("send peer punch: %w", err)
		return
	}
	result <- nil
}

func authenticatedPunch(token, peerID, sessionID, nonce string, issued time.Time) []byte {
	timestamp := issued.UnixMilli()
	input := strings.Join([]string{"v2", peerID, sessionID, strconv.FormatInt(timestamp, 10), nonce}, ":")
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(input))
	return []byte("XROK-P2P-PUNCH:" + input + ":" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}

func serveOneSTUNBinding(conn *net.UDPConn) {
	request := make([]byte, 1024)
	n, source, err := conn.ReadFromUDP(request)
	if err != nil || n < 20 {
		return
	}
	response := make([]byte, 32)
	response[0], response[1] = 0x01, 0x01
	binary.BigEndian.PutUint16(response[2:4], 12)
	copy(response[4:20], request[4:20])
	response[20], response[21] = 0x00, 0x01
	binary.BigEndian.PutUint16(response[22:24], 8)
	response[25] = 0x01
	binary.BigEndian.PutUint16(response[26:28], uint16(source.Port))
	copy(response[28:32], source.IP.To4())
	_, _ = conn.WriteToUDP(response, source)
}
