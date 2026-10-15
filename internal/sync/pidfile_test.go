package sync

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A pid file that outlives a reboot names a number the kernel has since handed
// to something else. That has to read as stale: taking the number as proof of a
// daemon is what locked `sync --watch` out of every restart for good.
func TestPidRecordRejectsRecycledPid(t *testing.T) {
	live := ProcRecord(os.Getpid())
	if !live.Identified() {
		t.Skip("/proc reports no boot id and start time here")
	}
	if !live.Alive() {
		t.Fatal("this process reads as not alive")
	}
	if (PidRecord{}).Alive() {
		t.Error("the empty record reads as alive")
	}

	stale := live
	stale.Start = live.Start + "0" // this pid, a process that started elsewhen
	if stale.Alive() {
		t.Error("a record whose start time disagrees with /proc reads as alive")
	}
	stale = live
	stale.Boot = "00000000-0000-0000-0000-000000000000"
	if stale.Alive() {
		t.Error("a record from an earlier boot reads as alive")
	}

	if round, err := ParsePid(live.Bytes(), "test"); err != nil || round != live {
		t.Errorf("pid file round trip = %+v, %v; want %+v", round, err, live)
	}
	// What an older build wrote carries no identity and still has to parse.
	legacy, err := ParsePid([]byte("1\n"), "test")
	if err != nil || legacy.PID != 1 || legacy.Identified() {
		t.Errorf("legacy pid file = %+v, %v", legacy, err)
	}
}

// NudgeDaemon only ever signals the process the pid file vouches for. SIGUSR1
// kills whatever does not handle it, so a missing or stale file has to come
// back as "no daemon" rather than a signal to a recycled number -- and the
// file the current daemon writes, three lines long, has to be read whole:
// the TUI once took it for a bare number and R stopped nudging anything.
func TestNudgeDaemon(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PidFileName)

	if _, err := NudgeDaemon(path); !errors.Is(err, ErrNoDaemon) || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no pid file: err = %v, want ErrNoDaemon wrapping ErrNotExist", err)
	}

	live := ProcRecord(os.Getpid())
	if !live.Identified() {
		t.Skip("/proc reports no boot id and start time here")
	}
	stale := live
	stale.Boot = "00000000-0000-0000-0000-000000000000"
	if err := os.WriteFile(path, stale.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if pid, err := NudgeDaemon(path); !errors.Is(err, ErrNoDaemon) || errors.Is(err, os.ErrNotExist) || pid != os.Getpid() {
		t.Errorf("stale pid file: pid, err = %d, %v; want ErrNoDaemon without ErrNotExist", pid, err)
	}

	// A live record names this very process, which has to be ready for the
	// signal or the test dies of it.
	got := make(chan os.Signal, 1)
	signal.Notify(got, syscall.SIGUSR1)
	defer signal.Stop(got)
	if err := os.WriteFile(path, live.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	pid, err := NudgeDaemon(path)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("live pid file: pid, err = %d, %v; want this process and no error", pid, err)
	}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGUSR1 never arrived")
	}
}
