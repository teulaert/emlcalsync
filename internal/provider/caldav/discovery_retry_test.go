package caldav

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/caldav/caldavfake"
)

// downAtBoot is a transport whose first `down` requests fail the way the
// daemon's did when it started before the network: the resolver timed out.
// After that the network is up and requests go through.
type downAtBoot struct {
	down int32
	seen atomic.Int32
}

func (d *downAtBoot) RoundTrip(r *http.Request) (*http.Response, error) {
	if d.seen.Add(1) <= d.down {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
			Err: "i/o timeout", Name: r.URL.Hostname(), Server: "127.0.0.53:53", IsTimeout: true,
		}}
	}
	return http.DefaultTransport.RoundTrip(r)
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// EML-2: iCloud sync was dead for four days because the daemon's first
// discovery ran before DNS worked, and that one failure was remembered for the
// life of the process. Every later pass replayed it without touching the
// network. Once the network is back, the next call must discover and work.
func TestICloudDiscoveryRecoversOnceTheNetworkIsBack(t *testing.T) {
	_, front, _, calPath := newICloudFixture(t)
	c, err := New(Options{
		Email: testEmail, Password: testPass, Vendor: model.VendorICloud,
		BaseURL: front.BaseURL(), Logger: quietLogger(),
		HTTPClient: &http.Client{Transport: &downAtBoot{down: 1}, CheckRedirect: noRedirects},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := c.Calendars(ctx); !errors.Is(err, model.ErrOffline) {
		t.Fatalf("with the resolver down, Calendars = %v, want an offline error", err)
	}

	cals, err := c.Calendars(ctx)
	if err != nil {
		t.Fatalf("network back, Calendars still fails: %v", err)
	}
	if len(cals) != 1 || cals[0].RemoteID != calPath {
		t.Fatalf("calendars = %+v, want the one at %s", cals, calPath)
	}

	// Discovery is remembered once it has succeeded: later calls do not
	// repeat it against the front door.
	before := len(front.Requests())
	if _, err := c.Calendars(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(front.Requests()); n != before {
		t.Errorf("a settled discovery went back to the front door %d more times", n-before)
	}
}

// A write is the other way in: the outbox reaches CreateEvent on a client that
// may never have listed anything. It must recover the same way.
func TestICloudWriteRecoversAfterAFailedDiscovery(t *testing.T) {
	_, front, part, calPath := newICloudFixture(t)
	c, err := New(Options{
		Email: testEmail, Password: testPass, Vendor: model.VendorICloud,
		BaseURL: front.BaseURL(), Logger: quietLogger(),
		HTTPClient: &http.Client{Transport: &downAtBoot{down: 1}, CheckRedirect: noRedirects},
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := &model.Event{Title: "Ki Kata", UID: "u-retry", Start: time.Date(2026, 11, 28, 19, 0, 0, 0, time.UTC), End: time.Date(2026, 11, 28, 21, 0, 0, 0, time.UTC)}
	if _, err := c.CreateEvent(context.Background(), calPath, ev); !errors.Is(err, model.ErrOffline) {
		t.Fatalf("first CreateEvent = %v, want an offline error (so it is queued)", err)
	}
	if got := part.Hrefs(); len(got) != 0 {
		t.Fatalf("objects after a failed write = %v, want none", got)
	}
	if _, err := c.CreateEvent(context.Background(), calPath, ev); err != nil {
		t.Fatalf("retried CreateEvent with the network back: %v", err)
	}
	if got := part.Hrefs(); len(got) != 1 {
		t.Fatalf("objects on the partition = %v, want the one written", got)
	}
}

// A vendor with a conventional home path must not settle on that guess because
// the network was down: the guess is for a server that answered without the
// property, and pinning it would hide a home that lives elsewhere.
func TestOfflineDiscoveryDoesNotPinTheFallbackPath(t *testing.T) {
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.User, srv.Password = testEmail, testPass
	srv.Home = "/dav/elsewhere/" + testEmail + "/"
	calPath := srv.AddCalendar(caldavfake.Calendar{Path: srv.Home + "Default/", Name: "Calendar"})

	c, err := New(Options{
		Email: testEmail, Password: testPass, Vendor: model.VendorFastmail,
		BaseURL: srv.BaseURL(), Logger: quietLogger(),
		HTTPClient: &http.Client{Transport: &downAtBoot{down: 1}, CheckRedirect: noRedirects},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Calendars(context.Background()); !errors.Is(err, model.ErrOffline) {
		t.Fatalf("with the resolver down, Calendars = %v, want an offline error", err)
	}
	cals, err := c.Calendars(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 1 || cals[0].RemoteID != calPath {
		t.Fatalf("calendars = %+v, want the discovered one at %s, not the fallback guess", cals, calPath)
	}
}

// hangsOnce never answers its first request; it fails it only when the
// request's own context ends, which is what http.Client.Timeout does. After
// that the server answers normally.
type hangsOnce struct{ seen atomic.Int32 }

func (h *hangsOnce) RoundTrip(r *http.Request) (*http.Response, error) {
	if h.seen.Add(1) == 1 {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	return http.DefaultTransport.RoundTrip(r)
}

// A client timeout fails the request with a deadline error while the caller's
// context is still live. That is no answer either, so it must not pin the
// fallback guess any more than a DNS failure may (review of PR #12).
func TestClientTimeoutDuringDiscoveryDoesNotPinTheFallbackPath(t *testing.T) {
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.User, srv.Password = testEmail, testPass
	srv.Home = "/dav/elsewhere/" + testEmail + "/"
	calPath := srv.AddCalendar(caldavfake.Calendar{Path: srv.Home + "Default/", Name: "Calendar"})

	c, err := New(Options{
		Email: testEmail, Password: testPass, Vendor: model.VendorFastmail,
		BaseURL: srv.BaseURL(), Logger: quietLogger(),
		HTTPClient: &http.Client{Transport: &hangsOnce{}, CheckRedirect: noRedirects, Timeout: 100 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Calendars(ctx); err == nil {
		t.Fatal("a discovery that timed out must fail, not settle on the fallback guess")
	}
	if ctx.Err() != nil {
		t.Fatal("the caller's context must still be live for this test to mean anything")
	}
	cals, err := c.Calendars(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 1 || cals[0].RemoteID != calPath {
		t.Fatalf("calendars = %+v, want the discovered one at %s, not the fallback guess", cals, calPath)
	}
}
