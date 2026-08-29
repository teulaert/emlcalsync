package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/ai"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
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
	cmd.AddCommand(aiSummarizeCmd(app))
	return cmd
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
					self.Email = a.Email
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
