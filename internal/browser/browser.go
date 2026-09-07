// Package browser hands a URL to the desktop's default handler.
//
// It exists so the places that need it -- the OAuth consent screen, `mail
// open`, the TUI's o -- say it the same way. Callers that a test drives hold
// the function rather than call it directly (App.OpenBrowser, Deps.Browser),
// so a test can watch what would have been launched without a browser
// appearing on somebody's screen.
package browser

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Open asks the desktop to open target. It returns as soon as the handler is
// started, not when it has finished: a browser that takes ten seconds to
// paint is still a success.
func Open(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open %s: %w", target, err)
	}
	// Reap the handler rather than leave a zombie behind: xdg-open exits as
	// soon as it has handed the URL on, long before the browser is done.
	go func() { _ = cmd.Wait() }()
	return nil
}

// FileURL turns a path on disk into the file:// URL a browser wants. The path
// is made absolute first, because a relative one would resolve against the
// browser's working directory rather than ours.
func FileURL(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: "file", Path: abs}).String(), nil
}

// pageTTL is how long a rendered page is kept. It only has to outlive the
// browser's read of it; a day is generous and keeps a second look cheap.
const pageTTL = 24 * time.Hour

// WritePage puts a rendered page where a browser can read it and returns its
// absolute path. The name is derived from key, so writing the same key twice
// reuses the file rather than growing the directory; pages older than a day
// are swept on the way past.
//
// The file is 0600 in a 0700 directory: a rendered message is the message,
// and a cache directory is not a place to leave somebody's mail readable.
func WritePage(dir, key string, doc []byte, now time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sweep(dir, now)
	path := filepath.Join(dir, pageName(key))
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

// WriteFile puts one file where the desktop's handler for its type can read
// it and returns its absolute path. It sits beside the pages, in a directory
// of its own named after key -- the message, in practice -- and under its own
// name, so the viewer that opens it shows the name the sender gave it and is
// picked by the extension. The same sweep that tidies the pages takes these.
//
// The file is 0600 in a 0700 directory, for the reason the pages are: an
// attachment is the mail.
func WriteFile(dir, key, name string, data []byte, now time.Time) (string, error) {
	sub := filepath.Join(dir, strings.TrimSuffix(pageName(key), ".html"))
	// Sweep first: the directory about to be made is empty until the write,
	// and an empty directory is exactly what the sweep takes away.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sweep(dir, now)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(sub, FileName(name, "file"))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

// FileName is name as a single path element. What came in a
// Content-Disposition is whatever the sender typed, and "../../.bashrc" is a
// name a sender can type: the directories go, so do control characters, and
// a name with nothing left is fallback.
func FileName(name, fallback string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return fallback
	}
	return name
}

// pageName turns a key into a file name nothing in it can escape.
func pageName(key string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, key)
	if safe == "" {
		safe = "page"
	}
	return safe + ".html"
}

// sweep removes pages nobody is coming back to, and the files WriteFile put
// in the directories beside them: each of those is emptied of what is stale
// and then removed if nothing is left. Errors are ignored on purpose: failing
// to tidy up is not a reason to refuse to show a message.
func sweep(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			sweepFiles(path, now)
			continue
		}
		if !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		if stale(e, now) {
			_ = os.Remove(path)
		}
	}
}

// sweepFiles is sweep for one message's directory of attachments.
func sweepFiles(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() && stale(e, now) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	// Fails while anything is still inside, which is the point.
	_ = os.Remove(dir)
}

func stale(e os.DirEntry, now time.Time) bool {
	info, err := e.Info()
	return err == nil && now.Sub(info.ModTime()) > pageTTL
}
