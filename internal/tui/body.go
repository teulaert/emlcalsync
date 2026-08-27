package tui

import (
	"context"
	"strings"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
)

// readableBody is what the reader pane shows: the extracted text with quoted
// replies and signatures stripped, the same choice `mail read` makes by
// default.
//
// HTML-only mail needs no special case here — mime.Parse already runs
// HTMLToText when there is no text/plain alternative, so the stored TextBody
// is readable text either way. What does need one is an oversized message
// stored as an envelope-only stub (raw_complete = 0, see DESIGN.md §16): it
// has no body at all until the raw bytes are fetched.
func readableBody(ctx context.Context, d Deps, m *model.Message) string {
	if s := strings.TrimSpace(m.TextBody); s != "" {
		return mime.StripQuotes(m.TextBody)
	}
	if m.RawComplete || d.Engine == nil {
		return ""
	}
	raw, err := d.Engine.EnsureRaw(ctx, m.AccountID, m.RemoteID)
	if err != nil {
		return "(this message was too large to archive in full, and fetching it now failed: " +
			err.Error() + ")"
	}
	parsed, err := mime.Parse(raw)
	if err != nil {
		return "(could not parse this message: " + err.Error() + ")"
	}
	return mime.StripQuotes(parsed.TextBody)
}
