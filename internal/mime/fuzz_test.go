package mime

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func corpus(t testing.TB) [][]byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "*.eml"))
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// checkParsed asserts the invariants every Parsed must satisfy, however broken
// the input was.
func checkParsed(t *testing.T, p *Parsed, raw []byte) {
	t.Helper()
	if p == nil {
		t.Fatal("Parse returned nil without an error")
	}
	if !utf8.ValidString(p.TextBody) {
		t.Error("TextBody is not valid UTF-8")
	}
	if !utf8.ValidString(p.Subject) {
		t.Error("Subject is not valid UTF-8")
	}
	seen := map[string]bool{}
	for _, part := range p.AllParts {
		if part.Path == "" {
			t.Error("part with empty path")
		}
		if seen[part.Path] {
			t.Errorf("duplicate part path %q", part.Path)
		}
		seen[part.Path] = true
		if strings.ContainsAny(part.Filename, "/\\") {
			t.Errorf("filename %q contains a path separator", part.Filename)
		}
		// Every advertised part must be retrievable.
		if _, _, _, err := PartContent(raw, part.Path); err != nil {
			t.Errorf("PartContent(%q): %v", part.Path, err)
		}
	}
	for _, a := range p.Attachments {
		if !seen[a.Path] {
			t.Errorf("attachment %q is not in AllParts", a.Path)
		}
	}
	if p.HTMLPart != "" && !seen[p.HTMLPart] {
		t.Errorf("HTMLPart %q is not in AllParts", p.HTMLPart)
	}
}

func FuzzParse(f *testing.F) {
	for _, b := range corpus(f) {
		f.Add(b)
	}
	f.Add([]byte(""))
	f.Add([]byte("From: a@b\r\n\r\nbody"))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\na\r\n--x--"))
	f.Add([]byte("Content-Type: multipart/mixed\r\n\r\nno boundary parameter"))
	f.Add([]byte("\xff\xfe\x00\x01garbage"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		p, err := Parse(raw)
		if err != nil {
			if p != nil {
				t.Error("Parse returned both a result and an error")
			}
			return
		}
		checkParsed(t, p, raw)

		// StripQuotes and Snippet must survive whatever came out.
		if s := StripQuotes(p.TextBody); strings.TrimSpace(p.TextBody) != "" && strings.TrimSpace(s) == "" {
			t.Error("StripQuotes emptied a non-empty body")
		}
		Snippet(p.TextBody, 80)
	})
}

func FuzzStripQuotes(f *testing.F) {
	seeds := []string{
		"",
		"hello",
		"> quoted\n> more",
		"reply\n\nOn Mon, 3 Feb 2025 at 09:14, Jane <j@x.com> wrote:\n> hi",
		"-- \nsig",
		"From: a\nSent: b\nTo: c\nSubject: d",
		"________________________________\nFrom: a",
		"Op ma 3 feb 2025 om 09:14 schreef Jan <j@x.nl>:\n> hoi",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := StripQuotes(s)
		if !utf8.ValidString(s) {
			return
		}
		if strings.TrimSpace(s) != "" && strings.TrimSpace(got) == "" {
			t.Errorf("StripQuotes(%q) = %q: emptied a non-empty message", s, got)
		}
		if got != StripQuotes(s) {
			t.Error("StripQuotes is not deterministic")
		}
	})
}

// TestParseMutatedCorpus runs a deterministic mutation pass over the corpus so
// the ordinary `go test` run exercises the same robustness fuzzing does.
func TestParseMutatedCorpus(t *testing.T) {
	base := corpus(t)
	rng := rand.New(rand.NewSource(20250825))

	for i := 0; i < 3000; i++ {
		src := base[rng.Intn(len(base))]
		raw := mutate(rng, src)
		p, err := Parse(raw)
		if err != nil {
			continue
		}
		checkParsed(t, p, raw)
		StripQuotes(p.TextBody)
		Snippet(p.TextBody, 100)
		if t.Failed() {
			t.Fatalf("failing input (%d bytes):\n%q", len(raw), raw)
		}
	}
}

func mutate(rng *rand.Rand, src []byte) []byte {
	b := append([]byte(nil), src...)
	if len(b) == 0 {
		return b
	}
	n := 1 + rng.Intn(8)
	for i := 0; i < n; i++ {
		switch rng.Intn(6) {
		case 0: // flip a byte
			b[rng.Intn(len(b))] = byte(rng.Intn(256))
		case 1: // truncate
			b = b[:rng.Intn(len(b))]
		case 2: // delete a run
			if len(b) > 10 {
				start := rng.Intn(len(b) - 5)
				end := start + 1 + rng.Intn(min(64, len(b)-start))
				b = append(b[:start], b[end:]...)
			}
		case 3: // insert random bytes
			ins := make([]byte, 1+rng.Intn(16))
			for j := range ins {
				ins[j] = byte(rng.Intn(256))
			}
			at := rng.Intn(len(b))
			b = append(b[:at], append(ins, b[at:]...)...)
		case 4: // duplicate a chunk
			if len(b) > 20 {
				start := rng.Intn(len(b) - 10)
				end := start + 1 + rng.Intn(min(200, len(b)-start))
				b = append(b[:end], append(append([]byte(nil), b[start:end]...), b[end:]...)...)
			}
		case 5: // strip CRs, breaking header folding
			b = []byte(strings.ReplaceAll(string(b), "\r", ""))
		}
		if len(b) == 0 {
			return b
		}
	}
	return b
}
