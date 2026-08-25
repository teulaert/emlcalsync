package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// coreHideSystemctl points PATH at an empty directory, so `service install`
// takes the "no systemd here" branch instead of touching the real user's units.
func coreHideSystemctl(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestServiceInstallDaemon(t *testing.T) {
	env := newTestEnv(t)
	coreHideSystemctl(t)

	out, errOut, code := env.Run("service", "install")
	if code != 0 {
		t.Fatalf("service install exit = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	dir := filepath.Join(env.Dir, "config", "systemd", "user")
	body := coreReadUnit(t, filepath.Join(dir, coreUnitDaemon))
	for _, want := range []string{
		"ExecStart=", "sync --watch", "Restart=always", "RestartSec=10",
		"After=network-online.target", "Wants=network-online.target", "WantedBy=default.target",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s does not contain %q:\n%s", coreUnitDaemon, want, body)
		}
	}
	if !strings.Contains(body, "ExecStart=/") {
		t.Errorf("ExecStart is not an absolute path:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(dir, coreUnitTimer)); err == nil {
		t.Errorf("timer unit written without --timer")
	}
	if !strings.Contains(errOut, "systemctl") {
		t.Errorf("stderr did not tell the user what to run: %s", errOut)
	}

	rows := coreDecodeRows[coreServiceRow](t, out)
	if len(rows) == 0 || rows[0].Unit != coreUnitDaemon || rows[0].Action != "written" {
		t.Fatalf("service install printed %+v", rows)
	}
}

func TestServiceInstallTimer(t *testing.T) {
	env := newTestEnv(t)
	coreHideSystemctl(t)
	env.MustRun("service", "install", "--timer")

	dir := filepath.Join(env.Dir, "config", "systemd", "user")
	svc := coreReadUnit(t, filepath.Join(dir, coreUnitTimerSvc))
	if !strings.Contains(svc, "Type=oneshot") || strings.Contains(svc, "--watch") {
		t.Errorf("%s is not a oneshot sync:\n%s", coreUnitTimerSvc, svc)
	}
	timer := coreReadUnit(t, filepath.Join(dir, coreUnitTimer))
	for _, want := range []string{"OnBootSec=1min", "OnUnitActiveSec=2min", "WantedBy=timers.target"} {
		if !strings.Contains(timer, want) {
			t.Errorf("%s does not contain %q:\n%s", coreUnitTimer, want, timer)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, coreUnitDaemon)); err == nil {
		t.Errorf("daemon unit written for --timer")
	}
}

func TestServiceUninstall(t *testing.T) {
	env := newTestEnv(t)
	coreHideSystemctl(t)

	if _, _, code := env.Run("service", "uninstall"); code != 3 {
		t.Errorf("uninstall with nothing installed exit = %d, want 3", code)
	}
	env.MustRun("service", "install")
	env.MustRun("service", "uninstall")

	dir := filepath.Join(env.Dir, "config", "systemd", "user")
	if _, err := os.Stat(filepath.Join(dir, coreUnitDaemon)); !os.IsNotExist(err) {
		t.Errorf("unit survived uninstall: %v", err)
	}
}

func coreReadUnit(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
