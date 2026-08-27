package cli

import (
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/output"
)

// The TUI paints escape codes and reads keys. Attached to a pipe there is
// nothing useful it could do, and the test harness — like `emlcal tui | cat` —
// is exactly that case, so the guard has to fail loudly rather than write
// control sequences into someone's pager.
func TestTUIRefusesToRunWithoutATerminal(t *testing.T) {
	e := newTestEnv(t)
	stdout, stderr, code := e.Run("tui")

	if code != output.ExitUsage {
		t.Errorf("exit code = %d, want %d (usage)", code, output.ExitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing written to a non-terminal", stdout)
	}
	if !strings.Contains(stderr, "needs a terminal") {
		t.Errorf("stderr = %q, want it to explain that a terminal is required", stderr)
	}
	if strings.ContainsRune(stdout+stderr, '\x1b') {
		t.Error("the guard emitted an ANSI escape sequence")
	}
}

func TestTUIIsRegisteredWithHelp(t *testing.T) {
	e := newTestEnv(t)
	out := e.MustRun("--help")
	if !strings.Contains(out, "tui") {
		t.Errorf("tui is missing from the root help:\n%s", out)
	}
}
