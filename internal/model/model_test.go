package model

import "testing"

func TestParseID(t *testing.T) {
	cases := []struct {
		in   string
		kind IDKind
		acct, remote, cal string
		err  bool
	}{
		{"work:18f3a2b9", KindMessage, "work", "18f3a2b9", "", false},
		{"work:t:18f3a2b9", KindThread, "work", "18f3a2b9", "", false},
		{"work:c:lennert@example.com:abc123", KindEvent, "work", "abc123", "lennert@example.com", false},
		{"personal:c:Mx:ev", KindEvent, "personal", "ev", "Mx", false},
		{"work", 0, "", "", "", true},
		{":x", 0, "", "", "", true},
		{"work:t:", 0, "", "", "", true},
		{"work:c:onlycal", 0, "", "", "", true},
	}
	for _, c := range cases {
		p, err := ParseID(c.in)
		if (err != nil) != c.err {
			t.Fatalf("%q: err=%v want err=%v", c.in, err, c.err)
		}
		if err != nil {
			continue
		}
		if p.Kind != c.kind || p.Account != c.acct || p.Remote != c.remote || p.Calendar != c.cal {
			t.Fatalf("%q: got %+v", c.in, p)
		}
	}
	if MessagePublicID("a", "b") != "a:b" || ThreadPublicID("a", "b") != "a:t:b" || EventPublicID("a", "c", "e") != "a:c:c:e" {
		t.Fatal("public id builders")
	}
}
