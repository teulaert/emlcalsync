package gmail

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"golang.org/x/sync/errgroup"
	gmailapi "google.golang.org/api/gmail/v1"

	"github.com/lennert/emlcal/internal/provider"
)

// FetchRaw downloads full messages (format=raw) and hands them to fn as they
// arrive. Ids the server no longer knows are skipped silently, per the
// provider contract.
func (m *Mail) FetchRaw(ctx context.Context, ids []string, fn func(provider.RawMessage) error) error {
	if len(ids) == 0 {
		return nil
	}
	var mu sync.Mutex
	emit := func(msg *gmailapi.Message) error {
		raw, err := decodeBase64URL(msg.Raw)
		if err != nil {
			return fmt.Errorf("gmail message %s: %w", msg.Id, err)
		}
		rm := provider.RawMessage{Envelope: envelopeOf(msg), Raw: raw}
		mu.Lock()
		defer mu.Unlock()
		return fn(rm)
	}
	if m.opts.FetchMode == FetchIndividual {
		return m.fetchIndividual(ctx, ids, "raw", emit)
	}
	return m.fetchBatched(ctx, ids, "raw", emit)
}

// FetchEnvelopes reads only the current labels and metadata of ids
// (format=minimal) and hands each one to fn. It is not part of
// provider.MailProvider: the sync engine uses it during a reconcile to refresh
// flags and mailboxes for messages whose raw bytes it already has (DESIGN.md
// §6.1). Ids that no longer exist are skipped silently.
func (m *Mail) FetchEnvelopes(ctx context.Context, ids []string, fn func(provider.Envelope) error) error {
	if len(ids) == 0 {
		return nil
	}
	var mu sync.Mutex
	emit := func(msg *gmailapi.Message) error {
		mu.Lock()
		defer mu.Unlock()
		return fn(envelopeOf(msg))
	}
	if m.opts.FetchMode == FetchIndividual {
		return m.fetchIndividual(ctx, ids, "minimal", emit)
	}
	return m.fetchBatched(ctx, ids, "minimal", emit)
}

// fetchBatched runs batchSize-sized batch requests, m.conc of them at a time.
func (m *Mail) fetchBatched(ctx context.Context, ids []string, format string, emit func(*gmailapi.Message) error) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(m.conc)
	for chunk := range chunks(ids, batchSize) {
		g.Go(func() error { return m.fetchChunk(gctx, chunk, format, emit) })
	}
	return g.Wait()
}

// fetchChunk fetches one batch, retrying only the individual ids whose part
// came back throttled or 5xx.
func (m *Mail) fetchChunk(ctx context.Context, ids []string, format string, emit func(*gmailapi.Message) error) error {
	pending := ids
	for round := 1; round <= maxAttempts; round++ {
		results, err := m.batchGet(ctx, pending, format)
		if err != nil {
			return err
		}
		var retry []string
		for _, r := range results {
			switch {
			case r.err == nil:
				if err := emit(r.msg); err != nil {
					return err
				}
			case r.status == http.StatusNotFound:
				m.log.Debug("gmail message vanished", "id", r.id)
			case retryableStatus(r.status, r.err):
				retry = append(retry, r.id)
			default:
				return wrapErr("messages.get (batch part)", r.err)
			}
		}
		if len(retry) == 0 {
			return nil
		}
		if round == maxAttempts {
			return fmt.Errorf("gmail messages.get: %d message(s) still rate limited after %d rounds",
				len(retry), maxAttempts)
		}
		pending = retry
		if err := sleepCtx(ctx, backoffFor(round, nil)); err != nil {
			return err
		}
	}
	return nil
}

// fetchIndividual is the fallback path: one messages.get per message.
func (m *Mail) fetchIndividual(ctx context.Context, ids []string, format string, emit func(*gmailapi.Message) error) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(m.conc)
	for _, id := range ids {
		g.Go(func() error {
			var msg *gmailapi.Message
			err := m.do(gctx, "messages.get", unitsMessagesGet, func() error {
				var err error
				msg, err = m.svc.Users.Messages.Get(me, id).Format(format).Context(gctx).Do()
				return err
			})
			if err != nil {
				if isNotFound(err) {
					m.log.Debug("gmail message vanished", "id", id)
					return nil
				}
				return err
			}
			return emit(msg)
		})
	}
	return g.Wait()
}

// fetchLabels reads just the current labelIds of the given messages
// (format=minimal). Ids that no longer exist come back in gone.
func (m *Mail) fetchLabels(ctx context.Context, ids []string) (found map[string]*gmailapi.Message, gone []string, err error) {
	found = make(map[string]*gmailapi.Message, len(ids))
	if len(ids) == 0 {
		return found, nil, nil
	}
	var mu sync.Mutex
	collect := func(msg *gmailapi.Message) error {
		mu.Lock()
		defer mu.Unlock()
		found[msg.Id] = msg
		return nil
	}
	if m.opts.FetchMode == FetchIndividual {
		err = m.fetchIndividual(ctx, ids, "minimal", collect)
	} else {
		err = m.fetchBatched(ctx, ids, "minimal", collect)
	}
	if err != nil {
		return nil, nil, err
	}
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			gone = append(gone, id)
		}
	}
	return found, gone, nil
}

// chunks yields successive slices of at most n elements.
func chunks[T any](s []T, n int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		for i := 0; i < len(s); i += n {
			end := min(i+n, len(s))
			if !yield(s[i:end]) {
				return
			}
		}
	}
}
