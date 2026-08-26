package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// coreQueueWrite flags a message while the provider is unreachable, which is
// the only way to get a row into the outbox.
func coreQueueWrite(t *testing.T, env *testEnv, account, remote string) {
	t.Helper()
	app, _, _ := env.App()
	defer app.Close()
	eng, err := app.Engine()
	if err != nil {
		t.Fatal(err)
	}
	env.Mail[account].FailNext(1)
	op := sync.Op{Kind: sync.OpFlags, IDs: []string{remote}}
	op.Flags.Set = model.Flags{Flagged: true}
	res, err := eng.Apply(context.Background(), account, op)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Queued {
		t.Fatalf("write was not queued; the fake provider did not fail")
	}
}

func TestOutboxListAndDrop(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work", fake.NewMsg("m1", RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)))

	if rows := coreDecodeRows[coreOutboxRow](t, env.MustRun("outbox", "list")); len(rows) != 0 {
		t.Fatalf("fresh outbox is not empty: %+v", rows)
	}

	coreQueueWrite(t, env, "work", "m1")

	rows := coreDecodeRows[coreOutboxRow](t, env.MustRun("outbox", "list"))
	if len(rows) != 1 {
		t.Fatalf("outbox list = %+v, want one row", rows)
	}
	if rows[0].Account != "work" || rows[0].Kind != string(sync.OpFlags) || rows[0].State != "pending" {
		t.Errorf("outbox row = %+v", rows[0])
	}

	// Retry drains it now that the provider answers again.
	env.MustRun("outbox", "retry")
	if rows := coreDecodeRows[coreOutboxRow](t, env.MustRun("outbox", "list")); len(rows) != 0 {
		t.Fatalf("outbox still pending after retry: %+v", rows)
	}
	if all := coreDecodeRows[coreOutboxRow](t, env.MustRun("outbox", "list", "--all")); len(all) != 1 {
		t.Fatalf("outbox list --all = %+v, want the completed row", all)
	}
}

func TestOutboxDrop(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work", fake.NewMsg("m1", RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)))
	coreQueueWrite(t, env, "work", "m1")

	rows := coreDecodeRows[coreOutboxRow](t, env.MustRun("outbox", "list"))
	if len(rows) != 1 {
		t.Fatalf("outbox list = %+v", rows)
	}
	env.MustRun("outbox", "drop", strconv.FormatInt(rows[0].ID, 10))
	if rows := coreDecodeRows[coreOutboxRow](t, env.MustRun("outbox", "list")); len(rows) != 0 {
		t.Fatalf("row survived drop: %+v", rows)
	}
	if _, _, code := env.Run("outbox", "drop", "not-a-number"); code != 2 {
		t.Errorf("drop with a bad id exit = %d, want 2", code)
	}
}

func TestExportMbox(t *testing.T) {
	env := newTestEnv(t)
	const n = 3
	var msgs []*fake.Msg
	for i := 0; i < n; i++ {
		msgs = append(msgs, fake.NewMsg(
			"m"+strconv.Itoa(i),
			RawMail(t, "a@example.com", "me@example.com", "subject "+strconv.Itoa(i), "body\nFrom the top\n", env.Now.Add(-time.Duration(i)*time.Hour)),
		))
	}
	env.Seed("work", msgs...)

	path := filepath.Join(t.TempDir(), "archive.mbox")
	out := env.MustRun("export", "--mbox", path, "--account", "work")
	sum := coreDecodeOne[coreExportSummary](t, out)
	if sum.Exported != n || sum.Skipped != 0 {
		t.Fatalf("export summary = %+v, want %d exported", sum, n)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	var separators, escaped int
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "From MAILER-DAEMON "):
			separators++
		case strings.HasPrefix(line, ">From "):
			escaped++
		}
	}
	if separators != n {
		t.Errorf("mbox has %d separators, want %d", separators, n)
	}
	if escaped != n {
		t.Errorf("mbox escaped %d body lines, want %d", escaped, n)
	}
}

func TestExportMaildir(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work",
		fake.NewMsg("m1", RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)).WithFlags(model.Flags{Flagged: true}),
		fake.NewMsg("m2", RawMail(t, "b@example.com", "me@example.com", "two", "body", env.Now)).WithFlags(model.Flags{Unread: true}),
	)
	dir := filepath.Join(t.TempDir(), "maildir")
	sum := coreDecodeOne[coreExportSummary](t, env.MustRun("export", "--maildir", dir, "--account", "work"))
	if sum.Exported != 2 {
		t.Fatalf("export summary = %+v", sum)
	}
	for _, sub := range []string{"cur", "new", "tmp"} {
		if fi, err := os.Stat(filepath.Join(dir, sub)); err != nil || !fi.IsDir() {
			t.Fatalf("maildir/%s missing: %v", sub, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "cur"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("cur/ holds %d files, want 2", len(entries))
	}
	var flagged, unseen int
	for _, e := range entries {
		name := e.Name()
		if !strings.Contains(name, ":2,") {
			t.Errorf("maildir name %q has no info suffix", name)
		}
		suffix := name[strings.Index(name, ":2,")+3:]
		if strings.Contains(suffix, "F") {
			flagged++
		}
		if !strings.Contains(suffix, "S") {
			unseen++
		}
	}
	if flagged != 1 || unseen != 1 {
		t.Errorf("maildir flags: flagged=%d unseen=%d, want 1 and 1", flagged, unseen)
	}
}

func TestExportRequiresATarget(t *testing.T) {
	env := newTestEnv(t)
	if _, _, code := env.Run("export"); code != 2 {
		t.Errorf("export without a target exit = %d, want 2", code)
	}
	if _, _, code := env.Run("export", "--mbox", "a", "--maildir", "b"); code != 2 {
		t.Errorf("export with both targets exit = %d, want 2", code)
	}
}

func TestReindexAndGC(t *testing.T) {
	env := newTestEnv(t)
	env.Seed("work",
		fake.NewMsg("m1", RawMail(t, "a@example.com", "me@example.com", "one", "body", env.Now)),
		fake.NewMsg("m2", RawMail(t, "b@example.com", "me@example.com", "two", "body", env.Now)),
	)
	rows := coreDecodeRows[map[string]any](t, env.MustRun("reindex", "--account", "work"))
	if len(rows) != 1 {
		t.Fatalf("reindex printed %d rows", len(rows))
	}
	if got := rows[0]["reindexed"].(float64); got != 2 {
		t.Errorf("reindexed = %v, want 2", got)
	}
	gc := coreDecodeOne[map[string]any](t, env.MustRun("gc"))
	if gc["deleted"].(float64) != 0 {
		t.Errorf("gc deleted live blobs: %v", gc)
	}
}
