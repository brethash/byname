// Package backup uploads the newest local snapshot to S3.
//
// It runs as its own systemd unit and user (byname-backup), separate from the web
// service: only this process ever holds AWS credentials, and it can read the
// snapshot directory but not write the live database.
package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"byname/internal/app"
)

// Uploader puts an object. *S3Uploader implements it; tests use a fake.
type Uploader interface {
	Put(ctx context.Context, key string, body []byte, contentType string) error
}

// Options configures a run.
type Options struct {
	SnapshotDir string // where `byname serve` writes byname-*.db snapshots
	Prefix      string // key prefix in the bucket, e.g. "byname/"
	StateFile   string // remembers the last uploaded snapshot name
}

// Result reports what happened.
type Result struct {
	Snapshot string // newest snapshot name ("" if none)
	Key      string // uploaded object key ("" if nothing uploaded)
	Bytes    int    // compressed size
	Skipped  bool   // newest snapshot was already uploaded
}

// Run uploads the newest snapshot if it has not been uploaded yet.
func Run(ctx context.Context, up Uploader, opt Options) (Result, error) {
	names, err := app.ListSnapshots(opt.SnapshotDir)
	if err != nil {
		return Result{}, fmt.Errorf("list snapshots: %w", err)
	}
	if len(names) == 0 {
		return Result{}, errors.New("no snapshots to upload yet")
	}
	newest := names[len(names)-1]
	res := Result{Snapshot: newest}
	if last, err := os.ReadFile(opt.StateFile); err == nil && strings.TrimSpace(string(last)) == newest {
		res.Skipped = true
		return res, nil
	}
	body, err := gzipFile(filepath.Join(opt.SnapshotDir, newest))
	if err != nil {
		return res, err
	}
	key := path.Join(strings.Trim(opt.Prefix, "/"), newest+".gz")
	if err := up.Put(ctx, key, body, "application/gzip"); err != nil {
		return res, fmt.Errorf("upload %s: %w", key, err)
	}
	res.Key, res.Bytes = key, len(body)
	if err := writeState(opt.StateFile, newest); err != nil {
		return res, fmt.Errorf("uploaded, but could not record state: %w", err)
	}
	return res, nil
}

func gzipFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Name = filepath.Base(p)
	if _, err := io.Copy(zw, f); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeState(p, name string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(name+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
