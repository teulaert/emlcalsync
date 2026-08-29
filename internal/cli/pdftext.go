package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/teulaert/emlcalsync/internal/doctext"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(pdfTextCmd(app))
	})
}

// pdfTextCmd is the hidden child behind `mail attachment text`: the pure-Go
// PDF reader, run in a process of its own so that a document it cannot cope
// with -- and there are some it loops on -- costs a kill, not the TUI. It
// reads the PDF on stdin and writes the text on stdout.
func pdfTextCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:    "__pdf-text",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := io.ReadAll(app.Stdin)
			if err != nil {
				return err
			}
			text, err := doctext.PDFInProcess(data)
			if err != nil {
				fmt.Fprintln(app.Stderr, err)
				os.Exit(1)
			}
			_, err = io.WriteString(app.Stdout, text)
			return err
		},
	}
}
