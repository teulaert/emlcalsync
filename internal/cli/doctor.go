package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/provider/oauth"
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
		if a.Mail != nil {
			out = append(out, coreCheckMailSecret(app, cfg, sec, a))
		}
		if a.Calendar != nil {
			out = append(out, coreCheckCalendarSecret(app, cfg, sec, a)...)
		}
	}
	return out
}

// coreCheckMailSecret audits the credential the account's mail backend needs.
func coreCheckMailSecret(app *App, cfg *config.Config, sec config.Secrets, a config.Account) coreCheck {
	name := "secret:" + a.Name + ".mail"
	switch a.Mail.Backend {
	case model.BackendJMAP:
		b, err := sec.Get(JMAPTokenKey(a))
		switch {
		case err != nil:
			return coreFail(name, fmt.Sprintf("no API token: run `emlcal account add fastmail --name %s`", a.Name))
		case len(b) == 0:
			return coreFail(name, "stored API token is empty")
		}
		return coreOK(name, "API token present")
	case model.BackendGmail:
		return coreCheckGoogleToken(name, cfg, a)
	case model.BackendIMAP:
		b, err := sec.Get(IMAPPasswordKey(a))
		switch {
		case err != nil:
			// A warning, not a failure: an account whose calendars work is
			// half-configured, not broken, and this is what an iCloud account
			// looks like between `account add` and `account imap-password`.
			if a.SyncsCalendar() {
				return coreWarn(name, fmt.Sprintf(
					"no IMAP password, so mail is skipped: run `emlcal account imap-password --name %s`", a.Name))
			}
			return coreFail(name, fmt.Sprintf(
				"no IMAP password: run `emlcal account imap-password --name %s`", a.Name))
		case len(b) == 0:
			return coreFail(name, "stored IMAP password is empty")
		}
		return coreOK(name, "IMAP password present")
	}
	return coreFail(name, fmt.Sprintf("unknown mail backend %q", a.Mail.Backend))
}

// coreCheckCalendarSecret audits the credential the calendar backend needs.
// A CalDAV backend with no stored password is why an account can silently sync
// no calendars at all, so it is reported rather than passed over.
func coreCheckCalendarSecret(app *App, cfg *config.Config, sec config.Secrets, a config.Account) []coreCheck {
	name := "secret:" + a.Name + ".calendar"
	switch a.Calendar.Backend {
	case model.BackendCalDAV:
		b, err := sec.Get(CalDAVPasswordKey(a))
		switch {
		case err != nil:
			// A warning, not a failure: the mail half still works, and this is
			// what a Fastmail account looks like between `account add` and
			// `account caldav-password`.
			return []coreCheck{coreWarn(name, fmt.Sprintf(
				"no CalDAV password, so calendars are skipped: run `emlcal account caldav-password --name %s`", a.Name))}
		case len(strings.TrimSpace(string(b))) == 0:
			return []coreCheck{coreWarn(name, "stored CalDAV password is empty; calendars are skipped")}
		}
		return []coreCheck{coreOK(name, "CalDAV password present")}
	case model.BackendGCal:
		// Same OAuth token as the mail half; do not report it twice.
		if a.Mail != nil && a.Mail.Backend == model.BackendGmail {
			return nil
		}
		return []coreCheck{coreCheckGoogleToken(name, cfg, a)}
	case model.BackendJMAP:
		if a.Mail != nil && a.Mail.Backend == model.BackendJMAP {
			return nil
		}
		b, err := sec.Get(JMAPTokenKey(a))
		if err != nil || len(b) == 0 {
			return []coreCheck{coreFail(name, "no JMAP token")}
		}
		return []coreCheck{coreOK(name, "API token present")}
	}
	return []coreCheck{coreFail(name, fmt.Sprintf("unknown calendar backend %q", a.Calendar.Backend))}
}

func coreCheckGoogleToken(name string, cfg *config.Config, a config.Account) coreCheck {
	tok, err := (oauth.FileTokenStore{Dir: cfg.SecretsDir()}).Load(GoogleTokenKey(a))
	switch {
	case err != nil:
		return coreFail(name, fmt.Sprintf("no OAuth token: run `emlcal account add gmail --name %s`", a.Name))
	case tok.RefreshToken == "":
		return coreWarn(name, "OAuth token has no refresh token; it will stop working when it expires")
	}
	return coreOK(name, "OAuth token present")
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
		detail, err := coreProbeAccount(ctx, app, a)
		cancel()
		switch {
		case err == nil:
			out = append(out, coreOK(name, detail))
		case errors.Is(err, provider.ErrNotSupported):
			// Nothing to reach: the account's only backend has no credential
			// yet, which the secret checks already reported.
			out = append(out, coreWarn(name, err.Error()))
		case provider.IsOffline(err) || errors.Is(err, context.DeadlineExceeded):
			out = append(out, coreWarn(name, "offline: "+err.Error()))
		default:
			out = append(out, coreFail(name, err.Error()))
		}
	}
	return out
}

// coreProbeAccount reaches the account's server the cheapest way it can:
// mailboxes when there is a mail backend, calendars otherwise. A calendar-only
// account has no mailboxes to list, and asking for them would report the
// missing mail block as a failure rather than as the configuration it is.
func coreProbeAccount(ctx context.Context, app *App, a config.Account) (string, error) {
	if a.SyncsMail() {
		mp, err := app.Factory.Mail(ctx, a)
		if err != nil {
			return "", err
		}
		mbs, err := mp.Mailboxes(ctx)
		if err != nil {
			return "", err
		}
		// What the server admits to supporting explains most of how it will
		// behave, and is the first thing worth knowing when a server emlcal has
		// never been run against misbehaves.
		if cp, ok := mp.(interface {
			Capabilities(context.Context) ([]string, error)
		}); ok {
			if caps, err := cp.Capabilities(ctx); err == nil && len(caps) > 0 {
				return fmt.Sprintf("%d mailboxes (%s)", len(mbs), strings.Join(caps, ", ")), nil
			}
		}
		return fmt.Sprintf("%d mailboxes", len(mbs)), nil
	}
	cp, err := app.Factory.Calendar(ctx, a)
	if err != nil {
		return "", err
	}
	cals, err := cp.Calendars(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d calendars", len(cals)), nil
}
