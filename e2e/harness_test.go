// Package e2e drives the real emlcal binary against a fake Fastmail JMAP
// server, exercising config → secrets → JMAP client → sync engine →
// SQLite/blobs → CLI output in one piece.
//
// The suite runs under a plain `go test ./...`: TestMain builds the binary
// once and every test skips (rather than fails) if that build did not work,
// so a tree that does not compile fails in its own package's tests instead of
// here as well.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/testutil/jmapfake"
)

var (
	binPath  string
	buildErr error
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "emlcal-e2e-bin")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: temp dir: %v\n", err)
		os.Exit(1)
	}
	bin := filepath.Join(dir, "emlcal")
	cmd := exec.Command("go", "build", "-o", bin, "../cmd/emlcal")
	if out, err := cmd.CombinedOutput(); err != nil {
		buildErr = fmt.Errorf("go build ../cmd/emlcal: %v\n%s", err, out)
	} else {
		binPath = bin
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Harness

// env is one isolated emlcal installation: its own XDG dirs, its own fake
// Fastmail server, its own working directory.
type env struct {
	t    *testing.T
	fake *jmapfake.Server

	Home   string
	Config string
	Data   string
	State  string
	Work   string

	// Extra environment entries appended to every run.
	Extra []string
	// SessionURL is what EMLCAL_JMAP_SESSION_URL is set to; kept separate from
	// the fake so a test can point the binary at a dead address.
	SessionURL string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if binPath == "" {
		t.Skipf("emlcal binary not built: %v", buildErr)
	}
	root := t.TempDir()
	e := &env{
		t:      t,
		fake:   jmapfake.New(t),
		Home:   filepath.Join(root, "home"),
		Config: filepath.Join(root, "config"),
		Data:   filepath.Join(root, "data"),
		State:  filepath.Join(root, "state"),
		Work:   filepath.Join(root, "work"),
	}
	for _, d := range []string{e.Home, e.Config, e.Data, e.State, e.Work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	e.SessionURL = e.fake.URL()
	return e
}

// environ builds the child process environment. EMLCAL_FORMAT is deliberately
// absent: stdout is a pipe, so the CLI must choose JSON on its own.
func (e *env) environ() []string {
	env := []string{
		"HOME=" + e.Home,
		"XDG_CONFIG_HOME=" + e.Config,
		"XDG_DATA_HOME=" + e.Data,
		"XDG_STATE_HOME=" + e.State,
		"EMLCAL_JMAP_SESSION_URL=" + e.SessionURL,
		"PATH=" + os.Getenv("PATH"),
		"TZ=UTC",
	}
	if v := os.Getenv("EMLCAL_E2E_VERBOSE"); v != "" {
		env = append(env, "EMLCAL_E2E_VERBOSE="+v)
	}
	return append(env, e.Extra...)
}

// run executes the binary and returns its output and exit code.
func (e *env) run(args ...string) (stdout, stderr string, code int) {
	e.t.Helper()
	return e.runInput("", args...)
}

func (e *env) runInput(stdin string, args ...string) (stdout, stderr string, code int) {
	e.t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env = e.environ()
	cmd.Dir = e.Work
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatalf("run %v: %v", args, err)
	}
	if os.Getenv("EMLCAL_E2E_VERBOSE") == "1" {
		e.t.Logf("emlcal %s -> %d\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), code, out.String(), errb.String())
	}
	return out.String(), errb.String(), code
}

// mustRun fails the test unless the command exits 0.
func (e *env) mustRun(args ...string) string {
	e.t.Helper()
	stdout, stderr, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("emlcal %s: exit %d\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

// addAccount runs `account add fastmail` with the fake's token on stdin.
func (e *env) addAccount(name, email string) string {
	e.t.Helper()
	stdout, stderr, code := e.runInput(jmapfake.DefaultToken+"\n",
		"account", "add", "fastmail", "--name", name, "--email", email, "--token-stdin")
	if code != 0 {
		e.t.Fatalf("account add: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return stdout
}

func (e *env) configPath() string  { return filepath.Join(e.Config, "emlcal", "config.toml") }
func (e *env) secretsDir() string  { return filepath.Join(e.Config, "emlcal", "secrets") }
func (e *env) blobsDir() string    { return filepath.Join(e.Data, "emlcal", "blobs") }
func (e *env) pidFilePath() string { return filepath.Join(e.State, "emlcal", "emlcal.pid") }

// blobFiles lists every archived raw message.
func (e *env) blobFiles() []string {
	e.t.Helper()
	var out []string
	_ = filepath.Walk(e.blobsDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".eml.zst") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// ---------------------------------------------------------------------------
// JSON helpers

func decodeArray(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("decode JSON array: %v\nbody: %s", err, s)
	}
	return out
}

func decodeObject(t *testing.T, s string) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("decode JSON object: %v\nbody: %s", err, s)
	}
	return out
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("no %q in %v", key, m)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%q is %T, want string (%v)", key, v, m)
	}
	return s
}

func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("no %q in %v", key, m)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%q is %T, want number (%v)", key, v, m)
	}
	return f
}

func boolean(t *testing.T, m map[string]any, key string) bool {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("no %q in %v", key, m)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("%q is %T, want bool (%v)", key, v, m)
	}
	return b
}

// ids extracts the "id" column of a list response.
func ids(t *testing.T, rows []map[string]any) []string {
	t.Helper()
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, str(t, r, "id"))
	}
	return out
}

// findRow returns the first row whose key equals want.
func findRow(t *testing.T, rows []map[string]any, key, want string) map[string]any {
	t.Helper()
	for _, r := range rows {
		if s, _ := r[key].(string); s == want {
			return r
		}
	}
	t.Fatalf("no row with %s=%q in %v", key, want, rows)
	return nil
}

// ---------------------------------------------------------------------------
// Message fixtures

// makeMessage builds an RFC 822 message with mime.Build.
func makeMessage(t *testing.T, msgID, subject, from, body string, opts ...func(*mime.Draft)) []byte {
	t.Helper()
	d := &mime.Draft{
		From:      model.Address{Name: strings.Split(from, "@")[0], Email: from},
		To:        []model.Address{{Email: "me@example.com"}},
		Subject:   subject,
		TextBody:  body,
		MessageID: msgID,
		Date:      time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC),
	}
	for _, o := range opts {
		o(d)
	}
	raw, err := mime.Build(d)
	if err != nil {
		t.Fatalf("mime.Build(%s): %v", subject, err)
	}
	return raw
}

// seed is the fixture set every mail scenario starts from: five messages, one
// with an attachment, one in a "Work" mailbox, one unread, and one that is a
// reply to another so a thread has two members.
type seed struct {
	WorkMailbox string
	IDs         map[string]string // label → JMAP Email id
	Attachment  []byte
}

const seedAttachmentBody = "col1,col2\n42,1234.56\n"

func seedMessages(t *testing.T, f *jmapfake.Server) seed {
	t.Helper()
	s := seed{IDs: map[string]string{}, Attachment: []byte(seedAttachmentBody)}
	s.WorkMailbox = f.AddMailbox("Work", "")
	seen := map[string]bool{"$seen": true}

	first := makeMessage(t, "weekly-1@example.com", "Weekly report", "alice@example.com",
		"The weekly report is ready.\nPlease review before Friday.\n")
	s.IDs["weekly"] = f.AddMessage(first, []string{jmapfake.MailboxInbox}, seen)

	reply := makeMessage(t, "weekly-2@example.com", "Re: Weekly report", "bob@example.com",
		"Sounds good to me.\n\nOn Mon, 01 Aug 2026 at 09:00, alice wrote:\n> The weekly report is ready.\n> Please review before Friday.\n",
		func(d *mime.Draft) { d.InReplyTo = "weekly-1@example.com" })
	s.IDs["reply"] = f.AddMessage(reply, []string{jmapfake.MailboxInbox}, seen)

	invoice := makeMessage(t, "invoice-1@example.com", "Invoice attached", "carol@example.com",
		"Here is the invoice for August.\n",
		func(d *mime.Draft) {
			d.Attachments = []mime.DraftAttachment{{
				Filename: "invoice.csv", ContentType: "text/csv", Data: []byte(seedAttachmentBody),
			}}
		})
	s.IDs["invoice"] = f.AddMessage(invoice, []string{jmapfake.MailboxInbox}, seen)

	kickoff := makeMessage(t, "kickoff-1@example.com", "Project kickoff", "dave@example.com",
		"Kickoff is on Tuesday in the big room.\n")
	s.IDs["kickoff"] = f.AddMessage(kickoff, []string{s.WorkMailbox}, seen)

	newsflash := makeMessage(t, "news-1@example.com", "Nothing to see", "eve@example.com",
		"An unread message so the filters have something to find.\n")
	s.IDs["unread"] = f.AddMessage(newsflash, []string{jmapfake.MailboxInbox}, nil)

	return s
}

// setup builds an env, seeds the fake, adds the account and runs one sync.
func setup(t *testing.T) (*env, seed) {
	t.Helper()
	e := newEnv(t)
	s := seedMessages(t, e.fake)
	e.addAccount("work", "me@example.com")
	e.mustRun("sync")
	return e, s
}

// pub is the public id of a seeded message.
func pub(s seed, label string) string { return "work:" + s.IDs[label] }
