// Package objstore gives the catalog and planner one view over a local
// directory and an S3 bucket, with byte accounting on reads. The accounting is
// how the POC proves that discovery reads Parquet footers only.
package objstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rksurwase/fedsearch/internal/config"
)

type Object struct {
	Key      string    // relative to the store root, '/'-separated
	Size     int64     //
	ETag     string    // change detector: S3 ETag, or size+mtime locally
	Modified time.Time //
}

type Store interface {
	// List returns all objects under prefix, recursively, sorted by key.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Dirs returns the immediate child "directories" under prefix.
	Dirs(ctx context.Context, prefix string) ([]string, error)
	// ReaderAt opens a random-access reader for footer reads.
	ReaderAt(ctx context.Context, key string, size int64) (*CountingReaderAt, error)
	// URI returns the location an engine uses to read the object.
	URI(key string) string
}

// Open returns a store for a root such as "/data/cold" or "s3://bucket/prefix".
func Open(root string, s3cfg *config.S3) (Store, error) {
	if strings.HasPrefix(root, "s3://") {
		if s3cfg == nil {
			return nil, fmt.Errorf("%s: s3 settings required", root)
		}
		return newS3(root, *s3cfg)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Local{Root: abs}, nil
}

// CountingReaderAt counts bytes and calls so tests and benchmarks can assert
// footer-only access.
type CountingReaderAt struct {
	r     io.ReaderAt
	close func() error
	bytes atomic.Int64
	calls atomic.Int64
}

func (c *CountingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.bytes.Add(int64(n))
	c.calls.Add(1)
	return n, err
}

func (c *CountingReaderAt) BytesRead() int64 { return c.bytes.Load() }
func (c *CountingReaderAt) Calls() int64     { return c.calls.Load() }
func (c *CountingReaderAt) Close() error {
	if c.close != nil {
		return c.close()
	}
	return nil
}

// --- local filesystem -------------------------------------------------------

type Local struct{ Root string }

func (l *Local) full(key string) string { return filepath.Join(l.Root, filepath.FromSlash(key)) }

func (l *Local) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	base := l.full(prefix)
	err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(l.Root, p)
		out = append(out, Object{
			Key: filepath.ToSlash(rel), Size: info.Size(), Modified: info.ModTime(),
			ETag: fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano()),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, err
}

func (l *Local) Dirs(_ context.Context, prefix string) ([]string, error) {
	entries, err := os.ReadDir(l.full(prefix))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, path.Join(prefix, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

func (l *Local) ReaderAt(_ context.Context, key string, _ int64) (*CountingReaderAt, error) {
	f, err := os.Open(l.full(key))
	if err != nil {
		return nil, err
	}
	return &CountingReaderAt{r: f, close: f.Close}, nil
}

func (l *Local) URI(key string) string { return l.full(key) }
