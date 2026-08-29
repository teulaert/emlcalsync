package cli

import (
	"time"

	"github.com/teulaert/emlcalsync/internal/ai"
	"github.com/teulaert/emlcalsync/internal/ai/ollama"
	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/output"
)

// AI builds the client for the default model in the [ai] table, or nil when
// the table is empty — the off state, which the TUI reports as such rather
// than failing to start. The switch on backend is the one place a new
// backend has to be added, the way Factory picks providers from an account's
// blocks; Validate has already refused a backend it does not know.
func (a *App) AI() (ai.Client, error) {
	if a.AIClient != nil {
		return a.AIClient, nil
	}
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	m, ok := cfg.DefaultAIModel()
	if !ok {
		return nil, nil
	}
	switch m.Backend {
	case config.AIBackendOllama:
		return ollama.New(ollama.Options{
			URL:     m.URL,
			Model:   m.Model,
			Timeout: time.Duration(m.Timeout),
		}), nil
	}
	return nil, output.Errorf(output.ExitUsage, "config: ai model %q: backend %q is not supported", m.Name, m.Backend)
}
