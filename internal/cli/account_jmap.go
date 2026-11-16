package cli

import (
	"github.com/spf13/cobra"
)

// coreAccountAddJMAPCmd adds an account on any JMAP server (RFC 8620): mail,
// sending, calendars and push over one session, authenticated with one bearer
// token. Fastmail has its own preset (`account add fastmail`); this is for
// every other server.
func coreAccountAddJMAPCmd(app *App) *cobra.Command {
	var opts coreAddOptions
	cmd := &cobra.Command{
		Use:   "jmap",
		Short: "Add an account on any JMAP server",
		Long: "Adds an account on a JMAP server emlcal has no preset for (RFC 8620).\n\n" +
			"--session-url is the server's JMAP session resource, usually\n" +
			"https://<host>/.well-known/jmap. The API token is sent as a Bearer token;\n" +
			"paste it when prompted, or pass --token-stdin.\n\n" +
			"Everything comes from that one session:\n" +
			"  mail       urn:ietf:params:jmap:mail\n" +
			"  sending    urn:ietf:params:jmap:submission\n" +
			"  calendars  urn:ietf:params:jmap:calendars, when the session offers it;\n" +
			"             without it the account syncs mail only\n" +
			"  push       the session's eventSourceUrl\n\n" +
			"The token needs whatever scopes the server uses for those: read and write\n" +
			"mail, send, and calendars.",
		Example: "  emlcal account add jmap --name work --email me@example.com \\\n" +
			"    --session-url https://mail.example.com/.well-known/jmap --token-stdin < token",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return coreAddAccount(app, opts)
		},
	}
	cmd.Flags().StringVar(&opts.Name, "name", "", "short account name, used in ids ([a-z0-9-])")
	cmd.Flags().StringVar(&opts.Email, "email", "", "the account's email address")
	cmd.Flags().StringVar(&opts.DisplayName, "display-name", "", "the name in front of the address on mail you send")
	cmd.Flags().StringVar(&opts.SessionURL, "session-url", "",
		"the server's JMAP session URL, e.g. https://mail.example.com/.well-known/jmap")
	cmd.Flags().BoolVar(&opts.TokenStdin, "token-stdin", false, "read the API token from stdin instead of prompting")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("email")
	_ = cmd.MarkFlagRequired("session-url")
	return cmd
}
