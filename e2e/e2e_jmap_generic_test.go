package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/testutil/jmapfake"
)

// A generic JMAP account (`account add jmap --session-url`) reaches its own
// server for mail, calendars, sending and push, with no Fastmail preset and no
// test override involved.
//
// EMLCAL_JMAP_SESSION_URL points at a dead address throughout: if anything
// fell back to the preset or the override instead of the account's own
// session_url, the commands below would fail to connect.
func TestGenericJMAPAccount(t *testing.T) {
	e := newEnv(t)
	e.SessionURL = "http://127.0.0.1:1/jmap/session"
	seedMessages(t, e.fake)
	seedCalendar(t, e.fake)

	stdout, stderr, code := e.runInput(jmapfake.DefaultToken+"\n",
		"account", "add", "jmap", "--name", "work", "--email", "me@example.com",
		"--session-url", e.fake.URL(), "--token-stdin")
	if code != 0 {
		t.Fatalf("account add jmap: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "1 calendars over JMAP") {
		t.Errorf("account add did not verify the JMAP calendars: %s", stderr)
	}
	row := decodeObject(t, lastLine(stdout))
	if got := str(t, row, "mail_api"); got != "jmap" {
		t.Errorf("mail_api = %q, want jmap", got)
	}
	if got := str(t, row, "calendar_api"); got != "jmap" {
		t.Errorf("calendar_api = %q, want jmap", got)
	}
	if got := str(t, row, "vendor"); got != "" {
		t.Errorf("vendor = %q, want none for a generic JMAP account", got)
	}

	// What landed in config.toml survives a reload as the same account.
	cfg, err := config.Load(e.configPath())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	acct, ok := cfg.Account("work")
	if !ok {
		t.Fatalf("account not saved: %+v", cfg.Accounts)
	}
	if acct.Mail == nil || acct.Mail.Backend != model.BackendJMAP || acct.Mail.SessionURL != e.fake.URL() {
		t.Errorf("mail block = %+v, want jmap with the session URL", acct.Mail)
	}
	if acct.Mail.Vendor != "" || acct.Calendar == nil || acct.Calendar.Backend != model.BackendJMAP || acct.Calendar.Vendor != "" {
		t.Errorf("account picked up a vendor or lost its jmap calendars: mail %+v calendar %+v", acct.Mail, acct.Calendar)
	}
	if !acct.Push {
		t.Errorf("push is off on a JMAP account")
	}
	body, err := os.ReadFile(e.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "fastmail") {
		t.Errorf("config.toml names fastmail for a generic account:\n%s", body)
	}

	// Mail and calendars both sync from the account's own server.
	e.mustRun("sync")
	if n := len(decodeArray(t, e.mustRun("mail", "list"))); n != 5 {
		t.Errorf("mail list = %d rows, want 5", n)
	}
	if n := len(decodeArray(t, e.mustRun("cal", "agenda", "--days", "21"))); n != 4 {
		t.Errorf("cal agenda --days 21 = %d rows, want 4", n)
	}

	// Sending goes out through its EmailSubmission.
	out := decodeObject(t, e.mustRun("mail", "send", "--account", "work",
		"--to", "someone@example.org", "--subject", "Over generic JMAP", "--body", "hello"))
	if boolean(t, out, "queued") {
		t.Errorf("send was queued although the server is up: %v", out)
	}
	subs := e.fake.Submissions()
	if len(subs) != 1 || !bytes.Contains(subs[0], []byte("Subject: Over generic JMAP")) {
		t.Fatalf("submissions = %d, want the one just sent", len(subs))
	}

	// Push: the daemon subscribes to the session's eventSourceUrl and picks up
	// new mail and new events from it.
	cmd := exec.Command(binPath, "sync", "--watch")
	cmd.Env = e.environ()
	cmd.Dir = e.Work
	var werr strings.Builder
	cmd.Stderr = &werr
	cmd.Stdout = &strings.Builder{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sync --watch: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	if !waitFor(10*time.Second, func() bool { return e.fake.SSEConnections() > 0 }) {
		t.Fatalf("the daemon never opened the push stream\nstderr: %s", werr.String())
	}

	e.fake.AddMessage(makeMessage(t, "pushed-generic@example.com", "Pushed over generic JMAP",
		"frank@example.com", "arrived by push\n"), []string{jmapfake.MailboxInbox}, nil)
	now := time.Now().UTC()
	e.fake.AddEvent(jmapfake.CalendarDefault, map[string]any{
		"title":    "Pushed meeting",
		"start":    now.AddDate(0, 0, 2).Truncate(time.Hour).Format("2006-01-02T15:04:05"),
		"duration": "PT1H",
		"timeZone": "UTC",
		"status":   "confirmed",
	})
	e.fake.Bump()

	if !waitFor(10*time.Second, func() bool {
		out, _, code := e.run("mail", "list")
		return code == 0 && strings.Contains(out, "Pushed over generic JMAP")
	}) {
		t.Fatalf("pushed mail never reached the index\nstderr: %s", werr.String())
	}
	if !waitFor(10*time.Second, func() bool {
		out, _, code := e.run("cal", "agenda", "--days", "7")
		return code == 0 && strings.Contains(out, "Pushed meeting")
	}) {
		t.Fatalf("pushed event never reached the agenda\nstderr: %s", werr.String())
	}
}

// The session URL is checked before anything is written.
func TestGenericJMAPAccountRejectsBadSessionURL(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{"", "   ", "agmail.example.com/.well-known/jmap", "ftp://x.example/jmap", "https:///jmap"} {
		_, stderr, code := e.runInput(jmapfake.DefaultToken+"\n",
			"account", "add", "jmap", "--name", "work", "--email", "me@example.com",
			"--session-url", bad, "--token-stdin")
		if code != 2 {
			t.Errorf("--session-url %q: exit %d, want 2 (usage)\n%s", bad, code, stderr)
		}
	}
	if _, err := os.Stat(e.configPath()); !os.IsNotExist(err) {
		t.Errorf("a rejected add wrote config.toml (err %v)", err)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
