package cli

import (
	"fmt"
	"net"
	"strings"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	imapprov "github.com/teulaert/emlcalsync/internal/provider/imap"
)

// coreIMAPPasswordCmd sets or replaces the IMAP password of an existing
// account, whichever server serves its mail.
func coreIMAPPasswordCmd(app *App) *cobra.Command {
	var name, value string
	var fromStdin, enableMail bool
	cmd := &cobra.Command{
		Use:   "imap-password",
		Short: "Store the password used for IMAP mail and SMTP submission",
		Long: "Stores the password emlcal uses to read mail over IMAP and to send over\n" +
			"SMTP. On iCloud and Fastmail this is a per-application password, never the\n" +
			"account's login password:\n" +
			"  iCloud    https://account.apple.com/account/manage\n" +
			"            (Sign-In and Security → App-Specific Passwords)\n" +
			"  Fastmail  https://app.fastmail.com/settings/security/devices\n" +
			"It is verified immediately by listing the account's mailboxes.\n\n" +
			"An account created before emlcal spoke IMAP has no [accounts.mail] block.\n" +
			"--enable-mail adds one, which is a change to config.toml and so is never\n" +
			"done implicitly.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return coreSetIMAPPassword(app, name, value, fromStdin, enableMail)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "account to set the password for")
	cmd.Flags().StringVar(&value, "password", "", "the password (prefer --stdin)")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read the password from stdin")
	cmd.Flags().BoolVar(&enableMail, "enable-mail", false,
		"add an [accounts.mail] block to an account that has none")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func coreSetIMAPPassword(app *App, name, value string, fromStdin, enableMail bool) error {
	if value != "" && fromStdin {
		return output.Errorf(output.ExitUsage, "--password and --stdin are mutually exclusive")
	}
	acct, err := app.ResolveAccount(name)
	if err != nil {
		return err
	}

	switch {
	case acct.Mail == nil && !enableMail:
		return output.Errorf(output.ExitUsage,
			"account %q has no [accounts.mail] block, so nothing would read this password.\n"+
				"Pass --enable-mail to add one (%s over IMAP).", name, acct.Vendor())
	case acct.Mail == nil:
		if err := coreEnableIMAPMail(app, acct); err != nil {
			return err
		}
	case acct.Mail.Backend != model.BackendIMAP:
		return output.Errorf(output.ExitUsage,
			"account %q uses the %q mail backend, not imap; nothing would read this password",
			name, acct.Mail.Backend)
	}

	pw, err := coreReadAppPassword(app, coreStdinReader(app), value, fromStdin)
	if err != nil {
		return err
	}
	if pw == "" {
		return output.Errorf(output.ExitUsage, "no password given: pass --password or --stdin")
	}
	sec, err := app.Secrets()
	if err != nil {
		return err
	}
	if err := sec.Set(IMAPPasswordKey(*acct), []byte(pw)); err != nil {
		return fmt.Errorf("store IMAP password: %w", err)
	}

	boxes, err := coreIMAPMailboxes(app, *acct)
	if err != nil {
		return coreVerifyFailed(app, *acct, "IMAP password", err)
	}
	return app.Printer().Print(struct {
		Name      string `json:"name"      table:"NAME"`
		Stored    bool   `json:"stored"    table:"STORED"`
		Backend   string `json:"mail"      table:"MAIL"`
		Mailboxes int    `json:"mailboxes" table:"MAILBOXES"`
	}{acct.Name, true, "imap", len(boxes)})
}

// coreEnableIMAPMail adds a mail block to an account that predates IMAP
// support, and writes it back to config.toml.
func coreEnableIMAPMail(app *App, acct *config.Account) error {
	vendor := acct.Vendor()
	if _, ok := imapprov.PresetFor(vendor); !ok {
		return output.Errorf(output.ExitUsage,
			"account %q has no vendor with an IMAP preset; add an [accounts.mail] block "+
				"with a host to config.toml by hand", acct.Name)
	}
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	for i := range cfg.Accounts {
		if cfg.Accounts[i].Name != acct.Name {
			continue
		}
		cfg.Accounts[i].Mail = &config.MailBackend{Backend: model.BackendIMAP, Vendor: vendor}
		// The Apple ID that authenticates CalDAV authenticates IMAP too.
		if cfg.Accounts[i].Calendar != nil && cfg.Accounts[i].Calendar.Username != "" {
			cfg.Accounts[i].Mail.Username = cfg.Accounts[i].Calendar.Username
		}
		cfg.Accounts[i].Push = true
		if err := config.Save(coreConfigPath(app, cfg), cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		app.SetConfig(cfg)
		*acct = cfg.Accounts[i]
		return nil
	}
	return output.Errorf(output.ExitNotFound, "account %q vanished while enabling mail", acct.Name)
}

// coreAccountAddIMAPCmd adds an account on a server emlcal has no preset for.
func coreAccountAddIMAPCmd(app *App) *cobra.Command {
	var o coreIMAPAddOptions
	cmd := &cobra.Command{
		Use:   "imap",
		Short: "Add an account on any IMAP server",
		Long: "Adds a mail account on a server emlcal has no preset for: a self-hosted\n" +
			"Dovecot, Migadu, mailbox.org, and so on.\n\n" +
			"--host is required. The submission host defaults to the IMAP host, which is\n" +
			"right more often than not. Ports default to 993 (implicit TLS) or 143\n" +
			"(STARTTLS) for IMAP, and 465 or 587 for submission.\n\n" +
			"If the domain publishes RFC 6186 SRV records, --host may be omitted and the\n" +
			"hosts are looked up from --email instead.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return coreAddIMAPAccount(app, o) },
	}
	cmd.Flags().StringVar(&o.Name, "name", "", "short account name, used in ids ([a-z0-9-])")
	cmd.Flags().StringVar(&o.Email, "email", "", "the account's email address")
	cmd.Flags().StringVar(&o.Host, "host", "", "IMAP host (omit to try SRV discovery)")
	cmd.Flags().IntVar(&o.Port, "port", 0, "IMAP port (default 993, or 143 with --security starttls)")
	cmd.Flags().StringVar(&o.Security, "security", "", "tls (default), starttls or none")
	cmd.Flags().StringVar(&o.Username, "username", "", "login, when it is not --email")
	cmd.Flags().StringVar(&o.SMTPHost, "smtp-host", "", "submission host (default: --host)")
	cmd.Flags().IntVar(&o.SMTPPort, "smtp-port", 0, "submission port (default 587)")
	cmd.Flags().StringVar(&o.SMTPSecurity, "smtp-security", "", "tls, starttls (default) or none")
	cmd.Flags().StringVar(&o.Password, "password", "", "the password (prefer --password-stdin)")
	cmd.Flags().BoolVar(&o.PasswordStdin, "password-stdin", false, "read the password from stdin")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

type coreIMAPAddOptions struct {
	Name, Email              string
	Host, Security, Username string
	Port                     int
	SMTPHost, SMTPSecurity   string
	SMTPPort                 int
	Password                 string
	PasswordStdin            bool
}

func coreAddIMAPAccount(app *App, o coreIMAPAddOptions) error {
	if !model.ValidAccountID(o.Name) {
		return output.Errorf(output.ExitUsage, "account name %q must be 1-32 characters of [a-z0-9-]", o.Name)
	}
	if !strings.Contains(o.Email, "@") {
		return output.Errorf(output.ExitUsage, "email %q does not look like an address", o.Email)
	}
	if o.Password != "" && o.PasswordStdin {
		return output.Errorf(output.ExitUsage, "--password and --password-stdin are mutually exclusive")
	}
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	if _, ok := cfg.Account(o.Name); ok {
		return output.Errorf(output.ExitUsage,
			"account %q already exists; remove it first with `emlcal account remove %s`", o.Name, o.Name)
	}

	if o.Host == "" {
		imapHost, imapPort, smtpHost, smtpPort := coreDiscoverSRV(o.Email)
		if imapHost == "" {
			return output.Errorf(output.ExitUsage,
				"no --host given and no IMAP SRV record for %s; pass --host",
				strings.SplitN(o.Email, "@", 2)[1])
		}
		o.Host, o.Port = imapHost, firstNonZero(o.Port, imapPort)
		if o.SMTPHost == "" {
			o.SMTPHost, o.SMTPPort = smtpHost, firstNonZero(o.SMTPPort, smtpPort)
		}
		fmt.Fprintf(app.Stderr, "discovered imap %s:%d, submission %s:%d\n",
			o.Host, o.Port, o.SMTPHost, o.SMTPPort)
	}

	// Read the credential before writing anything, so a typo does not leave a
	// half-configured account behind.
	pw, err := coreReadAppPassword(app, coreStdinReader(app), o.Password, o.PasswordStdin)
	if err != nil {
		return err
	}
	if pw == "" {
		return output.Errorf(output.ExitUsage, "no password given: pass --password or --password-stdin")
	}

	acct := config.NewAccount(o.Name, o.Email, "")
	acct.Mail = &config.MailBackend{
		Backend: model.BackendIMAP,
		Host:    o.Host, Port: o.Port, Security: o.Security, Username: o.Username,
		SMTPHost: firstNonEmptyStr(o.SMTPHost, o.Host),
		SMTPPort: o.SMTPPort, SMTPSecurity: o.SMTPSecurity,
	}
	acct.Calendar = nil
	acct.Poll = config.Duration(config.DefaultPollIMAPPush)
	acct.Push = true

	cfg.Accounts = append(cfg.Accounts, acct)
	if err := config.Save(coreConfigPath(app, cfg), cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	app.SetConfig(cfg)

	sec, err := app.Secrets()
	if err != nil {
		return err
	}
	if err := sec.Set(IMAPPasswordKey(acct), []byte(pw)); err != nil {
		return fmt.Errorf("store IMAP password: %w", err)
	}

	boxes, err := coreIMAPMailboxes(app, acct)
	if err != nil {
		return coreVerifyFailed(app, acct, "IMAP password", err)
	}
	return app.Printer().Print(struct {
		Name      string `json:"name"      table:"NAME"`
		Email     string `json:"email"     table:"EMAIL"`
		Host      string `json:"host"      table:"HOST"`
		Mailboxes int    `json:"mailboxes" table:"MAILBOXES"`
	}{acct.Name, acct.Email, o.Host, len(boxes)})
}

// coreDiscoverSRV looks up RFC 6186 submission and IMAP records. Failure is
// silent: it only ever saves the user some typing.
func coreDiscoverSRV(email string) (imapHost string, imapPort int, smtpHost string, smtpPort int) {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return "", 0, "", 0
	}
	domain := email[at+1:]
	if _, recs, err := net.LookupSRV("imaps", "tcp", domain); err == nil && len(recs) > 0 {
		imapHost, imapPort = strings.TrimSuffix(recs[0].Target, "."), int(recs[0].Port)
	}
	if _, recs, err := net.LookupSRV("submission", "tcp", domain); err == nil && len(recs) > 0 {
		smtpHost, smtpPort = strings.TrimSuffix(recs[0].Target, "."), int(recs[0].Port)
	}
	return imapHost, imapPort, smtpHost, smtpPort
}

func firstNonZero(v ...int) int {
	for _, n := range v {
		if n != 0 {
			return n
		}
	}
	return 0
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// coreIMAPMailboxes lists an account's folders through a freshly built
// provider, which is how a just-stored password gets verified: the Factory
// caches providers per account, so the cached one predates the write.
func coreIMAPMailboxes(app *App, acct config.Account) ([]model.Mailbox, error) {
	if f, ok := app.Factory.(*Factory); ok {
		f.forgetIMAP(acct.Name)
	}
	mp, err := app.Factory.Mail(app.Context(), acct)
	if err != nil {
		return nil, err
	}
	return mp.Mailboxes(app.Context())
}
