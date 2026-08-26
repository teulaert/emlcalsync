// Package blob implements the content-addressed, zstd-compressed archive of
// raw RFC 822 messages described in DESIGN.md §4.
//
// Layout:
//
//	<root>/<first 2 hex>/<sha256>.eml.zst
//	<root>/tmp/<random>                  staging area for atomic writes
//
// The key is the sha256 of the *uncompressed* bytes, which gives free
// deduplication across accounts. Blobs are never mutated: writes go to a temp
// file, are fsynced and renamed into place.
package blob

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/teulaert/emlcalsync/internal/model"
)

// ErrCorrupt is returned by Get when the stored bytes do not decompress, or
// decompress to something whose sha256 is not the requested key.
var ErrCorrupt = errors.New("blob corrupt")

const (
	ext    = ".eml.zst"
	tmpDir = "tmp"
)

// Store is a handle on a blob archive rooted at a directory. It is safe for
// concurrent use.
type Store struct {
	root string

	encPool sync.Pool // *zstd.Encoder
	decPool sync.Pool // *zstd.Decoder
}

// Open creates (if needed) and returns the blob store rooted at root.
func Open(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("blob: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("blob: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(abs, tmpDir), 0o700); err != nil {
		return nil, fmt.Errorf("blob: create root: %w", err)
	}
	s := &Store{root: abs}
	s.encPool.New = func() any {
		enc, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
			zstd.WithEncoderConcurrency(1),
		)
		if err != nil {
			return err
		}
		return enc
	}
	s.decPool.New = func() any {
		dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return err
		}
		return dec
	}
	return s, nil
}

// Root returns the archive root directory.
func (s *Store) Root() string { return s.root }

// Path returns the on-disk path a blob with this sha would occupy. It does not
// check for existence.
func (s *Store) Path(sha string) string {
	sha = strings.ToLower(sha)
	if len(sha) < 2 {
		return filepath.Join(s.root, "__invalid__", sha+ext)
	}
	return filepath.Join(s.root, sha[:2], sha+ext)
}

// SHA256 returns the archive key for raw.
func SHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validSHA(sha string) bool {
	if len(sha) != 64 {
		return false
	}
	for i := 0; i < len(sha); i++ {
		c := sha[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Put stores raw and returns its sha256. created reports whether the blob was
// actually written (false when an identical blob already existed).
func (s *Store) Put(raw []byte) (string, bool, error) {
	sha := SHA256(raw)
	dst := s.Path(sha)
	if _, err := os.Stat(dst); err == nil {
		return sha, false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return sha, false, fmt.Errorf("blob: stat %s: %w", sha, err)
	}

	comp, err := s.compress(raw)
	if err != nil {
		return sha, false, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return sha, false, fmt.Errorf("blob: mkdir: %w", err)
	}

	tmp, err := s.tempFile()
	if err != nil {
		return sha, false, err
	}
	name := tmp.Name()
	defer func() {
		// no-op once renamed
		_ = os.Remove(name)
	}()

	if _, err := tmp.Write(comp); err != nil {
		tmp.Close()
		return sha, false, fmt.Errorf("blob: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return sha, false, fmt.Errorf("blob: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return sha, false, fmt.Errorf("blob: close: %w", err)
	}
	// Link rather than rename: it is equally atomic but fails instead of
	// clobbering, so `created` stays exact when two goroutines (or two
	// processes) Put the same message at the same time. The temp file is
	// removed by the deferred cleanup either way.
	if err := os.Link(name, dst); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return sha, false, nil
		}
		// Filesystems without hard links (rare, but don't fail the archive):
		// fall back to rename and re-check for a lost race.
		if renameErr := os.Rename(name, dst); renameErr != nil {
			if _, statErr := os.Stat(dst); statErr == nil {
				return sha, false, nil
			}
			return sha, false, fmt.Errorf("blob: link: %w", err)
		}
	}
	syncDir(filepath.Dir(dst))
	return sha, true, nil
}

func (s *Store) tempFile() (*os.File, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("blob: rand: %w", err)
	}
	name := filepath.Join(s.root, tmpDir, hex.EncodeToString(b[:]))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("blob: temp file: %w", err)
	}
	return f, nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

func (s *Store) compress(raw []byte) ([]byte, error) {
	v := s.encPool.Get()
	if err, ok := v.(error); ok {
		return nil, fmt.Errorf("blob: zstd encoder: %w", err)
	}
	enc := v.(*zstd.Encoder)
	defer s.encPool.Put(enc)
	return enc.EncodeAll(raw, make([]byte, 0, len(raw)/3+64)), nil
}

func (s *Store) decompress(comp []byte) ([]byte, error) {
	v := s.decPool.Get()
	if err, ok := v.(error); ok {
		return nil, fmt.Errorf("blob: zstd decoder: %w", err)
	}
	dec := v.(*zstd.Decoder)
	defer s.decPool.Put(dec)
	return dec.DecodeAll(comp, nil)
}

// Get returns the raw bytes for sha. It verifies that what came off disk
// hashes back to sha; a mismatch yields ErrCorrupt. A missing blob yields
// model.ErrNotFound.
func (s *Store) Get(sha string) ([]byte, error) {
	sha = strings.ToLower(sha)
	if !validSHA(sha) {
		return nil, fmt.Errorf("blob: %w: bad sha %q", model.ErrNotFound, sha)
	}
	comp, err := os.ReadFile(s.Path(sha))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("blob %s: %w", sha, model.ErrNotFound)
		}
		return nil, fmt.Errorf("blob: read %s: %w", sha, err)
	}
	raw, err := s.decompress(comp)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w: %v", sha, ErrCorrupt, err)
	}
	if got := SHA256(raw); got != sha {
		return nil, fmt.Errorf("blob %s: %w: content hashes to %s", sha, ErrCorrupt, got)
	}
	return raw, nil
}

// Exists reports whether a blob for sha is present on disk.
func (s *Store) Exists(sha string) bool {
	sha = strings.ToLower(sha)
	if !validSHA(sha) {
		return false
	}
	_, err := os.Stat(s.Path(sha))
	return err == nil
}

// Size returns the compressed size on disk of one blob.
func (s *Store) Size(sha string) (int64, error) {
	sha = strings.ToLower(sha)
	if !validSHA(sha) {
		return 0, fmt.Errorf("blob: %w: bad sha %q", model.ErrNotFound, sha)
	}
	fi, err := os.Stat(s.Path(sha))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, fmt.Errorf("blob %s: %w", sha, model.ErrNotFound)
		}
		return 0, err
	}
	return fi.Size(), nil
}

// Delete removes a blob. Deleting a blob that is not there is not an error.
func (s *Store) Delete(sha string) error {
	sha = strings.ToLower(sha)
	if !validSHA(sha) {
		return fmt.Errorf("blob: bad sha %q", sha)
	}
	err := os.Remove(s.Path(sha))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blob: delete %s: %w", sha, err)
	}
	return nil
}

// Walk calls fn for every blob in the archive with its sha and compressed size
// on disk. Iteration stops on the first error fn returns, which is returned.
func (s *Store) Walk(fn func(sha string, size int64) error) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return fmt.Errorf("blob: read root: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) != 2 || e.Name() == tmpDir {
			continue
		}
		sub := filepath.Join(s.root, e.Name())
		files, err := os.ReadDir(sub)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("blob: read %s: %w", sub, err)
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !strings.HasSuffix(name, ext) {
				continue
			}
			sha := strings.TrimSuffix(name, ext)
			if !validSHA(sha) || !strings.HasPrefix(sha, e.Name()) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return fmt.Errorf("blob: stat %s: %w", name, err)
			}
			if err := fn(sha, info.Size()); err != nil {
				return err
			}
		}
	}
	return nil
}

// Stats returns the number of blobs and the total compressed bytes on disk.
func (s *Store) Stats() (count int, compressedBytes int64, err error) {
	err = s.Walk(func(_ string, size int64) error {
		count++
		compressedBytes += size
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return count, compressedBytes, nil
}

// CleanTemp removes leftover staging files (e.g. after a crash).
func (s *Store) CleanTemp() error {
	dir := filepath.Join(s.root, tmpDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("blob: read tmp: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("blob: clean tmp: %w", err)
		}
	}
	return nil
}

// Reader returns a streaming reader over the decompressed blob. The caller
// must Close it. Unlike Get it does not verify the sha256 (nothing has read
// the whole stream yet), so prefer Get unless the message is very large.
func (s *Store) Reader(sha string) (io.ReadCloser, error) {
	sha = strings.ToLower(sha)
	if !validSHA(sha) {
		return nil, fmt.Errorf("blob: %w: bad sha %q", model.ErrNotFound, sha)
	}
	f, err := os.Open(s.Path(sha))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("blob %s: %w", sha, model.ErrNotFound)
		}
		return nil, err
	}
	dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("blob: zstd reader: %w", err)
	}
	return &readCloser{r: dec.IOReadCloser(), f: f, dec: dec}, nil
}

type readCloser struct {
	r   io.ReadCloser
	f   *os.File
	dec *zstd.Decoder
}

func (rc *readCloser) Read(p []byte) (int, error) { return rc.r.Read(p) }
func (rc *readCloser) Close() error {
	rc.dec.Close()
	return rc.f.Close()
}
