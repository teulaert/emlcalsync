package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/model"
)

// Sending from the command line is off unless [mail] cli_send is set or
// --allow-send is passed (EML-5). Off, send/reply/forward keep the message as
// a draft and exit 7, naming it.

// sendOffRefusal checks a refused send: exit 7, the send_disabled code, the
// message, and nothing sent.
func sendOffRefusal(t *testing.T, env *testEnv, errs string, code int, want string) {
	t.Helper()
	if code != 7 {
		t.Fatalf("exit = %d, want 7\nstderr: %s", code, errs)
	}
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(errs)), &e); err != nil {
		t.Fatalf("stderr is not the JSON error envelope: %v\n%s", err, errs)
	}
	if e.Error.Code != "send_disabled" {
		t.Errorf("code = %q, want send_disabled", e.Error.Code)
	}
	for _, w := range []string{"Sending is disabled.", want, "from the TUI", "cli_send = true"} {
		if !strings.Contains(e.Error.Message, w) {
			t.Errorf("message lacks %q:\n%s", w, e.Error.Message)
		}
	}
	for name, m := range env.Mail {
		if n := len(m.Sent()); n != 0 {
			t.Errorf("%s sent %d messages with sending off", name, n)
		}
	}
}

func TestMailSendIsOffByDefault(t *testing.T) {
	env := mailSeed(t)
	_, errs, code := env.Run("mail", "send", "--account", "work", "--to", "x@y.example",
		"--subject", "kept", "--body", "for a human to send")
	sendOffRefusal(t, env, errs, code, "Draft created: work:")

	drafts := env.Mail["work"].Drafts()
	if len(drafts) != 1 || !strings.Contains(string(drafts[0]), "Subject: kept") {
		t.Fatalf("the refused send was not kept as a draft: %d drafts", len(drafts))
	}
}

func TestMailReplyIsOffByDefault(t *testing.T) {
	env := mailSeed(t)
	_, errs, code := env.Run("mail", "reply", "work:m-unread", "--body", "thanks")
	sendOffRefusal(t, env, errs, code, "Draft created: work:")

	drafts := env.Mail["work"].Drafts()
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	// Kept as the reply it was: threaded and quoting the original.
	for _, w := range []string{"Subject: Re: Hello there", "In-Reply-To:", "thanks"} {
		if !strings.Contains(string(drafts[0]), w) {
			t.Errorf("draft lacks %q:\n%s", w, drafts[0])
		}
	}
	if flags, _, _ := env.Mail["work"].Lookup("m-unread"); flags.Answered {
		t.Error("original marked answered although nothing was sent")
	}
}

func TestMailForwardIsOffByDefault(t *testing.T) {
	env := mailSeed(t)
	_, errs, code := env.Run("mail", "forward", "work:m-unread", "--to", "jan@example.com")
	sendOffRefusal(t, env, errs, code, "Draft created: work:")
	if drafts := env.Mail["work"].Drafts(); len(drafts) != 1 ||
		!strings.Contains(string(drafts[0]), "Fwd: Hello there") {
		t.Fatalf("the refused forward was not kept as a draft")
	}
}

func TestMailSendDraftIsOffByDefault(t *testing.T) {
	env := mailSeed(t)
	env.Mail["work"].Update("m-old", model.Flags{Draft: true}, nil)
	env.Sync("work")
	_, errs, code := env.Run("mail", "send", "--draft", "work:m-old")
	sendOffRefusal(t, env, errs, code, "Draft kept: work:m-old")
	if flags, mbs, ok := env.Mail["work"].Lookup("m-old"); !ok || !flags.Draft {
		t.Errorf("the draft did not stay where it was: ok=%v flags=%+v mailboxes=%v", ok, flags, mbs)
	}
}

// A draft cannot hold Bcc, so a refused send with --bcc makes none rather
// than one that would go out without the blind recipients.
func TestMailSendOffWithBccMakesNoDraft(t *testing.T) {
	env := mailSeed(t)
	_, errs, code := env.Run("mail", "send", "--account", "work", "--to", "x@y.example",
		"--bcc", "hidden@y.example", "--subject", "s", "--body", "b")
	sendOffRefusal(t, env, errs, code, "No draft was made")
	if n := len(env.Mail["work"].Drafts()); n != 0 {
		t.Errorf("stored %d drafts without their Bcc", n)
	}
}

// --dry-run sends nothing either way, so it is not refused.
func TestMailSendOffDryRunStillPrints(t *testing.T) {
	env := mailSeed(t)
	out := env.MustRun("mail", "send", "--account", "work", "--to", "x@y.example",
		"--subject", "preview", "--body", "b", "--dry-run")
	if !strings.Contains(out, "Subject: preview") {
		t.Errorf("dry run printed:\n%s", out)
	}
	if n := len(env.Mail["work"].Drafts()) + len(env.Mail["work"].Sent()); n != 0 {
		t.Errorf("a dry run stored or sent %d messages", n)
	}
}

func TestMailDraftUnaffectedBySendOff(t *testing.T) {
	env := mailSeed(t)
	env.MustRun("mail", "draft", "--account", "work", "--to", "x@y.example",
		"--subject", "later", "--body", "not yet")
	env.MustRun("mail", "draft", "--reply", "work:m-unread", "--body", "a reply, drafted")
	if n := len(env.Mail["work"].Drafts()); n != 2 {
		t.Errorf("drafts = %d, want 2", n)
	}
}

func TestMailSendConfigSwitchSends(t *testing.T) {
	env := mailSeed(t)
	env.EnableCLISend()
	env.MustRun("mail", "send", "--account", "work", "--to", "x@y.example",
		"--subject", "s", "--body", "b")
	if n := len(env.Mail["work"].Sent()); n != 1 {
		t.Errorf("sent = %d, want 1 with cli_send = true", n)
	}
	if n := len(env.Mail["work"].Drafts()); n != 0 {
		t.Errorf("stored %d drafts while sending was on", n)
	}
}

// --allow-send covers one invocation and no other.
func TestMailSendAllowSendOverride(t *testing.T) {
	env := mailSeed(t)
	env.MustRun("mail", "reply", "work:m-unread", "--body", "thanks", "--allow-send")
	if n := len(env.Mail["work"].Sent()); n != 1 {
		t.Fatalf("sent = %d, want 1 with --allow-send", n)
	}
	_, _, code := env.Run("mail", "send", "--account", "work", "--to", "x@y.example",
		"--subject", "s", "--body", "b")
	if code != 7 {
		t.Errorf("the next send without the flag exited %d, want 7", code)
	}
	if n := len(env.Mail["work"].Sent()); n != 1 {
		t.Errorf("sent = %d after the unflagged send, want still 1", n)
	}
}
