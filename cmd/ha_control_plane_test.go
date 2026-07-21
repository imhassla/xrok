package cmd

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInMemoryStateStoreBoundedAndLoads(t *testing.T) {
	s := NewInMemoryStateStore(2)
	now := time.Now()
	_ = s.Save(context.Background(), RuntimeState{ID: "a", UpdatedAt: now})
	_ = s.Save(context.Background(), RuntimeState{ID: "b", UpdatedAt: now.Add(time.Second)})
	_ = s.Save(context.Background(), RuntimeState{ID: "c", UpdatedAt: now.Add(2 * time.Second)})
	if _, ok, _ := s.Load(context.Background(), "a"); ok {
		t.Fatal("oldest state was not evicted")
	}
	if _, ok, _ := s.Load(context.Background(), "c"); !ok {
		t.Fatal("new state missing")
	}
}

func TestDrainControllerRunsOnceAndReturnsFirstError(t *testing.T) {
	d := &DrainController{}
	n := 0
	want := errors.New("drain")
	d.Add(func(context.Context) error { n++; return want })
	d.Add(func(context.Context) error { n++; return nil })
	if err := d.Drain(context.Background()); !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	_ = d.Drain(context.Background())
	if n != 2 {
		t.Fatalf("hooks ran %d times", n)
	}
}
