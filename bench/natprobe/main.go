package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"time"
)

const stunCookie uint32 = 0x2112a442

type result struct {
	Mode          string    `json:"mode"`
	Mappings      []string  `json:"mappings,omitempty"`
	Symmetric     bool      `json:"symmetric,omitempty"`
	Sent          int       `json:"sent,omitempty"`
	Received      int       `json:"received,omitempty"`
	LossPercent   float64   `json:"loss_percent,omitempty"`
	RTTMillis     []float64 `json:"rtt_ms,omitempty"`
	P50Millis     float64   `json:"p50_ms,omitempty"`
	P95Millis     float64   `json:"p95_ms,omitempty"`
	Error         string    `json:"error,omitempty"`
}

func main() {
	if len(os.Args) < 3 {
		fail(result{Error: "usage: natprobe symmetric host:port host:port | degraded host:port [attempts]"})
	}
	var out result
	switch os.Args[1] {
	case "symmetric":
		out = symmetric(os.Args[2:])
	case "degraded":
		attempts := 20
		if len(os.Args) > 3 {
			if n, err := strconv.Atoi(os.Args[3]); err == nil && n > 0 && n <= 1000 { attempts = n }
		}
		out = degraded(os.Args[2], attempts)
	default:
		out.Error = "unknown mode"
	}
	if out.Error != "" { fail(out) }
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

func symmetric(servers []string) result {
	out := result{Mode: "symmetric"}
	if len(servers) != 2 { out.Error = "two STUN endpoints required"; return out }
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil { out.Error = err.Error(); return out }
	defer conn.Close()
	for _, server := range servers {
		mapped, _, err := binding(conn, server, 2*time.Second)
		if err != nil { out.Error = fmt.Sprintf("%s: %v", server, err); return out }
		out.Mappings = append(out.Mappings, mapped)
	}
	out.Symmetric = out.Mappings[0] != out.Mappings[1]
	if !out.Symmetric { out.Error = "NAT mapping was endpoint-independent" }
	return out
}

func degraded(server string, attempts int) result {
	out := result{Mode: "degraded", Sent: attempts}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil { out.Error = err.Error(); return out }
	defer conn.Close()
	for i := 0; i < attempts; i++ {
		_, rtt, err := binding(conn, server, 750*time.Millisecond)
		if err == nil { out.Received++; out.RTTMillis = append(out.RTTMillis, float64(rtt.Microseconds())/1000) }
	}
	out.LossPercent = float64(out.Sent-out.Received) * 100 / float64(out.Sent)
	sort.Float64s(out.RTTMillis)
	if len(out.RTTMillis) > 0 {
		out.P50Millis = percentile(out.RTTMillis, .50)
		out.P95Millis = percentile(out.RTTMillis, .95)
	} else { out.Error = "all STUN probes lost" }
	return out
}

func binding(conn *net.UDPConn, server string, timeout time.Duration) (string, time.Duration, error) {
	addr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil { return "", 0, err }
	request := make([]byte, 20)
	binary.BigEndian.PutUint16(request[0:2], 0x0001)
	binary.BigEndian.PutUint32(request[4:8], stunCookie)
	if _, err := rand.Read(request[8:20]); err != nil { return "", 0, err }
	started := time.Now()
	if err := conn.SetDeadline(started.Add(timeout)); err != nil { return "", 0, err }
	if _, err := conn.WriteToUDP(request, addr); err != nil { return "", 0, err }
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil { return "", 0, err }
		if n < 20 || string(buf[8:20]) != string(request[8:20]) { continue }
		mapped, err := mappedAddress(buf[:n])
		return mapped, time.Since(started), err
	}
}

func mappedAddress(msg []byte) (string, error) {
	for off := 20; off+4 <= len(msg); {
		typ, size := binary.BigEndian.Uint16(msg[off:off+2]), int(binary.BigEndian.Uint16(msg[off+2:off+4]))
		start, end := off+4, off+4+size
		if end > len(msg) { break }
		if (typ == 0x0020 || typ == 0x0001) && size >= 8 && msg[start+1] == 0x01 {
			port := binary.BigEndian.Uint16(msg[start+2:start+4])
			ip := append(net.IP(nil), msg[start+4:start+8]...)
			if typ == 0x0020 {
				port ^= uint16(stunCookie >> 16)
				cookie := make([]byte, 4); binary.BigEndian.PutUint32(cookie, stunCookie)
				for i := range ip { ip[i] ^= cookie[i] }
			}
			return net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), nil
		}
		off = end + (4-size%4)%4
	}
	return "", errors.New("STUN response has no mapped address")
}

func percentile(values []float64, p float64) float64 {
	idx := int(float64(len(values)-1)*p + .5)
	return values[idx]
}

func fail(out result) {
	_ = json.NewEncoder(os.Stdout).Encode(out)
	os.Exit(1)
}
