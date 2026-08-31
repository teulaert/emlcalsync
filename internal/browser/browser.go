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

// sweep removes pages nobody is coming back to. Errors are ignored on
// purpose: failing to tidy up is not a reason to refuse to show a message.
func sweep(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= pageTTL {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
