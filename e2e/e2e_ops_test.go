package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/testutil/jmapfake"
)

// ---------------------------------------------------------------------------
// (h) maintenance commands

func TestExportMbox(t *testing.T) {
	e, _ := setup(t)

	path := filepath.Join(e.Work, "archive.mbox")
	out := decodeObject(t, e.mustRun("export", "--mbox", path))
	if got := num(t, out, "exported"); got != 5 {
		t.Errorf("exported = %v, want 5", got)
	}
	if got := num(t, out, "skipped"); got != 0 {
		t.Errorf("skipped = %v, want 0", got)
	}
	if got := str(t, out, "format"); got != "mbox" {
		t.Errorf("format = %q, want mbox", got)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mbox: %v", err)
	}
	var separators int
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "From ") {
			separators++
		}
	}
	if separators != 5 {
		t.Errorf("mbox has %d \"From \" separator lines, want 5", separators)
	}
	for _, want := range []string{"Subject: Weekly report", "Subject: Invoice attached", "Subject: Project kickoff"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("mbox missing %q", want)
		}
	}

	// Maildir is the other half of the same command.
	dir := filepath.Join(e.Work, "maildir")
	md := decodeObject(t, e.mustRun("export", "--maildir", dir))
	if got := num(t, md, "exported"); got != 5 {
		t.Errorf("maildir exported = %v, want 5", got)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "cur"))
	if err != nil || len(entries) != 5 {
		t.Errorf("maildir cur/ has %d files (err %v), want 5", len(entries), err)
	}

	if _, _, code := e.run("export"); code != 2 {
		t.Errorf("export with neither --mbox nor --maildir: want exit 2")
	}
}

func TestReindexAndGC(t *testing.T) {
	e, _ := setup(t)

	rows := decodeArray(t, e.mustRun("reindex"))
	if len(rows) != 1 {
		t.Fatalf("reindex = %d rows, want 1: %v", len(rows), rows)
	}
	if got := num(t, rows[0], "scanned"); got != 5 {
		t.Errorf("reindex scanned = %v, want 5", got)
	}
	if got := num(t, rows[0], "missing_blobs"); got != 0 {
		t.Errorf("reindex missing_blobs = %v, want 0", got)
	}
	// Reindexing changes nothing an agent can see.
	if list := decodeArray(t, e.mustRun("mail", "list")); len(list) != 5 {
		t.Errorf("mail list after reindex = %d rows, want 5", len(list))
	}
	if hits := decodeArray(t, e.mustRun("mail", "search", "invoice")); len(hits) != 1 {
		t.Errorf("search after reindex = %d hits, want 1", len(hits))
	}

	gc := decodeObject(t, e.mustRun("gc"))
	if got := num(t, gc, "deleted"); got != 0 {
		t.Errorf("gc deleted = %v, want 0 — every blob is still referenced", got)
	}
	if got := num(t, gc, "blobs"); got != 5 {
		t.Errorf("gc walked = %v blobs, want 5", got)
	}
	if blobs := e.blobFiles(); len(blobs) != 5 {
		t.Errorf("gc removed blobs: %d left, want 5", len(blobs))
	}
}

func TestDoctor(t *testing.T) {
	e, _ := setup(t)

	stdout, stderr, code := e.run("doctor")
	if code != 0 {
		t.Fatalf("doctor: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	checks := decodeArray(t, stdout)
	byName := map[string]string{}
	for _, c := range checks {
		byName[str(t, c, "check")] = str(t, c, "status")
		if str(t, c, "status") == "fail" {
			t.Errorf("check %s failed: %v", str(t, c, "check"), c)
		}
	}
	for _, want := range []string{"config", "secret:work.mail", "database", "blobs", "daemon", "online:work"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("doctor did not run the %q check: %v", want, byName)
		}
	}
	if byName["online:work"] != "ok" {
		t.Errorf("online check = %q, want ok while the fake server is up", byName["online:work"])
	}
}

func TestSkillPrintsFrontmatter(t *testing.T) {
	e := newEnv(t)

	out, _, code := e.run("skill")
	if code != 0 {
		t.Fatalf("skill: exit %d", code)
	}
	if !strings.HasPrefix(out, "---\n") {
		t.Fatalf("skill output does not start with YAML frontmatter:\n%.120s", out)
	}
	head := out[:strings.Index(out[4:], "---")+4]
	if !strings.Contains(head, "name: emlcal") {
		t.Errorf("frontmatter has no name: %s", head)
	}
	if !strings.Contains(head, "description:") {
		t.Errorf("frontmatter has no description: %s", head)
	}
	if !strings.Contains(out, "emlcal mail list") {
		t.Errorf("skill does not document `mail list`")
	}
}

func TestServiceInstallWritesUnit(t *testing.T) {
	e := newEnv(t)
	e.addAccount("work", "me@example.com")
	// An empty PATH means systemctl cannot be found, which is the macOS/CI
	// path through the command: the units are still written.
	e.Extra = append(e.Extra, "PATH=")

	stdout, stderr, code := e.run("service", "install")
	if code != 0 {
		t.Fatalf("service install: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	unit := filepath.Join(e.Config, "systemd", "user", "emlcal.service")
	body, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if !strings.Contains(string(body), "sync --watch") {
		t.Errorf("unit does not start the daemon:\n%s", body)
	}
	if !strings.Contains(string(body), "Restart=always") {
		t.Errorf("unit has no restart policy:\n%s", body)
	}
	rows := decodeArray(t, stdout)
	if r := findRow(t, rows, "unit", "emlcal.service"); str(t, r, "action") != "written" {
		t.Errorf("emlcal.service row = %v", r)
	}
	if !strings.Contains(stderr, "systemctl not found") {
		t.Errorf("stderr does not explain that systemctl is missing:\n%s", stderr)
	}

	// --timer writes the other pair.
	if _, _, code := e.run("service", "install", "--timer"); code != 0 {
		t.Fatalf("service install --timer: exit %d", code)
	}
	for _, name := range []string{"emlcal-sync.service", "emlcal-sync.timer"} {
		if _, err := os.Stat(filepath.Join(e.Config, "systemd", "user", name)); err != nil {
			t.Errorf("%s not written: %v", name, err)
		}
	}

	if _, _, code := e.run("service", "uninstall"); code != 0 {
		t.Errorf("service uninstall: exit %d", code)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Errorf("uninstall left %s behind (err %v)", unit, err)
	}
}

// ---------------------------------------------------------------------------
// (i) exit codes

func TestExitCodes(t *testing.T) {
	e, s := setup(t)

	cases := []struct {
		name string
		args []string
		code int
	}{
		{"unknown command", []string{"nope"}, 2},
		{"unknown subcommand", []string{"mail", "nope"}, 2},
		{"unknown flag", []string{"mail", "list", "--nope"}, 2},
		{"missing id", []string{"mail", "read"}, 2},
		{"malformed id", []string{"mail", "read", "not-an-id"}, 2},
		{"unknown message", []string{"mail", "read", "work:nope"}, 3},
		{"unknown account", []string{"mail", "read", "nosuch:1"}, 3},
		{"unknown mailbox", []string{"mail", "list", "--mailbox", "nosuchbox"}, 3},
		{"unknown event", []string{"cal", "show", "work:c:cal-personal:nope"}, 3},
		{"bad search query", []string{"mail", "search", "foo AND"}, 2},
		{"empty search query", []string{"mail", "search", ""}, 2},
		{"contradictory flags", []string{"mail", "mark", pub(s, "weekly"), "--read", "--unread"}, 2},
		{"no mark flag", []string{"mail", "mark", pub(s, "weekly")}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := e.run(tc.args...)
			if code != tc.code {
				t.Fatalf("emlcal %s: exit %d, want %d\nstdout: %s\nstderr: %s",
					strings.Join(tc.args, " "), code, tc.code, stdout, stderr)
			}
			// Errors are a JSON envelope on stderr, per DESIGN.md §9.1.
			env := decodeObject(t, stderr)
			errObj, ok := env["error"].(map[string]any)
			if !ok {
				t.Fatalf("stderr is not {\"error\":{...}}: %s", stderr)
			}
			wantCode := map[int]string{2: "usage", 3: "not_found"}[tc.code]
			if got := str(t, errObj, "code"); got != wantCode {
				t.Errorf("error code = %q, want %q (%s)", got, wantCode, stderr)
			}
			if str(t, errObj, "message") == "" {
				t.Errorf("error message is empty: %s", stderr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// (j) push: sync --watch reacts to a StateChange

func TestSyncWatchPicksUpPushedMail(t *testing.T) {
	e, _ := setup(t)

	cmd := exec.Command(binPath, "sync", "--watch")
	cmd.Env = e.environ()
	cmd.Dir = e.Work
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = &strings.Builder{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sync --watch: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// reaped is set once the test itself has taken the Wait result, so the
	// cleanup below does not wait on a channel nothing will ever send to again.
	reaped := false
	defer func() {
		if reaped {
			return
		}
		_ = cmd.Process.Kill()
		<-done
	}()

	// The daemon writes its pid file before it starts watching.
	pidPath := e.pidFilePath()
	if !waitFor(10*time.Second, func() bool {
		b, err := os.ReadFile(pidPath)
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil && pid == cmd.Process.Pid
	}) {
		t.Fatalf("no pid file at %s after 10s\nstderr: %s", pidPath, stderr.String())
	}
	// And it subscribes to the EventSource stream.
	if !waitFor(10*time.Second, func() bool { return e.fake.SSEConnections() > 0 }) {
		t.Fatalf("the daemon never opened the push stream\nstderr: %s", stderr.String())
	}

	raw := makeMessage(t, "pushed-1@example.com", "Pushed message", "frank@example.com",
		"This one arrived over the push stream.\n")
	e.fake.AddMessage(raw, []string{jmapfake.MailboxInbox}, map[string]bool{"$seen": true})
	e.fake.Bump()

	if !waitFor(10*time.Second, func() bool {
		out, _, code := e.run("mail", "list")
		if code != 0 {
			return false
		}
		return strings.Contains(out, "Pushed message")
	}) {
		t.Fatalf("the pushed message never reached the index\nstderr: %s", stderr.String())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-done:
		reaped = true
		if err != nil {
			t.Errorf("sync --watch exited with %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("sync --watch did not exit within 15s of SIGTERM\nstderr: %s", stderr.String())
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("pid file %s survived a clean shutdown (err %v)", pidPath, err)
	}
	if !strings.Contains(stderr.String(), "watching") {
		t.Errorf("daemon did not announce itself on stderr:\n%s", stderr.String())
	}
}

// waitFor polls cond every 100ms until it holds or the deadline passes.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}
