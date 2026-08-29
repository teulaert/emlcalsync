package cli

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/calendar"
	"github.com/teulaert/emlcalsync/internal/compose"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/store"
)

func init() {
	Register(func(root *cobra.Command, app *App) { root.AddCommand(mailCmd(app)) })
}

// mailCmd is the `mail` command group: read commands are safe to allowlist,
// write commands (mark/move/archive/trash/draft/send/reply) change the server.
func mailCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mail",
		Short: "Read, search and act on archived mail",
		Long: `Read and write commands are split so an agent policy can allowlist the
read half (mailboxes, list, search, read, thread, attachment list) and gate
the write half (mark, move, archive, trash, draft, send, reply).

All read commands work offline from the local index. The list-shaped commands
(list, search) share these filters — see 'emlcal mail list --help':

  --mailbox inbox|drafts|sent|archive|<name>  what is actually in a mailbox
  --unread  --flagged  --no-bulk              state filters
  --since 2d  --until 2026-08-01              time window
  --from X  --to X  --account A  --limit N    who / where / how many

Examples:
  emlcal mail list --mailbox inbox              everything still in the inbox
  emlcal mail list --mailbox inbox --unread     unread inbox mail only
  emlcal mail list --since 2d --no-bulk         last two days, no newsletters
  emlcal mail search "invoice august" --from acme
  emlcal mail read fastmail:Stn1JutmP6KN
  emlcal mail thread fastmail:Stn1JutmP6KN`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		mailMailboxesCmd(app),
		mailListCmd(app),
		mailSearchCmd(app),
		mailReadCmd(app),
		mailThreadCmd(app),
		mailAttachmentCmd(app),
		mailMarkCmd(app),
		mailMoveCmd(app),
		mailArchiveCmd(app),
		mailTrashCmd(app),
		mailDraftCmd(app),
		mailSendCmd(app),
		mailReplyCmd(app),
	)
	return cmd
}

// ---------------------------------------------------------------------------
// Presentation types shared by list, search, read and thread.

// mailMessageRow is one row of `mail list` / `mail search`. Field order is the
// table column order: ID, DATE, FROM, SUBJECT, FLAGS, ACCOUNT.
type mailMessageRow struct {
	ID       string      `json:"id"                      table:"ID"`
	ThreadID string      `json:"thread_id,omitempty"`
	Date     output.Time `json:"date"`
	DateUTC  int64       `json:"date_utc"`
	DateRel  string      `json:"-"                       table:"DATE"`

	From      model.Address `json:"from"`
	FromShort string        `json:"-"                    table:"FROM,max=24"`

	Subject string `json:"subject"                    table:"SUBJECT,max=60"`
	Snippet string `json:"snippet,omitempty"`
	FlagStr string `json:"-"                          table:"FLAGS"`

	Unread         bool     `json:"unread"`
	Flagged        bool     `json:"flagged"`
	Draft          bool     `json:"draft"`
	Answered       bool     `json:"answered"`
	HasAttachments bool     `json:"has_attachments"`
	Deleted        bool     `json:"deleted,omitempty"`
	Mailboxes      []string `json:"mailboxes"`

	Account string `json:"account"                    table:"ACCOUNT"`

	// Only `mail search` fills these in; a zero rank is omitted so plain list
	// output stays token-cheap.
	Rank      float64 `json:"rank,omitempty"`
	Highlight string  `json:"highlight,omitempty"`
}

// mailThreadRow is one row of `mail list --thread`.
type mailThreadRow struct {
	ID       string      `json:"id"           table:"ID"`
	Last     output.Time `json:"last"`
	LastUTC  int64       `json:"last_utc"`
	LastRel  string      `json:"-"            table:"LAST"`
	Subject  string      `json:"subject"      table:"SUBJECT,max=60"`
	Count    int         `json:"count"        table:"N"`
	Unread   int         `json:"unread"       table:"UNREAD"`
	First    output.Time `json:"first"`
	FirstUTC int64       `json:"first_utc"`

	Participants []model.Address `json:"participants"`
	PartShort    string          `json:"-"        table:"PARTICIPANTS,max=30"`

	Account string `json:"account"          table:"ACCOUNT"`
}

// mailAttachmentRow is one row of `mail attachment list`.
type mailAttachmentRow struct {
	Part        string `json:"part"          table:"PART"`
	Filename    string `json:"filename"      table:"FILENAME,max=40"`
	ContentType string `json:"content_type"  table:"TYPE,max=30"`
	Size        int64  `json:"size"          table:"SIZE"`
	Inline      bool   `json:"inline"        table:"INLINE"`
}

// mailWriteRow is what every flag/mailbox write prints per message.
type mailWriteRow struct {
	ID       string `json:"id"                  table:"ID"`
	OK       bool   `json:"ok"                  table:"OK"`
	Queued   bool   `json:"queued"              table:"QUEUED"`
	RemoteID string `json:"remote_id,omitempty" table:"REMOTE"`
	// NewID is set when the write moved the message and gave it a new id, which
	// on IMAP is what archiving or trashing does. Without it the caller is left
	// holding an id that no longer names anything — and an agent that recorded
	// the old one would look it up and be told it does not exist.
	NewID string `json:"new_id,omitempty" table:"NEW-ID"`
}

// mailSendRow is what draft/send/reply print.
type mailSendRow struct {
	ID      string   `json:"id"              table:"ID"`
	Account string   `json:"account"         table:"ACCOUNT"`
	Queued  bool     `json:"queued"          table:"QUEUED"`
	To      []string `json:"to"`
	ToStr   string   `json:"-"               table:"TO,max=40"`
	Subject string   `json:"subject"         table:"SUBJECT,max=50"`
	// DraftTrashed is set only by `send --draft`: the stored draft is removed
	// once its copy is on its way. nil elsewhere, so it stays out of the JSON.
	DraftTrashed *bool `json:"draft_trashed,omitempty" table:"DRAFT-TRASHED"`
}

// mailNameIndex maps account → mailbox remote id → display name, so list rows
// can show names instead of provider ids.
type mailNameIndex map[string]map[string]string

func mailMailboxNames(ctx context.Context, st *store.Store, accounts []string) (mailNameIndex, error) {
	idx := mailNameIndex{}
	for _, acct := range accounts {
		mbs, err := st.ListMailboxes(ctx, acct)
		if err != nil {
			return nil, err
		}
		m := make(map[string]string, len(mbs))
		for _, mb := range mbs {
			m[mb.RemoteID] = mb.Name
		}
		idx[acct] = m
	}
	return idx, nil
}

// names resolves a message's mailbox remote ids to display names, keeping the
// raw id when the mailbox list has not caught up yet.
func (idx mailNameIndex) names(account string, remotes []string) []string {
	out := make([]string, 0, len(remotes))
	for _, r := range remotes {
		if n, ok := idx[account][r]; ok && n != "" {
			out = append(out, n)
			continue
		}
		out = append(out, r)
	}
	return out
}

// mailRowOf builds a list row from an indexed message.
func mailRowOf(m *model.Message, idx mailNameIndex, now time.Time) mailMessageRow {
	flags := output.MailFlags(m.Flags, m.HasAttachments)
	if m.DeletedAt != nil {
		// X, not D: MailFlags spends D on drafts, and a column where one
		// letter could mean either "not sent yet" or "gone from the server"
		// is worse than no column.
		flags += "X"
	}
	return mailMessageRow{
		ID:             m.PublicID(),
		ThreadID:       model.ThreadPublicID(m.AccountID, m.ThreadID),
		Date:           output.T(m.Received),
		DateUTC:        m.Received.Unix(),
		DateRel:        output.RelTime(m.Received, now),
		From:           m.From,
		FromShort:      output.ShortAddr(m.From),
		Subject:        m.Subject,
		Snippet:        m.Snippet,
		FlagStr:        flags,
		Deleted:        m.DeletedAt != nil,
		Unread:         m.Flags.Unread,
		Flagged:        m.Flags.Flagged,
		Draft:          m.Flags.Draft,
		Answered:       m.Flags.Answered,
		HasAttachments: m.HasAttachments,
		Mailboxes:      idx.names(m.AccountID, m.MailboxRemotes),
		Account:        m.AccountID,
	}
}

// ---------------------------------------------------------------------------
// Shared filter flags.

// mailFilterFlags holds the filter set every list-shaped command accepts.
type mailFilterFlags struct {
	mailbox        string
	unread         bool
	flagged        bool
	from           string
	to             string
	since          string
	until          string
	noBulk         bool
	includeDeleted bool
	byThread       bool
	limit          int
	offset         int
}

// register binds the filter flags; withThread adds --thread (list only).
func (f *mailFilterFlags) register(cmd *cobra.Command, withThread bool) {
	fl := cmd.Flags()
	fl.StringVar(&f.mailbox, "mailbox", "", "restrict to a mailbox name or role (inbox, sent, archive, …)")
	fl.BoolVar(&f.unread, "unread", false, "only unread messages")
	fl.BoolVar(&f.flagged, "flagged", false, "only flagged messages")
	fl.StringVar(&f.from, "from", "", "sender address or name contains")
	fl.StringVar(&f.to, "to", "", "recipient (To or Cc) contains")
	fl.StringVar(&f.since, "since", "", "only messages received after (2d, 12h, 2026-08-01)")
	fl.StringVar(&f.until, "until", "", "only messages received before (2d, 12h, 2026-08-01)")
	fl.BoolVar(&f.noBulk, "no-bulk", false, "exclude mailing lists and automated mail")
	fl.BoolVar(&f.includeDeleted, "include-deleted", false, "also show messages that no longer exist on the server (kept in the archive)")
	if withThread {
		fl.BoolVar(&f.byThread, "thread", false, "group results by thread")
	}
	fl.IntVar(&f.limit, "limit", 50, "maximum results")
	fl.IntVar(&f.offset, "offset", 0, "skip the first N results")
}

// build turns the flags into a store filter, resolving --mailbox against the
// selected accounts.
func (f *mailFilterFlags) build(cmd *cobra.Command, app *App, st *store.Store, accounts []string) (store.MessageFilter, error) {
	filter := store.MessageFilter{
		Accounts:       accounts,
		From:           f.from,
		To:             f.to,
		NoBulk:         f.noBulk,
		IncludeDeleted: f.includeDeleted,
		Limit:          f.limit,
		Offset:         f.offset,
	}
	if cmd.Flags().Changed("unread") {
		v := f.unread
		filter.Unread = &v
	}
	if cmd.Flags().Changed("flagged") {
		v := f.flagged
		filter.Flagged = &v
	}
	since, until, err := calendar.ParseRange(f.since, f.until, app.Now(), app.Location())
	if err != nil {
		return filter, output.Errorf(output.ExitUsage, "%v", err)
	}
	filter.Since, filter.Until = since, until

	if f.mailbox != "" {
		role, name, err := mailResolveMailbox(cmd.Context(), st, accounts, f.mailbox)
		if err != nil {
			return filter, err
		}
		filter.MailboxRole, filter.MailboxName = role, name
	}
	return filter, nil
}

// mailResolveMailbox resolves a user-typed mailbox across the selected
// accounts. When the input names a role (and the match carries it) the filter
// matches by role, so `--mailbox inbox` works across providers that call it
// different things; otherwise it matches by name.
func mailResolveMailbox(ctx context.Context, st *store.Store, accounts []string, nameOrRole string) (role, name string, err error) {
	for _, acct := range accounts {
		mb, err := st.FindMailbox(ctx, acct, nameOrRole)
		if err != nil {
			continue
		}
		if mb.Role != "" && strings.EqualFold(string(mb.Role), nameOrRole) {
			return string(mb.Role), "", nil
		}
		return "", mb.Name, nil
	}
	return "", "", output.Errorf(output.ExitNotFound, "no mailbox %q in %s: %w",
		nameOrRole, strings.Join(accounts, ", "), model.ErrNotFound)
}

// mailAccountMailbox resolves a mailbox for one account (write commands).
func mailAccountMailbox(ctx context.Context, st *store.Store, account, nameOrRole string) (*model.Mailbox, error) {
	mb, err := st.FindMailbox(ctx, account, nameOrRole)
	if err != nil {
		return nil, output.Errorf(output.ExitNotFound, "no mailbox %q in account %s: %w",
			nameOrRole, account, model.ErrNotFound)
	}
	return mb, nil
}

// ---------------------------------------------------------------------------
// Addresses.

// mailParseAddress accepts "Name <a@b>" and a bare "a@b". The parsing lives
// in internal/compose, which the TUI's composer shares; what belongs here is
// the exit code, which is a CLI concept.
func mailParseAddress(s string) (model.Address, error) {
	a, err := compose.ParseAddress(s)
	if err != nil {
		return model.Address{}, output.Errorf(output.ExitUsage, "%v", err)
	}
	return a, nil
}

// mailParseAddressList splits repeated/comma-separated flag values into
// addresses.
func mailParseAddressList(values []string) ([]model.Address, error) {
	out, err := compose.ParseAddressList(values)
	if err != nil {
		return nil, output.Errorf(output.ExitUsage, "%v", err)
	}
	return out, nil
}

// mailAddressStrings renders addresses for a header line or a JSON list.
func mailAddressStrings(addrs []model.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

func mailJoinAddresses(addrs []model.Address) string {
	return strings.Join(mailAddressStrings(addrs), ", ")
}

// mailEmails returns just the addresses, for the `to` field of send output.
func mailEmails(addrs []model.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Email)
	}
	return out
}
