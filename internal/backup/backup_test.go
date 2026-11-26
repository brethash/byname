package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDB is a one-page SQLite file header (512-byte pages) carrying marker,
// enough for store.CheckSnapshot.
func fakeDB(marker string) []byte {
	b := make([]byte, 512)
	copy(b, "SQLite format 3\x00")
	binary.BigEndian.PutUint16(b[16:], 512)
	binary.BigEndian.PutUint32(b[24:], 1) // change counter
	binary.BigEndian.PutUint32(b[28:], 1) // pages
	binary.BigEndian.PutUint32(b[92:], 1) // version-valid-for = change counter
	copy(b[100:], marker)
	return b
}

type fakeUploader struct {
	puts map[string][]byte
}

func (f *fakeUploader) Put(_ context.Context, key string, body []byte, _ string) error {
	if f.puts == nil {
		f.puts = map[string][]byte{}
	}
	f.puts[key] = body
	return nil
}

func TestRunUploadsNewestOnce(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "state", "last-upload")
	opt := Options{SnapshotDir: dir, Prefix: "/byname/", StateFile: state}
	up := &fakeUploader{}

	if _, err := Run(context.Background(), up, opt); err == nil {
		t.Fatal("expected error with no snapshots")
	}

	os.WriteFile(filepath.Join(dir, "byname-20261101T000000.000Z.db"), fakeDB("old"), 0o640)
	os.WriteFile(filepath.Join(dir, "byname-20261124T000000.000Z.db"), fakeDB("newest"), 0o640)
	os.WriteFile(filepath.Join(dir, ".byname-20261125T000000.000Z.db.tmp"), []byte("partial"), 0o640)
	os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("x"), 0o640)

	res, err := Run(context.Background(), up, opt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Key != "byname/byname-20261124T000000.000Z.db.gz" || res.Skipped {
		t.Fatalf("result = %+v", res)
	}
	zr, err := gzip.NewReader(bytes.NewReader(up.puts[res.Key]))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if !bytes.Equal(got, fakeDB("newest")) {
		t.Fatalf("uploaded content = %q", got)
	}

	res, err = Run(context.Background(), up, opt)
	if err != nil || !res.Skipped || len(up.puts) != 1 {
		t.Fatalf("second run = %+v, %v (puts=%d)", res, err, len(up.puts))
	}
}

// An empty or partial newest snapshot is never uploaded, nor recorded as
// done.
func TestRunRefusesBadSnapshot(t *testing.T) {
	for name, body := range map[string][]byte{
		"empty":     nil,
		"no header": []byte(strings.Repeat("x", 4096)),
		"truncated": fakeDB("x")[:300],
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(t.TempDir(), "last-upload")
			os.WriteFile(filepath.Join(dir, "byname-20261101T000000.000Z.db"), fakeDB("older, good"), 0o640)
			os.WriteFile(filepath.Join(dir, "byname-20261124T000000.000Z.db"), body, 0o640)
			up := &fakeUploader{}
			_, err := Run(context.Background(), up, Options{SnapshotDir: dir, Prefix: "byname/", StateFile: state})
			if err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if len(up.puts) != 0 {
				t.Fatalf("uploaded %v", up.puts)
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatalf("state file written: %v", err)
			}
		})
	}
}
