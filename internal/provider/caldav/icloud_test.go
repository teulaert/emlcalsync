package caldav

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/caldav/caldavfake"
)

// iCloud's account id. Discovery is the only way to learn it, which is why the
// iCloud preset has no home fallback.
const dsid = "/1234567890"

// newICloudFixture stands up the two hosts iCloud really has: a front door that
// answers discovery and points at a per-user partition host, and the partition
// itself, which holds every calendar. It returns the client, both servers and
// the calendar path.
//
// Two servers is the point. One server cannot show whether the client followed
// the partition host or kept talking to the front door, because both answers
// would come from the same place.
func newICloudFixture(t *testing.T) (c *Calendar, front, part *caldavfake.Server, calPath string) {
	t.Helper()
	front = caldavfake.New()
	t.Cleanup(front.Close)
	part = caldavfake.New()
	t.Cleanup(part.Close)

	for _, s := range []*caldavfake.Server{front, part} {
		s.Root = "/"
		s.User, s.Password = testEmail, testPass
		s.Principal = dsid + "/principal/"
		s.Home = dsid + "/calendars/"
	}
	// The partition advertises itself in every href it emits, the way iCloud
	// does; the front door hands out the partition's home.
	part.HrefHost = part.URL()
	front.HrefHost = part.URL()

	calPath = part.Home + "home/"
	part.AddCalendar(caldavfake.Calendar{Path: calPath, Name: "Home"})

	var err error
	c, err = New(Options{
		Email: testEmail, Password: testPass, Vendor: model.VendorICloud,
		BaseURL: front.BaseURL(), Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, front, part, calPath
}

// After discovery every request must go to the partition host. Before the
// client tracked hosts separately it kept resolving paths against the
// configured root, so all of this traffic went back to the front door.
func TestICloudDiscoveryRebasesOntoThePartitionHost(t *testing.T) {
	c, front, part, _ := newICloudFixture(t)

	cals, err := c.Calendars(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 1 || cals[0].Name != "Home" {
		t.Fatalf("calendars = %+v", cals)
	}
	if !cals[0].Primary {
		t.Error(`the iCloud preset should mark "Home" as the primary calendar`)
	}

	// The front door answers discovery and nothing else.
	for _, r := range front.Requests() {
		if r.Depth == "1" {
			t.Errorf("the calendar listing went to the front door: %s %s", r.Method, r.Path)
		}
	}
	if len(part.Requests()) == 0 {
		t.Fatal("no request reached the partition host")
	}
}

// Remote ids are persisted in events.remote_id, so they must stay bare paths
// however the server spells its hrefs. An absolute id would break every stored
// row the moment iCloud changed which partition serves the account.
func TestICloudRemoteIDsStayPaths(t *testing.T) {
	c, _, part, calPath := newICloudFixture(t)
	part.Put(calPath, "one.ics", ics(
		"BEGIN:VCALENDAR", "VERSION:2.0", "BEGIN:VEVENT", "UID:u1",
		"DTSTART:20260830T120000Z", "DTEND:20260830T130000Z", "SUMMARY:One",
		"END:VEVENT", "END:VCALENDAR"))

	cals, err := c.Calendars(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cals[0].RemoteID, "://") {
		t.Errorf("calendar RemoteID = %q, want a bare path", cals[0].RemoteID)
	}

	ch, err := c.EventChanges(context.Background(), cals[0].RemoteID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Upserted) != 1 {
		t.Fatalf("upserted = %d, want 1", len(ch.Upserted))
	}
	if id := ch.Upserted[0].RemoteID; strings.Contains(id, "://") {
		t.Errorf("event RemoteID = %q, want a bare path", id)
	}
}

// The outbox reaches the write methods on a client that has never listed
// anything, so discovery cannot be a side effect of Calendars(). Without it
// every iCloud write would be addressed to the front door.
func TestICloudWritesDiscoverWithoutListingFirst(t *testing.T) {
	c, _, part, calPath := newICloudFixture(t)

	ev := &model.Event{
		Title: "Retro", UID: "u-new",
		Start: time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 30, 15, 30, 0, 0, time.UTC),
	}

	if _, err := c.CreateEvent(context.Background(), calPath, ev); err != nil {
		t.Fatalf("CreateEvent on a client that never listed calendars: %v", err)
	}
	if got := part.Hrefs(); len(got) != 1 {
		t.Fatalf("objects on the partition = %v, want the one just written", got)
	}
	if _, ok := part.LastRequest("PUT"); !ok {
		t.Error("the PUT did not reach the partition host")
	}
}

// Go rewrites a redirected PROPFIND, REPORT or PUT into a GET, so a followed
// redirect looks like success while doing nothing at all. Outside discovery the
// client must refuse, loudly, and name where it was being sent.
func TestICloudRedirectIsRefusedRatherThanFollowed(t *testing.T) {
	c, _, part, calPath := newICloudFixture(t)
	if _, err := c.Calendars(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The partition starts redirecting after discovery has already settled.
	other := caldavfake.New()
	t.Cleanup(other.Close)
	part.RedirectTo(other)

	_, err := c.EventChanges(context.Background(), calPath, "")
	if err == nil {
		t.Fatal("a redirected REPORT must be an error, not a silent no-op")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error %q should say the server redirected", err)
	}
	if len(other.Requests()) != 0 {
		t.Error("the client followed the redirect; it must not")
	}
}

// iCloud's calendar home is a numeric account id, so a failed discovery has
// nothing to guess. Reporting that beats silently listing an empty account.
func TestICloudDiscoveryFailureIsFatal(t *testing.T) {
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.User, srv.Password = testEmail, testPass
	srv.NoPrincipal = true

	c, err := New(Options{
		Email: testEmail, Password: testPass, Vendor: model.VendorICloud,
		BaseURL: srv.BaseURL(), Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Calendars(context.Background()); err == nil {
		t.Fatal("discovery failure must not fall back to a guessed path")
	} else if !strings.Contains(err.Error(), "icloud") {
		t.Errorf("error %q should say which vendor cannot fall back", err)
	}
}

// An auth failure has to name the credential the user actually has to create,
// which differs per vendor.
func TestAuthErrorNamesTheVendorsCredential(t *testing.T) {
	for _, tc := range []struct {
		vendor model.Vendor
		want   string
	}{
		{model.VendorICloud, "account.apple.com"},
		{model.VendorFastmail, "app.fastmail.com"},
	} {
		t.Run(string(tc.vendor), func(t *testing.T) {
			srv := caldavfake.New()
			t.Cleanup(srv.Close)
			srv.User, srv.Password = testEmail, "the-right-one"

			c, err := New(Options{
				Email: testEmail, Password: "wrong", Vendor: tc.vendor,
				BaseURL: srv.BaseURL(), Logger: quietLogger(),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Calendars(context.Background())
			if !IsAuth(err) {
				t.Fatalf("error = %v, want an *AuthError", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should point at %s", err, tc.want)
			}
		})
	}
}

// The Apple ID that authenticates is often not the address that identifies the
// user on an invitation, so the two are configured separately.
func TestICloudUsernameDiffersFromEmail(t *testing.T) {
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.Root = "/"
	srv.Principal, srv.Home = dsid+"/principal/", dsid+"/calendars/"
	// The server accepts the Apple ID, not the iCloud address.
	srv.User, srv.Password = "apple-id@example.com", testPass
	srv.AddCalendar(caldavfake.Calendar{Path: srv.Home + "home/", Name: "Home"})

	c, err := New(Options{
		Email: testEmail, Username: "apple-id@example.com", Password: testPass,
		Vendor: model.VendorICloud, BaseURL: srv.BaseURL(), Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Calendars(context.Background()); err != nil {
		t.Fatalf("Calendars with a distinct Apple ID: %v", err)
	}
}

// A real iCloud account localizes and lets the user rename the default
// calendar — a Dutch one is "Privé" — while the server keeps it at
// .../calendars/home/. Matching on the display name alone flagged whichever
// calendar happened to come back first, which on a real account was an empty
// test calendar.
func TestICloudPrimaryFollowsThePathNotTheDisplayName(t *testing.T) {
	srv := caldavfake.New()
	t.Cleanup(srv.Close)
	srv.Root = "/"
	srv.User, srv.Password = testEmail, testPass
	srv.Principal, srv.Home = dsid+"/principal/", dsid+"/calendars/"
	// Ordered the way the real account came back: a UUID collection first,
	// and nothing called "Home" anywhere.
	srv.AddCalendar(caldavfake.Calendar{Path: srv.Home + "DA69BB82-3AB1-4DF8-84CD-762F5490917C/", Name: "onoma-test"})
	srv.AddCalendar(caldavfake.Calendar{Path: srv.Home + "home/", Name: "Privé"})
	srv.AddCalendar(caldavfake.Calendar{Path: srv.Home + "work/", Name: "Werk"})

	c, err := New(Options{
		Email: testEmail, Password: testPass, Vendor: model.VendorICloud,
		BaseURL: srv.BaseURL(), Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cals, err := c.Calendars(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, cal := range cals {
		if cal.Primary != strings.HasSuffix(cal.RemoteID, "/home/") {
			t.Errorf("%q (%s) primary = %v", cal.Name, cal.RemoteID, cal.Primary)
		}
	}
}
