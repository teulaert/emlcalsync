package cli

import (
	"context"
	"io"
	stdmime "mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/compose"
	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/store"
	"github.com/teulaert/emlcalsync/internal/sync"
)

// ---------------------------------------------------------------------------
// Shared apply plumbing.

// mailApply runs one op per account group, prints a row per message and
// returns exit 6 when anything had to be queued.
func mailApply(cmd *cobra.Command, app *App, ids []string, opFor func(group IDGroup) (sync.Op, error)) error {
	groups, err := app.GroupMessageIDs(ids)
	if err != nil {
		return err
	}
	eng, err := app.Engine()
	if err != nil {
		return err
	}
	rows := make([]mailWriteRow, 0, len(ids))
	queued := 0
	for _, g := range groups {
		op, err := opFor(g)
		if err != nil {
			return err
		}
		op.IDs = g.Remotes
		res, err := eng.Apply(cmd.Context(), g.Account, op)
		if err != nil {
			return err
		}
		if res.Queued {
			queued++
		}
		moved := map[string]string{}
		for _, rn := range res.Renames {
			moved[rn.Old] = rn.New
		}
		for _, r := range g.Remotes {
			row := mailWriteRow{
				ID:       PublicMessageID(g.Account, r),
				OK:       !res.Queued,
				Queued:   res.Queued,
				RemoteID: res.RemoteID,
			}
			if to, ok := moved[r]; ok && to != r {
				row.NewID = PublicMessageID(g.Account, to)
			}
			rows = append(rows, row)
		}
	}
	if err := app.Printer().Print(rows); err != nil {
		return err
	}
	if queued > 0 {
		return Queued(queued)
	}
	return nil
}

// ---------------------------------------------------------------------------
// mail mark

func mailMarkCmd(app *App) *cobra.Command {
	var read, unread, flag, unflag bool
	cmd := &cobra.Command{
		Use:   "mark <id>... --read|--unread|--flag|--unflag",
		Short: "Change read/flagged state",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !read && !unread && !flag && !unflag {
				return output.Errorf(output.ExitUsage, "pass at least one of --read, --unread, --flag, --unflag")
			}
			if read && unread {
				return output.Errorf(output.ExitUsage, "--read and --unread contradict each other")
			}
			if flag && unflag {
				return output.Errorf(output.ExitUsage, "--flag and --unflag contradict each other")
			}
			var op sync.Op
			op.Kind = sync.OpFlags
			op.Flags.Set.Unread = unread
			op.Flags.Clear.Unread = read
			op.Flags.Set.Flagged = flag
			op.Flags.Clear.Flagged = unflag
			return mailApply(cmd, app, args, func(IDGroup) (sync.Op, error) { return op, nil })
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&read, "read", false, "mark as read")
	fl.BoolVar(&unread, "unread", false, "mark as unread")
	fl.BoolVar(&flag, "flag", false, "flag (star)")
	fl.BoolVar(&unflag, "unflag", false, "remove the flag")
	return cmd
}

// ---------------------------------------------------------------------------
// mail move / archive / trash

func mailMoveCmd(app *App) *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "move <id>... --to <mailbox>",
		Short: "Move messages to another mailbox",
		Long: `Add the messages to the target mailbox and take them out of the inbox.
The mailbox is resolved per account by role, exact name, or unique prefix.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := app.Store()
			if err != nil {
				return err
			}
			return mailApply(cmd, app, args, func(g IDGroup) (sync.Op, error) {
				target, err := mailAccountMailbox(cmd.Context(), st, g.Account, to)
				if err != nil {
					return sync.Op{}, err
				}
				op := sync.Op{Kind: sync.OpMailboxes, AddMailboxes: []string{target.RemoteID}}
				if target.Role != model.RoleInbox {
					if inbox, err := st.FindMailbox(cmd.Context(), g.Account, string(model.RoleInbox)); err == nil {
						op.RemoveMailboxes = []string{inbox.RemoteID}
					}
				}
				return op, nil
			})
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "target mailbox name or role")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func mailArchiveCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "archive <id>...",
		Short: "Archive messages (out of the inbox)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return mailApply(cmd, app, args, func(IDGroup) (sync.Op, error) {
				return sync.Op{Kind: sync.OpArchive}, nil
			})
		},
	}
}

func mailTrashCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "trash <id>...",
		Short: "Move messages to the trash",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return mailApply(cmd, app, args, func(IDGroup) (sync.Op, error) {
				return sync.Op{Kind: sync.OpTrash}, nil
			})
		},
	}
}

func mailRestoreCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <id>...",
		Short: "Move messages back to the inbox, out of the archive or trash",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return mailApply(cmd, app, args, func(IDGroup) (sync.Op, error) {
				return sync.Op{Kind: sync.OpRestore}, nil
			})
		},
	}
}

// ---------------------------------------------------------------------------
// Compose: draft / send / reply

// mailComposeFlags is the flag set draft, send and reply share.
type mailComposeFlags struct {
	account  string
	from     string
	to       []string
	cc       []string
	bcc      []string
	subject  string
	body     string
	bodyFile string
	attach   []string
	reply    string
	all      bool
	dryRun   bool
	draft    string
	forward  string
	noFiles  bool
}

func (f *mailComposeFlags) register(cmd *cobra.Command, withReply, withDraft bool) {
	fl := cmd.Flags()
	fl.StringVar(&f.account, "account", "", "account to send from (overrides the default)")
	fl.StringVar(&f.from, "from", "", "From address (defaults to the account address)")
	fl.StringArrayVar(&f.to, "to", nil, "recipient (repeatable, comma separated)")
	fl.StringArrayVar(&f.cc, "cc", nil, "Cc recipient (repeatable, comma separated)")
	fl.StringArrayVar(&f.bcc, "bcc", nil, "Bcc recipient (repeatable, comma separated)")
	fl.StringVar(&f.subject, "subject", "", "subject")
	fl.StringVar(&f.body, "body", "", "message text")
	fl.StringVar(&f.bodyFile, "body-file", "", "read the message text from a file (\"-\" for stdin)")
	fl.StringArrayVar(&f.attach, "attach", nil, "file to attach (repeatable)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the RFC 822 message that would be sent and stop")
	if withReply {
		fl.StringVar(&f.reply, "reply", "", "reply to this message id")
		fl.BoolVar(&f.all, "all", false, "reply to all recipients")
		fl.StringVar(&f.forward, "forward", "", "forward this message id")
		fl.BoolVar(&f.noFiles, "no-attachments", false,
			"with --forward: leave the original's files behind")
	}
	if withDraft {
		fl.StringVar(&f.draft, "draft", "", "send an existing draft by id")
	}
}

// mailBodyText resolves --body / --body-file. optional is for a forward,
// where sending somebody a message with nothing written above it is an
// ordinary thing to do -- a send or a reply with no body is a mistake.
func (f *mailComposeFlags) bodyText(cmd *cobra.Command, app *App, optional bool) (string, error) {
	if optional && f.bodyFile == "" && !cmd.Flags().Changed("body") {
		return "", nil
	}
	switch {
	case f.bodyFile != "" && cmd.Flags().Changed("body"):
		return "", output.Errorf(output.ExitUsage, "--body and --body-file are mutually exclusive")
	case f.bodyFile == "-":
		b, err := io.ReadAll(app.Stdin)
		if err != nil {
			return "", err
		}
		return string(b), nil
	case f.bodyFile != "":
		b, err := os.ReadFile(f.bodyFile)
		if err != nil {
			return "", output.Errorf(output.ExitUsage, "--body-file: %v", err)
		}
		return string(b), nil
	case cmd.Flags().Changed("body"):
		return f.body, nil
	}
	return "", output.Errorf(output.ExitUsage, "one of --body or --body-file is required")
}

// mailAttachments reads the --attach files.
func (f *mailComposeFlags) attachments() ([]mime.DraftAttachment, error) {
	var out []mime.DraftAttachment
	for _, p := range f.attach {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, output.Errorf(output.ExitUsage, "--attach: %v", err)
		}
		ct := stdmime.TypeByExtension(filepath.Ext(p))
		if ct == "" {
			ct = "application/octet-stream"
		}
		out = append(out, mime.DraftAttachment{Filename: filepath.Base(p), ContentType: ct, Data: data})
	}
	return out, nil
}

// mailComposed is a built outgoing message plus the context needed to send it.
type mailComposed struct {
	account  *config.Account
	raw      []byte
	to       []model.Address
	subject  string
	threadID string
	// from and recipients are the SMTP envelope. Bcc is deliberately absent
	// from raw -- the header must not reach the recipients -- so a backend that
	// submits over SMTP has nothing to put in RCPT TO unless we say it here.
	from       string
	recipients []string
	original   *model.Message // set for replies, so Answered can be flagged
	// draftRemote is the remote id of the stored draft `send --draft` is
	// submitting; the draft is trashed once the copy is on its way.
	draftRemote string
}

// mailCompose resolves the account, builds the reply context if any and
// produces the RFC 822 bytes.
func mailCompose(cmd *cobra.Command, app *App, f *mailComposeFlags, replyID string) (*mailComposed, error) {
	if replyID != "" && f.forward != "" {
		return nil, output.Errorf(output.ExitUsage,
			"--reply and --forward contradict each other: a message is either answered or passed on")
	}
	// contextID is the message this one is about, answered or forwarded. It
	// says which account to send from when nothing else does.
	contextID := replyID
	if contextID == "" {
		contextID = f.forward
	}

	// Account: --account wins, then the account of the message replied to,
	// then the configured default (or the only account).
	var acct *config.Account
	var err error
	switch {
	case f.account != "":
		acct, err = app.ResolveAccount(f.account)
	case contextID != "":
		var a string
		a, _, err = app.ParseMessageID(contextID)
		if err == nil {
			acct, err = app.ResolveAccount(a)
		}
	default:
		acct, err = app.SendAccount("")
	}
	if err != nil {
		return nil, err
	}

	from := model.Address{Email: acct.Email}
	if f.from != "" {
		from, err = mailParseAddress(f.from)
		if err != nil {
			return nil, err
		}
	}

	to, err := mailParseAddressList(f.to)
	if err != nil {
		return nil, err
	}
	cc, err := mailParseAddressList(f.cc)
	if err != nil {
		return nil, err
	}
	bcc, err := mailParseAddressList(f.bcc)
	if err != nil {
		return nil, err
	}

	text, err := f.bodyText(cmd, app, f.forward != "")
	if err != nil {
		return nil, err
	}
	atts, err := f.attachments()
	if err != nil {
		return nil, err
	}

	draft := &mime.Draft{
		From:        from,
		To:          to,
		Cc:          cc,
		Bcc:         bcc,
		Subject:     f.subject,
		TextBody:    text,
		Attachments: atts,
		Date:        app.Now(),
	}
	out := &mailComposed{account: acct}

	switch {
	case replyID != "":
		orig, err := mailReplyContext(cmd.Context(), app, replyID, acct, from, f.all, draft, text)
		if err != nil {
			return nil, err
		}
		out.original = orig
		out.threadID = orig.ThreadID
	case f.forward != "":
		// No original and no thread id on purpose: a forward answers nothing,
		// so nothing is marked answered, and it starts a conversation with
		// somebody who was never in the old one.
		if err := mailForwardContext(cmd.Context(), app, f.forward, draft, text, f.noFiles); err != nil {
			return nil, err
		}
	}

	if len(draft.To) == 0 && len(draft.Cc) == 0 && len(draft.Bcc) == 0 {
		return nil, output.Errorf(output.ExitUsage, "at least one recipient is required (--to)")
	}

	raw, err := mime.Build(draft)
	if err != nil {
		return nil, err
	}
	out.raw = raw
	out.to = draft.To
	out.subject = draft.Subject
	out.from = draft.From.Email
	out.recipients = compose.Envelope(draft)
	return out, nil
}

// mailReplyContext fills subject, recipients, threading headers and the quoted
// original into draft, and returns the message being replied to.
func mailReplyContext(ctx context.Context, app *App, replyID string, acct *config.Account,
	from model.Address, all bool, draft *mime.Draft, text string) (*model.Message, error) {

	_, orig, _, err := mailLoadMessage(ctx, app, replyID)
	if err != nil {
		return nil, err
	}
	compose.Reply(draft, orig, all, []string{acct.Email, from.Email})
	draft.TextBody = text + "\n\n" + compose.Quote(orig, app.Location())
	return orig, nil
}

// mailForwardContext fills the subject, the body and the files of a forward
// into draft.
//
// It is [mailReplyContext]'s opposite number and differs from it in every way
// that matters: no recipients are worked out (who a forward goes to is the
// whole of what the sender has to say), no threading headers are set, and the
// original goes in whole through compose.Forwarded rather than quoted through
// compose.Quote -- the person receiving it has seen none of it.
func mailForwardContext(ctx context.Context, app *App, id string,
	draft *mime.Draft, text string, noFiles bool) error {

	_, orig, st, err := mailLoadMessage(ctx, app, id)
	if err != nil {
		return err
	}
	// An envelope-only stub (DESIGN.md §16) has no body until the raw bytes
	// are fetched, and a forward with nothing in it is not worth sending.
	if strings.TrimSpace(orig.TextBody) == "" && !orig.RawComplete {
		if raw, err := mailRawBytes(ctx, app, orig); err == nil {
			if parsed, err := mime.Parse(raw); err == nil {
				orig.TextBody = parsed.TextBody
			}
		}
	}
	if draft.Subject == "" {
		draft.Subject = compose.ForwardSubject(orig.Subject)
	}
	body := compose.Forwarded(orig, app.Location())
	if strings.TrimSpace(text) == "" {
		draft.TextBody = body
	} else {
		draft.TextBody = text + "\n\n" + body
	}
	if noFiles || !orig.HasAttachments {
		return nil
	}
	files, err := mailForwardFiles(ctx, app, st, orig)
	if err != nil {
		return err
	}
	// After --attach, so a file named on the command line is not pushed down
	// the list by the ones that came with the message.
	draft.Attachments = append(draft.Attachments, files...)
	return nil
}

// mailForwardFiles fetches the attachments the forward carries, through the
// same engine call `mail attachment` uses: out of the archived raw message
// when there is one, off the provider when there is not.
//
// A file that will not come is an error here, where the TUI names it in the
// composer and lets the person decide. Nobody is watching a command run, and
// forwarding is mostly done for the attachment, so a forward that quietly went
// without one is the worst of the outcomes available. --no-attachments is how
// you ask for the text alone.
func mailForwardFiles(ctx context.Context, app *App, st *store.Store,
	orig *model.Message) ([]mime.DraftAttachment, error) {

	atts, err := st.ListAttachments(ctx, orig.ID)
	if err != nil {
		return nil, err
	}
	carry := compose.ForwardAttachments(atts)
	if len(carry) == 0 {
		return nil, nil
	}
	eng, err := app.Engine()
	if err != nil {
		return nil, err
	}
	out := make([]mime.DraftAttachment, 0, len(carry))
	total := 0
	for _, a := range carry {
		name := a.Filename
		if name == "" {
			name = a.PartPath
		}
		ref := a.RemoteRef
		if ref == "" {
			ref = a.PartPath
		}
		data, err := eng.FetchAttachment(ctx, orig.AccountID, orig.RemoteID, ref)
		if err != nil {
			code := output.ExitProvider
			if provider.IsOffline(err) {
				code = output.ExitOffline
			}
			return nil, output.Errorf(code,
				"attachment %q could not be fetched: %w — pass --no-attachments to forward the text alone",
				name, err)
		}
		total += len(data)
		if total > compose.MaxForwardBytes {
			return nil, output.Errorf(output.ExitUsage,
				"the attachments come to more than %s, which no provider will take: "+
					"pass --no-attachments and send the files another way",
				output.HumanSize(compose.MaxForwardBytes))
		}
		out = append(out, mime.DraftAttachment{
			Filename: name, ContentType: a.ContentType, Data: data,
		})
	}
	return out, nil
}

// mailSubmit applies a draft/send op and prints the result row.
func mailSubmit(cmd *cobra.Command, app *App, c *mailComposed, kind sync.OpKind) error {
	eng, err := app.Engine()
	if err != nil {
		return err
	}
	op := sync.Op{Kind: kind, Raw: c.raw}
	if kind == sync.OpSend {
		op.ThreadID = c.threadID
		op.From = c.from
		op.Recipients = c.recipients
	}
	res, err := eng.Apply(cmd.Context(), c.account.Name, op)
	if err != nil {
		// A submission that failed before the request left the machine comes
		// back queued (Queued=true, no error). Reaching here with a transport
		// error means the connection dropped mid-request, so the engine will
		// not replay it: the server may already have the message. Say that,
		// or the exit-4 error reads as "queued somewhere, will go out later".
		if provider.IsOffline(err) {
			what := "message not sent"
			if kind == sync.OpDraft {
				what = "draft not stored"
			}
			return output.Errorf(output.ExitOffline,
				"%s: %w — the connection dropped mid-request, so it was not queued for retry (the provider may already have it); check your sent mail before sending again", what, err)
		}
		return err
	}
	id := res.RemoteID
	if id != "" {
		id = PublicMessageID(c.account.Name, id)
	}
	row := mailSendRow{
		ID:      id,
		Account: c.account.Name,
		Queued:  res.Queued,
		To:      mailEmails(c.to),
		ToStr:   strings.Join(mailEmails(c.to), ", "),
		Subject: c.subject,
	}
	// The draft the send was built from is a separate message on the server:
	// submitting its raw bytes leaves it behind, so trash it once the send
	// actually went through. Best effort — a failure here must not fail the
	// send, it only leaves a stale draft the user can delete.
	if !res.Queued && c.draftRemote != "" && kind == sync.OpSend {
		trashed := true
		if _, err := eng.Apply(cmd.Context(), c.account.Name,
			sync.Op{Kind: sync.OpTrash, IDs: []string{c.draftRemote}}); err != nil {
			app.Logger().Warn("trash sent draft", "id",
				PublicMessageID(c.account.Name, c.draftRemote), "err", err)
			trashed = false
		}
		row.DraftTrashed = &trashed
	}
	// A reply that actually reached the provider marks the original answered;
	// failing to do so must not fail the send.
	if !res.Queued && c.original != nil && kind == sync.OpSend {
		var flagOp sync.Op
		flagOp.Kind = sync.OpFlags
		flagOp.IDs = []string{c.original.RemoteID}
		flagOp.Flags.Set.Answered = true
		if _, err := eng.Apply(cmd.Context(), c.account.Name, flagOp); err != nil {
			app.Logger().Warn("mark answered", "id", c.original.PublicID(), "err", err)
		}
	}
	if err := app.Printer().Print(row); err != nil {
		return err
	}
	if res.Queued {
		return Queued(1)
	}
	return nil
}

func mailDraftCmd(app *App) *cobra.Command {
	var f mailComposeFlags
	cmd := &cobra.Command{
		Use:   "draft",
		Short: "Store a draft on the server",
		Long: `Store a draft on the server, optionally as a reply (--reply <id>).

Like send, this is queued offline (exit 6) only when the request never left
the machine; a connection that dropped mid-request is not retried (exit 4).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := mailCompose(cmd, app, &f, f.reply)
			if err != nil {
				return err
			}
			if f.dryRun {
				_, err := app.Stdout.Write(c.raw)
				return err
			}
			return mailSubmit(cmd, app, c, sync.OpDraft)
		},
	}
	f.register(cmd, true, false)
	return cmd
}

func mailSendCmd(app *App) *cobra.Command {
	var f mailComposeFlags
	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send a message",
		Long: `Send a new message, a reply (--reply <id>), or an existing draft
(--draft <id>). --dry-run prints the exact RFC 822 bytes and sends nothing.

Offline, a send is queued in the outbox (exit 6) only when the request never
left the machine, so replaying it is safe. If the connection dropped while the
request was in flight the send is not retried (exit 4): the provider may
already have the message, so check your sent mail before sending again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.draft != "" {
				return mailSendDraft(cmd, app, &f)
			}
			c, err := mailCompose(cmd, app, &f, f.reply)
			if err != nil {
				return err
			}
			if f.dryRun {
				_, err := app.Stdout.Write(c.raw)
				return err
			}
			return mailSubmit(cmd, app, c, sync.OpSend)
		},
	}
	f.register(cmd, true, true)
	return cmd
}

// mailSendDraft submits the raw bytes of a stored draft.
func mailSendDraft(cmd *cobra.Command, app *App, f *mailComposeFlags) error {
	_, msg, _, err := mailLoadMessage(cmd.Context(), app, f.draft)
	if err != nil {
		return err
	}
	raw, err := mailRawBytes(cmd.Context(), app, msg)
	if err != nil {
		return err
	}
	acct, err := app.ResolveAccount(msg.AccountID)
	if err != nil {
		return err
	}
	if f.account != "" {
		if acct, err = app.ResolveAccount(f.account); err != nil {
			return err
		}
	}
	c := &mailComposed{
		account:  acct,
		raw:      raw,
		to:       msg.To,
		subject:  msg.Subject,
		threadID: msg.ThreadID,
	}
	// The draft is only ours to clean up when it lives in the account we are
	// sending from; --account elsewhere leaves it alone.
	if acct.Name == msg.AccountID {
		c.draftRemote = msg.RemoteID
	}
	if f.dryRun {
		_, err := app.Stdout.Write(raw)
		return err
	}
	return mailSubmit(cmd, app, c, sync.OpSend)
}

func mailForwardCmd(app *App) *cobra.Command {
	var f mailComposeFlags
	cmd := &cobra.Command{
		Use:   "forward <id> --to <addr>",
		Short: "Forward a message to somebody else",
		Long: `Pass a message on to somebody who has not seen it, from the account that
holds it. The original goes below your text whole -- the earlier rounds of the
conversation included, none of it quoted -- under the header block every mail
client writes, and the files come with it.

A body is optional here: a forward with nothing written above it is an
ordinary thing to send. The subject becomes "Fwd: ..." unless --subject says
otherwise, and no threading headers are set, so it starts a conversation with
the person receiving it rather than landing in one they were never in. The
message being forwarded is not marked answered, because it has not been.

The attachments are fetched from the archive, or from the provider when the
archive kept only the envelope. If one cannot be fetched the forward fails
rather than going out without it; --no-attachments forwards the text alone.`,
		Example: `  emlcal mail forward work:18f3a2b9c1d4e5f6 --to jan@example.com
  emlcal mail forward work:18f3a2b9c1d4e5f6 --to jan@example.com --body "Zie hieronder." --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f.forward = args[0]
			c, err := mailCompose(cmd, app, &f, "")
			if err != nil {
				return err
			}
			if f.dryRun {
				_, err := app.Stdout.Write(c.raw)
				return err
			}
			return mailSubmit(cmd, app, c, sync.OpSend)
		},
	}
	f.register(cmd, false, false)
	fl := cmd.Flags()
	fl.BoolVar(&f.noFiles, "no-attachments", false, "forward the text alone, without the original's files")
	return cmd
}

func mailReplyCmd(app *App) *cobra.Command {
	var f mailComposeFlags
	cmd := &cobra.Command{
		Use:   "reply <id>",
		Short: "Reply to a message",
		Long: `Reply to a message from the account that received it. The original is
quoted below your text and the thread headers are set, so the reply lands in
the same conversation. --all keeps the other recipients.

Offline the reply is queued (exit 6) when the request never left the machine,
and not retried (exit 4) when the connection dropped mid-request, since the
provider may already have it. Either way the original is marked answered only
once the reply has actually gone out.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := mailCompose(cmd, app, &f, args[0])
			if err != nil {
				return err
			}
			if f.dryRun {
				_, err := app.Stdout.Write(c.raw)
				return err
			}
			return mailSubmit(cmd, app, c, sync.OpSend)
		},
	}
	f.register(cmd, false, false)
	cmd.Flags().BoolVar(&f.all, "all", false, "reply to all recipients")
	return cmd
}
