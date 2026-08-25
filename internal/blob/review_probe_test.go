package blob

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func reviewFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("EMLCAL_REVIEW") == "1" {
		t.Errorf(format, args...)
		return
	}
	t.Logf("[review] "+format, args...)
}

// TestReviewConcurrentPutSameSHA hammers the same content from many goroutines.
func TestReviewConcurrentPutSameSHA(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Repeat([]byte("From: a@b\r\nSubject: x\r\n\r\nhello world\r\n"), 500)

	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	errs := []error{}
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, c, err := s.Put(raw)
			mu.Lock()
			if err != nil {
				errs = append(errs, err)
			} else if c {
				created++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		reviewFail(t, "concurrent Put of identical content failed: %v", errs[0])
	}
	if created != 1 {
		reviewFail(t, "concurrent Put reported created=true %d times, want exactly 1", created)
	}
	got, err := s.Get(SHA256(raw))
	if err != nil || !bytes.Equal(got, raw) {
		reviewFail(t, "Get after concurrent Put: err=%v equal=%v", err, bytes.Equal(got, raw))
	}
	// No staging files left behind.
	ents, _ := os.ReadDir(filepath.Join(s.Root(), "tmp"))
	if len(ents) != 0 {
		reviewFail(t, "concurrent Put left %d files in tmp/", len(ents))
	}
}

// TestReviewPutPermissionDenied: an unwritable archive must error and leave no
// partial blob.
func TestReviewPutPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: nope\r\n\r\nbody\r\n")
	sha := SHA256(raw)

	if err := os.Chmod(root, 0o500); err != nil {
		t.Skip("chmod unsupported")
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	_, _, err = s.Put(raw)
	if err == nil {
		reviewFail(t, "Put into a read-only archive returned no error")
	}
	_ = os.Chmod(root, 0o700)
	if s.Exists(sha) {
		reviewFail(t, "failed Put left a blob behind at %s", s.Path(sha))
	}
}

// TestReviewGetDetectsCorruption verifies the sha check on read.
func TestReviewGetDetectsCorruption(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: ok\r\n\r\nthe body\r\n")
	sha, _, err := s.Put(raw)
	if err != nil {
		t.Fatal(err)
	}
	other := []byte("Subject: other\r\n\r\ndifferent body\r\n")
	comp, err := s.compress(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(sha), comp, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(sha); err == nil {
		reviewFail(t, "Get returned content whose sha256 does not match the key")
	}
	if err := os.WriteFile(s.Path(sha), []byte("not zstd at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(sha); err == nil {
		reviewFail(t, "Get returned undecompressable content without an error")
	}
}

// TestReviewEmptyPut stores zero bytes, which a provider can return.
func TestReviewEmptyPut(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sha, _, err := s.Put(nil)
	if err != nil {
		reviewFail(t, "Put(nil) failed: %v", err)
		return
	}
	got, err := s.Get(sha)
	if err != nil {
		reviewFail(t, "Get of an empty blob failed: %v", err)
	}
	if len(got) != 0 {
		reviewFail(t, "empty blob read back %d bytes", len(got))
	}
}
