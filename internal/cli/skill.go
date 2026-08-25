package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/lennert/emlcal/internal/skill"
)

func init() {
	Register(func(root *cobra.Command, app *App) {
		root.AddCommand(coreSkillCmd(app))
	})
}

// coreSuggestedPermissions is the ~/.claude/settings.json fragment from
// DESIGN.md §10: every read command allowed outright, every write asked for.
const coreSuggestedPermissions = `{
  "permissions": {
    "allow": [
      "Bash(emlcal mail list*)", "Bash(emlcal mail search*)",
      "Bash(emlcal mail read*)", "Bash(emlcal mail thread*)",
      "Bash(emlcal mail mailboxes*)", "Bash(emlcal mail attachment list*)",
      "Bash(emlcal cal agenda*)", "Bash(emlcal cal show*)",
      "Bash(emlcal cal free*)", "Bash(emlcal cal calendars*)",
      "Bash(emlcal status*)", "Bash(emlcal sync)"
    ],
    "ask": [
      "Bash(emlcal mail send*)", "Bash(emlcal mail reply*)",
      "Bash(emlcal mail trash*)", "Bash(emlcal mail move*)",
      "Bash(emlcal cal create*)", "Bash(emlcal cal update*)",
      "Bash(emlcal cal delete*)"
    ]
  }
}
`

func coreSkillCmd(app *App) *cobra.Command {
	var install bool
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Print the agent skill describing this CLI",
		Long: `Prints SKILL.md: the command surface, id format, JSON shapes and usage
guidance an agent needs to use emlcal well.

--install writes it to ~/.claude/skills/emlcal/SKILL.md, where Claude Code
discovers it, and prints the suggested permission rules on stderr.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if !install {
				_, err := io.WriteString(app.Stdout, skill.Text())
				return err
			}
			return coreInstallSkill(app)
		},
	}
	cmd.Flags().BoolVar(&install, "install", false, "write the skill to ~/.claude/skills/emlcal/SKILL.md")
	return cmd
}

func coreInstallSkill(app *App) error {
	home := os.Getenv("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locate home directory: %w", err)
		}
		home = h
	}
	dir := filepath.Join(home, ".claude", "skills", "emlcal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "SKILL.md")
	body := skill.Text()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(app.Stderr, "Add to ~/.claude/settings.json so reads run unattended and writes ask:\n%s", coreSuggestedPermissions)
	return app.Printer().Print(struct {
		Path      string `json:"path"      table:"PATH"`
		Bytes     int    `json:"bytes"     table:"BYTES"`
		Installed bool   `json:"installed" table:"INSTALLED"`
	}{path, len(body), true})
}
