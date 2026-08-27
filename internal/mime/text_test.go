package mime

import (
	"strings"
	"testing"
)

func TestHTMLToText(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		notWant []string
	}{
		{
			name:    "script and style dropped",
			in:      "<style>body{color:red}</style><script>evil()</script><p>Hello</p>",
			want:    []string{"Hello"},
			notWant: []string{"color:red", "evil()"},
		},
		{
			name: "link keeps text and url",
			in:   `<a href="https://example.com/x">click here</a>`,
			want: []string{"click here (https://example.com/x)"},
		},
		{
			name:    "link whose text is the url is not duplicated",
			in:      `<a href="https://example.com/x">https://example.com/x</a>`,
			want:    []string{"https://example.com/x"},
			notWant: []string{"("},
		},
		{
			name:    "table rows on separate lines",
			in:      "<table><tr><td>a</td><td>b</td></tr><tr><td>c</td><td>d</td></tr></table>",
			want:    []string{"a b", "c d"},
			notWant: []string{"a b c d"},
		},
		{
			name: "divs break lines",
			in:   "<div>one</div><div>two</div>",
			want: []string{"one\ntwo"},
		},
		{
			name: "entities decoded",
			in:   "<p>caf&eacute; &amp; cr&egrave;me &mdash; 5 &lt; 6</p>",
			want: []string{"café & crème — 5 < 6"},
		},
		{
			name:    "comments and conditional markup dropped",
			in:      "<!--[if mso]><table><tr><td>outlook junk</td></tr></table><![endif]--><p>real</p>",
			want:    []string{"real"},
			notWant: []string{"outlook junk"},
		},
		{
			name: "unclosed tags do not eat the document",
			in:   "<div><p>text that never closes",
			want: []string{"text that never closes"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := HTMLToText(tc.in)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("HTMLToText(%q) = %q, missing %q", tc.in, got, w)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got, w) {
					t.Errorf("HTMLToText(%q) = %q, should not contain %q", tc.in, got, w)
				}
			}
		})
	}
}

func TestDecodeBytes(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{"utf8", []byte("café"), "café"},
		{"bom stripped", append([]byte("\xef\xbb\xbf"), []byte("hi")...), "hi"},
		{"latin1 fallback", []byte{'c', 'a', 'f', 0xe9}, "café"},
		{"cp1252 smart quote", []byte{0x93, 'h', 'i', 0x94}, "“hi”"},
		{"empty", nil, ""},
	}
	for _, tc := range tests {
		if got := decodeBytes(tc.in); got != tc.want {
			t.Errorf("%s: decodeBytes(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestDecodeWord(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain subject", "plain subject"},
		{"=?utf-8?Q?caf=C3=A9?=", "café"},
		{"=?iso-8859-1?Q?Gr=FC=DFe?=", "Grüße"},
		{"=?utf-8?B?VmVyZ2FkZXJpbmc=?=", "Vergadering"},
		{"=?unknown-charset?Q?abc?=", "=?unknown-charset?Q?abc?="},
		{"folded\r\n subject", "folded subject"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := decodeWord(tc.in); got != tc.want {
			t.Errorf("decodeWord(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRepairUTF8(t *testing.T) {
	got := repairUTF8("café \xe9 ok")
	if got != "café é ok" {
		t.Errorf("repairUTF8 = %q", got)
	}
	if got := repairUTF8("clean"); got != "clean" {
		t.Errorf("repairUTF8(clean) = %q", got)
	}
}
