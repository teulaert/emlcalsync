package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/ai"
	"github.com/teulaert/emlcalsync/internal/compose"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/store"
	"github.com/teulaert/emlcalsync/internal/sync"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(aiCmd(app))
	})
}

// aiCmd is `emlcal ai`: the configured model, asked about the archive from
// the command line. It is not on the read allow list, on purpose: the
// commands there are what the model itself gets as tools, and a model that
// can call the model is a loop.
func aiCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ai",
		Short: "Ask the configured model about the archive",
		Long: `Ask the model configured under [ai] in config.toml about the archive.

The model gets the read commands as tools, the same ones ` + "`emlcal skill`" + `
allows an agent to run unasked, and may look other mail and the calendar up
before answering. It can never send, move or delete.`,
	}
	cmd.AddCommand(aiSummarizeCmd(app), aiDraftCmd(app))
	return cmd
}

// aiDraftOut is the JSON shape of `ai draft` without --save: the reply as
// the composer would open it, body by the model, headers by compose.Reply.
type aiDraftOut struct {
	ID       string     `json:"id"`
	ThreadID string     `json:"thread_id"`
	Model    string     `json:"model"`
	Intent   string     `json:"intent,omitempty"`
	From     string     `json:"from"`
	To       []string   `json:"to"`
	Cc       []string   `json:"cc,omitempty"`
	Subject  string     `json:"subject"`
	Body     string     `json:"body"`
	Lookups  []aiLookup `json:"lookups"`
}

func aiDraftCmd(app *App) *cobra.Command {
	var (
		intent    string
		all       bool
		save      bool
		dryRun    bool
		noLookups bool
	)
	cmd := &cobra.Command{
		Use:   "draft <id> [--intent \"…\"]",
		Short: "Draft a reply with the model",
		Long: `Draft a reply to a message the way ctrl+g in the composer does: the model
reads the whole thread, may look other mail and the calendar up, and writes
the body; the subject, recipients and threading headers are the ones
` + "`mail reply`" + ` would use. --intent says what the reply should do; without it
the model answers what the thread asks. The id may be a message id or a
thread id, which drafts a reply to its newest message.

Without --save the draft is printed and nothing is stored: JSON when piped,
or with -o plain just the body, ready for ` + "`mail reply <id> --body-file -`" + `.
--save stores it as a draft on the server, in the thread, where any mail
client shows it under the message; --dry-run prints the RFC 822 message that
would be stored. Nothing is ever sent.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := app.AI()
			if err != nil {
				return err
			}
			if client == nil {
				return output.Errorf(output.ExitUsage, "no AI model configured: add an [[ai.models]] block to config.toml")
			}
			st, err := app.Store()
			if err != nil {
				return err
			}
			orig, err := aiResolveMessage(cmd.Context(), app, st, args[0])
			if err != nil {
				return err
			}
			acct, err := app.ResolveAccount(orig.AccountID)
			if err != nil {
				return err
			}
			_, msgs, err := st.GetThread(cmd.Context(), orig.AccountID, orig.ThreadID, false)
			if err != nil {
				return err
			}
			var tools ai.Toolset
			if !noLookups {
				tools = app.AITools()
			}
			from := model.Address{Email: acct.Email}
			req := ai.ReplyPrompt(ai.ReplyInput{
				Self:          aiSelf(cmd.Context(), st, orig.AccountID, acct.Email),
				Thread:        msgs,
				Answering:     orig,
				Instructions:  intent,
				ContextWindow: client.ContextWindow(),
				Lookups:       tools != nil,
				Loc:           app.Location(),
			})
			var lookups []aiLookup
			answer, err := ai.Run(cmd.Context(), client, req, tools, ai.Observer{
				Lookup: func(call ai.ToolCall) {
					lookups = append(lookups, aiLookup{Tool: call.Name, Args: call.Arguments})
					if app.IsTTY {
						fmt.Fprintf(app.Stderr, "looking up: %s %s\n", call.Name, call.Arguments)
					}
				},
			})
			if err != nil {
				if errors.Is(err, ai.ErrUnavailable) {
					return output.Errorf(output.ExitOffline, "%v", err)
				}
				return err
			}
			body := ai.CleanText(answer)
			if body == "" {
				return errors.New("the model returned nothing")
			}
			if lookups == nil {
				lookups = []aiLookup{}
			}

			// The header half is the one `mail reply` builds, so what is
			// stored -- or printed -- is the reply the CLI would send.
			draft := &mime.Draft{From: from, Date: app.Now()}
			compose.Reply(draft, orig, all, []string{acct.Email})

			if !save && !dryRun {
				out := aiDraftOut{
					ID:       orig.PublicID(),
					ThreadID: model.ThreadPublicID(orig.AccountID, orig.ThreadID),
					Model:    client.Describe(),
					Intent:   intent,
					From:     from.Email,
					To:       mailEmails(draft.To),
					Cc:       mailEmails(draft.Cc),
					Subject:  draft.Subject,
					Body:     body,
					Lookups:  lookups,
				}
				if app.Printer().Format == output.JSON || app.Printer().Format == output.Auto {
					return app.Printer().Print(out)
				}
				_, err = io.WriteString(app.Stdout, strings.TrimRight(body, "\n")+"\n")
				return err
			}

			draft.TextBody = body + "\n\n" + compose.Quote(orig, app.Location())
			raw, err := mime.Build(draft)
			if err != nil {
				return err
			}
			if dryRun {
				_, err := app.Stdout.Write(raw)
				return err
			}
			return mailSubmit(cmd, app, &mailComposed{
				account:    acct,
				raw:        raw,
				to:         draft.To,
				subject:    draft.Subject,
				threadID:   orig.ThreadID,
				from:       draft.From.Email,
				recipients: compose.Envelope(draft),
				original:   orig,
			}, sync.OpDraft)
		},
	}
	cmd.Flags().StringVar(&intent, "intent", "", "what the reply should do; empty answers what the thread asks")
	cmd.Flags().BoolVar(&all, "all", false, "reply to all recipients")
	cmd.Flags().BoolVar(&save, "save", false, "store the draft on the server instead of printing it")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the RFC 822 message --save would store and stop")
	cmd.Flags().BoolVar(&noLookups, "no-lookups", false, "give the model the thread only, no tools")
	return cmd
}

// aiSelf is who the model writes as, or for: the account's address, with the
// display name its own sent mail carries when it has any.
func aiSelf(ctx context.Context, st *store.Store, account, email string) model.Address {
	return model.Address{Name: st.SenderName(ctx, account, email), Email: email}
}

// aiResolveMessage is the message a draft answers: the one named, or the
// newest message of the thread named that actually went somewhere -- the
// same rule the TUI's r follows from a list row.
func aiResolveMessage(ctx context.Context, app *App, st *store.Store, id string) (*model.Message, error) {
	p, err := model.ParseID(id)
	if err != nil {
		return nil, output.Errorf(output.ExitUsage, "not a message or thread id: %q", id)
	}
	switch p.Kind {
	case model.KindMessage:
		_, msg, _, err := mailLoadMessage(ctx, app, id)
		return msg, err
	case model.KindThread:
		_, msgs, err := st.GetThread(ctx, p.Account, p.Remote, false)
		if err != nil {
			if errors.Is(err, model.ErrNotFound) {
				return nil, errNotFound("thread", id)
			}
			return nil, err
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if !msgs[i].Flags.Draft {
				return &msgs[i], nil
			}
		}
		return nil, errNotFound("thread", id)
	}
	return nil, output.Errorf(output.ExitUsage, "not a message or thread id: %q", id)
}

// aiSummaryOut is the JSON shape of `ai summarize`.
type aiSummaryOut struct {
	ID       string     `json:"id"`
	Subject  string     `json:"subject"`
	Model    string     `json:"model"`
	Question string     `json:"question,omitempty"`
	Summary  string     `json:"summary"`
	Lookups  []aiLookup `json:"lookups"`
}

type aiLookup struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

func aiSummarizeCmd(app *App) *cobra.Command {
	var (
		ask       string
		noLookups bool
	)
	cmd := &cobra.Command{
		Use:   "summarize <id>",
		Short: "Summarize a conversation, or answer a question about it",
		Long: `Summarize a thread so it can be acted on without reading it: what it is
about, what is asked of you, the facts, what is open. The id may be a thread
id or any message id in it. --ask answers a question about the thread instead.

This is ctrl+g in the TUI, from the command line.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := app.AI()
			if err != nil {
				return err
			}
			if client == nil {
				return output.Errorf(output.ExitUsage, "no AI model configured: add an [[ai.models]] block to config.toml")
			}
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
			var tools ai.Toolset
			if !noLookups {
				tools = app.AITools()
			}
			self := model.Address{}
			if cfg, err := app.Config(); err == nil {
				if a, ok := cfg.Account(account); ok {
					self = aiSelf(cmd.Context(), st, account, a.Email)
				}
			}
			req := ai.SummaryPrompt(ai.SummaryInput{
				Self:          self,
				Thread:        msgs,
				Question:      ask,
				ContextWindow: client.ContextWindow(),
				Lookups:       tools != nil,
				Loc:           app.Location(),
			})
			out := aiSummaryOut{
				ID:       model.ThreadPublicID(account, threadID),
				Subject:  thread.Subject,
				Model:    client.Describe(),
				Question: ask,
				Lookups:  []aiLookup{},
			}
			// Lookups are narrated on stderr as they happen, the way the
			// TUI's status line does: a wait with nothing said is a hang.
			answer, err := ai.Run(cmd.Context(), client, req, tools, ai.Observer{
				Lookup: func(call ai.ToolCall) {
					out.Lookups = append(out.Lookups, aiLookup{Tool: call.Name, Args: call.Arguments})
					if app.IsTTY {
						fmt.Fprintf(app.Stderr, "looking up: %s %s\n", call.Name, call.Arguments)
					}
				},
			})
			if err != nil {
				if errors.Is(err, ai.ErrUnavailable) {
					return output.Errorf(output.ExitOffline, "%v", err)
				}
				return err
			}
			out.Summary = ai.CleanText(answer)
			if app.Printer().Format == output.JSON || app.Printer().Format == output.Auto {
				return app.Printer().Print(out)
			}
			_, err = io.WriteString(app.Stdout, strings.TrimRight(out.Summary, "\n")+"\n")
			return err
		},
	}
	cmd.Flags().StringVar(&ask, "ask", "", "answer this question about the thread instead of summarizing it")
	cmd.Flags().BoolVar(&noLookups, "no-lookups", false, "give the model the thread only, no tools")
	return cmd
}
