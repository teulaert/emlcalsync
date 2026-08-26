package sync

import (
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/provider/gcal"
	"github.com/teulaert/emlcalsync/internal/provider/gmail"
	"github.com/teulaert/emlcalsync/internal/provider/jmap"
)

// The engine reaches for FetchEnvelopes and Total through optional interfaces,
// which no compiler checks at the call site: a provider that stops satisfying
// one degrades silently — gmail.Mail shipped without Total, so every Gmail
// backfill reported a bare count with no percentage or ETA and nothing failed.
// These assertions do what the type switches cannot.
var (
	_ envelopeFetcher = (*gmail.Mail)(nil)

	_ totalHinter = (*gmail.Mail)(nil)
	_ totalHinter = (*jmap.Mail)(nil)

	_ provider.MailProvider     = (*gmail.Mail)(nil)
	_ provider.MailProvider     = (*jmap.Mail)(nil)
	_ provider.CalendarProvider = (*gcal.Calendar)(nil)
	_ provider.CalendarProvider = (*jmap.Calendar)(nil)
	_ provider.Pusher           = (*jmap.Client)(nil)
)
