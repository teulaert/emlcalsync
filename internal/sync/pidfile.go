package sync

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/teulaert/emlcalsync/internal/config"
)

// The daemon's pid file. `sync --watch` writes it, and everything that wants
// to know whether a daemon is up -- `sync`, `status`, `doctor`, the TUI's R --
// reads it through here, so that they all agree on what counts as a running
// daemon. They have to: the TUI once parsed the file on its own, read the
// record below as "not a number" the day the file grew an identity, and R
// quietly stopped nudging anything.

// PidFileName is the daemon's pid file, in the state directory.
const PidFileName = "emlcal.pid"

// PidPath is where the daemon of cfg leaves its pid file.
func PidPath(cfg *config.Config) string {
	_, _, _, lock, _ := cfg.Paths()
	return filepath.Join(filepath.Dir(lock), PidFileName)
}

// PidRecord is what the daemon leaves behind: its pid, and enough of the
// kernel's own bookkeeping to tell that process apart from whatever inherits
// the number later. A pid alone proves nothing across a reboot -- the daemon's
// old number came back as the keyring daemon, and a bare liveness probe read
// that as a daemon still running and locked `sync --watch` out for good.
type PidRecord struct {
	PID   int
	Boot  string // kernel boot id: a different boot is a different pid space
	Start string // start time in jiffies since boot, /proc/<pid>/stat field 22
}

// Identified says whether the record carries more than a bare pid. Pid files
// written before the daemon recorded an identity do not.
func (r PidRecord) Identified() bool { return r.Boot != "" && r.Start != "" }

// Alive reports whether the process the record describes is still the one
// holding that pid. The signal-0 probe only proves the number is taken, so
// when both records carry an identity they have to agree as well. EPERM means
// the process exists but belongs to someone else, which still counts as taken.
func (r PidRecord) Alive() bool {
	if r.PID <= 0 {
		return false
	}
	if err := syscall.Kill(r.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	live := ProcRecord(r.PID)
	if !r.Identified() || !live.Identified() {
		return true // nothing to compare; the probe is all there is
	}
	return live.Boot == r.Boot && live.Start == r.Start
}

// Bytes renders the record for the pid file: the pid on its own first line,
// so anything that only wants the number still reads it, then the identity.
func (r PidRecord) Bytes() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", r.PID)
	if r.Boot != "" {
		fmt.Fprintf(&b, "boot %s\n", r.Boot)
	}
	if r.Start != "" {
		fmt.Fprintf(&b, "start %s\n", r.Start)
	}
	return []byte(b.String())
}

// ProcRecord reads the identity the kernel keeps for a live pid. It comes
// back bare when /proc does not answer, which callers read as "cannot tell".
func ProcRecord(pid int) PidRecord {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return PidRecord{PID: pid}
	}
	start, err := procStart(pid)
	if err != nil {
		return PidRecord{PID: pid}
	}
	return PidRecord{PID: pid, Boot: strings.TrimSpace(string(boot)), Start: start}
}

// procStart is field 22 of /proc/<pid>/stat, the jiffies since boot at which
// the process started. Paired with the boot id it pins a pid to one process:
// a recycled number always starts later than the one it replaced.
func procStart(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	// The comm field is parenthesised and may itself hold spaces and
	// parentheses, so the fixed columns only start after its closing brace.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return "", fmt.Errorf("proc stat %d: no comm field", pid)
	}
	fields := strings.Fields(string(b[i+1:]))
	const startCol = 22 - 3 // fields[0] is state, column 3 of the stat line
	if len(fields) <= startCol {
		return "", fmt.Errorf("proc stat %d: %d columns after comm", pid, len(fields))
	}
	return fields[startCol], nil
}

// ReadPid reads the pid file at path. A missing file comes back as the
// os.ErrNotExist the read produced, so a caller can tell "no daemon" from
// "cannot tell".
func ReadPid(path string) (PidRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PidRecord{}, err
	}
	return ParsePid(b, path)
}

// ParsePid reads a pid file. A file holding nothing but a pid is one an older
// build wrote, and parses into a record with no identity.
func ParsePid(b []byte, path string) (PidRecord, error) {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return PidRecord{}, fmt.Errorf("pid file %s: %w", path, err)
	}
	r := PidRecord{PID: pid}
	for _, line := range lines[1:] {
		key, val, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		switch key {
		case "boot":
			r.Boot = val
		case "start":
			r.Start = val
		}
	}
	return r, nil
}

// ErrNoDaemon is what NudgeDaemon returns when there is no daemon to nudge:
// no pid file, or one naming a process that is gone or has been replaced.
// It wraps the reason, so errors.Is(err, os.ErrNotExist) still tells a
// missing file from a stale one.
var ErrNoDaemon = errors.New("no daemon running")

// NudgeDaemon asks the daemon whose pid file is at path for a pass now, the
// SIGUSR1 handshake `sync --watch` listens for, and returns its pid. It never
// signals a pid the record no longer vouches for: SIGUSR1 kills a process
// that does not handle it, and the number may have been handed to some
// unrelated process since the daemon died.
func NudgeDaemon(path string) (int, error) {
	rec, err := ReadPid(path)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrNoDaemon, err)
	}
	if !rec.Alive() {
		return rec.PID, fmt.Errorf("%w: pid file %s is stale (pid %d is gone or a different process)",
			ErrNoDaemon, path, rec.PID)
	}
	if err := syscall.Kill(rec.PID, syscall.SIGUSR1); err != nil {
		return rec.PID, fmt.Errorf("nudge daemon (pid %d): %w", rec.PID, err)
	}
	return rec.PID, nil
}
