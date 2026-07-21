package p2p

import "testing"

func FuzzDecodeSignal(f *testing.F) {
	f.Add([]byte(`{"type":"p2p_discover","from_peer":"a","to_peer":"b"}`))
	f.Add([]byte(`{"type":"p2p_ready","from_peer":"a","to_peer":"b"}`))
	f.Add([]byte("{}"))
	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := DecodeSignal(data)
		if err == nil && msg == nil {
			t.Fatal("DecodeSignal returned nil message without error")
		}
	})
}

func TestDecodeSignalRejectsUnknownAndOversizedMessages(t *testing.T) {
	if _, err := DecodeSignal([]byte(`{"type":"attacker_defined"}`)); err == nil {
		t.Fatal("unknown signal type accepted")
	}
	if _, err := DecodeSignal(make([]byte, MaxSignalMessageSize+1)); err == nil {
		t.Fatal("oversized signal accepted")
	}
}
