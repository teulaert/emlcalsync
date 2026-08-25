package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lennert/emlcal/internal/config"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/output"
	"github.com/lennert/emlcal/internal/provider"
	"github.com/lennert/emlcal/internal/provider/oauth"
	"github.com/lennert/emlcal/internal/store"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(coreAccountCmd(app))
	})
}

// coreAccountRow is the presentation form of one configured account.
type coreAccountRow struct {
	Name      string `json:"name"     table:"NAME"`
	Provider  string `json:"provider" table:"PROVIDER"`
	Email     string `json:"email"    table:"EMAIL"`
	Poll      string `json:"poll"     table:"POLL"`
	Push      bool   `json:"push"     table:"PUSH"`
	Messages  int    `json:"messages" table:"MESSAGES"`
	Mailboxes int    `json:"mailboxes,omitempty" table:"-"`
}

func coreRow(a config.Account) coreAccountRow {
	return coreAccountRow{
		Name:     a.Name,
		Provider: string(a.Provider),
		Email:    a.Email,
		Poll:     a.Poll.String(),
		Push:     a.Push,
	}
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
	add.AddCommand(coreAccountAddCmd(app, model.ProviderGmail))
	add.AddCommand(coreAccountAddCmd(app, model.ProviderFastmail))
	cmd.AddCommand(add)
	cmd.AddCommand(coreAccountListCmd(app))
	cmd.AddCommand(coreAccountRemoveCmd(app))
	cmd.AddCommand(coreGoogleClientCmd(app))
	return cmd
}

func coreAccountAddCmd(app *App, prov model.Provider) *cobra.Command {
	var name, email string
	var tokenStdin bool
	cmd := &cobra.Command{
		Use:   string(prov),
		Short: fmt.Sprintf("Add a %s account", prov),
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return coreAddAccount(app, prov, name, email, tokenStdin)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "short account name, used in ids ([a-z0-9-])")
	cmd.Flags().StringVar(&email, "email", "", "the account's email address")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("email")
	if prov == model.ProviderFastmail {
		cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the API token from stdin instead of prompting")
	}
	if prov == model.ProviderGmail {
		cmd.Long = "Adds a Gmail account and runs the Google OAuth consent flow in a browser.\n" +
			"The OAuth client must be configured first: `emlcal account google-client --id ID --secret SECRET`."
	} else {
		cmd.Long = "Adds a Fastmail account. Create an API token with mail and calendar\n" +
			"scopes at https://app.fastmail.com/settings/security/tokens and paste it when prompted."
	}
	return cmd
}

// coreAddAccount writes the account to config.toml, stores its secret and
// verifies the login by listing mailboxes.
func coreAddAccount(app *App, prov model.Provider, name, email string, tokenStdin bool) error {
	if !model.ValidAccountID(name) {
		return output.Errorf(output.ExitUsage, "account name %q must be 1-32 characters of [a-z0-9-]", name)
	}
	if !strings.Contains(email, "@") {
		return output.Errorf(output.ExitUsage, "email %q does not look like an address", email)
	}
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	if _, ok := cfg.Account(name); ok {
		return output.Errorf(output.ExitUsage, "account %q already exists; remove it first with `emlcal account remove %s`", name, name)
	}

	acct := config.Account{
		Name:             name,
		Provider:         prov,
		Email:            email,
		IncludeSpamTrash: true,
		Calendars:        []string{"*"},
	}
	switch prov {
	case model.ProviderFastmail:
		acct.Poll = config.Duration(config.DefaultPollFastmail)
		acct.Push = true
	default:
		acct.Poll = config.Duration(config.DefaultPollGmail)
	}

	cfg.Accounts = append(cfg.Accounts, acct)
	if err := config.Save(coreConfigPath(app, cfg), cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	app.SetConfig(cfg)

	ctx := app.Context()
	switch prov {
	case model.ProviderFastmail:
		if err := coreStoreFastmailToken(app, acct, tokenStdin); err != nil {
			return err
		}
	default:
		if err := coreGoogleLogin(ctx, app, acct); err != nil {
			return err
		}
	}
	return coreVerifyAccount(ctx, app, acct)
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

func coreStoreFastmailToken(app *App, acct config.Account, tokenStdin bool) error {
	token, err := coreReadToken(app, tokenStdin)
	if err != nil {
		return err
	}
	if token == "" {
		return output.Errorf(output.ExitUsage, "no Fastmail API token given")
	}
	sec, err := app.Secrets()
	if err != nil {
		return err
	}
	if err := sec.Set(FastmailTokenKey(acct), []byte(token)); err != nil {
		return fmt.Errorf("store token: %w", err)
	}
	return nil
}

func coreReadToken(app *App, tokenStdin bool) (string, error) {
	in := app.Stdin
	if in == nil {
		in = strings.NewReader("")
	}
	if tokenStdin {
		b, err := io.ReadAll(in)
		if err != nil {
			return "", fmt.Errorf("read token from stdin: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	fmt.Fprint(app.Stdout, "Fastmail API token: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return "", output.Errorf(output.ExitUsage, "no Fastmail API token on stdin (use --token-stdin when scripting)")
	}
	fmt.Fprintln(app.Stdout)
	return strings.TrimSpace(line), nil
}

func coreGoogleLogin(ctx context.Context, app *App, acct config.Account) error {
	cfg, err := app.GoogleOAuthConfig()
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

// coreVerifyAccount confirms the credentials work and prints the new row.
func coreVerifyAccount(ctx context.Context, app *App, acct config.Account) error {
	row := coreRow(acct)
	mp, err := app.Factory.Mail(ctx, acct)
	if err == nil {
		var mbs []model.Mailbox
		mbs, err = mp.Mailboxes(ctx)
		row.Mailboxes = len(mbs)
	}
	if err != nil {
		code := output.ExitProvider
		if provider.IsOffline(err) {
			code = output.ExitOffline
		}
		return output.Errorf(code,
			"account %q was written to %s but could not be verified: %v (run `emlcal doctor` once the problem is fixed)",
			acct.Name, coreConfigPathOf(app), err)
	}
	fmt.Fprintf(app.Stderr, "account %q added; run `emlcal sync` to start the backfill\n", acct.Name)
	return app.Printer().Print(row)
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
	Provider string `json:"provider" table:"PROVIDER"`
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
	row := coreRemoveRow{Name: acct.Name, Provider: string(acct.Provider), Email: acct.Email}
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

// coreSecretKeys lists every secrets key an account may own.
func coreSecretKeys(a config.Account) []string {
	keys := []string{a.SecretKey()}
	switch a.Provider {
	case model.ProviderFastmail:
		keys = append(keys, FastmailTokenKey(a))
	case model.ProviderGmail:
		keys = append(keys, GoogleTokenKey(a)+".json")
	}
	return keys
}

func coreGoogleClientCmd(app *App) *cobra.Command {
	var id, secret string
	cmd := &cobra.Command{
		Use:   "google-client",
		Short: "Store the Google OAuth desktop client id and secret",
		Long: "Stores the OAuth client credentials used by `account add gmail`.\n" +
			"Create a \"Desktop app\" OAuth client in a Google Cloud project with the\n" +
			"Gmail and Calendar APIs enabled, then pass its id and secret here.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if strings.TrimSpace(id) == "" || strings.TrimSpace(secret) == "" {
				return output.Errorf(output.ExitUsage, "--id and --secret are both required")
			}
			sec, err := app.Secrets()
			if err != nil {
				return err
			}
			b, err := json.Marshal(struct {
				ID     string `json:"client_id"`
				Secret string `json:"client_secret"`
			}{strings.TrimSpace(id), strings.TrimSpace(secret)})
			if err != nil {
				return err
			}
			if err := sec.Set(GoogleClientKey, b); err != nil {
				return fmt.Errorf("store client credentials: %w", err)
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

// coreNow is the app clock, defaulting to time.Now for Apps built by hand.
func coreNow(a *App) time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
