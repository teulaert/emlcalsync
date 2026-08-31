package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/browser"
	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/output"
	"github.com/teulaert/emlcalsync/internal/webasset"
)

type mailOpenOut struct {
	ID     string `json:"id"      table:"ID"`
	Path   string `json:"path"    table:"PATH,max=60"`
	URL    string `json:"url,omitempty"`
	Size   int64  `json:"size"    table:"SIZE"`
	Remote bool   `json:"remote_content"`
}

func mailOpenCmd(app *App) *cobra.Command {
	var outPath string
	var remote, noRemote bool
	cmd := &cobra.Command{
		Use:   "open <id>",
		Short: "Open one message in the browser, as the sender wrote it",
		Long: `Render one message as a standalone HTML page and open it in the
desktop's browser.

This is the escape hatch for mail the text extractor mangles. HTML mail is a
pile of nested tables rather than a document, and turning it into text is a
heuristic that will sometimes lose the one line that mattered — a one-time
login code, a button with the amount on it. Rather than read around the gap,
look at what the sender actually sent.

The page never touches the network: it declares a content security policy that
lets it load nothing, so the browser cannot fetch, cannot run a script and
cannot post a form. The pictures a message hosts elsewhere are fetched by
emlcal instead and folded into the page — without cookies, without a referrer,
and never from a private address — so the sender is told that somebody opened
the message, and not which account did.

That request is still a request. --no-remote leaves those pictures out, and
nothing about the message leaves this machine; the layout then has holes where
they were. Setting remote_content = false under [general] in config.toml makes
that the default, and --remote overrides it the other way.

The page is written under the cache directory and left there; pages older than
a day are swept on the way past. Use -O to put it somewhere of your own
choosing (-O - writes the HTML to stdout), which skips the browser.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			account, remoteID, err := app.ParseMessageID(args[0])
			if err != nil {
				return err
			}
			st, err := app.Store()
			if err != nil {
				return err
			}
			msg, err := st.GetMessage(cmd.Context(), account, remoteID)
			if err != nil {
				if errors.Is(err, model.ErrNotFound) {
					return errNotFound("message", args[0])
				}
				return err
			}
			raw, err := mailRawBytes(cmd.Context(), app, msg)
			if err != nil {
				return err
			}
			fetchRemote, err := app.remoteContent(cmd, remote, noRemote)
			if err != nil {
				return err
			}
			var opts mime.HTMLDocOptions
			if fetchRemote {
				opts.Fetch = app.assetFetcher()
			}
			doc, err := mime.HTMLDocument(cmd.Context(), raw, opts)
			if err != nil {
				return err
			}

			if outPath == "-" {
				_, err := app.Stdout.Write(doc)
				return err
			}

			var abs string
			if outPath == "" {
				abs, err = browser.WritePage(config.ViewDir(), msg.PublicID(), doc, app.Now())
			} else {
				abs, err = writeTo(outPath, doc)
			}
			if err != nil {
				return err
			}

			out := mailOpenOut{ID: msg.PublicID(), Path: abs, Size: int64(len(doc)), Remote: fetchRemote}
			if outPath == "" {
				url, err := browser.FileURL(abs)
				if err != nil {
					return err
				}
				open := app.OpenBrowser
				if open == nil {
					open = browser.Open
				}
				if err := open(url); err != nil {
					return fmt.Errorf("%w (the page is at %s)", err, abs)
				}
				out.URL = url
			}
			return app.Printer().Print(out)
		},
	}
	fl := cmd.Flags()
	// -O rather than -o, which is taken by the global --format shorthand.
	fl.StringVarP(&outPath, "output", "O", "",
		"write the page here instead of opening it (\"-\" for stdout)")
	fl.BoolVar(&remote, "remote", true,
		"fetch the pictures the sender hosts elsewhere (tells the sender it was opened)")
	fl.BoolVar(&noRemote, "no-remote", false,
		"leave those pictures out; nothing about the message leaves this machine")
	return cmd
}

// remoteContent settles whether the pictures a message hosts elsewhere are
// fetched: --no-remote wins, then an explicit --remote, then config.toml.
// Passing both at once is a contradiction rather than a precedence puzzle,
// so it is refused.
func (a *App) remoteContent(cmd *cobra.Command, remote, noRemote bool) (bool, error) {
	if noRemote && cmd.Flags().Changed("remote") {
		return false, output.Errorf(output.ExitUsage,
			"--remote and --no-remote cannot both be given")
	}
	if noRemote {
		return false, nil
	}
	if cmd.Flags().Changed("remote") {
		return remote, nil
	}
	cfg, err := a.Config()
	if err != nil {
		return false, err
	}
	return cfg.General.RemoteContent, nil
}

// assetFetcher is the one the real process uses. A test replaces it through
// App.Fetch so that opening a message launches no requests.
func (a *App) assetFetcher() mime.FetchFunc {
	if a.Fetch != nil {
		return a.Fetch
	}
	return webasset.New().Fetch
}

// writeTo honours -O: the caller picked the path, so the sweep and the cache
// directory are none of our business.
func writeTo(path string, doc []byte) (string, error) {
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, nil
	}
	return abs, nil
}
