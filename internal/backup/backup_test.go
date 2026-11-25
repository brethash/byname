package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

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

	os.WriteFile(filepath.Join(dir, "byname-20261101T000000.000Z.db"), []byte("old"), 0o640)
	os.WriteFile(filepath.Join(dir, "byname-20261124T000000.000Z.db"), []byte("newest"), 0o640)
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
	if string(got) != "newest" {
		t.Fatalf("uploaded content = %q", got)
	}

	res, err = Run(context.Background(), up, opt)
	if err != nil || !res.Skipped || len(up.puts) != 1 {
		t.Fatalf("second run = %+v, %v (puts=%d)", res, err, len(up.puts))
	}
}
