package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var (
	ctx   = context.Background()
	alice = Actor{Login: "alice@example.com", Node: "alice-laptop", IP: "100.64.0.1"}
	bob   = Actor{IP: "100.64.0.2"}
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "byname.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	clock := time.UnixMilli(1_700_000_000_000)
	s.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	return s
}

func mustCreate(t *testing.T, s *Store, name, target string) Link {
	t.Helper()
	l, err := s.Create(ctx, Fields{Name: name, Target: target}, alice)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return l
}

func TestCreateGetAndKeyUniqueness(t *testing.T) {
	s := openTest(t)
	l := mustCreate(t, s, "Wiki-Home", "https://wiki.example.com")
	if l.Key != "wikihome" || l.Rev != 1 || l.CreatedBy != "alice@example.com (alice-laptop)" {
		t.Fatalf("unexpected link %+v", l)
	}
	got, err := s.Get(ctx, "wikihome")
	if err != nil || got.ID != l.ID {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := s.Create(ctx, Fields{Name: "wiki_home", Target: "https://x.com"}, bob); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate key err = %v", err)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
}

func TestMaxLinks(t *testing.T) {
	s := openTest(t)
	s.SetMaxLinks(1)
	a := mustCreate(t, s, "a", "https://a.com")
	if _, err := s.Create(ctx, Fields{Name: "b", Target: "https://b.com"}, alice); !errors.Is(err, ErrLimit) {
		t.Fatalf("create over cap err = %v, want ErrLimit", err)
	}
	if r, _ := s.Import(ctx, []Fields{{Name: "c", Target: "https://c.com"}}, alice, ImportOptions{}); r.Created != 0 || len(r.Errors) != 1 || r.Committed {
		t.Fatalf("import over cap = %+v", r)
	}
	// Delete a, create b, then restoring or reverting a must not exceed the cap.
	if err := s.Delete(ctx, a.ID, a.Rev, alice); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, "b", "https://b.com")
	if _, err := s.Restore(ctx, a.ID, alice); !errors.Is(err, ErrLimit) {
		t.Fatalf("restore over cap err = %v", err)
	}
	hist, _ := s.History(ctx, a.ID, 0, 0)
	if _, err := s.RevertTo(ctx, hist[len(hist)-1].ID, alice); !errors.Is(err, ErrLimit) {
		t.Fatalf("revert-undelete over cap err = %v", err)
	}
}

func TestUpdateConcurrencyAndHistory(t *testing.T) {
	s := openTest(t)
	l := mustCreate(t, s, "bank", "https://bank.example.com")
	l2, err := s.Update(ctx, l.ID, l.Rev, Fields{Name: "bank", Target: "https://new.example.com"}, bob)
	if err != nil {
		t.Fatal(err)
	}
	if l2.Rev != 2 || l2.UpdatedBy != "100.64.0.2" {
		t.Fatalf("unexpected %+v", l2)
	}
	// Stale revision is refused.
	if _, err := s.Update(ctx, l.ID, l.Rev, Fields{Name: "bank", Target: "https://evil.example.com"}, bob); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update err = %v", err)
	}
	// No-op update records nothing.
	if _, err := s.Update(ctx, l.ID, l2.Rev, l2.Fields(), bob); err != nil {
		t.Fatal(err)
	}
	hist, err := s.History(ctx, l.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].Action != "update" || hist[1].Action != "create" {
		t.Fatalf("history = %+v", hist)
	}
	if hist[0].Before.Target != "https://bank.example.com" || hist[0].After.Target != "https://new.example.com" {
		t.Fatalf("update event before/after wrong: %+v %+v", hist[0].Before, hist[0].After)
	}
	if hist[0].Actor != bob {
		t.Fatalf("actor = %+v", hist[0].Actor)
	}
}

func TestRenameCollision(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, "a", "https://a.com")
	b := mustCreate(t, s, "b", "https://b.com")
	if _, err := s.Update(ctx, b.ID, b.Rev, Fields{Name: "A", Target: "https://b.com"}, alice); !errors.Is(err, ErrExists) {
		t.Fatalf("rename into existing key err = %v", err)
	}
}

func TestDeleteRestoreRevert(t *testing.T) {
	s := openTest(t)
	l := mustCreate(t, s, "docs", "https://v1.example.com")
	l, _ = s.Update(ctx, l.ID, l.Rev, Fields{Name: "docs", Target: "https://v2.example.com"}, alice)
	// Deleting a revision that isn't the current one is a conflict.
	if err := s.Delete(ctx, l.ID, l.Rev-1, bob); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete with a stale rev: err = %v, want ErrConflict", err)
	}
	if _, err := s.Get(ctx, "docs"); err != nil {
		t.Fatalf("stale delete took effect: %v", err)
	}
	if err := s.Delete(ctx, l.ID, l.Rev, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "docs"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted link still live: %v", err)
	}
	if err := s.Delete(ctx, l.ID, l.Rev+1, bob); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete err = %v", err)
	}
	// A new link may take the name of a deleted one...
	other := mustCreate(t, s, "docs", "https://other.example.com")
	// ...and then the old one can't be restored over it.
	if _, err := s.Restore(ctx, l.ID, alice); !errors.Is(err, ErrExists) {
		t.Fatalf("restore over live key err = %v", err)
	}
	if err := s.Delete(ctx, other.ID, other.Rev, alice); err != nil {
		t.Fatal(err)
	}
	r, err := s.Restore(ctx, l.ID, alice)
	if err != nil || r.Deleted() || r.Target != "https://v2.example.com" {
		t.Fatalf("restore = %+v, %v", r, err)
	}
	// Revert to the create event's version.
	hist, _ := s.History(ctx, l.ID, 0, 0)
	create := hist[len(hist)-1]
	if create.Action != "create" {
		t.Fatalf("oldest event = %s", create.Action)
	}
	v, err := s.RevertTo(ctx, create.ID, bob)
	if err != nil || v.Target != "https://v1.example.com" {
		t.Fatalf("revert = %+v, %v", v, err)
	}
	// A delete event has no "after" version.
	hist, _ = s.History(ctx, l.ID, 0, 0)
	for _, e := range hist {
		if e.Action == "delete" {
			if _, err := s.RevertTo(ctx, e.ID, bob); !errors.Is(err, ErrNotRevertible) {
				t.Fatalf("revert to delete err = %v", err)
			}
		}
	}
	if hist[0].Action != "revert" || hist[0].Actor != bob {
		t.Fatalf("newest event = %+v", hist[0])
	}
}

func TestAuditLogIsAppendOnly(t *testing.T) {
	s := openTest(t)
	l := mustCreate(t, s, "x", "https://x.com")
	if _, err := s.db.ExecContext(ctx, `UPDATE link_events SET actor_ip = 'forged'`); err == nil ||
		!strings.Contains(err.Error(), "append-only") {
		t.Fatalf("update of audit log err = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM link_events`); err == nil {
		t.Fatal("delete from audit log succeeded")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, l.ID); err == nil {
		t.Fatal("hard delete of link succeeded")
	}
}

func TestListSearchSort(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, "alpha", "https://a.example.com")
	b := mustCreate(t, s, "beta", "https://b.example.com/100%_done")
	mustCreate(t, s, "gamma-ray", "https://c.example.com")
	if err := s.AddClicks(ctx, map[int64]int64{b.ID: 5}, time.Now()); err != nil {
		t.Fatal(err)
	}
	all, _ := s.List(ctx, ListOptions{})
	if len(all) != 3 || all[0].Name != "alpha" {
		t.Fatalf("list = %v", names(all))
	}
	pop, _ := s.List(ctx, ListOptions{Sort: "popular", Limit: 1})
	if len(pop) != 1 || pop[0].Name != "beta" || pop[0].Clicks != 5 || pop[0].LastClick.IsZero() {
		t.Fatalf("popular = %+v", pop)
	}
	if got, _ := s.List(ctx, ListOptions{Query: "GAMMARAY"}); len(got) != 1 {
		t.Fatalf("key search = %v", names(got))
	}
	// LIKE wildcards in the query are literal.
	if got, _ := s.List(ctx, ListOptions{Query: "%_"}); len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("wildcard search = %v", names(got))
	}
	if n, _ := s.CountLive(ctx); n != 3 {
		t.Fatalf("count = %d", n)
	}
}

func TestSnapshotAndReopen(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, "keep", "https://keep.example.com")
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := s.Snapshot(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(ctx, dest); err == nil {
		t.Fatal("snapshot over existing file succeeded")
	}
	s2, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.Get(ctx, "keep"); err != nil {
		t.Fatalf("snapshot missing link: %v", err)
	}
	if v, _ := s2.SchemaVersion(ctx); v != 2 {
		t.Fatalf("schema version = %d", v)
	}
	if err := CheckSnapshot(dest); err != nil {
		t.Fatalf("CheckSnapshot(good snapshot) = %v", err)
	}
	if _, err := os.Stat(SnapshotTempName(dest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file left behind: %v", err)
	}
}

// An interrupted snapshot must never leave a file under the final name, nor
// its temporary file.
func TestSnapshotInterrupted(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, "keep", "https://keep.example.com")
	dir := t.TempDir()
	noFiles := func(t *testing.T) {
		t.Helper()
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			t.Errorf("left behind: %s", e.Name())
		}
	}

	t.Run("failure before rename", func(t *testing.T) {
		beforeSnapshotRename = func(string) error { return errors.New("power cut") }
		t.Cleanup(func() { beforeSnapshotRename = nil })
		if err := s.Snapshot(ctx, filepath.Join(dir, "a.db")); err == nil {
			t.Fatal("snapshot succeeded")
		}
		noFiles(t)
	})
	t.Run("partial write", func(t *testing.T) {
		beforeSnapshotRename = func(tmp string) error { return os.Truncate(tmp, 1000) }
		t.Cleanup(func() { beforeSnapshotRename = nil })
		if err := s.Snapshot(ctx, filepath.Join(dir, "b.db")); !errors.Is(err, ErrBadSnapshot) {
			t.Fatalf("snapshot of a truncated copy: err = %v, want ErrBadSnapshot", err)
		}
		noFiles(t)
	})
	t.Run("cancelled", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.Snapshot(cctx, filepath.Join(dir, "c.db")); err == nil {
			t.Fatal("snapshot succeeded with a cancelled context")
		}
		noFiles(t)
	})
	t.Run("leftover temporary file", func(t *testing.T) {
		dest := filepath.Join(dir, "d.db")
		if err := os.WriteFile(SnapshotTempName(dest), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := s.Snapshot(ctx, dest); err != nil {
			t.Fatal(err)
		}
		if err := CheckSnapshot(dest); err != nil {
			t.Fatal(err)
		}
		os.Remove(dest)
		noFiles(t)
	})
}

func TestCheckSnapshot(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, "keep", "https://keep.example.com")
	dir := t.TempDir()
	good := filepath.Join(dir, "good.db")
	if err := s.Snapshot(ctx, good); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string][]byte{
		"empty":         nil,
		"text":          []byte("not a database at all, but long enough to have a header-sized prefix........................................"),
		"header only":   body[:100],
		"truncated":     body[:len(body)-1],
		"one page less": body[:len(body)-4096],
		"extra bytes":   append(append([]byte{}, body...), 0),
	}
	for name, b := range bad {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := CheckSnapshot(p); !errors.Is(err, ErrBadSnapshot) {
			t.Errorf("%s: CheckSnapshot = %v, want ErrBadSnapshot", name, err)
		}
	}
	if err := CheckSnapshot(dir); !errors.Is(err, ErrBadSnapshot) {
		t.Errorf("directory: CheckSnapshot = %v, want ErrBadSnapshot", err)
	}
	if err := CheckSnapshot(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: CheckSnapshot = %v", err)
	}
}

func TestImport(t *testing.T) {
	s := openTest(t)
	mustCreate(t, s, "exists", "https://old.example.com")
	items := []Fields{
		{Name: "exists", Target: "https://new.example.com"},
		{Name: "fresh", Target: "https://fresh.example.com"},
	}
	r, err := s.Import(ctx, items, Actor{IP: "import"}, ImportOptions{})
	if err != nil || r.Created != 1 || r.Skipped != 1 || !r.Committed {
		t.Fatalf("import = %+v, %v", r, err)
	}
	r, err = s.Import(ctx, items, Actor{IP: "import"}, ImportOptions{Overwrite: true})
	if err != nil || r.Updated != 1 || r.Skipped != 1 || !r.Committed {
		t.Fatalf("overwrite import = %+v, %v", r, err)
	}
	if l, _ := s.Get(ctx, "exists"); l.Target != "https://new.example.com" {
		t.Fatalf("overwrite didn't apply: %+v", l)
	}
	// The same key twice in one file: the second sees the first.
	r, err = s.Import(ctx, []Fields{{Name: "dup", Target: "https://a.example.com"}, {Name: "D-U-P", Target: "https://b.example.com"}},
		Actor{IP: "import"}, ImportOptions{})
	if err != nil || r.Created != 1 || r.Skipped != 1 {
		t.Fatalf("duplicate import = %+v, %v", r, err)
	}
}

func TestImportDryRun(t *testing.T) {
	s := openTest(t)
	r, err := s.Import(ctx, []Fields{{Name: "a", Target: "https://a.example.com"}}, Actor{IP: "import"}, ImportOptions{DryRun: true})
	if err != nil || r.Created != 1 || r.Committed {
		t.Fatalf("dry run = %+v, %v", r, err)
	}
	if n, _ := s.CountLive(ctx); n != 0 {
		t.Fatalf("dry run created %d links", n)
	}
	if ev, _ := s.RecentEvents(ctx, 10); len(ev) != 0 {
		t.Fatalf("dry run left %d history entries", len(ev))
	}
}

// Import honours the link cap, and a failure anywhere imports nothing.
func TestImportIsAllOrNothing(t *testing.T) {
	s := openTest(t)
	s.SetMaxLinks(3)
	mustCreate(t, s, "one", "https://one.example.com")
	items := []Fields{
		{Name: "two", Target: "https://two.example.com"},
		{Name: "three", Target: "https://three.example.com"},
		{Name: "four", Target: "https://four.example.com"},
		{Name: "five", Target: "https://five.example.com"},
	}
	r, err := s.Import(ctx, items, Actor{IP: "import"}, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Committed || r.Created != 2 || len(r.Errors) != 2 || !strings.Contains(r.Errors[0], "four") {
		t.Fatalf("import over the cap = %+v", r)
	}
	if n, _ := s.CountLive(ctx); n != 1 {
		t.Fatalf("%d live links after a failed import, want 1", n)
	}
	if ev, _ := s.RecentEvents(ctx, 10); len(ev) != 1 {
		t.Fatalf("%d history entries after a failed import, want 1", len(ev))
	}
	// Within the cap it goes through.
	r, err = s.Import(ctx, items[:2], Actor{IP: "import"}, ImportOptions{})
	if err != nil || !r.Committed || r.Created != 2 {
		t.Fatalf("import within the cap = %+v, %v", r, err)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "byname.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`PRAGMA user_version = 99`)
	s.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open newer schema err = %v", err)
	}
}

func names(ls []Link) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func TestPragmasApplied(t *testing.T) {
	s := openTest(t)
	var mode string
	var sync, fk, busy int
	s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	s.db.QueryRow(`PRAGMA synchronous`).Scan(&sync)
	s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy)
	if mode != "wal" || sync != 2 || fk != 1 || busy != 5000 {
		t.Fatalf("journal_mode=%s synchronous=%d foreign_keys=%d busy_timeout=%d", mode, sync, fk, busy)
	}
}

func TestHistoryPages(t *testing.T) {
	s := openTest(t)
	l := mustCreate(t, s, "h", "https://v0.example.com")
	for i := 1; i < 8; i++ {
		var err error
		if l, err = s.Update(ctx, l.ID, l.Rev, Fields{Name: "h", Target: "https://v" + strconv.Itoa(i) + ".example.com"}, alice); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.History(ctx, l.ID, 0, 0)
	if err != nil || len(all) != 8 {
		t.Fatalf("History = %d entries, %v", len(all), err)
	}
	var paged []Event
	for before := int64(0); ; {
		page, err := s.History(ctx, l.ID, before, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 3 {
			t.Fatalf("page of %d entries, limit 3", len(page))
		}
		paged = append(paged, page...)
		if len(page) < 3 {
			break
		}
		before = page[len(page)-1].ID
	}
	if len(paged) != len(all) {
		t.Fatalf("pages hold %d entries, want %d", len(paged), len(all))
	}
	for i := range all {
		if paged[i].ID != all[i].ID {
			t.Fatalf("entry %d: %d, want %d (newest first, no gaps)", i, paged[i].ID, all[i].ID)
		}
	}
}

func TestListOffset(t *testing.T) {
	s := openTest(t)
	for _, n := range []string{"a", "b", "c"} {
		mustCreate(t, s, n, "https://"+n+".example.com")
	}
	got, err := s.List(ctx, ListOptions{Limit: 2, Offset: 1})
	if err != nil || strings.Join(names(got), ",") != "b,c" {
		t.Fatalf("List(offset 1) = %v, %v", names(got), err)
	}
}
