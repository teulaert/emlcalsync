package jmapfake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// call posts one method call and returns the response name and arguments.
func call(t *testing.T, s *Server, using []string, name string, args map[string]any) (string, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"using":       append([]string{CapCore}, using...),
		"methodCalls": []any{[]any{name, args, "c0"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, s.BaseURL()+"/jmap/api", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: HTTP %d: %s", name, resp.StatusCode, b)
	}
	var out struct {
		MethodResponses []json.RawMessage `json:"methodResponses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.MethodResponses) != 1 {
		t.Fatalf("%s: %d method responses, want 1", name, len(out.MethodResponses))
	}
	var trip []json.RawMessage
	if err := json.Unmarshal(out.MethodResponses[0], &trip); err != nil {
		t.Fatal(err)
	}
	var gotName string
	json.Unmarshal(trip[0], &gotName)
	gotArgs := map[string]any{}
	json.Unmarshal(trip[1], &gotArgs)
	return gotName, gotArgs
}

func addN(t *testing.T, s *Server, n int) []string {
	t.Helper()
	out := make([]string, 0, n)
	for i := range n {
		raw := []byte("Subject: message " + string(rune('a'+i%26)) + "\r\n\r\nbody\r\n")
		out = append(out, s.AddMessage(raw, []string{MailboxInbox}, map[string]bool{"$seen": true}))
	}
	return out
}

func strList(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%v is not a list", v)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

func TestUnknownMethodIsVisible(t *testing.T) {
	s := New(t)
	name, args := call(t, s, []string{CapMail}, "Email/nonsense", map[string]any{})
	if name != "error" {
		t.Fatalf("response name = %q, want error", name)
	}
	if args["type"] != "unknownMethod" {
		t.Errorf("error type = %v, want unknownMethod", args["type"])
	}
}

func TestCalendarMethodNeedsItsCapability(t *testing.T) {
	s := New(t)
	// Without the calendars URN in "using", a calendar method is unknown.
	name, args := call(t, s, []string{CapMail}, "Calendar/get", map[string]any{"ids": nil})
	if name != "error" || args["type"] != "unknownMethod" {
		t.Fatalf("Calendar/get without the capability = %s %v, want an unknownMethod error", name, args)
	}
	name, _ = call(t, s, []string{CapCalendars}, "Calendar/get", map[string]any{"ids": nil})
	if name != "Calendar/get" {
		t.Fatalf("Calendar/get with the capability = %q", name)
	}
}

func TestEmailQueryPositionAndTotal(t *testing.T) {
	s := New(t)
	want := addN(t, s, 7)

	_, args := call(t, s, []string{CapMail}, "Email/query", map[string]any{
		"accountId":      AccountID,
		"sort":           []any{map[string]any{"property": "receivedAt", "isAscending": true}},
		"limit":          3,
		"position":       0,
		"calculateTotal": true,
	})
	if got := strList(t, args["ids"]); len(got) != 3 || got[0] != want[0] || got[2] != want[2] {
		t.Errorf("first page = %v, want the first three of %v", got, want)
	}
	if args["total"] != float64(7) {
		t.Errorf("total = %v, want 7", args["total"])
	}

	_, args = call(t, s, []string{CapMail}, "Email/query", map[string]any{
		"accountId": AccountID, "limit": 3, "position": 3,
	})
	if got := strList(t, args["ids"]); len(got) != 3 || got[0] != want[3] {
		t.Errorf("second page = %v, want %v…", got, want[3])
	}
	if args["position"] != float64(3) {
		t.Errorf("position echoed as %v, want 3", args["position"])
	}
}

func TestEmailQueryAnchorPaging(t *testing.T) {
	s := New(t)
	want := addN(t, s, 5)

	// anchor + anchorOffset 1 starts at the record after the anchor.
	_, args := call(t, s, []string{CapMail}, "Email/query", map[string]any{
		"accountId": AccountID, "limit": 10,
		"anchor": want[1], "anchorOffset": 1,
	})
	got := strList(t, args["ids"])
	if len(got) != 3 || got[0] != want[2] {
		t.Errorf("anchored page = %v, want %v", got, want[2:])
	}
	if args["position"] != float64(2) {
		t.Errorf("position = %v, want 2", args["position"])
	}

	// A destroyed anchor is an anchorNotFound error, which is what makes the
	// client fall back to a plain position.
	s.Delete(want[1])
	name, args := call(t, s, []string{CapMail}, "Email/query", map[string]any{
		"accountId": AccountID, "limit": 10, "anchor": want[1], "anchorOffset": 1,
	})
	if name != "error" || args["type"] != "anchorNotFound" {
		t.Errorf("query on a destroyed anchor = %s %v, want anchorNotFound", name, args)
	}
}

func TestEmailChangesTracksCreatesUpdatesDestroys(t *testing.T) {
	s := New(t)
	ids := addN(t, s, 3)

	_, args := call(t, s, []string{CapMail}, "Email/get", map[string]any{
		"accountId": AccountID, "ids": []string{},
	})
	state, _ := args["state"].(string)
	if state == "" {
		t.Fatal("Email/get returned no state")
	}

	newID := addN(t, s, 1)[0]
	s.SetFlags(ids[0], map[string]bool{"$seen": true, "$flagged": true})
	s.Delete(ids[2])

	_, args = call(t, s, []string{CapMail}, "Email/changes", map[string]any{
		"accountId": AccountID, "sinceState": state, "maxChanges": 100,
	})
	if got := strList(t, args["created"]); len(got) != 1 || got[0] != newID {
		t.Errorf("created = %v, want [%s]", got, newID)
	}
	if got := strList(t, args["updated"]); len(got) != 1 || got[0] != ids[0] {
		t.Errorf("updated = %v, want [%s]", got, ids[0])
	}
	if got := strList(t, args["destroyed"]); len(got) != 1 || got[0] != ids[2] {
		t.Errorf("destroyed = %v, want [%s]", got, ids[2])
	}
	if args["hasMoreChanges"] != false {
		t.Errorf("hasMoreChanges = %v, want false", args["hasMoreChanges"])
	}

	// A state the server never issued cannot be diffed.
	name, args := call(t, s, []string{CapMail}, "Email/changes", map[string]any{
		"accountId": AccountID, "sinceState": "not-a-state", "maxChanges": 100,
	})
	if name != "error" || args["type"] != "cannotCalculateChanges" {
		t.Errorf("changes from a bogus state = %s %v, want cannotCalculateChanges", name, args)
	}
}

func TestSessionAdvertisesEverythingTheClientNeeds(t *testing.T) {
	s := New(t)
	resp, err := http.Get(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated session request = HTTP %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, s.URL(), nil)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var session map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"apiUrl", "downloadUrl", "uploadUrl", "eventSourceUrl", "state", "username"} {
		if v, _ := session[key].(string); v == "" {
			t.Errorf("session has no %s", key)
		}
	}
	primary, _ := session["primaryAccounts"].(map[string]any)
	for _, urn := range []string{CapMail, CapSubmission, CapCalendars} {
		if primary[urn] != AccountID {
			t.Errorf("primaryAccounts[%s] = %v, want %s", urn, primary[urn], AccountID)
		}
	}
	caps, _ := session["capabilities"].(map[string]any)
	core, _ := caps[CapCore].(map[string]any)
	if core["maxObjectsInGet"] == nil || core["maxCallsInRequest"] == nil {
		t.Errorf("core capability has no limits: %v", core)
	}
}
