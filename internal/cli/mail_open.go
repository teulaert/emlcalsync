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
)

type mailOpenOut struct {
	ID     string `json:"id"      table:"ID"`
	Path   string `json:"path"    table:"PATH,max=60"`
	URL    string `json:"url,omitempty"`
	Size   int64  `json:"size"    table:"SIZE"`
	Remote bool   `json:"remote_content_allowed"`
}

func mailOpenCmd(app *App) *cobra.Command {
	var outPath string
	var remote bool
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

Remote content is blocked: the page declares a content security policy that
lets it load nothing from the network, so the tracking pixels in marketing and
"verify it was you" mail do not fire, and the sender learns nothing. The
message's own inline images travel inside the page and still show. Pass
--remote to lift that, which does tell the sender you opened it.

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
			doc, err := mime.HTMLDocument(raw, mime.HTMLDocOptions{AllowRemote: remote})
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

			out := mailOpenOut{ID: msg.PublicID(), Path: abs, Size: int64(len(doc)), Remote: remote}
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
	fl.BoolVar(&remote, "remote", false,
		"let the page load remote content (fires the sender's tracking pixels)")
	return cmd
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
