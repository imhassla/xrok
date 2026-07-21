package cmd

import (
	"fmt"
	"sync"
	"testing"
)

// Registries are shared by handlers and lifecycle goroutines. This test is
// intentionally write-heavy so `go test -race` catches accidental map access
// that bypasses the registry lock.
func TestClientRegistryConcurrentAccess(t *testing.T) {
	r := NewClientRegistry()
	const workers = 16
	const iterations = 250
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				id := fmt.Sprintf("client-%d", i%32)
				r.Put(id, &serverClientInfo{})
				_, _ = r.Get(id)
				_ = r.Len()
				_ = r.Snapshot()
				if (i+worker)%7 == 0 {
					r.Delete(id)
				}
			}
		}(w)
	}
	wg.Wait()
}

func TestRouteRegistryConcurrentAccess(t *testing.T) {
	r := NewRouteRegistry()
	const workers = 16
	const iterations = 250
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				name := fmt.Sprintf("route-%d", i%32)
				r.Put(name, &TunnelRoute{ClientID: fmt.Sprintf("client-%d", worker)})
				_, _ = r.Get(name)
				_ = r.Snapshot()
				if (i+worker)%7 == 0 {
					r.Delete(name)
				}
			}
		}(w)
	}
	wg.Wait()
}
