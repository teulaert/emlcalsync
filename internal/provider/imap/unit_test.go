package imap

import (
	"strings"
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"

	"github.com/teulaert/emlcalsync/internal/model"
)

// Remote ids travel through model.ParseID, which splits on ":" and demands
// exactly two parts. A folder name that leaked a colon into an id would break
// every id the CLI prints, so the encoding has to survive whatever users call
// their folders.
func TestRefRoundTripsAwkwardFolderNames(t *testing.T) {
	for _, name := range []string{
		"INBOX",
		"INBOX.Projects",
		"INBOX/Clients/Acme",
		"Werk: belangrijk",
		"with spaces and : colons",
		"Ünïcodé/Ø",
		"[Gmail]/All Mail",
		"quote\"and'apostrophe",
	} {
		t.Run(name, func(t *testing.T) {
			in := ref{Mailbox: name, UIDValidity: 1750000000, UID: 9123}
			id := in.String()
			if strings.Contains(id, ":") {
				t.Fatalf("id %q contains a colon; model.ParseID would mis-split it", id)
			}
			if strings.Count(id, ".") != 2 {
				t.Fatalf("id %q does not have exactly two separators", id)
			}
			out, err := parseRef(id)
			if err != nil {
				t.Fatalf("parseRef(%q): %v", id, err)
			}
			if out != in {
				t.Errorf("round trip: %+v -> %+v", in, out)
			}
		})
	}
}

func TestParseRefRejectsRubbish(t *testing.T) {
	for _, bad := range []string{"", "nope", "a.b", "a.b.c.d", "aaaa.notanumber.1", "aaaa.1.0"} {
		if _, err := parseRef(bad); err == nil {
			t.Errorf("parseRef(%q) succeeded", bad)
		}
	}
}

// The uid codec is what keeps the state small enough to live in a TEXT column.
func TestUIDRangeCodec(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []imapv2.UID
		want string
	}{
		{"empty", nil, ""},
		{"one", []imapv2.UID{7}, "7"},
		{"contiguous collapses", []imapv2.UID{1, 2, 3, 4, 5}, "1:5"},
		{"holes split", []imapv2.UID{1, 2, 3, 7, 9, 10}, "1:3,7,9:10"},
		{"unsorted is sorted", []imapv2.UID{9, 1, 3, 2}, "1:3,9"},
		{"duplicates collapse", []imapv2.UID{4, 4, 5}, "4:5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeUIDs(tc.in)
			if got != tc.want {
				t.Fatalf("encodeUIDs = %q, want %q", got, tc.want)
			}
			back, err := decodeUIDs(got)
			if err != nil {
				t.Fatalf("decodeUIDs(%q): %v", got, err)
			}
			if got2 := encodeUIDs(back); got2 != tc.want {
				t.Errorf("round trip: %q -> %q", tc.want, got2)
			}
		})
	}
}

// A healthy folder must encode to almost nothing, or the state outgrows the
// column it lives in.
func TestUIDRangeStaysSmallForAWholeFolder(t *testing.T) {
	uids := make([]imapv2.UID, 0, 100000)
	for i := 1; i <= 100000; i++ {
		uids = append(uids, imapv2.UID(i))
	}
	if got := encodeUIDs(uids); got != "1:100000" {
		t.Errorf("100k contiguous uids encoded to %q", got)
	}
}

func TestDecodeUIDsRefusesAnImplausibleRange(t *testing.T) {
	if _, err := decodeUIDs("1:4000000000"); err == nil {
		t.Error("decodeUIDs accepted a range that would exhaust memory")
	}
}

func TestStateRoundTrips(t *testing.T) {
	in := newMailState()
	in.Pass = 3
	in.Folders["INBOX"] = folderState{
		UIDVal: 12, UIDNext: 500, Count: 400, Unseen: 2,
		UIDs: "1:400", Seen: "1:398", Flagged: "7",
	}
	out, ok := parseMailState(in.String())
	if !ok {
		t.Fatal("parseMailState refused what String produced")
	}
	if out.Pass != 3 || out.Folders["INBOX"].UIDs != "1:400" || out.Folders["INBOX"].Seen != "1:398" {
		t.Errorf("round trip lost data: %+v", out.Folders["INBOX"])
	}
}

// A state from a future or older build must be reported as unreadable rather
// than half-understood.
func TestStateRejectsOtherVersions(t *testing.T) {
	for _, bad := range []string{"", "  ", "not json", `{"v":0,"f":{}}`, `{"v":99,"f":{}}`, `{"v":1}`} {
		if _, ok := parseMailState(bad); ok {
			t.Errorf("parseMailState(%q) was accepted", bad)
		}
	}
}

func TestStateCompressesWhenLarge(t *testing.T) {
	st := newMailState()
	// Many folders, each with a pathologically holey uid set.
	for i := 0; i < 400; i++ {
		var b strings.Builder
		for u := 1; u < 200; u += 2 {
			if b.Len() > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strings.Repeat("9", 4))
		}
		st.Folders[strings.Repeat("f", 20)+string(rune('a'+i%26))+string(rune('a'+i/26))] =
			folderState{UIDVal: 1, UIDs: b.String()}
	}
	s := st.String()
	if !strings.HasPrefix(s, "z:") {
		t.Fatalf("a %d-byte state was not compressed", len(s))
	}
	if _, ok := parseMailState(s); !ok {
		t.Error("a compressed state did not parse back")
	}
}

// SPECIAL-USE mapping is pure logic, and the fake cannot advertise it
// (imapmemserver drops CreateOptions.SpecialUse), so it is tested here.
func TestRoleForSpecialUseAttributes(t *testing.T) {
	m := &Mail{}
	for _, tc := range []struct {
		attr imapv2.MailboxAttr
		want model.MailboxRole
	}{
		{imapv2.MailboxAttrArchive, model.RoleArchive},
		{imapv2.MailboxAttrDrafts, model.RoleDrafts},
		{imapv2.MailboxAttrJunk, model.RoleJunk},
		{imapv2.MailboxAttrSent, model.RoleSent},
		{imapv2.MailboxAttrTrash, model.RoleTrash},
		{imapv2.MailboxAttrAll, model.RoleAll},
		{imapv2.MailboxAttrImportant, model.RoleImportant},
	} {
		got := m.roleFor("Whatever", '/', []imapv2.MailboxAttr{tc.attr})
		if got != tc.want {
			t.Errorf("%s -> %q, want %q", tc.attr, got, tc.want)
		}
	}

	// \Flagged is a virtual starred view, not a folder. Mapping it to a role
	// the engine files into would let an archive write somewhere that cannot
	// hold messages.
	if got := m.roleFor("Starred", '/', []imapv2.MailboxAttr{imapv2.MailboxAttrFlagged}); got != "" {
		t.Errorf("\\Flagged mapped to %q; it must map to nothing", got)
	}

	// INBOX is never marked by SPECIAL-USE and must be recognised by name.
	if got := m.roleFor("inbox", '/', nil); got != model.RoleInbox {
		t.Errorf("INBOX -> %q", got)
	}
}

func TestRoleForFallsBackToNames(t *testing.T) {
	m := &Mail{preset: presets[model.VendorICloud]}
	for name, want := range map[string]model.MailboxRole{
		"Sent Messages":    model.RoleSent,
		"Deleted Messages": model.RoleTrash,
		"Drafts":           model.RoleDrafts,
		"Junk":             model.RoleJunk,
		"Archive":          model.RoleArchive,
		"Some Project":     "",
	} {
		if got := m.roleFor(name, '/', nil); got != want {
			t.Errorf("%q -> %q, want %q", name, got, want)
		}
	}
}

// "All Mail" must not be read as an archive: syncing it would file a second
// copy of the entire account.
func TestAllMailIsNotAnArchiveByName(t *testing.T) {
	m := &Mail{}
	if got := m.roleFor("All Mail", '/', nil); got != model.RoleAll {
		t.Errorf("All Mail -> %q, want %q", got, model.RoleAll)
	}
}

// Explicit config always wins: it is the user telling us about a server we
// could not recognise.
func TestRoleOverridesWinOverEverything(t *testing.T) {
	m := &Mail{opts: Options{ArchiveFolder: "Sent"}}
	if got := m.roleFor("Sent", '/', []imapv2.MailboxAttr{imapv2.MailboxAttrSent}); got != model.RoleArchive {
		t.Errorf("override lost to SPECIAL-USE: got %q", got)
	}
}

func TestParentOfOnlyPointsAtFoldersThatExist(t *testing.T) {
	known := map[string]bool{"INBOX": true, "INBOX.a": true, "INBOX.a.b": true}
	if got := parentOf("INBOX.a.b", '.', known); got != "INBOX.a" {
		t.Errorf("parent = %q, want INBOX.a", got)
	}
	// A hidden intermediate level must not leave a child pointing at a mailbox
	// row that was never created.
	if got := parentOf("INBOX.x.y", '.', known); got != "" {
		t.Errorf("parent = %q, want none", got)
	}
	if got := parentOf("Flat", 0, known); got != "" {
		t.Errorf("a flat namespace has no parents, got %q", got)
	}
}

func TestThreadIDPrefersTheReferencesRoot(t *testing.T) {
	root := "From: a@x\r\nMessage-ID: <root@x>\r\n\r\nbody\r\n"
	reply := "From: b@x\r\nMessage-ID: <reply@x>\r\nIn-Reply-To: <root@x>\r\n" +
		"References: <root@x>\r\n\r\nbody\r\n"
	if a, b := threadIDFor([]byte(root)), threadIDFor([]byte(reply)); a != b {
		t.Errorf("root and reply threaded apart: %q vs %q", a, b)
	}
	if id := threadIDFor([]byte("From: a@x\r\n\r\nno headers\r\n")); id != "" {
		t.Errorf("a message with no ids got thread %q", id)
	}
}

// A header-shaped line in the body must not be read as a header.
func TestHeaderFieldsStopsAtTheBlankLine(t *testing.T) {
	raw := []byte("Message-ID: <real@x>\r\n\r\nMessage-ID: <fake@x>\r\n")
	if got := headerFields(raw, "message-id")["message-id"]; got != "<real@x>" {
		t.Errorf("message-id = %q, want the real one", got)
	}
}

func TestHeaderFieldsUnfolds(t *testing.T) {
	raw := []byte("References: <a@x>\r\n <b@x>\r\n\r\nbody")
	if got := headerFields(raw, "references")["references"]; got != "<a@x> <b@x>" {
		t.Errorf("references = %q, want both ids", got)
	}
}
