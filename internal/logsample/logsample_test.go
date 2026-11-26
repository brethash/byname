package logsample

import (
	"testing"
	"time"
)

func TestSampler(t *testing.T) {
	s := New(time.Minute)
	t0 := time.Unix(1_000_000, 0)
	if ok, held := s.Take(t0); !ok || held != 0 {
		t.Fatalf("first = %v, %d", ok, held)
	}
	for i := 1; i <= 5; i++ {
		if ok, _ := s.Take(t0.Add(time.Duration(i) * time.Second)); ok {
			t.Fatalf("occurrence %d within the interval let through", i)
		}
	}
	if ok, held := s.Take(t0.Add(time.Minute)); !ok || held != 5 {
		t.Fatalf("after the interval = %v, %d; want true, 5", ok, held)
	}
	if ok, held := s.Take(t0.Add(3 * time.Minute)); !ok || held != 0 {
		t.Fatalf("after a quiet spell = %v, %d; want true, 0", ok, held)
	}
}
