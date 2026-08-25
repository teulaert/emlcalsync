package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/lennert/emlcal/internal/config"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/output"
	"github.com/lennert/emlcal/internal/provider"
	"github.com/lennert/emlcal/internal/provider/oauth"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(coreDoctorCmd(app))
	})
}

// Check statuses. A warning is something the user may not be able to fix right
// now (no network); it does not fail the command.
const (
	coreStatusOK   = "ok"
	coreStatusWarn = "warn"
	coreStatusFail = "fail"
)

// coreCheck is one diagnostic line.
type coreCheck struct {
	Name   string `json:"check"  table:"CHECK"`
	OK     bool   `json:"ok"     table:"-"`
	Status string `json:"status" table:"STATUS"`
	Detail string `json:"detail,omitempty" table:"DETAIL"`
}

func coreOK(name, detail string) coreCheck {
	return coreCheck{Name: name, OK: true, Status: coreStatusOK, Detail: detail}
}

func coreWarn(name, detail string) coreCheck {
	return coreCheck{Name: name, OK: true, Status: coreStatusWarn, Detail: detail}
}

func coreFail(name, detail string) coreCheck {
	return coreCheck{Name: name, OK: false, Status: coreStatusFail, Detail: detail}
}

// coreOnlineTimeout bounds each provider probe so `doctor` always returns.
const coreOnlineTimeout = 10 * time.Second

func coreDoctorCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check configuration, credentials, database and connectivity",
		Long: `Runs every check emlcal can run without changing anything: the config parses,
each account still has its credentials, the index opens and passes an integrity
check, the blob archive is writable, the daemon pid file is sane, and each
account answers a mailbox listing.

Exit code 1 means at least one check failed. Being offline is reported as a
warning, not a failure.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return coreDoctor(app) },
	}
}

func coreDoctor(app *App) error {
	var checks []coreCheck

	cfg, err := app.Config()
	if err != nil {
		checks = append(checks, coreFail("config", err.Error()))
		return coreReport(app, checks)
	}
	checks = append(checks, coreOK("config", fmt.Sprintf("%s, %d account(s)", coreConfigPath(app, cfg), len(cfg.Accounts))))
	checks = append(checks, coreCheckSecrets(app, cfg)...)
	checks = append(checks, coreCheckDB(app))
	checks = append(checks, coreCheckBlobs(app, cfg))
	checks = append(checks, coreCheckDaemon(app, cfg))
	checks = append(checks, coreCheckOnline(app, cfg)...)
	return coreReport(app, checks)
}

func coreReport(app *App, checks []coreCheck) error {
	if err := app.Printer().Print(checks); err != nil {
		return err
	}
	var bad int
	for _, c := range checks {
		if !c.OK {
			bad++
		}
	}
	if bad > 0 {
		return output.Errorf(output.ExitGeneric, "%d check(s) failed", bad)
	}
	return nil
}

func coreCheckSecrets(app *App, cfg *config.Config) []coreCheck {
	var out []coreCheck
	sec, err := app.Secrets()
	if err != nil {
		return []coreCheck{coreFail("secrets", err.Error())}
	}
	for _, a := range cfg.Accounts {
		name := "secret:" + a.Name
		switch a.Provider {
		case model.ProviderFastmail:
			b, err := sec.Get(FastmailTokenKey(a))
			switch {
			case err != nil:
				out = append(out, coreFail(name, fmt.Sprintf("no API token: run `emlcal account add fastmail --name %s`", a.Name)))
			case len(b) == 0:
				out = append(out, coreFail(name, "stored API token is empty"))
			default:
				out = append(out, coreOK(name, "API token present"))
			}
		case model.ProviderGmail:
			tok, err := (oauth.FileTokenStore{Dir: cfg.SecretsDir()}).Load(GoogleTokenKey(a))
			switch {
			case err != nil:
				out = append(out, coreFail(name, fmt.Sprintf("no OAuth token: run `emlcal account add gmail --name %s`", a.Name)))
			case tok.RefreshToken == "":
				out = append(out, coreWarn(name, "OAuth token has no refresh token; it will stop working when it expires"))
			default:
				out = append(out, coreOK(name, "OAuth token present"))
			}
		default:
			out = append(out, coreFail(name, fmt.Sprintf("unknown provider %q", a.Provider)))
		}
	}
	return out
}

func coreCheckDB(app *App) coreCheck {
	st, err := app.Store()
	if err != nil {
		return coreFail("database", err.Error())
	}
	res, err := st.IntegrityCheck(app.Context())
	if err != nil {
		return coreFail("database", err.Error())
	}
	if res != "ok" {
		return coreFail("database", "integrity check: "+res)
	}
	return coreOK("database", st.Path()+": integrity ok")
}

func coreCheckBlobs(app *App, cfg *config.Config) coreCheck {
	dir := cfg.BlobsDir()
	if _, err := app.Blobs(); err != nil {
		return coreFail("blobs", err.Error())
	}
	probe := filepath.Join(dir, ".emlcal-doctor")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return coreFail("blobs", fmt.Sprintf("%s is not writable: %v", dir, err))
	}
	if err := os.Remove(probe); err != nil {
		return coreFail("blobs", fmt.Sprintf("%s: cannot remove test file: %v", dir, err))
	}
	detail := dir + ": writable"
	if bl, err := app.Blobs(); err == nil {
		if n, bytes, err := bl.Stats(); err == nil {
			detail = fmt.Sprintf("%s: writable, %d blobs, %s", dir, n, coreHumanBytes(bytes))
		}
	}
	return coreOK("blobs", detail)
}

func coreCheckDaemon(app *App, cfg *config.Config) coreCheck {
	path := corePidPathOf(cfg)
	pid, err := coreReadPid(app)
	if errors.Is(err, os.ErrNotExist) {
		return coreOK("daemon", "not running (no pid file)")
	}
	if err != nil {
		return coreFail("daemon", fmt.Sprintf("%s: %v", path, err))
	}
	if !coreDaemonRunning(pid) {
		return coreWarn("daemon", fmt.Sprintf("stale pid file %s (pid %d is gone); it is removed on the next `sync --watch`", path, pid))
	}
	return coreOK("daemon", fmt.Sprintf("running (pid %d)", pid))
}

func coreCheckOnline(app *App, cfg *config.Config) []coreCheck {
	var out []coreCheck
	for _, a := range cfg.Accounts {
		name := "online:" + a.Name
		ctx, cancel := context.WithTimeout(app.Context(), coreOnlineTimeout)
		mp, err := app.Factory.Mail(ctx, a)
		var mbs []model.Mailbox
		if err == nil {
			mbs, err = mp.Mailboxes(ctx)
		}
		cancel()
		switch {
		case err == nil:
			out = append(out, coreOK(name, fmt.Sprintf("%d mailboxes", len(mbs))))
		case provider.IsOffline(err) || errors.Is(err, context.DeadlineExceeded):
			out = append(out, coreWarn(name, "offline: "+err.Error()))
		default:
			out = append(out, coreFail(name, err.Error()))
		}
	}
	return out
}
