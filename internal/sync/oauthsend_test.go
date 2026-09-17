package sync

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/teulaert/emlcalsync/internal/provider/gmail"
	"github.com/teulaert/emlcalsync/internal/provider/oauth"
)

// TestSendIsQueuedWhenTheTokenRefreshCannotConnect drives the real Gmail
// provider, over the real OAuth token source, through the outbox: the stored
// access token has expired and the token endpoint is unreachable. That is a
// Gmail account on a resolver having a bad afternoon, and it is the one case
// where the failure is not the provider's own — it belongs to a prerequisite
// request to a different host.
//
// The outbox has to hold the send. oauth2.Transport asks its source for a
// token before it hands anything to the base transport, so a refresh that
// never connected means the message never reached Google, and sending it again
// cannot deliver it twice. The fake Gmail API here would accept a send and
// count it, which is how the test knows nothing went out.
func TestSendIsQueuedWhenTheTokenRefreshCannotConnect(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	var hits atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"sent-1","threadId":"thread-1"}`) //nolint:errcheck // test server
	}))
	defer api.Close()

	// A token endpoint that nothing listens on any more: the dial is refused.
	dead := httptest.NewServer(http.NotFoundHandler())
	tokenURL := dead.URL + "/token"
	dead.Close()

	tokens := &oauth.MemoryTokenStore{}
	if err := tokens.Save("work.google", &oauth2.Token{
		AccessToken: "expired", RefreshToken: "refresh-0", TokenType: "Bearer",
		Expiry: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	hc, err := oauth.HTTPClient(ctx, oauth.Config{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		Endpoint:     oauth2.Endpoint{TokenURL: tokenURL},
	}, tokens, "work.google")
	if err != nil {
		t.Fatalf("oauth.HTTPClient: %v", err)
	}
	m, err := gmail.New(ctx, gmail.Options{
		HTTPClient: hc,
		Email:      "user@example.com",
		Endpoint:   api.URL + "/",
	})
	if err != nil {
		t.Fatalf("gmail.New: %v", err)
	}
	h.fact.mail = m

	res, err := h.eng.Apply(ctx, "work", Op{Kind: OpSend, Raw: mailRaw(t, "hello", "hi")})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Queued {
		t.Fatal("a send whose token refresh never connected was not queued")
	}
	item, err := h.st.GetOutbox(ctx, res.OutboxID)
	if err != nil {
		t.Fatal(err)
	}
	if item.FailedAt != nil || item.DoneAt != nil {
		t.Fatalf("queued send row = %+v, want still pending", item)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the Gmail API saw %d requests, want none", n)
	}
}
