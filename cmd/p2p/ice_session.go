package p2p

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// CandidatePairState describes the state of an ICE connectivity check.
type CandidatePairState string

const (
	CandidatePairWaiting    CandidatePairState = "waiting"
	CandidatePairInProgress CandidatePairState = "in-progress"
	CandidatePairSucceeded  CandidatePairState = "succeeded"
	CandidatePairFailed     CandidatePairState = "failed"
	CandidatePairNominated  CandidatePairState = "nominated"
)

// CandidatePair is a local/remote candidate pair in an ICE checklist.
type CandidatePair struct {
	Local    Candidate          `json:"local"`
	Remote   Candidate          `json:"remote"`
	Priority uint64             `json:"priority"`
	State    CandidatePairState `json:"state"`
}

// ConnectivityChecker performs a request/response connectivity check and
// returns the connected transport when the pair is reachable. Implementations
// are responsible for authenticating the response; a successful UDP write is
// not sufficient to nominate a pair.
type ConnectivityChecker interface {
	Check(context.Context, Candidate, Candidate) (net.Conn, error)
}

type ConnectivityCheckFunc func(context.Context, Candidate, Candidate) (net.Conn, error)

func (f ConnectivityCheckFunc) Check(ctx context.Context, local, remote Candidate) (net.Conn, error) {
	return f(ctx, local, remote)
}

// ConsentProbe verifies that the nominated peer still consents to receive
// traffic. It must perform an authenticated request/response exchange.
type ConsentProbe interface {
	Probe(context.Context, net.Conn) error
}

type ConsentProbeFunc func(context.Context, net.Conn) error

func (f ConsentProbeFunc) Probe(ctx context.Context, conn net.Conn) error { return f(ctx, conn) }

// TickSource makes consent freshness deterministic in tests. A production
// agent uses a time.Ticker when this hook is nil.
type TickSource func(context.Context, time.Duration) <-chan time.Time

// ICESession owns the nominated connection and its consent lifecycle.
type ICESession struct {
	pair     CandidatePair
	conn     net.Conn
	viaRelay bool
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
	mu       sync.RWMutex
	err      error
}

func (s *ICESession) Conn() net.Conn                { return s.conn }
func (s *ICESession) Pair() CandidatePair           { return s.pair }
func (s *ICESession) ViaRelay() bool                { return s.viaRelay }
func (s *ICESession) Done() <-chan struct{}         { return s.done }
func (s *ICESession) Read(p []byte) (int, error)    { return s.conn.Read(p) }
func (s *ICESession) Write(p []byte) (int, error)   { return s.conn.Write(p) }
func (s *ICESession) LocalAddr() net.Addr           { return s.conn.LocalAddr() }
func (s *ICESession) RemoteAddr() net.Addr          { return s.conn.RemoteAddr() }
func (s *ICESession) SetDeadline(t time.Time) error { return s.conn.SetDeadline(t) }

func (s *ICESession) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

func (s *ICESession) Close() error {
	s.terminate(nil)
	return nil
}

func (s *ICESession) terminate(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		s.cancel()
		_ = s.conn.Close()
		close(s.done)
	})
}

func defaultTicks(ctx context.Context, interval time.Duration) <-chan time.Time {
	out := make(chan time.Time)
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		defer close(out)
		for {
			select {
			case tick := <-ticker.C:
				select {
				case out <- tick:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func candidatePriority(c Candidate) uint32 {
	if c.Priority != 0 {
		return c.Priority
	}
	switch c.Type {
	case CandidateHost:
		return 126
	case CandidateSrflx:
		return 100
	case CandidateRelay:
		return 1
	default:
		return 0
	}
}

// pairPriority follows the ICE pair-priority shape from RFC 8445 section 6.1.2.3.
// The final bit is a stable tie breaker because this agent is always controlling.
func pairPriority(local, remote Candidate) uint64 {
	g, d := uint64(candidatePriority(local)), uint64(candidatePriority(remote))
	min, max := g, d
	if min > max {
		min, max = max, min
	}
	tie := uint64(0)
	if g > d {
		tie = 1
	}
	return (min << 32) + (max << 1) + tie
}

// Checklist builds a deterministic, highest-priority-first set of usable
// candidate pairs. Relay pairs are deliberately excluded: TURN is allocated
// lazily only after all direct connectivity checks fail.
func (a *ICEAgent) Checklist(local, remote []Candidate) []CandidatePair {
	pairs := make([]CandidatePair, 0, len(local)*len(remote))
	for _, l := range local {
		if l.Addr == "" || l.Type == CandidateRelay {
			continue
		}
		for _, r := range remote {
			if r.Addr == "" || r.Type == CandidateRelay {
				continue
			}
			pairs = append(pairs, CandidatePair{
				Local: l, Remote: r, Priority: pairPriority(l, r), State: CandidatePairWaiting,
			})
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if pairs[i].Priority != pairs[j].Priority {
			return pairs[i].Priority > pairs[j].Priority
		}
		if pairs[i].Local.ID != pairs[j].Local.ID {
			return pairs[i].Local.ID < pairs[j].Local.ID
		}
		return pairs[i].Remote.ID < pairs[j].Remote.ID
	})
	return pairs
}

// Connect executes the ICE checklist, nominates the first authenticated pair,
// and lazily requests a TURN allocation if all direct checks fail.
func (a *ICEAgent) Connect(ctx context.Context, peerID string, local, remote []Candidate) (*ICESession, error) {
	if ctx == nil {
		return nil, fmt.Errorf("ICE context is required")
	}
	if peerID == "" {
		return nil, fmt.Errorf("ICE peer ID is required")
	}

	var failures []error
	if a.cfg.Checker != nil {
		checklist := a.Checklist(local, remote)
		if len(checklist) == 0 {
			failures = append(failures, fmt.Errorf("ICE checklist has no usable direct candidate pairs"))
		}
		for _, pair := range checklist {
			pair.State = CandidatePairInProgress
			checkCtx, cancel := context.WithTimeout(ctx, a.cfg.CheckTimeout)
			conn, err := a.cfg.Checker.Check(checkCtx, pair.Local, pair.Remote)
			cancel()
			if err == nil && conn == nil {
				err = fmt.Errorf("checker returned a nil connection")
			}
			if err != nil {
				pair.State = CandidatePairFailed
				failures = append(failures, fmt.Errorf("check %s/%s: %w", pair.Local.ID, pair.Remote.ID, err))
				continue
			}
			pair.State = CandidatePairNominated
			return a.newSession(ctx, pair, conn, false), nil
		}
	} else {
		failures = append(failures, fmt.Errorf("ICE connectivity checker is not configured"))
	}

	if a.cfg.Relay != nil {
		conn, relay, err := a.GatherRelay(ctx, peerID)
		if err == nil && conn == nil {
			err = fmt.Errorf("TURN provider returned a nil connection")
		}
		if err == nil && (relay.Type != CandidateRelay || relay.Addr == "") {
			err = fmt.Errorf("TURN provider returned an invalid relay candidate")
		}
		if err == nil {
			var target Candidate
			if len(remote) != 0 {
				target = remote[0]
			}
			pair := CandidatePair{Local: relay, Remote: target, Priority: pairPriority(relay, target), State: CandidatePairNominated}
			return a.newSession(ctx, pair, conn, true), nil
		}
		failures = append(failures, fmt.Errorf("TURN allocation: %w", err))
	}
	return nil, fmt.Errorf("ICE connection to %q failed: %w", peerID, errors.Join(failures...))
}

func (a *ICEAgent) newSession(parent context.Context, pair CandidatePair, conn net.Conn, viaRelay bool) *ICESession {
	ctx, cancel := context.WithCancel(parent)
	s := &ICESession{pair: pair, conn: conn, viaRelay: viaRelay, cancel: cancel, done: make(chan struct{})}
	go a.monitorConsent(ctx, s)
	return s
}

func (a *ICEAgent) monitorConsent(ctx context.Context, session *ICESession) {
	if a.cfg.ConsentProbe == nil {
		<-ctx.Done()
		session.terminate(ctx.Err())
		return
	}

	ticks := a.cfg.TickSource
	if ticks == nil {
		ticks = defaultTicks
	}
	tickChannel := ticks(ctx, a.cfg.ConsentInterval)
	failures := 0
	for {
		select {
		case <-ctx.Done():
			session.terminate(ctx.Err())
			return
		case _, ok := <-tickChannel:
			if !ok {
				return
			}
			probeCtx, cancel := context.WithTimeout(ctx, a.cfg.CheckTimeout)
			err := a.cfg.ConsentProbe.Probe(probeCtx, session.conn)
			cancel()
			if err == nil {
				failures = 0
				continue
			}
			failures++
			if failures >= a.cfg.ConsentFailures {
				session.terminate(fmt.Errorf("ICE consent expired after %d failed probes: %w", failures, err))
				return
			}
		}
	}
}
