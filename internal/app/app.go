// Package app is the business layer between HTTP handlers and storage: it
// validates input, batches click counts so an idle Pi writes almost nothing to
// its SD card, and takes database snapshots for backup.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"byname/internal/link"
	"byname/internal/store"
)

// Config tunes the service.
type Config struct {
	SelfHosts        []string      // names the service answers to (redirect-loop check)
	MaxLinks         int           // live link cap; 0 = unlimited
	ClickFlush       time.Duration // how often batched clicks are written
	SnapshotDir      string        // "" disables snapshots
	SnapshotKeep     int           // snapshots kept locally
	SnapshotEvery    time.Duration // periodic snapshot interval
	SnapshotDebounce time.Duration // delay after an edit before snapshotting
	Logger           *slog.Logger
}

// Service is safe for concurrent use.
type Service struct {
	Store *store.Store // read paths use the store directly
	cfg   Config
	log   *slog.Logger

	mu     sync.Mutex
	clicks map[int64]int64

	changed chan struct{}
}

// New returns a service. Call Run to start background work.
func New(st *store.Store, cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ClickFlush <= 0 {
		cfg.ClickFlush = 5 * time.Minute
	}
	if cfg.SnapshotKeep <= 0 {
		cfg.SnapshotKeep = 14
	}
	if cfg.SnapshotEvery <= 0 {
		cfg.SnapshotEvery = 24 * time.Hour
	}
	if cfg.SnapshotDebounce <= 0 {
		cfg.SnapshotDebounce = time.Minute
	}
	st.SetMaxLinks(cfg.MaxLinks)
	return &Service{
		Store:   st,
		cfg:     cfg,
		log:     cfg.Logger,
		clicks:  make(map[int64]int64),
		changed: make(chan struct{}, 1),
	}
}

// SelfHosts returns the configured service host names.
func (s *Service) SelfHosts() []string { return s.cfg.SelfHosts }

// FieldError is a validation failure on one input field.
type FieldError struct {
	Field string // "name", "target" or "description"
	Err   error
}

func (e *FieldError) Error() string { return e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

// Input is user-supplied link data.
type Input struct {
	Name, Target, Description string
}

// Validate normalises and checks input.
func (s *Service) Validate(in Input) (store.Fields, error) {
	name, err := link.ParseName(in.Name)
	if err != nil {
		return store.Fields{}, &FieldError{"name", err}
	}
	target, err := link.ValidateTarget(in.Target, s.cfg.SelfHosts)
	if err != nil {
		return store.Fields{}, &FieldError{"target", err}
	}
	desc, err := link.ValidateDescription(in.Description)
	if err != nil {
		return store.Fields{}, &FieldError{"description", err}
	}
	return store.Fields{Name: name, Target: target, Description: desc}, nil
}

// Create validates and stores a new link.
func (s *Service) Create(ctx context.Context, in Input, a store.Actor) (store.Link, error) {
	f, err := s.Validate(in)
	if err != nil {
		return store.Link{}, err
	}
	l, err := s.Store.Create(ctx, f, a)
	if err == nil {
		s.log.Info("link created", "name", l.Name, "target", l.Target, "actor", a.Display(), "ip", a.IP)
		s.markChanged()
	}
	return l, err
}

// Update validates and applies an edit.
func (s *Service) Update(ctx context.Context, id, rev int64, in Input, a store.Actor) (store.Link, error) {
	f, err := s.Validate(in)
	if err != nil {
		return store.Link{}, err
	}
	l, err := s.Store.Update(ctx, id, rev, f, a)
	if err == nil {
		s.log.Info("link updated", "id", id, "name", l.Name, "target", l.Target, "actor", a.Display(), "ip", a.IP)
		s.markChanged()
	}
	return l, err
}

// Delete soft-deletes a link.
func (s *Service) Delete(ctx context.Context, id int64, a store.Actor) error {
	err := s.Store.Delete(ctx, id, a)
	if err == nil {
		s.log.Info("link deleted", "id", id, "actor", a.Display(), "ip", a.IP)
		s.markChanged()
	}
	return err
}

// Restore undeletes a link.
func (s *Service) Restore(ctx context.Context, id int64, a store.Actor) (store.Link, error) {
	l, err := s.Store.Restore(ctx, id, a)
	if err == nil {
		s.log.Info("link restored", "id", id, "name", l.Name, "actor", a.Display(), "ip", a.IP)
		s.markChanged()
	}
	return l, err
}

// RevertTo restores the version recorded in an audit event.
func (s *Service) RevertTo(ctx context.Context, eventID int64, a store.Actor) (store.Link, error) {
	l, err := s.Store.RevertTo(ctx, eventID, a)
	if err == nil {
		s.log.Info("link reverted", "event", eventID, "name", l.Name, "target", l.Target, "actor", a.Display(), "ip", a.IP)
		s.markChanged()
	}
	return l, err
}

// Destination computes where a request for l (with extra path and query)
// should go.
func (s *Service) Destination(l store.Link, rest, rawQuery string) (string, error) {
	return link.Expand(l.Target, rest, rawQuery, s.cfg.SelfHosts)
}

// RecordClick counts a redirect; counts reach the database on the next flush.
func (s *Service) RecordClick(id int64) {
	s.mu.Lock()
	s.clicks[id]++
	s.mu.Unlock()
}

// FlushClicks writes batched clicks now.
func (s *Service) FlushClicks(ctx context.Context) error {
	s.mu.Lock()
	batch := s.clicks
	s.clicks = make(map[int64]int64)
	s.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	if err := s.Store.AddClicks(ctx, batch, time.Now()); err != nil {
		// Put them back so a transient failure loses nothing.
		s.mu.Lock()
		for id, n := range batch {
			s.clicks[id] += n
		}
		s.mu.Unlock()
		return err
	}
	return nil
}

func (s *Service) markChanged() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Run does background work until ctx is cancelled, then flushes clicks.
func (s *Service) Run(ctx context.Context) {
	flush := time.NewTicker(s.cfg.ClickFlush)
	defer flush.Stop()
	var (
		snapTick  <-chan time.Time
		debounceC <-chan time.Time
		debounce  *time.Timer
	)
	if s.cfg.SnapshotDir != "" {
		t := time.NewTicker(s.cfg.SnapshotEvery)
		defer t.Stop()
		snapTick = t.C
		if !s.hasSnapshot() {
			s.snapshotLogged(ctx)
		}
	}
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.FlushClicks(fctx); err != nil {
				s.log.Error("final click flush failed", "err", err)
			}
			cancel()
			return
		case <-flush.C:
			if err := s.FlushClicks(ctx); err != nil {
				s.log.Error("click flush failed", "err", err)
			}
		case <-s.changed:
			if s.cfg.SnapshotDir == "" {
				continue
			}
			if debounce == nil {
				debounce = time.NewTimer(s.cfg.SnapshotDebounce)
			} else {
				debounce.Reset(s.cfg.SnapshotDebounce)
			}
			debounceC = debounce.C
		case <-debounceC:
			debounceC = nil
			s.snapshotLogged(ctx)
		case <-snapTick:
			s.snapshotLogged(ctx)
		}
	}
}

func (s *Service) snapshotLogged(ctx context.Context) {
	if err := s.FlushClicks(ctx); err != nil {
		s.log.Warn("click flush before snapshot failed", "err", err)
	}
	path, err := s.Snapshot(ctx)
	if err != nil {
		s.log.Error("snapshot failed", "err", err)
		return
	}
	s.log.Info("snapshot written", "path", path)
}

const snapPrefix, snapSuffix = "byname-", ".db"

// SnapshotName returns the file name for a snapshot taken at t. Names sort
// chronologically.
func SnapshotName(t time.Time) string {
	return snapPrefix + t.UTC().Format("20060102T150405.000Z") + snapSuffix
}

// Snapshot writes a snapshot into the snapshot dir and prunes old ones.
func (s *Service) Snapshot(ctx context.Context) (string, error) {
	return SnapshotTo(ctx, s.Store, s.cfg.SnapshotDir, s.cfg.SnapshotKeep)
}

// SnapshotTo writes a snapshot of st into dir, keeping the newest keep files.
func SnapshotTo(ctx context.Context, st *store.Store, dir string, keep int) (string, error) {
	if dir == "" {
		return "", errors.New("no snapshot directory configured")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	path := filepath.Join(dir, SnapshotName(time.Now()))
	if err := st.Snapshot(ctx, path); err != nil {
		return "", err
	}
	if err := PruneSnapshots(dir, keep); err != nil {
		return path, fmt.Errorf("snapshot written but prune failed: %w", err)
	}
	return path, nil
}

// ListSnapshots returns snapshot file names in dir, oldest first.
func ListSnapshots(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasPrefix(n, snapPrefix) && strings.HasSuffix(n, snapSuffix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// PruneSnapshots deletes all but the newest keep snapshots in dir.
func PruneSnapshots(dir string, keep int) error {
	names, err := ListSnapshots(dir)
	if err != nil {
		return err
	}
	for len(names) > keep {
		if err := os.Remove(filepath.Join(dir, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}

func (s *Service) hasSnapshot() bool {
	names, err := ListSnapshots(s.cfg.SnapshotDir)
	return err == nil && len(names) > 0
}
