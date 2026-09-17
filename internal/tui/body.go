package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
)

// bodyText is a message's text in the two halves a screen draws it in: what
// the sender typed, and the quoted material under it. Both are kept because
// which one matters depends on the message. On a reply the quote is the part
// you already read; on a forward it is the part you have not -- "see below"
// over the whole point of the mail -- and F swaps between them.
type bodyText struct {
	own  string // quoted material stripped: what the sender typed
	rest string // what was cut off, empty when nothing was
}

// hiddenLines is how many lines sit under the fold.
func (b bodyText) hiddenLines() int {
	if b.rest == "" {
		return 0
	}
	return strings.Count(b.rest, "\n") + 1
}

// fold is the line drawn where the message was cut: what is down there and
// which key shows it. Empty when nothing was cut, so a plain message gets no
// furniture. Callers style it -- the thread wraps its lines before styling
// them, the reader does not.
func (b bodyText) fold(full bool) string {
	if b.rest == "" {
		return ""
	}
	if full {
		return fmt.Sprintf("── %d quoted lines · F to hide ──", b.hiddenLines())
	}
	return fmt.Sprintf("── %d quoted lines hidden · F to show ──", b.hiddenLines())
}

// text is the body as one string, the way the reader's viewport wants it.
func (b bodyText) text(full bool) string {
	if b.rest == "" {
		return b.own
	}
	out := styleFaint.Render(b.fold(full))
	if b.own != "" {
		out = b.own + "\n\n" + out
	}
	if full {
		out += "\n\n" + b.rest
	}
	return out
}

// readableBody is what the reader pane shows: the extracted text split at the
// quote, the same cut `mail read` makes by default -- except that here the
// half `mail read` drops is kept, one keystroke away, since a terminal has no
// second window to open it in.
//
// HTML-only mail needs no special case here — mime.Parse already runs
// HTMLToText when there is no text/plain alternative, so the stored TextBody
// is readable text either way. What does need one is an oversized message
// stored as an envelope-only stub (raw_complete = 0, see DESIGN.md §16): it
// has no body at all until the raw bytes are fetched.
func readableBody(ctx context.Context, d Deps, m *model.Message) bodyText {
	if s := strings.TrimSpace(m.TextBody); s != "" {
		return splitBody(m.TextBody)
	}
	if m.RawComplete || d.Engine == nil {
		return bodyText{}
	}
	raw, err := d.Engine.EnsureRaw(ctx, m.AccountID, m.RemoteID)
	if err != nil {
		d.log().Warn("read body: fetch raw", "id", m.PublicID(), "err", err)
		return bodyText{own: "(this message was too large to archive in full, and fetching it now failed: " +
			err.Error() + ")"}
	}
	parsed, err := mime.Parse(raw)
	if err != nil {
		return bodyText{own: "(could not parse this message: " + err.Error() + ")"}
	}
	return splitBody(parsed.TextBody)
}

func splitBody(text string) bodyText {
	own, rest := mime.SplitReadable(text)
	return bodyText{own: own, rest: rest}
}

// ensureText fills in the body of an envelope-only stub (DESIGN.md §16) so a
// reply has the original to quote. Every other message already carries its
// text. It is best effort: a reply to a message whose bytes cannot be fetched
// is still worth writing, it just quotes nothing.
func ensureText(ctx context.Context, d Deps, m *model.Message) {
	if strings.TrimSpace(m.TextBody) != "" || m.RawComplete || d.Engine == nil {
		return
	}
	raw, err := d.Engine.EnsureRaw(ctx, m.AccountID, m.RemoteID)
	if err != nil {
		d.log().Warn("quote original: fetch raw", "id", m.PublicID(), "err", err)
		return
	}
	if parsed, err := mime.Parse(raw); err == nil {
		m.TextBody = parsed.TextBody
	}
}
