// Package objstore keeps receipt images outside the database. A Store is a
// content-addressed blob store: Put returns the key (the hex sha256 of the bytes) and
// Get returns the bytes for a key.
//
// The only implementation is a directory on the local disk (FS). It is meant for one
// host, which is how the project is deployed; MinIO or S3 behind the same interface is
// the extension path and is not built.
//
// Properties the callers rely on, all tested:
//
//   - Atomic: a reader sees a whole object or none. Bytes go to a temporary file in the
//     same directory, are synced, and are renamed into place; the directory is synced
//     after the rename, so an object whose Put returned survives a power cut.
//   - Idempotent: the same bytes always have the same key, so concurrent or repeated
//     Puts of one image are harmless (the second finds the object and returns).
//   - Private: the directory is 0700 and objects are 0600.
//   - Keys never reach the file system unchecked: a key must be exactly 64 lowercase hex
//     digits, so a key that came from anywhere (a database row, a bug) cannot name a
//     path outside the directory.
//
// There is no delete and no garbage collection. A Put whose database transaction later
// fails leaves a blob that nothing points at; it is harmless and not swept (DESIGN.md,
// "M6 plan decisions"). Because identical images share a blob, a delete operation
// would need reference counting; add it with a retention policy, not before.
package objstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// ErrNotFound means no object has the key.
var ErrNotFound = errors.New("objstore: object not found")

// ErrBadKey means the key is not 64 lowercase hex digits.
var ErrBadKey = errors.New("objstore: malformed key")

// ErrCorrupt means the stored bytes are not what the key promises (damaged, replaced or
// over the size limit). Retrying will not help.
var ErrCorrupt = errors.New("objstore: stored object is corrupt")

// MaxObjectBytes bounds what Put accepts and Get reads, so a caller bug cannot fill the
// disk or memory. Receipts are re-encoded images of a few megabytes.
const MaxObjectBytes = 32 << 20

// Store is a content-addressed blob store.
type Store interface {
	// Put stores data and returns its key. It is safe to call concurrently and
	// repeatedly with the same data.
	Put(ctx context.Context, data []byte) (key string, err error)
	// Get returns the object, or ErrNotFound, or ErrBadKey.
	Get(ctx context.Context, key string) ([]byte, error)
}

// FS stores objects as files named by key under one directory.
type FS struct {
	dir string
}

// NewFS opens a store in dir, creating it (mode 0700) if needed. A directory that
// already exists is used as it is, and is refused if group or others can access it:
// receipt images are private data, and silently tightening someone's directory would be
// a surprise of its own.
func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, errors.New("objstore: directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("objstore: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("objstore: %w", err)
	}
	// (MkdirAll fails when dir exists and is not a directory, so no separate check.)
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("objstore: %s has mode %v; receipt images are private, use 0700", dir, fi.Mode().Perm())
	}
	return &FS{dir: dir}, nil
}

// Key is the key Put would return for data.
func Key(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *FS) path(key string) string { return filepath.Join(s.dir, key) }

// tmpPrefix marks files that are still being written. No key starts with it (keys are
// hex), so a leftover temporary file from a crash can never be read as an object.
const tmpPrefix = ".tmp-"

// Put implements Store.
func (s *FS) Put(ctx context.Context, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > MaxObjectBytes {
		return "", fmt.Errorf("objstore: object size %d outside 1-%d bytes", len(data), MaxObjectBytes)
	}
	key := Key(data)
	final := s.path(key)
	// Content addressing makes an existing object identical to this one, provided it is
	// whole; rename (below) is what guarantees that, so a name that exists is complete.
	// (Damage after the fact is caught by Get, which checks the content against the key.)
	if _, err := os.Stat(final); err == nil {
		return key, nil
	}

	// CreateTemp makes the file 0600. Whatever happens next, the temporary name must not
	// outlive this call: after a successful rename it no longer exists and the remove is
	// a harmless no-op.
	tmp, err := os.CreateTemp(s.dir, tmpPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("objstore: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best effort; see above
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("objstore: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("objstore: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("objstore: close: %w", err)
	}
	// rename replaces an existing name atomically; two writers of the same key both
	// write identical bytes, so whichever wins leaves the same object.
	if err := os.Rename(tmpName, final); err != nil {
		return "", fmt.Errorf("objstore: rename: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		return "", fmt.Errorf("objstore: sync dir: %w", err)
	}
	return key, nil
}

// Get implements Store.
func (s *FS) Get(ctx context.Context, key string) ([]byte, error) {
	if !validKey(key) {
		return nil, ErrBadKey
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("objstore: %w", err)
	}
	defer f.Close()
	// One byte past the maximum tells "exactly the limit" from "larger".
	b, err := io.ReadAll(io.LimitReader(f, MaxObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("objstore: read: %w", err)
	}
	if len(b) > MaxObjectBytes {
		return nil, fmt.Errorf("%w: object %s is larger than %d bytes", ErrCorrupt, key, MaxObjectBytes)
	}
	// The name is a claim about the content; a file that does not match it was damaged
	// or replaced, and handing it to a decoder or an OCR engine would be a mistake.
	if Key(b) != key {
		return nil, fmt.Errorf("%w: object %s does not match its key", ErrCorrupt, key)
	}
	return b, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Some file systems (certain network and container mounts) refuse fsync on a
	// directory with EINVAL; the rename is still atomic there, only its durability
	// across a power cut is not guaranteed.
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}
