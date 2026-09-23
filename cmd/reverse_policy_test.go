package cmd

import "testing"

func TestLegacyPortAllocatorIsBounded(t *testing.T) {
	serverPortMutex.Lock()
	saved := serverUsedPorts
	serverUsedPorts = make(map[int]bool)
	for p := serverBaseClientPortTLS; p < serverBaseClientPortTLS+serverLegacyPortSpan; p++ {
		serverUsedPorts[p] = true
	}
	serverPortMutex.Unlock()
	defer func() {
		serverPortMutex.Lock()
		serverUsedPorts = saved
		serverPortMutex.Unlock()
	}()

	if _, _, err := generateServerPorts(); err != errServerPortsExhausted {
		t.Fatalf("expected exhaustion once the range is full, got %v", err)
	}
}
