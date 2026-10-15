package tui

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/provider/fake"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// writePid leaves rec in d.StatePath where the daemon would have left it.
func writePid(t *testing.T, d Deps, rec sync.PidRecord) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.StatePath, sync.PidFileName), rec.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// R finds the daemon through the pid file the daemon actually writes -- pid,
// boot id, start time -- and nudges it. The TUI once parsed that file as one
// bare number, which the three-line record is not, and R silently nudged
// nothing while claiming to refresh.
func TestRefreshNudgesTheDaemonItFinds(t *testing.T) {
	live := sync.ProcRecord(os.Getpid())
	if !live.Identified() {
		t.Skip("/proc reports no boot id and start time here")
	}
	d := newTestDeps(t, "work")
	d.StatePath = t.TempDir()
	writePid(t, d, live) // this process stands in for the daemon

	got := make(chan os.Signal, 1)
	signal.Notify(got, syscall.SIGUSR1)
	defer signal.Stop(got)

	r := newTestRoot(t, d)
	send(t, r, "R")
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("R did not nudge the daemon")
	}
	if !strings.Contains(r.status, "nudged") {
		t.Errorf("status = %q, want it to say the daemon was nudged", r.status)
	}
}

// A pid file from before a reboot names whatever process has the number now.
// SIGUSR1 kills a process that does not expect it, so R must not send it.
func TestRefreshLeavesAStalePidAlone(t *testing.T) {
	live := sync.ProcRecord(os.Getpid())
	if !live.Identified() {
		t.Skip("/proc reports no boot id and start time here")
	}
	d := newTestDeps(t, "work")
	d.StatePath = t.TempDir()
	stale := live
	stale.Boot = "00000000-0000-0000-0000-000000000000"
	writePid(t, d, stale)

	got := make(chan os.Signal, 1)
	signal.Notify(got, syscall.SIGUSR1)
	defer signal.Stop(got)

	r := newTestRoot(t, d)
	send(t, r, "R")
	select {
	case <-got:
		t.Fatal("R signalled the pid of a process that is not the daemon")
	case <-time.After(200 * time.Millisecond):
	}
	if !strings.Contains(r.status, "no daemon") {
		t.Errorf("status = %q, want it to say there is no daemon", r.status)
	}
}

// Without a daemon -- the timer install, or none -- R runs the pass itself,
// so a refresh is a refresh and not just a re-read of what was already there.
func TestRefreshSyncsWhenNoDaemonRuns(t *testing.T) {
	d, mail := newTriageDeps(t)
	d.StatePath = t.TempDir() // no pid file
	addTriageMessage(t, d, mail, "m1", "Already here", time.Hour)
	r := newTestRoot(t, d)

	// What the provider has and the archive does not.
	raw := []byte("From: anna@example.com\r\nSubject: Arrived while you looked\r\n\r\nbody\r\n")
	mail.Add(fake.NewMsg("m2", raw).WithMailboxes("INBOX").WithReceived(testNow.Add(-time.Minute)))

	send(t, r, "R")
	if !strings.Contains(r.status, "synced") {
		t.Fatalf("status = %q, want it to report the sync", r.status)
	}
	if r.syncing {
		t.Error("syncing is still set after the pass reported back")
	}
	ctx := context.Background()
	m, err := d.Store.GetMessage(ctx, "work", "m2")
	if err != nil || m == nil {
		t.Fatalf("m2 after R: %v, %v; want it in the archive", m, err)
	}
	if !strings.Contains(r.render(), "Arrived while you looked") {
		t.Errorf("the list does not show what the pass brought in:\n%s", r.render())
	}
	if _, err := d.Store.GetMessage(ctx, "work", "m1"); err != nil {
		t.Errorf("m1 after R: %v", err)
	}
}

// A second R while the first pass is still out says so instead of starting
// another pass behind it.
func TestRefreshDoesNotStackPasses(t *testing.T) {
	d, _ := newTriageDeps(t)
	d.StatePath = t.TempDir()
	r := newTestRoot(t, d)
	r.syncing = true
	_, cmd := r.Update(keyPress("R"))
	if cmd != nil {
		t.Error("R during a pass returned a command, want none")
	}
	if !strings.Contains(r.status, "still syncing") {
		t.Errorf("status = %q, want it to say a pass is running", r.status)
	}
}
