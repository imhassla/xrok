package p2p

import "testing"

func FuzzDecodeSignal(f *testing.F) {
	f.Add([]byte(`{"type":"offer","payload":"abc"}`))
	f.Add([]byte(`{"type":"candidate","candidate":"127.0.0.1:1"}`))
	f.Add([]byte("{}"))
	f.Fuzz(func(t *testing.T, data []byte) {
		msg, err := DecodeSignal(data)
		if err == nil && msg == nil {
			t.Fatal("DecodeSignal returned nil message without error")
		}
	})
}
