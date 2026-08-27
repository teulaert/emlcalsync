package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/provider/caldav"
	"github.com/teulaert/emlcalsync/internal/provider/oauth"
	"github.com/teulaert/emlcalsync/internal/store"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(coreAccountCmd(app))
	})
}

// coreAccountRow is the presentation form of one configured account.
type coreAccountRow struct {
	Name   string `json:"name"   table:"NAME"`
	Vendor string `json:"vendor" table:"VENDOR"`
	Email  string `json:"email"  table:"EMAIL"`
	// MailAPI is the mail backend, or "-" when the account syncs no mail.
	MailAPI   string `json:"mail_api" table:"MAIL"`
	Poll      string `json:"poll"     table:"POLL"`
	Push      bool   `json:"push"     table:"PUSH"`
	Messages  int    `json:"messages" table:"MESSAGES"`
	Mailboxes int    `json:"mailboxes,omitempty" table:"-"`
	// CalendarAPI says which calendar backend this account will use: "-" with
	// no calendar block, "none" when the backend is configured but its
	// credential is missing. Derived from the secrets present, not a network
	// call.
	Calendars   int    `json:"calendars,omitempty" table:"-"`
	CalendarAPI string `json:"calendar_api,omitempty" table:"CALENDARS"`
}

func coreRow(a config.Account) coreAccountRow {
	return coreAccountRow{
		Name:    a.Name,
		Vendor:  string(a.Vendor()),
		Email:   a.Email,
		MailAPI: coreMailBackend(a),
		Poll:    a.Poll.String(),
		Push:    a.Push,
	}
}

// coreMailBackend names the mail backend, or "-" when there is no mail block.
func coreMailBackend(a config.Account) string {
	if a.Mail == nil {
		return "-"
	}
	return string(a.Mail.Backend)
}

// coreCalendarAPI names the calendar backend an account will use: "-" with no
// calendar block, and "none" for a CalDAV backend whose password has not been
// stored, since without it emlcal has no calendar access at all.
func coreCalendarAPI(app *App, a config.Account) string {
	if a.Calendar == nil {
		return "-"
	}
	if a.Calendar.Backend == model.BackendCalDAV && app.CalDAVPassword(a) == "" {
		return "none"
	}
	return string(a.Calendar.Backend)
}

func coreAccountCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "Add, list and remove mail/calendar accounts",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	add := &cobra.Command{
		Use:   "add",
		Short: "Add an account and log in",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	add.AddCommand(coreAccountAddCmd(app, model.VendorGoogle))
	add.AddCommand(coreAccountAddCmd(app, model.VendorFastmail))
	add.AddCommand(coreAccountAddCmd(app, model.VendorICloud))
	add.AddCommand(coreAccountAddIMAPCmd(app))
	cmd.AddCommand(add)
	cmd.AddCommand(coreAccountListCmd(app))
	cmd.AddCommand(coreAccountRemoveCmd(app))
	cmd.AddCommand(coreGoogleClientCmd(app))
	cmd.AddCommand(coreCalDAVPasswordCmd(app))
	cmd.AddCommand(coreIMAPPasswordCmd(app))
	return cmd
}

// coreVendorCommand is the subcommand name for a vendor: `account add icloud`.
func coreVendorCommand(v model.Vendor) string {
	if v == model.VendorGoogle {
		return "gmail" // the command has always been spelled after the product
	}
	return string(v)
}

// coreVendorArticle is the vendor's name with its indefinite article, so the
// help line reads "Add an iCloud account" rather than "Add a icloud account".
func coreVendorArticle(v model.Vendor) string {
	name := coreVendorTitle(v)
	if v == model.VendorICloud {
		return "an " + name
	}
	return "a " + name
}

// coreVendorTitle is the vendor's own spelling, for help text.
func coreVendorTitle(v model.Vendor) string {
	switch v {
	case model.VendorGoogle:
		return "Gmail"
	case model.VendorFastmail:
		return "Fastmail"
	case model.VendorICloud:
		return "iCloud"
	}
	return string(v)
}

func coreAccountAddCmd(app *App, prov model.Vendor) *cobra.Command {
	opts := coreAddOptions{Vendor: prov}
	cmd := &cobra.Command{
		Use:   coreVendorCommand(prov),
		Short: fmt.Sprintf("Add %s account", coreVendorArticle(prov)),
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return coreAddAccount(app, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Name, "name", "", "short account name, used in ids ([a-z0-9-])")
	cmd.Flags().StringVar(&opts.Email, "email", "", "the account's email address")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("email")
	if prov == model.VendorICloud {
		cmd.Flags().StringVar(&opts.AppPassword, "app-password", "", "Apple app-specific password for CalDAV calendars")
		cmd.Flags().BoolVar(&opts.AppPasswordStdin, "app-password-stdin", false,
			"read the app-specific password from stdin instead of prompting")
		cmd.Flags().StringVar(&opts.Username, "username", "",
			"Apple ID, when it is not the --email address")
		cmd.Long = "Adds an iCloud account: mail over IMAP and SMTP, calendars over CalDAV.\n\n" +
			"Both halves authenticate with the same **app-specific password**, not your\n" +
			"Apple ID password. Create one at https://account.apple.com/account/manage\n" +
			"under Sign-In and Security → App-Specific Passwords; two-factor\n" +
			"authentication must be on for that option to exist.\n\n" +
			"If your Apple ID is not the same as your iCloud mail address, pass the\n" +
			"Apple ID as --username: it is what authenticates, while --email is what\n" +
			"matches your own ATTENDEE line on invitations."
	}
	if prov == model.VendorFastmail {
		cmd.Flags().BoolVar(&opts.TokenStdin, "token-stdin", false, "read the API token from stdin instead of prompting")
		cmd.Flags().StringVar(&opts.AppPassword, "app-password", "", "Fastmail app password for CalDAV calendars")
		cmd.Flags().BoolVar(&opts.AppPasswordStdin, "app-password-stdin", false,
			"read the app password from stdin (after the API token, one per line, when both are read from stdin)")
		cmd.Long = "Adds a Fastmail account.\n\n" +
			"Mail uses JMAP: create an API token at\n" +
			"https://app.fastmail.com/settings/security/tokens and paste it when prompted.\n\n" +
			"Calendars need a separate credential. Fastmail API tokens have no calendar\n" +
			"scope, so emlcal reaches calendars over CalDAV with an **app password**:\n" +
			"create one at https://app.fastmail.com/settings/security/devices\n" +
			"(\"New app password\", access \"Calendars (CalDAV)\") and pass it with\n" +
			"--app-password or --app-password-stdin. Without it calendars are skipped;\n" +
			"`emlcal account caldav-password` adds it later."
	}
	if prov == model.VendorGoogle {
		cmd.Flags().StringVar(&opts.ClientID, "client-id", "", "OAuth client id for just this account")
		cmd.Flags().StringVar(&opts.ClientSecret, "client-secret", "", "OAuth client secret for just this account")
		cmd.Long = "Adds a Gmail account and runs the Google OAuth consent flow in a browser.\n" +
			"The OAuth client must be configured first: `emlcal account google-client --id ID --secret SECRET`,\n" +
			"or pass --client-id/--client-secret here to use a client for this account only."
	}
	return cmd
}

// coreWantsAppPassword reports whether the account has a resource that
// authenticates with a per-application password.
func coreWantsAppPassword(a config.Account) bool {
	return len(coreAppPasswordKeys(a)) > 0
}

// coreAppPasswordKeys are the secret keys one app-specific password fills.
// iCloud reaches mail over IMAP and calendars over CalDAV with the same
// credential, so it fills both.
func coreAppPasswordKeys(a config.Account) []string {
	var keys []string
	if a.Mail != nil && a.Mail.Backend == model.BackendIMAP {
		keys = append(keys, IMAPPasswordKey(a))
	}
	if a.Calendar != nil && a.Calendar.Backend == model.BackendCalDAV {
		keys = append(keys, CalDAVPasswordKey(a))
	}
	return keys
}

// coreAddOptions is everything `account add` collected from flags.
type coreAddOptions struct {
	Vendor           model.Vendor
	Name             string
	Email            string
	TokenStdin       bool
	AppPassword      string
	AppPasswordStdin bool
	Username         string
	ClientID         string
	ClientSecret     string
}

// coreAddAccount writes the account to config.toml, stores its secret and
// verifies the login by listing mailboxes.
func coreAddAccount(app *App, opts coreAddOptions) error {
	prov, name, email := opts.Vendor, opts.Name, opts.Email
	if !model.ValidAccountID(name) {
		return output.Errorf(output.ExitUsage, "account name %q must be 1-32 characters of [a-z0-9-]", name)
	}
	if !strings.Contains(email, "@") {
		return output.Errorf(output.ExitUsage, "email %q does not look like an address", email)
	}
	if opts.AppPassword != "" && opts.AppPasswordStdin {
		return output.Errorf(output.ExitUsage, "--app-password and --app-password-stdin are mutually exclusive")
	}
	if (opts.ClientID == "") != (opts.ClientSecret == "") {
		return output.Errorf(output.ExitUsage, "--client-id and --client-secret must be given together")
	}
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	if _, ok := cfg.Account(name); ok {
		return output.Errorf(output.ExitUsage, "account %q already exists; remove it first with `emlcal account remove %s`", name, name)
	}

	acct := config.NewAccount(name, email, prov)
	if opts.Username != "" {
		// The Apple ID authenticates both halves.
		if acct.Calendar != nil {
			acct.Calendar.Username = opts.Username
		}
		if acct.Mail != nil && acct.Mail.Backend == model.BackendIMAP {
			acct.Mail.Username = opts.Username
		}
	}

	// Read every credential before the account is written, so a typo on the
	// command line does not leave a half-configured account behind.
	var token, appPassword string
	stdin := coreStdinReader(app)
	if acct.Mail != nil && acct.Mail.Backend == model.BackendJMAP {
		if token, err = coreReadToken(app, stdin, opts.TokenStdin); err != nil {
			return err
		}
		if token == "" {
			return output.Errorf(output.ExitUsage, "no Fastmail API token given")
		}
	}
	if coreWantsAppPassword(acct) {
		if appPassword, err = coreReadAppPassword(app, stdin, opts.AppPassword, opts.AppPasswordStdin); err != nil {
			return err
		}
		// On iCloud that one password is the entire account: both halves
		// authenticate with it, so there is nothing to fall back on.
		if appPassword == "" && token == "" {
			return output.Errorf(output.ExitUsage,
				"no app-specific password given; a %s account cannot sync anything without one", prov)
		}
	}

	cfg.Accounts = append(cfg.Accounts, acct)
	if err := config.Save(coreConfigPath(app, cfg), cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	app.SetConfig(cfg)

	ctx := app.Context()
	sec, err := app.Secrets()
	if err != nil {
		return err
	}
	if token != "" {
		if err := sec.Set(JMAPTokenKey(acct), []byte(token)); err != nil {
			return fmt.Errorf("store token: %w", err)
		}
	}
	if appPassword != "" {
		// One credential, stored under each protocol's own key. Scoping by
		// backend is what lets either be rotated without disturbing the other.
		for _, key := range coreAppPasswordKeys(acct) {
			if err := sec.Set(key, []byte(appPassword)); err != nil {
				return fmt.Errorf("store app password: %w", err)
			}
		}
	}
	if coreUsesGoogle(acct) {
		if opts.ClientID != "" {
			if err := coreStoreGoogleClient(app, GoogleClientKeyFor(acct), opts.ClientID, opts.ClientSecret); err != nil {
				return err
			}
		}
		if err := coreGoogleLogin(ctx, app, acct); err != nil {
			return err
		}
	}
	return coreVerifyAccount(ctx, app, acct)
}

// coreUsesGoogle reports whether either half of the account authenticates with
// the Google OAuth token.
func coreUsesGoogle(a config.Account) bool {
	return (a.Mail != nil && a.Mail.Backend == model.BackendGmail) ||
		(a.Calendar != nil && a.Calendar.Backend == model.BackendGCal)
}

// coreConfigPath picks where a modified config is written back.
func coreConfigPath(app *App, cfg *config.Config) string {
	if app.ConfigPath != "" {
		return app.ConfigPath
	}
	if cfg.Path != "" {
		return cfg.Path
	}
	return config.DefaultPath()
}

// coreStdinReader wraps the app's stdin once, so a command that reads two
// secrets from it (an API token and an app password) gets one line each
// instead of the first reader swallowing everything.
func coreStdinReader(app *App) *bufio.Reader {
	in := app.Stdin
	if in == nil {
		in = strings.NewReader("")
	}
	return bufio.NewReader(in)
}

// coreReadLine reads one line, treating EOF after some text as a complete
// line (a pipe without a trailing newline).
func coreReadLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func coreReadToken(app *App, r *bufio.Reader, tokenStdin bool) (string, error) {
	if tokenStdin {
		tok, err := coreReadLine(r)
		if err != nil {
			return "", fmt.Errorf("read token from stdin: %w", err)
		}
		return tok, nil
	}
	fmt.Fprint(app.Stdout, "Fastmail API token: ")
	line, err := coreReadLine(r)
	if err != nil {
		return "", output.Errorf(output.ExitUsage, "no Fastmail API token on stdin (use --token-stdin when scripting)")
	}
	fmt.Fprintln(app.Stdout)
	return line, nil
}

// coreReadAppPassword resolves the CalDAV app password from the flags. It
// returns "" when neither was given, which means "no calendars for now".
func coreReadAppPassword(app *App, r *bufio.Reader, value string, fromStdin bool) (string, error) {
	if value != "" {
		return strings.TrimSpace(value), nil
	}
	if !fromStdin {
		return "", nil
	}
	pw, err := coreReadLine(r)
	if err != nil {
		return "", fmt.Errorf("read app password from stdin: %w", err)
	}
	if pw == "" {
		return "", output.Errorf(output.ExitUsage, "--app-password-stdin was given but stdin held no app password")
	}
	return pw, nil
}

// coreStoreGoogleClient writes an OAuth client document to a secrets key after
// checking that the values look like Google's.
func coreStoreGoogleClient(app *App, key, id, secret string) error {
	id, secret = strings.TrimSpace(id), strings.TrimSpace(secret)
	if !coreGoogleClientID.MatchString(id) {
		return output.Errorf(output.ExitUsage, "--client-id %q does not look like a Google OAuth client id (expected <digits>-<hash>.apps.googleusercontent.com — paste only the id, without a label)", id)
	}
	if !strings.HasPrefix(secret, "GOCSPX-") {
		return output.Errorf(output.ExitUsage, "--client-secret does not look like a Google OAuth client secret (expected GOCSPX-…)")
	}
	sec, err := app.Secrets()
	if err != nil {
		return err
	}
	b, err := json.Marshal(struct {
		ID     string `json:"client_id"`
		Secret string `json:"client_secret"`
	}{id, secret})
	if err != nil {
		return err
	}
	if err := sec.Set(key, b); err != nil {
		return fmt.Errorf("store client credentials: %w", err)
	}
	return nil
}

func coreGoogleLogin(ctx context.Context, app *App, acct config.Account) error {
	cfg, err := app.GoogleOAuthConfig(acct)
	if err != nil {
		return err
	}
	open := app.OpenBrowser
	if open == nil {
		open = oauth.OpenSystemBrowser
	}
	tok, err := oauth.Login(ctx, cfg, oauth.LoginOptions{OpenBrowser: open, Output: app.Stderr})
	if err != nil {
		return output.Errorf(output.ExitProvider, "Google login for %q failed: %v", acct.Name, err)
	}
	appCfg, err := app.Config()
	if err != nil {
		return err
	}
	if err := appCfg.EnsureDirs(); err != nil {
		return err
	}
	if err := (oauth.FileTokenStore{Dir: appCfg.SecretsDir()}).Save(GoogleTokenKey(acct), tok); err != nil {
		return fmt.Errorf("store token: %w", err)
	}
	return nil
}

// coreVerifyAccount confirms the credentials work and prints the new row: the
// mail login by listing mailboxes, and the calendar login by listing calendars.
// Each half is checked only when the account has that block, so a calendar-only
// account is not failed for the mail login it does not have.
func coreVerifyAccount(ctx context.Context, app *App, acct config.Account) error {
	row := coreRow(acct)
	row.CalendarAPI = coreCalendarAPI(app, acct)

	if acct.SyncsMail() {
		mp, err := app.Factory.Mail(ctx, acct)
		if err == nil {
			var mbs []model.Mailbox
			mbs, err = mp.Mailboxes(ctx)
			row.Mailboxes = len(mbs)
		}
		if err != nil {
			return coreVerifyFailed(app, acct, "", err)
		}
	}

	if row.CalendarAPI == string(model.BackendCalDAV) {
		cals, err := coreListCalendars(ctx, app, acct)
		if err != nil {
			return coreVerifyFailed(app, acct, "app password", err)
		}
		row.Calendars = len(cals)
	}

	fmt.Fprintf(app.Stderr, "account %q added", acct.Name)
	if acct.SyncsMail() {
		fmt.Fprintf(app.Stderr, ": %d mailboxes", row.Mailboxes)
	}
	if row.CalendarAPI == string(model.BackendCalDAV) {
		sep := ", "
		if !acct.SyncsMail() {
			sep = ": "
		}
		fmt.Fprintf(app.Stderr, "%s%d calendars over CalDAV", sep, row.Calendars)
	}
	fmt.Fprint(app.Stderr, "; run `emlcal sync` to start the backfill\n")
	return app.Printer().Print(row)
}

// coreListCalendars asks the account's calendar provider for its calendars.
// provider.ErrNotSupported is not a failure: it is what the JMAP client says
// about an account with no app password, and the sync engine skips calendars.
func coreListCalendars(ctx context.Context, app *App, acct config.Account) ([]model.Calendar, error) {
	cp, err := app.Factory.Calendar(ctx, acct)
	if err != nil {
		return nil, err
	}
	cals, err := cp.Calendars(ctx)
	if errors.Is(err, provider.ErrNotSupported) {
		return nil, nil
	}
	return cals, err
}

// coreVerifyFailed reports a credential that did not work, leaving the account
// in config.toml so the user can fix just the broken half.
func coreVerifyFailed(app *App, acct config.Account, credential string, err error) error {
	code := output.ExitProvider
	switch {
	case provider.IsOffline(err):
		code = output.ExitOffline
	case caldav.IsAuth(err):
		code = output.ExitProvider
	}
	what := "could not be verified"
	if credential != "" {
		what = "was written, but its " + credential + " could not be verified"
	}
	hint := "run `emlcal doctor` once the problem is fixed"
	if credential == "app password" {
		hint = fmt.Sprintf("fix it with `emlcal account caldav-password --name %s`", acct.Name)
	}
	return output.Errorf(code, "account %q %s in %s: %v (%s)",
		acct.Name, what, coreConfigPathOf(app), err, hint)
}

func coreConfigPathOf(app *App) string {
	cfg, err := app.Config()
	if err != nil {
		return config.DefaultPath()
	}
	return coreConfigPath(app, cfg)
}

func coreAccountListCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured accounts",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := app.Config()
			if err != nil {
				return err
			}
			rows := make([]coreAccountRow, 0, len(cfg.Accounts))
			st := coreOpenStoreIfExists(app)
			for _, a := range cfg.Accounts {
				row := coreRow(a)
				row.CalendarAPI = coreCalendarAPI(app, a)
				if st != nil {
					if s, err := st.AccountStats(app.Context(), a.Name); err == nil {
						row.Messages = s.Messages
						row.Mailboxes = s.Mailboxes
					}
				}
				rows = append(rows, row)
			}
			return app.Printer().Print(rows)
		},
	}
}

// coreOpenStoreIfExists opens the index only when the database file is already
// there, so `account list` on a fresh machine does not create one.
func coreOpenStoreIfExists(app *App) *store.Store {
	cfg, err := app.Config()
	if err != nil {
		return nil
	}
	if _, err := os.Stat(cfg.DBPath()); err != nil {
		return nil
	}
	st, err := app.Store()
	if err != nil {
		return nil
	}
	return st
}

func coreAccountRemoveCmd(app *App) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove an account, its secrets and its indexed messages",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return coreRemoveAccount(app, args[0], yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "actually remove (required; there is no interactive prompt)")
	return cmd
}

type coreRemoveRow struct {
	Name     string `json:"name"     table:"NAME"`
	Vendor   string `json:"vendor" table:"VENDOR"`
	Email    string `json:"email"    table:"EMAIL"`
	Messages int    `json:"messages" table:"MESSAGES"`
	Removed  bool   `json:"removed"  table:"REMOVED"`
	// Blobs are content-addressed and shared, so they survive until `gc`.
	Note string `json:"note,omitempty" table:"NOTE"`
}

func coreRemoveAccount(app *App, name string, yes bool) error {
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	acct, err := app.ResolveAccount(name)
	if err != nil {
		return err
	}
	row := coreRemoveRow{Name: acct.Name, Vendor: string(acct.Vendor()), Email: acct.Email}
	if st := coreOpenStoreIfExists(app); st != nil {
		if s, err := st.AccountStats(app.Context(), name); err == nil {
			row.Messages = s.Messages
		}
	}
	if !yes {
		row.Note = "pass --yes to remove"
		if err := app.Printer().Print(row); err != nil {
			return err
		}
		return output.Errorf(output.ExitUsage,
			"refusing to remove account %q (%d indexed messages) without --yes", name, row.Messages)
	}

	if sec, err := app.Secrets(); err == nil {
		for _, key := range coreSecretKeys(*acct) {
			_ = sec.Delete(key)
		}
	}
	if st := coreOpenStoreIfExists(app); st != nil {
		if err := st.DeleteAccount(app.Context(), name); err != nil {
			return fmt.Errorf("delete indexed messages: %w", err)
		}
	}
	kept := cfg.Accounts[:0]
	for _, a := range cfg.Accounts {
		if a.Name != name {
			kept = append(kept, a)
		}
	}
	cfg.Accounts = kept
	if cfg.General.DefaultAccount == name {
		cfg.General.DefaultAccount = ""
	}
	if err := config.Save(coreConfigPath(app, cfg), cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	app.SetConfig(cfg)
	row.Removed = true
	row.Note = "blobs kept; run `emlcal gc` to reclaim disk"
	return app.Printer().Print(row)
}

// coreSecretKeys lists every secrets key an account may own, per configured
// backend, so `account remove` leaves nothing behind.
func coreSecretKeys(a config.Account) []string {
	var keys []string
	add := func(k ...string) { keys = append(keys, k...) }
	if m := a.Mail; m != nil {
		switch m.Backend {
		case model.BackendJMAP:
			add(JMAPTokenKey(a))
		case model.BackendGmail:
			add(GoogleTokenKey(a)+".json", GoogleClientKeyFor(a))
		case model.BackendIMAP:
			add(IMAPPasswordKey(a), SMTPPasswordKey(a))
		}
	}
	if c := a.Calendar; c != nil {
		switch c.Backend {
		case model.BackendCalDAV:
			add(CalDAVPasswordKey(a))
		case model.BackendGCal:
			add(GoogleTokenKey(a)+".json", GoogleClientKeyFor(a))
		case model.BackendJMAP:
			add(JMAPTokenKey(a))
		}
	}
	seen := map[string]bool{}
	out := keys[:0]
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func coreGoogleClientCmd(app *App) *cobra.Command {
	var id, secret string
	cmd := &cobra.Command{
		Use:   "google-client",
		Short: "Store the Google OAuth desktop client id and secret",
		Long: "Stores the OAuth client credentials used by `account add gmail`.\n" +
			"Create a \"Desktop app\" OAuth client in a Google Cloud project with the\n" +
			"Gmail and Calendar APIs enabled, then pass its id and secret here.\n" +
			"`account add gmail --client-id … --client-secret …` stores one for a\n" +
			"single account instead, and that one wins.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if strings.TrimSpace(id) == "" || strings.TrimSpace(secret) == "" {
				return output.Errorf(output.ExitUsage, "--id and --secret are both required")
			}
			if err := coreStoreGoogleClient(app, GoogleClientKey, id, secret); err != nil {
				return coreRenameClientFlags(err)
			}
			cfg, err := app.Config()
			if err != nil {
				return err
			}
			return app.Printer().Print(struct {
				Stored   bool        `json:"stored"    table:"STORED"`
				ClientID string      `json:"client_id" table:"CLIENT ID"`
				Path     string      `json:"path"      table:"PATH"`
				At       output.Time `json:"at"        table:"-"`
			}{true, strings.TrimSpace(id), cfg.SecretsDir(), output.T(coreNow(app))})
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "OAuth client id")
	cmd.Flags().StringVar(&secret, "secret", "", "OAuth client secret")
	return cmd
}

// coreRenameClientFlags rewrites the validation messages of
// coreStoreGoogleClient, which name the `account add gmail` flags, for the
// `google-client` command's own flag names.
func coreRenameClientFlags(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.NewReplacer("--client-id", "--id", "--client-secret", "--secret").Replace(err.Error())
	var oe *output.ExitError
	if errors.As(err, &oe) {
		return output.Errorf(oe.Code, "%s", msg)
	}
	return errors.New(msg)
}

// coreCalDAVPasswordCmd sets or replaces the CalDAV password of an existing
// account, whichever vendor serves its calendars.
func coreCalDAVPasswordCmd(app *App) *cobra.Command {
	var name, value string
	var fromStdin bool
	cmd := &cobra.Command{
		Use:     "caldav-password",
		Aliases: []string{"fastmail-password"},
		Short:   "Store the password used for CalDAV calendars",
		Long: "emlcal reads and writes CalDAV calendars with a per-application password,\n" +
			"never an account's login password. Create one at\n" +
			"  Fastmail  https://app.fastmail.com/settings/security/devices\n" +
			"            (\"New app password\", access \"Calendars (CalDAV)\")\n" +
			"  iCloud    https://account.apple.com/account/manage\n" +
			"            (Sign-In and Security → App-Specific Passwords)\n" +
			"and store it here. The password is verified immediately by listing the\n" +
			"account's calendars.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return coreSetCalDAVPassword(app, name, value, fromStdin)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "account to set the app password for")
	cmd.Flags().StringVar(&value, "app-password", "", "the app password (prefer --stdin)")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read the app password from stdin")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func coreSetCalDAVPassword(app *App, name, value string, fromStdin bool) error {
	if value != "" && fromStdin {
		return output.Errorf(output.ExitUsage, "--app-password and --stdin are mutually exclusive")
	}
	acct, err := app.ResolveAccount(name)
	if err != nil {
		return err
	}
	if acct.Calendar == nil || acct.Calendar.Backend != model.BackendCalDAV {
		return output.Errorf(output.ExitUsage,
			"account %q does not use a CalDAV calendar backend; nothing would read this password", name)
	}
	pw, err := coreReadAppPassword(app, coreStdinReader(app), value, fromStdin)
	if err != nil {
		return err
	}
	if pw == "" {
		return output.Errorf(output.ExitUsage, "no app password given: pass --app-password or --stdin")
	}
	sec, err := app.Secrets()
	if err != nil {
		return err
	}
	if err := sec.Set(CalDAVPasswordKey(*acct), []byte(pw)); err != nil {
		return fmt.Errorf("store app password: %w", err)
	}

	// The factory caches providers per account, so a fresh one is built here
	// and picks up the password that was just written.
	cals, err := coreListCalendars(app.Context(), app, *acct)
	if err != nil {
		return coreVerifyFailed(app, *acct, "app password", err)
	}
	return app.Printer().Print(struct {
		Name      string `json:"name"      table:"NAME"`
		Stored    bool   `json:"stored"    table:"STORED"`
		API       string `json:"calendar_api" table:"CALENDARS"`
		Calendars int    `json:"calendars" table:"COUNT"`
	}{acct.Name, true, "caldav", len(cals)})
}

// coreNow is the app clock, defaulting to time.Now for Apps built by hand.
func coreNow(a *App) time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// coreGoogleClientID matches the id format Google issues for OAuth clients.
var coreGoogleClientID = regexp.MustCompile(`^[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com$`)
