package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestSyntheticScaleBounded exercises the control-plane at representative
// virtual tunnel counts without opening sockets. It is intentionally bounded
// so it can run in local CI and produce machine-readable results.
func TestSyntheticScaleBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("synthetic scale disabled in short mode")
	}
	results := make([]map[string]any, 0, 2)
	for _, n := range []int{1000, 10000} {
		r := NewClientRegistry()
		start := time.Now()
		for i := 0; i < n; i++ {
			r.Put(fmt.Sprintf("t-%d", i), &serverClientInfo{})
		}
		putDur := time.Since(start)
		start = time.Now()
		for i := 0; i < n; i++ {
			if _, ok := r.Get(fmt.Sprintf("t-%d", i)); !ok {
				t.Fatalf("missing tunnel %d", i)
			}
		}
		getDur := time.Since(start)
		if r.Len() != n {
			t.Fatalf("registry size=%d want %d", r.Len(), n)
		}
		results = append(results, map[string]any{
			"tunnels": n, "put_ms": float64(putDur.Microseconds()) / 1000,
			"get_ms": float64(getDur.Microseconds()) / 1000,
			"ops":    2 * n,
		})
	}
	if out := os.Getenv("SCALE_OUTPUT"); out != "" {
		f, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := json.NewEncoder(f).Encode(results); err != nil {
			t.Fatal(err)
		}
	}
}
