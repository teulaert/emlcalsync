package blob

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/teulaert/emlcalsync/internal/model"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newStore(t)
	raw := []byte("From: a@example.com\r\nSubject: hello\r\n\r\nbody body body\r\n")

	sha, created, err := s.Put(raw)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !created {
		t.Fatal("first Put should report created=true")
	}
	if sha != SHA256(raw) {
		t.Fatalf("sha mismatch: %s", sha)
	}
	if !s.Exists(sha) {
		t.Fatal("Exists=false after Put")
	}

	got, err := s.Get(sha)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("round trip mismatch:\n got %q\nwant %q", got, raw)
	}

	// Path layout: <root>/<first2>/<sha>.eml.zst
	want := filepath.Join(s.Root(), sha[:2], sha+".eml.zst")
	if s.Path(sha) != want {
		t.Fatalf("Path = %s, want %s", s.Path(sha), want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("blob not at expected path: %v", err)
	}
}

func TestPutIdempotent(t *testing.T) {
	s := newStore(t)
	raw := bytes.Repeat([]byte("compressible text, over and over. "), 500)

	sha1, created1, err := s.Put(raw)
	if err != nil || !created1 {
		t.Fatalf("first Put: %v created=%v", err, created1)
	}
	fi1, err := os.Stat(s.Path(sha1))
	if err != nil {
		t.Fatal(err)
	}

	sha2, created2, err := s.Put(raw)
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if sha2 != sha1 {
		t.Fatalf("sha changed: %s vs %s", sha1, sha2)
	}
	if created2 {
		t.Fatal("second Put should report created=false")
	}
	fi2, err := os.Stat(s.Path(sha1))
	if err != nil {
		t.Fatal(err)
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("existing blob was rewritten")
	}
	// zstd should have done something useful on this input.
	if fi1.Size() >= int64(len(raw)) {
		t.Fatalf("no compression: %d >= %d", fi1.Size(), len(raw))
	}
	count, bytesOnDisk, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || bytesOnDisk != fi1.Size() {
		t.Fatalf("Stats = %d,%d want 1,%d", count, bytesOnDisk, fi1.Size())
	}
}

func TestGetMissing(t *testing.T) {
	s := newStore(t)
	missing := SHA256([]byte("never stored"))
	_, err := s.Get(missing)
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
	if s.Exists(missing) {
		t.Fatal("Exists=true for missing blob")
	}
	if _, err := s.Size(missing); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Size(missing) = %v, want ErrNotFound", err)
	}
	// A syntactically invalid key is "not found", never a panic.
	if _, err := s.Get("zzz"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Get(bad) = %v, want ErrNotFound", err)
	}
}

func TestGetDetectsCorruption(t *testing.T) {
	s := newStore(t)

	// 1. Garbage that is not a zstd frame at all.
	shaA := SHA256([]byte("a"))
	pathA := s.Path(shaA)
	if err := os.MkdirAll(filepath.Dir(pathA), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathA, []byte("not zstd at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(shaA); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get(garbage) = %v, want ErrCorrupt", err)
	}

	// 2. A valid zstd frame whose content hashes to something else: store
	//    blob B's bytes under blob A's name.
	shaB, _, err := s.Put([]byte("the real contents of B"))
	if err != nil {
		t.Fatal(err)
	}
	compB, err := os.ReadFile(s.Path(shaB))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathA, compB, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = s.Get(shaA)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get(mislabelled) = %v, want ErrCorrupt", err)
	}
	// The genuine blob still reads fine.
	if _, err := s.Get(shaB); err != nil {
		t.Fatalf("Get(B): %v", err)
	}
}

func TestWalkAndDelete(t *testing.T) {
	s := newStore(t)
	want := map[string]bool{}
	for i := range 25 {
		sha, _, err := s.Put(fmt.Appendf(nil, "message number %d with some padding text", i))
		if err != nil {
			t.Fatal(err)
		}
		want[sha] = true
	}
	// A stray file in the archive must not be reported as a blob.
	if err := os.WriteFile(filepath.Join(s.Root(), "README"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.tempFile(); err != nil {
		t.Fatal(err)
	}

	got := map[string]int64{}
	if err := s.Walk(func(sha string, size int64) error {
		if size <= 0 {
			t.Errorf("blob %s has size %d", sha, size)
		}
		got[sha] = size
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Walk saw %d blobs, want %d", len(got), len(want))
	}
	for sha := range want {
		if _, ok := got[sha]; !ok {
			t.Fatalf("Walk missed %s", sha)
		}
	}

	// Errors from fn abort the walk.
	sentinel := errors.New("stop")
	n := 0
	if err := s.Walk(func(string, int64) error {
		n++
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Walk error = %v, want sentinel", err)
	}
	if n != 1 {
		t.Fatalf("Walk kept going after error (%d calls)", n)
	}

	// Delete is idempotent.
	var one string
	for sha := range want {
		one = sha
		break
	}
	if err := s.Delete(one); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Exists(one) {
		t.Fatal("blob still present after Delete")
	}
	if err := s.Delete(one); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	count, _, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if count != len(want)-1 {
		t.Fatalf("count after delete = %d, want %d", count, len(want)-1)
	}

	if err := s.CleanTemp(); err != nil {
		t.Fatalf("CleanTemp: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(s.Root(), "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("tmp not empty after CleanTemp: %d entries", len(entries))
	}
}

func TestReader(t *testing.T) {
	s := newStore(t)
	raw := bytes.Repeat([]byte("streamed content\n"), 10000)
	sha, _, err := s.Put(raw)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := s.Reader(sha)
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("streamed content mismatch")
	}
	if _, err := s.Reader(SHA256([]byte("nope"))); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Reader(missing) = %v, want ErrNotFound", err)
	}
}

func TestConcurrentPut(t *testing.T) {
	s := newStore(t)
	raws := make([][]byte, 8)
	for i := range raws {
		raws[i] = fmt.Appendf(nil, "concurrent message %d\n%s", i, bytes.Repeat([]byte("x"), 1000))
	}

	var mu sync.Mutex
	createdCount := map[string]int{}
	var wg sync.WaitGroup
	for range 10 {
		for _, raw := range raws {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sha, created, err := s.Put(raw)
				if err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				mu.Lock()
				if created {
					createdCount[sha]++
				}
				mu.Unlock()
			}()
		}
	}
	wg.Wait()

	for sha, n := range createdCount {
		if n != 1 {
			t.Errorf("blob %s reported created %d times", sha, n)
		}
	}
	count, _, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if count != len(raws) {
		t.Fatalf("stored %d blobs, want %d", count, len(raws))
	}
	for _, raw := range raws {
		got, err := s.Get(SHA256(raw))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !bytes.Equal(got, raw) {
			t.Fatal("content mismatch after concurrent Put")
		}
	}
}

func TestOpenCreatesDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "b", "blobs")
	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "tmp")); err != nil {
		t.Fatalf("tmp dir not created: %v", err)
	}
	// Reopening an existing store is fine and sees the same data.
	sha, _, err := s.Put([]byte("persisted"))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !s2.Exists(sha) {
		t.Fatal("reopened store does not see the blob")
	}
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") should fail")
	}
}
