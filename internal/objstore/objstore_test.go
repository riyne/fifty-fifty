package objstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/riyne/fifty-fifty/internal/objstore"
)

func newStore(t *testing.T) (*objstore.FS, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "receipts")
	s, err := objstore.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestPutGet_RoundTripsAndIsContentAddressed(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	data := []byte("receipt bytes")
	key, err := s.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if key != objstore.Key(data) || len(key) != 64 {
		t.Fatalf("key %q", key)
	}
	got, err := s.Get(ctx, key)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("get: %q %v", got, err)
	}
	if again, err := s.Put(ctx, data); err != nil || again != key {
		t.Fatalf("second put: %q %v", again, err)
	}
	other, _ := s.Put(ctx, []byte("other"))
	if other == key {
		t.Fatal("different bytes, same key")
	}
}

func TestPut_RefusesEmptyAndOversize(t *testing.T) {
	s, _ := newStore(t)
	for name, data := range map[string][]byte{"empty": nil, "oversize": make([]byte, objstore.MaxObjectBytes+1)} {
		if _, err := s.Put(context.Background(), data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := s.Put(context.Background(), make([]byte, objstore.MaxObjectBytes)); err != nil {
		t.Errorf("an object of exactly the maximum was refused: %v", err)
	}
}

func TestPut_CanceledContextWritesNothing(t *testing.T) {
	s, dir := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Put(ctx, []byte("x")); err == nil {
		t.Fatal("put with a canceled context succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("%d files left", len(entries))
	}
}

func TestGet_Refusals(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	for _, k := range []string{
		"", "abc", "../" + strings.Repeat("a", 61), strings.Repeat("A", 64), strings.Repeat("g", 64),
		strings.Repeat("a", 63), strings.Repeat("a", 65), ".tmp-" + strings.Repeat("a", 59), strings.Repeat("a", 63) + "/",
		strings.Repeat("a", 32) + "/../" + strings.Repeat("a", 28),
	} {
		if _, err := s.Get(ctx, k); !errors.Is(err, objstore.ErrBadKey) {
			t.Errorf("key %q: err=%v, want ErrBadKey", k, err)
		}
	}
	if _, err := s.Get(ctx, strings.Repeat("a", 64)); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("missing object: err=%v", err)
	}

	// A file that does not match its name was damaged or replaced: refuse to return it.
	key, _ := s.Put(ctx, []byte("genuine"))
	if err := os.WriteFile(filepath.Join(dir, key), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, key); err == nil || errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("tampered object: err=%v, want a mismatch error", err)
	}
}

func TestPut_Permissions(t *testing.T) {
	s, dir := newStore(t)
	key, err := s.Put(context.Background(), []byte("private"))
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(dir, key))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("object mode %v, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v, want 0700", di.Mode().Perm())
	}
}

func TestPut_LeavesNothingButTheObject(t *testing.T) {
	s, dir := newStore(t)
	key, _ := s.Put(context.Background(), []byte("one"))
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != key {
		t.Fatalf("directory holds %v, want only %s", entries, key)
	}
}

// Many writers of the same image, and readers running meanwhile: every read sees the
// whole object, never a partial one, and every Put returns the same key.
func TestPut_ConcurrentSameObjectIsSafe(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	want := objstore.Key(data)

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if k, err := s.Put(ctx, data); err != nil || k != want {
				errs <- errors.New("put: " + k + " " + errString(err))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				b, err := s.Get(ctx, want)
				if errors.Is(err, objstore.ErrNotFound) {
					continue // not written yet
				}
				if err != nil || !bytes.Equal(b, data) {
					errs <- errors.New("get saw a partial or wrong object: " + errString(err))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("%d files left, want 1", len(entries))
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// A temporary file left by a crash is never an object: it has no valid key.
func TestLeftoverTempFileIsNotAnObject(t *testing.T) {
	s, dir := newStore(t)
	if err := os.WriteFile(filepath.Join(dir, ".tmp-123"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), ".tmp-123"); !errors.Is(err, objstore.ErrBadKey) {
		t.Fatalf("err=%v", err)
	}
	key, err := s.Put(context.Background(), []byte("half"))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := s.Get(context.Background(), key); err != nil || string(b) != "half" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestNewFS(t *testing.T) {
	if _, err := objstore.NewFS(""); err == nil {
		t.Error("empty directory accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := objstore.NewFS(file); err == nil {
		t.Error("a regular file accepted as the directory")
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := objstore.NewFS(open); err == nil {
		t.Error("a world-readable directory accepted")
	}
	// An existing private directory is fine, and so is opening it twice.
	private := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := objstore.NewFS(private); err != nil {
			t.Errorf("open %d: %v", i, err)
		}
	}
	// Nested directories are created.
	if _, err := objstore.NewFS(filepath.Join(t.TempDir(), "a", "b")); err != nil {
		t.Errorf("nested: %v", err)
	}
}

// Get stops reading at the maximum even if the file on disk is far larger: it reports the
// size without reading a huge file into memory.
func TestGet_RefusesAnOversizeFileWithoutReadingItAll(t *testing.T) {
	s, dir := newStore(t)
	key := strings.Repeat("b", 64)
	f, err := os.Create(filepath.Join(dir, key))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(300 << 20); err != nil { // sparse: no disk used
		t.Fatal(err)
	}
	f.Close()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err = s.Get(context.Background(), key)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err=%v, want the size refusal", err)
	}
	// Reading the cap (32 MiB) takes under 100 MB of allocation, including growth;
	// reading the whole 300 MiB takes at least that.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 150<<20 {
		t.Errorf("Get allocated %d MiB for a 300 MiB file", grew>>20)
	}
}
