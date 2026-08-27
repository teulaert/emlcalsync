package smoke

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/testutil/imapfake"
)

// TestSmokeIMAP drives the built binary against a fake, printing what a user
// would see. Run with: go test ./internal/testutil/smoke -run Smoke -v
// It is skipped unless EMLCAL_SMOKE_BIN points at a built emlcal.
func TestSmokeIMAP(t *testing.T) {
	bin := os.Getenv("EMLCAL_SMOKE_BIN")
	if bin == "" {
		t.Skip("set EMLCAL_SMOKE_BIN to a built emlcal binary")
	}
	srv := imapfake.New(t)
	for _, m := range []string{"Archive", "Sent", "Trash", "Drafts"} {
		srv.CreateMailbox(m)
	}
	srv.Mail("INBOX", "Quarterly invoice", "Please find the zeppelin invoice attached.")
	srv.AddMessage("INBOX", []byte(imapfake.Message("Lunch?", "Are you free Thursday?")))
	srv.AddMessage("INBOX", []byte(imapfake.Reply("Lunch?", "Thursday works.", "lunch-")))
	srv.Mail("Archive", "Old contract", "signed last year")
	smtpSrv := imapfake.NewSMTP(t)

	root := t.TempDir()
	env := append(os.Environ(),
		"HOME="+root, "XDG_CONFIG_HOME="+root+"/config",
		"XDG_DATA_HOME="+root+"/data", "XDG_STATE_HOME="+root+"/state",
		"EMLCAL_IMAP_ADDR="+srv.Addr(), "EMLCAL_SMTP_ADDR="+smtpSrv.Addr(),
		"EMLCAL_IMAP_INSECURE=1", "TZ=UTC")

	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		t.Logf("$ emlcal %s\n%s", strings.Join(args, " "), out)
		if err != nil {
			t.Fatalf("emlcal %v: %v", args, err)
		}
		return string(out)
	}

	run(imapfake.Password+"\n", "account", "add", "imap", "--name", "home",
		"--email", imapfake.Username, "--host", "127.0.0.1", "--security", "none",
		"--password-stdin")
	run("", "-o", "table", "doctor")
	run("", "sync", "--account", "home")
	run("", "-o", "table", "mail", "mailboxes", "--account", "home")
	out := run("", "-o", "table", "mail", "list", "--account", "home")
	run("", "-o", "table", "mail", "search", "zeppelin", "--account", "home")
	run("", "-o", "table", "status")

	// Archive the first message and show that the id moved rather than the
	// message being lost.
	if id := firstTableID(out); id != "" {
		run("", "-o", "table", "mail", "archive", id)
		run("", "sync", "--account", "home")
		run("", "-o", "table", "mail", "list", "--account", "home")
	}

	run("", "mail", "send", "--account", "home", "--to", "someone@example.com",
		"--bcc", "blind@example.com", "--subject", "hello", "--body", "a message")
	for _, s := range smtpSrv.Sent() {
		t.Logf("submitted: from=%s to=%v", s.From, s.To)
	}
}

func firstTableID(out string) string {
	for _, line := range strings.Split(out, "\n")[1:] {
		if f := strings.Fields(line); len(f) > 0 && strings.Contains(f[0], ":") {
			return f[0]
		}
	}
	return ""
}
