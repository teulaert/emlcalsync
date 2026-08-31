package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
)

// otpMail is the shape `mail open` exists for: an HTML-only message whose
// <style> tag carries attributes and whose CSS carries a bare child
// combinator. html2text loses the body of one of these, code and all.
const otpMail = "From: Sample <noreply@example.com>\r\n" +
	"To: me@example.com\r\n" +
	"Subject: Your one-time verification code\r\n" +
	"Date: Mon, 24 Aug 2026 09:00:00 +0000\r\n" +
	"Message-ID: <otp-1@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n\r\n" +
	`<!doctype html><html><head><style type="text/css">` +
	".imageContent>table>tbody>tr>td { padding: 0 }</style></head>" +
	`<body style="background-color:#eee;"><div>Code: 678863</div>` +
	`<img src="https://tracker.example.com/pixel.gif"></body></html>` + "\r\n"

func openSeed(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	env.Seed("work", fake.NewMsg("m-otp", []byte(otpMail)))
	return env
}

type openOut struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	Remote bool   `json:"remote_content_allowed"`
}

func TestMailOpen(t *testing.T) {
	env := openSeed(t)

	// The text body is the failure this command answers; assert it, so that a
	// day when extraction improves is a day this test says so.
	body := env.MustRun("mail", "read", "work:m-otp")
	if strings.Contains(body, "678863") {
		t.Log("note: text extraction now finds the code; mail open is still the escape hatch")
	}

	var got openOut
	out := env.MustRun("mail", "open", "work:m-otp")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if got.ID != "work:m-otp" {
		t.Errorf("id = %q", got.ID)
	}
	if len(env.Opened) != 1 {
		t.Fatalf("browser was handed %d URLs, want 1: %v", len(env.Opened), env.Opened)
	}
	if env.Opened[0] != got.URL || !strings.HasPrefix(got.URL, "file://") {
		t.Errorf("opened %q, reported %q", env.Opened[0], got.URL)
	}

	page, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("read the page: %v", err)
	}
	if !strings.Contains(string(page), "678863") {
		t.Error("the page lost the code, which is the whole point of it")
	}
	if !strings.Contains(string(page), "Content-Security-Policy") {
		t.Error("no policy: opening the message would fire the tracking pixel")
	}
	if got.Size != int64(len(page)) {
		t.Errorf("size = %d, page is %d bytes", got.Size, len(page))
	}
	if !strings.HasPrefix(got.Path, config.ViewDir()) {
		t.Errorf("page at %q, want it under %q", got.Path, config.ViewDir())
	}
}

func TestMailOpenRemote(t *testing.T) {
	env := openSeed(t)

	var got openOut
	out := env.MustRun("mail", "open", "work:m-otp", "--remote")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if !got.Remote {
		t.Error("--remote is not reported in the output")
	}
	page, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "Content-Security-Policy") {
		t.Error("--remote still blocked remote content")
	}
}

// -O is "write it, I will open it myself": no browser, no cache directory.
func TestMailOpenOutputPath(t *testing.T) {
	env := openSeed(t)
	dest := filepath.Join(t.TempDir(), "code.html")

	var got openOut
	out := env.MustRun("mail", "open", "work:m-otp", "-O", dest)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if got.Path != dest {
		t.Errorf("path = %q, want %q", got.Path, dest)
	}
	if got.URL != "" {
		t.Errorf("url = %q, want none: -O does not open a browser", got.URL)
	}
	if len(env.Opened) != 0 {
		t.Errorf("-O launched a browser: %v", env.Opened)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("nothing written to %s: %v", dest, err)
	}
}

func TestMailOpenStdout(t *testing.T) {
	env := openSeed(t)

	out := env.MustRun("mail", "open", "work:m-otp", "-O", "-")
	if !strings.HasPrefix(out, "<!doctype html>") {
		t.Errorf("stdout is not the page: %.60q", out)
	}
	if !strings.Contains(out, "678863") {
		t.Error("the page lost the code")
	}
	if len(env.Opened) != 0 {
		t.Errorf("-O - launched a browser: %v", env.Opened)
	}
}

func TestMailOpenUnknownID(t *testing.T) {
	env := openSeed(t)

	_, errs, code := env.Run("mail", "open", "work:nope")
	if code != 3 {
		t.Errorf("exit = %d, want 3 (not found); stderr: %s", code, errs)
	}
}

// Opening the same message twice reuses its page rather than filling the
// cache directory one copy at a time.
func TestMailOpenReusesThePage(t *testing.T) {
	env := openSeed(t)

	var first, second openOut
	if err := json.Unmarshal([]byte(env.MustRun("mail", "open", "work:m-otp")), &first); err != nil {
		t.Fatal(err)
	}
	env.Now = env.Now.Add(time.Minute)
	if err := json.Unmarshal([]byte(env.MustRun("mail", "open", "work:m-otp")), &second); err != nil {
		t.Fatal(err)
	}
	if first.Path != second.Path {
		t.Errorf("second open wrote %q, want %q", second.Path, first.Path)
	}
	entries, err := os.ReadDir(config.ViewDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("view dir holds %d pages, want 1", len(entries))
	}
}
