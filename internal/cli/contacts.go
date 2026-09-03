package cli

import (
	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/compose"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/store"
)

func init() {
	Register(func(root *cobra.Command, app *App) { root.AddCommand(contactsCmd(app)) })
}

func contactsCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "contacts",
		Short: "The people in the archive, ranked by who you write to",
		Long: `The address book, derived from the mail already here: everyone who has
been on a message, ranked by how often you have written to them, then by how
often they turn up, then by how recently. Your own addresses and the robots
(noreply, notifications, mailer-daemon, …) are left out. There is nothing to
sync and nothing to maintain; the book follows the archive.

Both commands read the local index and never touch the network. The 'address'
field of a row is what to pass to --to, --cc or --bcc.

Examples:
  emlcal contacts list --limit 20
  emlcal contacts search vries
  emlcal mail send --to "$(emlcal contacts search vries | jq -r '.[0].address')" …`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(contactsListCmd(app), contactsSearchCmd(app))
	return cmd
}

func contactsListCmd(app *App) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the people you write to most, first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return contactsRun(cmd, app, "", limit)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "at most this many people (0 = everyone)")
	return cmd
}

func contactsSearchCmd(app *App) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Find a person by part of their name or address",
		Long: `Find a person by part of their name or address, case-insensitive. The
result is ranked like 'contacts list', so the first row is the best guess.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return contactsRun(cmd, app, args[0], limit)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "at most this many people (0 = everyone)")
	return cmd
}

// contactRow is one row of `contacts list` / `contacts search`. Address is
// the one field an agent needs: it goes straight into --to.
type contactRow struct {
	Name     string      `json:"name,omitempty"  table:"NAME,max=30"`
	Email    string      `json:"email"           table:"EMAIL,max=40"`
	Address  string      `json:"address"`
	Sent     int         `json:"sent"            table:"SENT"`
	Messages int         `json:"messages"        table:"MSGS"`
	Last     output.Time `json:"last"`
	LastUTC  int64       `json:"last_utc"`
	LastRel  string      `json:"-"               table:"LAST"`
	Accounts []string    `json:"accounts"        table:"ACCOUNTS"`
}

func contactsRun(cmd *cobra.Command, app *App, query string, limit int) error {
	accounts, err := app.AccountIDs()
	if err != nil {
		return err
	}
	st, err := app.Store()
	if err != nil {
		return err
	}
	// AccountIDs is every configured account when none is asked for, which
	// is the whole book; the filter is only worth passing when it narrows.
	var filter store.ContactFilter
	if len(app.Accounts) > 0 {
		filter.Accounts = accounts
	}
	filter.Query, filter.Limit = query, limit
	cs, err := st.SearchContacts(cmd.Context(), filter)
	if err != nil {
		return err
	}
	rows := make([]contactRow, 0, len(cs))
	for i := range cs {
		rows = append(rows, contactRowOf(&cs[i], app))
	}
	return app.Printer().Print(rows)
}

func contactRowOf(c *model.Contact, app *App) contactRow {
	return contactRow{
		Name:     c.Name,
		Email:    c.Email,
		Address:  compose.FormatAddress(c.Address()),
		Sent:     c.SentCount,
		Messages: c.Count,
		Last:     output.T(c.Last),
		LastUTC:  c.Last.Unix(),
		LastRel:  output.RelTime(c.Last, app.Now()),
		Accounts: c.Accounts,
	}
}
