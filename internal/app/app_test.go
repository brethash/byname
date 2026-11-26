package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"byname/internal/link"
	"byname/internal/store"
)

func newTest(t *testing.T, cfg Config) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "byname.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg.SelfHosts = []string{"me"}
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(st, cfg)
}

var who = store.Actor{IP: "100.64.0.1"}

func TestCreateValidates(t *testing.T) {
	s := newTest(t, Config{})
	ctx := context.Background()
	_, err := s.Create(ctx, Input{Name: "bad name", Target: "https://x.com"}, who)
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Field != "name" {
		t.Fatalf("err = %v", err)
	}
	_, err = s.Create(ctx, Input{Name: "x", Target: "javascript:alert(1)"}, who)
	if !errors.As(err, &fe) || fe.Field != "target" || !errors.Is(err, link.ErrTargetScheme) {
		t.Fatalf("err = %v", err)
	}
	_, err = s.Create(ctx, Input{Name: "loop", Target: "http://me/loop"}, who)
	if !errors.Is(err, link.ErrTargetSelf) {
		t.Fatalf("err = %v", err)
	}
	l, err := s.Create(ctx, Input{Name: " Docs ", Target: " https://docs.example.com ", Description: " hi "}, who)
	if err != nil || l.Name != "Docs" || l.Target != "https://docs.example.com" || l.Description != "hi" {
		t.Fatalf("create = %+v, %v", l, err)
	}
}

func TestClickBatching(t *testing.T) {
	s := newTest(t, Config{})
	ctx := context.Background()
	l, _ := s.Create(ctx, Input{Name: "a", Target: "https://a.com"}, who)
	for range 3 {
		s.RecordClick(l.ID)
	}
	got, _ := s.Store.GetByID(ctx, l.ID)
	if got.Clicks != 0 {
		t.Fatalf("clicks written before flush: %d", got.Clicks)
	}
	if err := s.FlushClicks(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Store.GetByID(ctx, l.ID)
	if got.Clicks != 3 {
		t.Fatalf("clicks = %d", got.Clicks)
	}
}

func TestSnapshotsAndPrune(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snaps")
	s := newTest(t, Config{SnapshotDir: dir, SnapshotKeep: 2})
	ctx := context.Background()
	for range 4 {
		if _, err := s.Snapshot(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // names have millisecond resolution
	}
	names, err := ListSnapshots(dir)
	if err != nil || len(names) != 2 {
		t.Fatalf("snapshots = %v, %v", names, err)
	}
}

// A snapshot left empty or partial by an older version (which wrote in
// place) must not stand in for a real one: startup takes a fresh snapshot.
func TestStartupReplacesPartialSnapshot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snaps")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	partial := SnapshotName(time.Now().Add(-time.Hour))
	if err := os.WriteFile(filepath.Join(dir, partial), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	s := newTest(t, Config{SnapshotDir: dir, ClickFlush: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	waitFor(t, func() bool { n, _ := ListSnapshots(dir); return len(n) == 2 })
	cancel()
	<-done
	names, _ := ListSnapshots(dir)
	if err := store.CheckSnapshot(filepath.Join(dir, names[1])); err != nil {
		t.Fatalf("new snapshot %s: %v", names[1], err)
	}
}

func TestPruneRemovesStaleTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	stale := "." + SnapshotName(time.Now().Add(-2*time.Hour)) + ".tmp"
	fresh := "." + SnapshotName(time.Now()) + ".tmp"
	for _, n := range []string{stale, fresh, ".unrelated.tmp"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * time.Hour)
		if n != fresh {
			os.Chtimes(p, old, old)
		}
	}
	if names, _ := ListSnapshots(dir); len(names) != 0 {
		t.Fatalf("temporary files listed as snapshots: %v", names)
	}
	if err := PruneSnapshots(dir, 14); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, stale)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale temporary file kept: %v", err)
	}
	for _, n := range []string{fresh, ".unrelated.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s removed: %v", n, err)
		}
	}
}

func TestRunSnapshotsAfterEditAndFlushesOnStop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snaps")
	s := newTest(t, Config{SnapshotDir: dir, SnapshotDebounce: 20 * time.Millisecond, ClickFlush: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	// Startup snapshot (none existed).
	waitFor(t, func() bool { n, _ := ListSnapshots(dir); return len(n) == 1 })
	l, err := s.Create(context.Background(), Input{Name: "a", Target: "https://a.com"}, who)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { n, _ := ListSnapshots(dir); return len(n) == 2 })

	s.RecordClick(l.ID)
	cancel()
	<-done
	got, _ := s.Store.GetByID(context.Background(), l.ID)
	if got.Clicks != 1 {
		t.Fatalf("clicks not flushed on stop: %d", got.Clicks)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
