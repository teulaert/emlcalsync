package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lennert/emlcal/internal/output"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(coreServiceCmd(app))
	})
}

// Unit file names written under the systemd user directory.
const (
	coreUnitDaemon      = "emlcal.service"
	coreUnitTimerSvc    = "emlcal-sync.service"
	coreUnitTimer       = "emlcal-sync.timer"
	coreSystemdUserPath = "systemd/user"
)

type coreServiceRow struct {
	Unit   string `json:"unit"   table:"UNIT"`
	Path   string `json:"path"   table:"PATH"`
	Action string `json:"action" table:"ACTION"`
}

func coreServiceCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install or remove the systemd user service",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(coreServiceInstallCmd(app), coreServiceUninstallCmd(app))
	return cmd
}

func coreServiceInstallCmd(app *App) *cobra.Command {
	var timer bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write and enable the systemd user unit that keeps the archive fresh",
		Long: `Writes a systemd user unit that runs the sync daemon and enables it.

Without --timer this is emlcal.service: a long-running ` + "`emlcal sync --watch`" + `
with Restart=always, which is what you want for push-capable accounts.
With --timer it is emlcal-sync.timer plus a oneshot emlcal-sync.service that
runs ` + "`emlcal sync`" + ` every two minutes instead — no daemon, slightly staler mail.

On systems without systemd (macOS) the unit files are still written and the
commands you would run are printed.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return coreServiceInstall(app, timer) },
	}
	cmd.Flags().BoolVar(&timer, "timer", false, "install a 2-minute timer instead of the always-on daemon")
	return cmd
}

func coreServiceUninstallCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Disable and remove the systemd user units",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return coreServiceUninstall(app) },
	}
}

// coreUnitDir is $XDG_CONFIG_HOME/systemd/user, falling back to ~/.config.
func coreUnitDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, coreSystemdUserPath), nil
}

// coreExePath is the absolute path of this binary, used in ExecStart.
func coreExePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the emlcal binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

func coreServiceInstall(app *App, timer bool) error {
	dir, err := coreUnitDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	exe, err := coreExePath()
	if err != nil {
		return err
	}

	units := map[string]string{}
	var enable string
	if timer {
		units[coreUnitTimerSvc] = coreTimerServiceUnit(exe)
		units[coreUnitTimer] = coreTimerUnit()
		enable = coreUnitTimer
	} else {
		units[coreUnitDaemon] = coreDaemonUnit(exe)
		enable = coreUnitDaemon
	}

	var rows []coreServiceRow
	for _, name := range []string{coreUnitDaemon, coreUnitTimerSvc, coreUnitTimer} {
		body, ok := units[name]
		if !ok {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		rows = append(rows, coreServiceRow{Unit: name, Path: path, Action: "written"})
	}

	cmds := [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", enable}}
	if systemctl, err := exec.LookPath("systemctl"); err == nil {
		for _, args := range cmds {
			out, err := exec.Command(systemctl, args...).CombinedOutput()
			action := "ran: systemctl " + strings.Join(args, " ")
			if err != nil {
				action = fmt.Sprintf("failed: systemctl %s: %v: %s",
					strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			rows = append(rows, coreServiceRow{Unit: enable, Action: action})
		}
	} else {
		fmt.Fprintf(app.Stderr, "systemctl not found — run these once systemd is available:\n")
		for _, args := range cmds {
			fmt.Fprintf(app.Stderr, "  systemctl %s\n", strings.Join(args, " "))
		}
		rows = append(rows, coreServiceRow{Unit: enable, Action: "not enabled: systemctl not found"})
	}
	return app.Printer().Print(rows)
}

func coreServiceUninstall(app *App) error {
	dir, err := coreUnitDir()
	if err != nil {
		return err
	}
	systemctl, hasSystemctl := "", false
	if p, err := exec.LookPath("systemctl"); err == nil {
		systemctl, hasSystemctl = p, true
	}

	var rows []coreServiceRow
	var found bool
	for _, name := range []string{coreUnitDaemon, coreUnitTimer, coreUnitTimerSvc} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		found = true
		if hasSystemctl && name != coreUnitTimerSvc {
			_ = exec.Command(systemctl, "--user", "disable", "--now", name).Run()
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		rows = append(rows, coreServiceRow{Unit: name, Path: path, Action: "removed"})
	}
	if !found {
		return output.Errorf(output.ExitNotFound, "no emlcal units installed under %s", dir)
	}
	if hasSystemctl {
		_ = exec.Command(systemctl, "--user", "daemon-reload").Run()
		rows = append(rows, coreServiceRow{Action: "ran: systemctl --user daemon-reload"})
	} else {
		fmt.Fprintln(app.Stderr, "systemctl not found — run `systemctl --user daemon-reload` when systemd is available")
	}
	return app.Printer().Print(rows)
}

func coreDaemonUnit(exe string) string {
	return `[Unit]
Description=emlcal — local mail & calendar archive
Documentation=man:emlcal(1)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + exe + ` sync --watch
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`
}

func coreTimerServiceUnit(exe string) string {
	return `[Unit]
Description=emlcal — one sync pass
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=` + exe + ` sync

[Install]
WantedBy=default.target
`
}

func coreTimerUnit() string {
	return `[Unit]
Description=emlcal — sync every two minutes

[Timer]
OnBootSec=1min
OnUnitActiveSec=2min
Unit=` + coreUnitTimerSvc + `

[Install]
WantedBy=timers.target
`
}
