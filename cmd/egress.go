package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// serverAllowPrivateEgress lets SOCKS/UDP streams reach loopback, private,
// link-local and the server's own addresses. Off by default: the server is
// usually co-hosted with services bound to localhost (databases, admin
// panels) and cloud metadata endpoints that tunnel clients must not reach.
var serverAllowPrivateEgress bool

var errEgressBlocked = errors.New("destination blocked by egress policy")

var (
	cgnatNet    = mustCIDR("100.64.0.0/10")
	thisNet     = mustCIDR("0.0.0.0/8")
	ietfNet     = mustCIDR("192.0.0.0/24")
	benchNet    = mustCIDR("198.18.0.0/15")
	reservedNet = mustCIDR("240.0.0.0/4")
	nat64Net    = mustCIDR("64:ff9b::/96")
)

// localAddrs caches the host's interface addresses. A globally routable
// IPv6 (or public IPv4) assigned to an interface reaches 0.0.0.0-bound
// sockets directly, bypassing any cloud firewall in front of the host.
var localAddrs struct {
	sync.Mutex
	set     map[string]struct{}
	refresh time.Time
}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func isLocalInterfaceIP(ip net.IP) bool {
	localAddrs.Lock()
	defer localAddrs.Unlock()
	if localAddrs.set == nil || time.Since(localAddrs.refresh) > time.Minute {
		set := make(map[string]struct{})
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok {
					set[ipn.IP.String()] = struct{}{}
				}
			}
		}
		localAddrs.set = set
		localAddrs.refresh = time.Now()
	}
	_, ok := localAddrs.set[ip.String()]
	return ok
}

func isBlockedEgressIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	for _, n := range []*net.IPNet{cgnatNet, thisNet, ietfNet, benchNet, reservedNet, nat64Net} {
		if n.Contains(ip) {
			return true
		}
	}
	return isLocalInterfaceIP(ip)
}

// egressControl runs after name resolution with the concrete peer address,
// so DNS names that resolve (or rebind) to internal addresses are rejected.
func egressControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || isBlockedEgressIP(ip) {
		return fmt.Errorf("%w: %s", errEgressBlocked, address)
	}
	return nil
}

// dialEgress dials a client-requested destination under the egress policy.
func dialEgress(network, target string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	if !serverAllowPrivateEgress {
		d.Control = egressControl
	}
	return d.Dial(network, target)
}

// serverEgressIdleTimeout tears down a SOCKS/dynamic relay after no bytes
// moved in either direction for this long, so abandoned streams do not pin
// goroutines and file descriptors indefinitely.
var serverEgressIdleTimeout = 10 * time.Minute

type activityConn struct {
	net.Conn
	last *atomic.Int64
}

func (c activityConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// relayEgress copies both directions until each finishes. The first EOF only
// half-closes the opposite side so an in-flight response is still delivered.
func relayEgress(stream, target net.Conn, idle time.Duration) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	a := activityConn{Conn: stream, last: &last}
	b := activityConn{Conn: target, last: &last}

	closeAll := func() {
		stream.Close()
		target.Close()
	}
	done := make(chan struct{}, 2)
	pipe := func(dst net.Conn, src io.Reader) {
		// io.Copy returns nil on a clean EOF: only then half-close so the
		// other direction can finish. A reset/closed session is an error and
		// tears both sides down at once instead of waiting for the idle timer.
		if _, err := io.Copy(dst, src); err != nil {
			closeAll()
		} else if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go pipe(target, a)
	go pipe(stream, b)

	ticker := time.NewTicker(idle / 4)
	defer ticker.Stop()
	for finished := 0; finished < 2; {
		select {
		case <-done:
			finished++
		case <-ticker.C:
			if time.Since(time.Unix(0, last.Load())) > idle {
				closeAll()
			}
		}
	}
	closeAll()
}

// acceptBackoff decides what an accept loop does after an error: stop when
// the listener is closed or ctx is done, otherwise sleep with exponential
// backoff (5ms..1s) so transient errors such as EMFILE do not spin a CPU.
type acceptBackoff struct{ delay time.Duration }

func (b *acceptBackoff) retry(ctx context.Context, err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	if b.delay == 0 {
		b.delay = 5 * time.Millisecond
	} else if b.delay *= 2; b.delay > time.Second {
		b.delay = time.Second
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(b.delay):
		return true
	}
}

func (b *acceptBackoff) reset() { b.delay = 0 }
