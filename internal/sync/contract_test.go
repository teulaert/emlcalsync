package sync

import (
	"github.com/teulaert/emlcalsync/internal/provider"
	"github.com/teulaert/emlcalsync/internal/provider/gcal"
	"github.com/teulaert/emlcalsync/internal/provider/gmail"
	"github.com/teulaert/emlcalsync/internal/provider/jmap"
)

// The engine reaches for FetchEnvelopes through an optional interface, which
// no compiler checks at the call site. These assertions do.
var (
	_ envelopeFetcher = (*gmail.Mail)(nil)

	_ provider.MailProvider     = (*gmail.Mail)(nil)
	_ provider.MailProvider     = (*jmap.Mail)(nil)
	_ provider.CalendarProvider = (*gcal.Calendar)(nil)
	_ provider.CalendarProvider = (*jmap.Calendar)(nil)
	_ provider.Pusher           = (*jmap.Client)(nil)
)
