// Package skill embeds the Claude Code skill description shipped with the
// binary. `emlcal skill` prints it; `emlcal skill --install` writes it to
// ~/.claude/skills/emlcal/SKILL.md so an agent discovers the command surface
// without being told about it.
package skill

import _ "embed"

//go:embed SKILL.md
var text string

// Text returns the SKILL.md contents, always ending in a newline.
func Text() string { return text }
