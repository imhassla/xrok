package cmd

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// RuntimeState is the small, serializable control-plane state needed by an
// embedded runtime. It intentionally avoids coupling the runtime to a DB.
type RuntimeState struct {
	ID        string
	Status    string
	UpdatedAt time.Time
}

type RuntimeStateStore interface {
	Save(context.Context, RuntimeState) error
	Load(context.Context, string) (RuntimeState, bool, error)
}

// InMemoryStateStore is a bounded HA-ready store for local development and
// single-process deployments. A future Redis/Postgres adapter can implement
// RuntimeStateStore without changing handlers.
type InMemoryStateStore struct {
	mu  sync.RWMutex
	max int
	m   map[string]RuntimeState
}

func NewInMemoryStateStore(max int) *InMemoryStateStore {
	if max <= 0 {
		max = 1024
	}
	return &InMemoryStateStore{max: max, m: make(map[string]RuntimeState)}
}
func (s *InMemoryStateStore) Save(_ context.Context, st RuntimeState) error {
	if st.ID == "" {
		return fmt.Errorf("runtime state id is required")
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[st.ID]; !ok && len(s.m) >= s.max {
		var oldest string
		var at time.Time
		for id, v := range s.m {
			if oldest == "" || v.UpdatedAt.Before(at) {
				oldest, at = id, v.UpdatedAt
			}
		}
		delete(s.m, oldest)
	}
	s.m[st.ID] = st
	return nil
}
func (s *InMemoryStateStore) Load(_ context.Context, id string) (RuntimeState, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[id]
	return v, ok, nil
}

type DrainHook func(context.Context) error

type DrainController struct {
	mu    sync.Mutex
	hooks []DrainHook
	once  sync.Once
	err   error
}

func (d *DrainController) Add(h DrainHook) {
	if h == nil {
		return
	}
	d.mu.Lock()
	d.hooks = append(d.hooks, h)
	d.mu.Unlock()
}
func (d *DrainController) Drain(ctx context.Context) error {
	d.once.Do(func() {
		d.mu.Lock()
		hooks := append([]DrainHook(nil), d.hooks...)
		d.mu.Unlock()
		for _, h := range hooks {
			if err := h(ctx); err != nil && d.err == nil {
				d.err = err
			}
		}
	})
	return d.err
}
