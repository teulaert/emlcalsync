package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	gomessage "github.com/emersion/go-message"
	gotextproto "github.com/emersion/go-message/textproto"
	"github.com/spf13/cobra"

	"github.com/lennert/emlcal/internal/blob"
	"github.com/lennert/emlcal/internal/mime"
	"github.com/lennert/emlcal/internal/model"
	"github.com/lennert/emlcal/internal/output"
	"github.com/lennert/emlcal/internal/store"
)

// ---------------------------------------------------------------------------
// mail mailboxes

type mailMailboxRow struct {
	Account string `json:"account"  table:"ACCOUNT"`
	Name    string `json:"name"     table:"NAME,max=40"`
	Role    string `json:"role"     table:"ROLE"`
	Total   int    `json:"total"    table:"TOTAL"`
	Unread  int    `json:"unread"   table:"UNREAD"`
	ID      string `json:"id"`
}

func mailMailboxesCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "mailboxes",
		Short: "List mailboxes (labels) per account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			accounts, err := app.AccountIDs()
			if err != nil {
				return err
			}
			st, err := app.Store()
			if err != nil {
				return err
			}
			rows := []mailMailboxRow{}
			for _, acct := range accounts {
				mbs, err := st.ListMailboxes(cmd.Context(), acct)
				if err != nil {
					return err
				}
				for _, mb := range mbs {
					rows = append(rows, mailMailboxRow{
						Account: acct,
						Name:    mb.Name,
						Role:    string(mb.Role),
						Total:   mb.TotalCount,
						Unread:  mb.UnreadCount,
						ID:      mb.RemoteID,
					})
				}
			}
			return app.Printer().Print(rows)
		},
	}
}

// ---------------------------------------------------------------------------
// mail list

func mailListCmd(app *App) *cobra.Command {
	var f mailFilterFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List messages newest first",
		Long: `List messages from the local index, newest first.

Bodies are not included; use ` + "`mail read <id>`" + ` for one message. Prefer
--since to bound the result set.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			accounts, err := app.AccountIDs()
			if err != nil {
				return err
			}
			st, err := app.Store()
			if err != nil {
				return err
			}
			filter, err := f.build(cmd, app, st, accounts)
			if err != nil {
				return err
			}
			if f.byThread {
				threads, err := st.ListThreads(cmd.Context(), filter)
				if err != nil {
					return err
				}
				return app.Printer().Print(mailThreadRows(threads, app))
			}
			msgs, err := st.ListMessages(cmd.Context(), filter)
			if err != nil {
				return err
			}
			idx, err := mailMailboxNames(cmd.Context(), st, accounts)
			if err != nil {
				return err
			}
			rows := make([]mailMessageRow, 0, len(msgs))
			for i := range msgs {
				rows = append(rows, mailRowOf(&msgs[i], idx, app.Now()))
			}
			return app.Printer().Print(rows)
		},
	}
	f.register(cmd, true)
	return cmd
}

func mailThreadRows(threads []model.Thread, app *App) []mailThreadRow {
	rows := make([]mailThreadRow, 0, len(threads))
	for i := range threads {
		t := &threads[i]
		short := make([]string, 0, len(t.Participants))
		for _, p := range t.Participants {
			short = append(short, mailShortAddr(p))
		}
		rows = append(rows, mailThreadRow{
			ID:           t.PublicID(),
			Last:         output.T(t.Last),
			LastUTC:      t.Last.Unix(),
			LastRel:      output.RelTime(t.Last, app.Now()),
			Subject:      t.Subject,
			Count:        t.MessageCount,
			Unread:       t.UnreadCount,
			First:        output.T(t.First),
			FirstUTC:     t.First.Unix(),
			Participants: t.Participants,
			PartShort:    strings.Join(short, ", "),
			Account:      t.AccountID,
		})
	}
	return rows
}

// ---------------------------------------------------------------------------
// mail search

func mailSearchCmd(app *App) *cobra.Command {
	var f mailFilterFlags
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search the archive",
		Long: `Search message text, subjects, addresses and attachment names.

FTS5 syntax: terms are ANDed, "quoted phrases" match literally, AND/OR/NOT
combine, invo* is a prefix match, and subject:/from_addr:/from_name:/to_json:/
text_body:/attachment_names: restrict a term to one column.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			accounts, err := app.AccountIDs()
			if err != nil {
				return err
			}
			st, err := app.Store()
			if err != nil {
				return err
			}
			filter, err := f.build(cmd, app, st, accounts)
			if err != nil {
				return err
			}
			hits, err := st.Search(cmd.Context(), args[0], filter)
			if err != nil {
				if errors.Is(err, store.ErrBadQuery) {
					return output.Errorf(output.ExitUsage,
						"%v: quote phrases (\"quarterly report\"), combine with AND/OR/NOT, and escape stray punctuation", err)
				}
				return err
			}
			idx, err := mailMailboxNames(cmd.Context(), st, accounts)
			if err != nil {
				return err
			}
			rows := make([]mailMessageRow, 0, len(hits))
			for i := range hits {
				h := &hits[i]
				row := mailRowOf(&h.Message, idx, app.Now())
				row.Rank, row.Highlight = h.Rank, h.Highlight
				rows = append(rows, row)
			}
			return app.Printer().Print(rows)
		},
	}
	f.register(cmd, false)
	return cmd
}

// ---------------------------------------------------------------------------
// mail read

// mailHeader is one raw header field, in the order it appears in the message.
type mailHeader struct {
	Name  string `json:"name"   table:"HEADER"`
	Value string `json:"value"  table:"VALUE,max=80"`
}

// mailReadOut is the JSON shape of `mail read`.
type mailReadOut struct {
	ID             string              `json:"id"`
	ThreadID       string              `json:"thread_id,omitempty"`
	Account        string              `json:"account"`
	Subject        string              `json:"subject"`
	From           model.Address       `json:"from"`
	To             []model.Address     `json:"to,omitempty"`
	Cc             []model.Address     `json:"cc,omitempty"`
	ReplyTo        []model.Address     `json:"reply_to,omitempty"`
	Date           output.Time         `json:"date"`
	DateUTC        int64               `json:"date_utc"`
	Received       output.Time         `json:"received"`
	ReceivedUTC    int64               `json:"received_utc"`
	Mailboxes      []string            `json:"mailboxes"`
	Unread         bool                `json:"unread"`
	Flagged        bool                `json:"flagged"`
	Answered       bool                `json:"answered"`
	HasAttachments bool                `json:"has_attachments"`
	Attachments    []mailAttachmentRow `json:"attachments,omitempty"`
	Headers        []mailHeader        `json:"headers,omitempty"`
	Body           string              `json:"body"`
	MessageID      string              `json:"message_id,omitempty"`
	InReplyTo      string              `json:"in_reply_to,omitempty"`
}

func mailReadCmd(app *App) *cobra.Command {
	var full, html, raw, headers bool
	cmd := &cobra.Command{
		Use:   "read <id>",
		Short: "Show one message",
		Long: `Show one message. By default the text body is printed with quoted
replies and signatures stripped; --full keeps them, --html renders the HTML
alternative and --raw writes the archived RFC 822 bytes verbatim.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			account, remote, err := app.ParseMessageID(args[0])
			if err != nil {
				return err
			}
			st, err := app.Store()
			if err != nil {
				return err
			}
			msg, err := st.GetMessage(cmd.Context(), account, remote)
			if err != nil {
				if errors.Is(err, model.ErrNotFound) {
					return errNotFound("message", args[0])
				}
				return err
			}

			if raw {
				b, err := mailRawBytes(cmd.Context(), app, msg)
				if err != nil {
					return err
				}
				_, err = app.Stdout.Write(b)
				return err
			}

			body := mime.StripQuotes(msg.TextBody)
			switch {
			case full:
				body = msg.TextBody
			case html:
				body, err = mailHTMLBody(cmd.Context(), app, msg)
				if err != nil {
					return err
				}
			}

			out := mailReadOutOf(cmd.Context(), app, st, msg, body)
			if headers {
				hs, err := mailHeaderList(cmd.Context(), app, msg)
				if err != nil {
					return err
				}
				out.Headers = hs
			}
			if app.Printer().Format == output.JSON || app.Printer().Format == output.Auto {
				return app.Printer().Print(out)
			}
			return mailPrintReadable(app.Stdout, out)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&full, "full", false, "keep quoted replies and signatures")
	fl.BoolVar(&html, "html", false, "show the HTML alternative as text")
	fl.BoolVar(&raw, "raw", false, "write the raw RFC 822 message")
	fl.BoolVar(&headers, "headers", false, "include all headers")
	cmd.MarkFlagsMutuallyExclusive("full", "html", "raw")
	return cmd
}

// mailReadOutOf assembles the read shape, loading attachment rows.
func mailReadOutOf(ctx context.Context, app *App, st *store.Store, msg *model.Message, body string) mailReadOut {
	idx, _ := mailMailboxNames(ctx, st, []string{msg.AccountID})
	out := mailReadOut{
		ID:             msg.PublicID(),
		ThreadID:       model.ThreadPublicID(msg.AccountID, msg.ThreadID),
		Account:        msg.AccountID,
		Subject:        msg.Subject,
		From:           msg.From,
		To:             msg.To,
		Cc:             msg.Cc,
		ReplyTo:        msg.ReplyTo,
		Date:           output.T(msg.Date),
		DateUTC:        msg.Date.Unix(),
		Received:       output.T(msg.Received),
		ReceivedUTC:    msg.Received.Unix(),
		Mailboxes:      idx.names(msg.AccountID, msg.MailboxRemotes),
		Unread:         msg.Flags.Unread,
		Flagged:        msg.Flags.Flagged,
		Answered:       msg.Flags.Answered,
		HasAttachments: msg.HasAttachments,
		Body:           body,
		MessageID:      msg.MessageIDHeader,
		InReplyTo:      msg.InReplyTo,
	}
	if msg.HasAttachments {
		if atts, err := st.ListAttachments(ctx, msg.ID); err == nil {
			out.Attachments = mailAttachmentRows(atts)
		}
	}
	return out
}

func mailAttachmentRows(atts []model.Attachment) []mailAttachmentRow {
	rows := make([]mailAttachmentRow, 0, len(atts))
	for _, a := range atts {
		rows = append(rows, mailAttachmentRow{
			Part:        a.PartPath,
			Filename:    a.Filename,
			ContentType: a.ContentType,
			Size:        a.Size,
			Inline:      a.Inline,
		})
	}
	return rows
}

// mailPrintReadable writes the human form: a header block, a blank line, the
// body.
func mailPrintReadable(w io.Writer, out mailReadOut) error {
	var b strings.Builder
	fmt.Fprintf(&b, "From:    %s\n", out.From.String())
	if len(out.To) > 0 {
		fmt.Fprintf(&b, "To:      %s\n", mailJoinAddresses(out.To))
	}
	if len(out.Cc) > 0 {
		fmt.Fprintf(&b, "Cc:      %s\n", mailJoinAddresses(out.Cc))
	}
	fmt.Fprintf(&b, "Date:    %s\n", out.Date.String())
	fmt.Fprintf(&b, "Subject: %s\n", out.Subject)
	if len(out.Mailboxes) > 0 {
		fmt.Fprintf(&b, "Mailboxes: %s\n", strings.Join(out.Mailboxes, ", "))
	}
	for _, a := range out.Attachments {
		fmt.Fprintf(&b, "Attachment: %s  %s  %s  (part %s)\n",
			a.Filename, a.ContentType, output.HumanSize(a.Size), a.Part)
	}
	for _, h := range out.Headers {
		fmt.Fprintf(&b, "%s: %s\n", h.Name, h.Value)
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimRight(out.Body, "\n"))
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// mailRawBytes returns the archived RFC 822 bytes. A complete blob is read
// straight from the archive so `--raw` works offline and without credentials;
// only an envelope-only row (raw_complete = 0) needs the provider.
func mailRawBytes(ctx context.Context, app *App, msg *model.Message) ([]byte, error) {
	if msg.RawComplete && msg.BlobSHA256 != "" {
		if bs, err := app.Blobs(); err == nil {
			if raw, err := bs.Get(msg.BlobSHA256); err == nil {
				return raw, nil
			} else if !errors.Is(err, model.ErrNotFound) && !errors.Is(err, blob.ErrCorrupt) {
				return nil, err
			}
		}
	}
	eng, err := app.Engine()
	if err != nil {
		return nil, err
	}
	return eng.EnsureRaw(ctx, msg.AccountID, msg.RemoteID)
}

// mailHTMLBody renders the HTML alternative of a message as text.
func mailHTMLBody(ctx context.Context, app *App, msg *model.Message) (string, error) {
	raw, err := mailRawBytes(ctx, app, msg)
	if err != nil {
		return "", err
	}
	parsed, err := mime.Parse(raw)
	if err != nil {
		return "", err
	}
	if !parsed.HasHTML || parsed.HTMLPart == "" {
		return "", output.Errorf(output.ExitNotFound, "message %s has no HTML part: %w",
			msg.PublicID(), model.ErrNotFound)
	}
	data, _, _, err := mime.PartContent(raw, parsed.HTMLPart)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// mailHeaderList returns every header field in message order.
func mailHeaderList(ctx context.Context, app *App, msg *model.Message) ([]mailHeader, error) {
	raw, err := mailRawBytes(ctx, app, msg)
	if err != nil {
		return nil, err
	}
	th, err := gotextproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return nil, fmt.Errorf("read headers: %w", err)
	}
	h := gomessage.Header{Header: th}
	var out []mailHeader
	fields := h.Fields()
	for fields.Next() {
		v, err := fields.Text()
		if err != nil {
			// Unknown charset in an encoded word: show the raw value rather
			// than failing the whole command.
			v = fields.Value()
		}
		out = append(out, mailHeader{Name: fields.Key(), Value: v})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// mail thread

type mailThreadOut struct {
	ID       string        `json:"id"`
	Subject  string        `json:"subject"`
	Count    int           `json:"count"`
	Account  string        `json:"account"`
	Messages []mailReadOut `json:"messages"`
}

func mailThreadCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "thread <id>",
		Short: "Show a whole thread, oldest first",
		Long: `Show every message of a thread, oldest first, with quoted replies
stripped. The id may be a thread id (work:t:abc) or any message id in it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := app.Store()
			if err != nil {
				return err
			}
			account, threadID, err := mailResolveThreadID(cmd.Context(), app, st, args[0])
			if err != nil {
				return err
			}
			thread, msgs, err := st.GetThread(cmd.Context(), account, threadID, false)
			if err != nil {
				if errors.Is(err, model.ErrNotFound) {
					return errNotFound("thread", args[0])
				}
				return err
			}
			out := mailThreadOut{
				ID:      model.ThreadPublicID(account, threadID),
				Subject: thread.Subject,
				Count:   len(msgs),
				Account: account,
			}
			for i := range msgs {
				out.Messages = append(out.Messages,
					mailReadOutOf(cmd.Context(), app, st, &msgs[i], mime.StripQuotes(msgs[i].TextBody)))
			}
			if app.Printer().Format == output.JSON || app.Printer().Format == output.Auto {
				return app.Printer().Print(out)
			}
			var b strings.Builder
			for _, m := range out.Messages {
				fmt.Fprintf(&b, "--- %s  %s\n", m.Date.String(), m.From.String())
				b.WriteString(strings.TrimRight(m.Body, "\n"))
				b.WriteString("\n\n")
			}
			_, err = io.WriteString(app.Stdout, b.String())
			return err
		},
	}
}

// mailResolveThreadID accepts a thread id or a message id.
func mailResolveThreadID(ctx context.Context, app *App, st *store.Store, id string) (account, threadID string, err error) {
	p, perr := model.ParseID(id)
	if perr != nil {
		return "", "", output.Errorf(output.ExitUsage, "not a thread or message id: %q", id)
	}
	if _, err := app.ResolveAccount(p.Account); err != nil {
		return "", "", err
	}
	switch p.Kind {
	case model.KindThread:
		return p.Account, p.Remote, nil
	case model.KindMessage:
		msg, err := st.GetMessage(ctx, p.Account, p.Remote)
		if err != nil {
			if errors.Is(err, model.ErrNotFound) {
				return "", "", errNotFound("message", id)
			}
			return "", "", err
		}
		return p.Account, msg.ThreadID, nil
	}
	return "", "", output.Errorf(output.ExitUsage, "not a thread or message id: %q", id)
}

// ---------------------------------------------------------------------------
// mail attachment

func mailAttachmentCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attachment",
		Short: "List and download attachments",
		Args:  cobra.NoArgs,
		RunE:  func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(mailAttachmentListCmd(app), mailAttachmentGetCmd(app))
	return cmd
}

func mailAttachmentListCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "list <id>",
		Short: "List the attachments of a message",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, msg, st, err := mailLoadMessage(cmd.Context(), app, args[0])
			if err != nil {
				return err
			}
			atts, err := st.ListAttachments(cmd.Context(), msg.ID)
			if err != nil {
				return err
			}
			return app.Printer().Print(mailAttachmentRows(atts))
		},
	}
}

type mailAttachmentGetOut struct {
	Path string `json:"path"  table:"PATH"`
	Size int64  `json:"size"  table:"SIZE"`
}

func mailAttachmentGetCmd(app *App) *cobra.Command {
	var outPath string
	cmd := &cobra.Command{
		Use:   "get <id> <part|filename>",
		Short: "Download one attachment",
		Long: `Write one attachment to disk. The second argument is the part path
(as printed by ` + "`mail attachment list`" + `) or the file name. Use -o - to write
to stdout. Messages whose raw bytes were not archived are fetched from the
provider, which needs the network.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, msg, st, err := mailLoadMessage(cmd.Context(), app, args[0])
			if err != nil {
				return err
			}
			atts, err := st.ListAttachments(cmd.Context(), msg.ID)
			if err != nil {
				return err
			}
			var att *model.Attachment
			for i := range atts {
				if atts[i].PartPath == args[1] || strings.EqualFold(atts[i].Filename, args[1]) {
					att = &atts[i]
					break
				}
			}
			if att == nil {
				return output.Errorf(output.ExitNotFound, "message %s has no attachment %q: %w",
					args[0], args[1], model.ErrNotFound)
			}

			var data []byte
			if msg.RawComplete {
				raw, err := mailRawBytes(cmd.Context(), app, msg)
				if err != nil {
					return err
				}
				data, _, _, err = mime.PartContent(raw, att.PartPath)
				if err != nil {
					return err
				}
			} else {
				eng, err := app.Engine()
				if err != nil {
					return err
				}
				ref := att.RemoteRef
				if ref == "" {
					ref = att.PartPath
				}
				data, err = eng.FetchAttachment(cmd.Context(), msg.AccountID, msg.RemoteID, ref)
				if err != nil {
					return err
				}
			}

			dest := outPath
			if dest == "" {
				dest = att.Filename
				if dest == "" {
					dest = strings.ReplaceAll(msg.RemoteID+"-part-"+att.PartPath, "/", "_")
				}
				dest = filepath.Base(dest)
			}
			if dest == "-" {
				if _, err := app.Stdout.Write(data); err != nil {
					return err
				}
				return nil
			}
			if err := os.WriteFile(dest, data, 0o600); err != nil {
				return err
			}
			abs, err := filepath.Abs(dest)
			if err != nil {
				abs = dest
			}
			return app.Printer().Print(mailAttachmentGetOut{Path: abs, Size: int64(len(data))})
		},
	}
	// The design writes this as -o, but -o is taken by the global --format
	// shorthand; -O is the closest thing that does not shadow it.
	cmd.Flags().StringVarP(&outPath, "output", "O", "", "write to this path (\"-\" for stdout)")
	return cmd
}

// mailLoadMessage parses a public id and loads the message.
func mailLoadMessage(ctx context.Context, app *App, id string) (account string, msg *model.Message, st *store.Store, err error) {
	account, remote, err := app.ParseMessageID(id)
	if err != nil {
		return "", nil, nil, err
	}
	st, err = app.Store()
	if err != nil {
		return "", nil, nil, err
	}
	msg, err = st.GetMessage(ctx, account, remote)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return "", nil, nil, errNotFound("message", id)
		}
		return "", nil, nil, err
	}
	return account, msg, st, nil
}
