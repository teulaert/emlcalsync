package cli

import (
	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/ai"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/tui"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(coreTUICmd(app))
	})
}

func coreTUICmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Browse mail and calendar interactively",
		Long: `Open the interactive view over the local archive.

Mail and calendar are both shown merged across every configured account —
--account narrows them. Reads come from the same SQLite index the other
commands use, and writes go through the same sync engine, so an action taken
here is the action ` + "`emlcal mail archive`" + ` would have taken. That
includes replying: r and a open a composer on the message in focus, and what
it sends is what ` + "`emlcal mail reply`" + ` would have sent. f forwards that
message on and c starts a new one. o opens the message in focus in the
browser, as the sender wrote it, for the mail no text extraction does justice
to; nothing on that page reaches the network. With a model
configured under [ai] in config.toml, ctrl+g in the composer drafts the reply
from the thread, with or without instructions; the model can look other mail
and the calendar up first, through the same read commands.

Press ? for the keys.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The TUI paints escape codes and reads keys; there is nothing
			// sensible to do when it is not attached to a terminal, and
			// failing loudly beats scribbling over a pipe.
			if !app.IsTTY {
				return output.Errorf(output.ExitUsage,
					"tui needs a terminal: stdout is not a TTY")
			}
			// --verbose tees the log to stderr, which would scribble over
			// the alternate screen. The file log keeps everything, so drop
			// only the tee -- before anything can memoise the logger.
			app.Verbose = false

			cfg, err := app.Config()
			if err != nil {
				return err
			}
			st, err := app.Store()
			if err != nil {
				return err
			}
			eng, err := app.Engine()
			if err != nil {
				return err
			}
			accounts, err := app.AccountIDs()
			if err != nil {
				return err
			}
			model, err := app.AI()
			if err != nil {
				return err
			}
			var tools ai.Toolset
			if model != nil {
				tools = app.AITools()
			}
			return tui.Run(cmd.Context(), tui.Deps{
				Store:     st,
				Engine:    eng,
				Config:    cfg,
				Accounts:  accounts,
				Loc:       app.Location(),
				Now:       app.Now,
				Logger:    app.Logger(),
				AI:        model,
				Tools:     tools,
				StatePath: cfg.General.StateDir,
			})
		},
	}
}
