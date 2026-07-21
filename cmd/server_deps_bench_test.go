package cmd

import (
	"fmt"
	"log"
	"testing"
	"time"
)

// BenchmarkClientRegistryScale measures the local control-plane cost of
// maintaining virtual tunnel identities. It intentionally avoids sockets so it
// is deterministic on developer machines and CI runners.
func BenchmarkClientRegistryScale(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("tunnels-%d", n), func(b *testing.B) {
			r := NewServerDeps("bench.local", time.Now(), log.Default()).Clients
			for i := 0; i < n; i++ {
				r.Put(fmt.Sprintf("t-%d", i), &serverClientInfo{})
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = r.Get(fmt.Sprintf("t-%d", i%n))
			}
		})
	}
}
