package mime

import (
	"regexp"
	"strings"
)

// Reply attributions written by the common clients, in the languages this tool
// cares about. They are matched against a single line and, because Gmail and
// Outlook wrap them, against a line joined with the one that follows it.
var replyAttribution = []*regexp.Regexp{
	// English: "On Mon, 3 Feb 2025 at 09:14, Jane Doe <jane@example.com> wrote:"
	regexp.MustCompile(`(?i)^on\b.{0,400}?\bwrote\s*:\s*$`),
	// Dutch: "Op ma 3 feb 2025 om 09:14 schreef Jan Jansen <jan@example.nl>:"
	regexp.MustCompile(`(?i)^op\b.{0,400}?\bschreef\b.{0,200}?:\s*$`),
	regexp.MustCompile(`(?i)^op\b.{0,400}?\bheeft\b.{0,200}?\bgeschreven\s*:\s*$`),
	// German: "Am 03.02.2025 um 09:14 schrieb Hans Müller <hans@example.de>:"
	regexp.MustCompile(`(?i)^am\b.{0,400}?\bschrieb\b.{0,200}?:\s*$`),
	// French: "Le lun. 3 févr. 2025 à 09:14, Marie Curie <marie@example.fr> a écrit :"
	regexp.MustCompile(`(?i)^le\b.{0,400}?\ba\s+écrit\s*:\s*$`),
	// Apple Mail style without a leading weekday.
	regexp.MustCompile(`(?i)^\d{1,2}[./ ][a-zé.]{3,12}[./ ]\d{2,4}.{0,200}?\b(wrote|schrieb|schreef|a écrit)\s*:\s*$`),
}

// Separator lines that introduce a forwarded or quoted original message.
var replySeparator = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*-{2,}\s*(original message|oorspronkelijk bericht|urspr(ü|ue)ngliche nachricht|message d'origine|forwarded message|doorgestuurd bericht|weitergeleitete nachricht|message transf(é|e)r(é|e))\s*-{2,}\s*$`),
	regexp.MustCompile(`(?i)^\s*-{3,}\s*forwarded (message|by)\b.*$`),
	regexp.MustCompile(`(?i)^\s*_{10,}\s*$`),
	regexp.MustCompile(`(?i)^\s*begin forwarded message\s*:\s*$`),
}

// Outlook's four-line quoted header, English/Dutch/German/French.
var (
	reHdrFrom    = regexp.MustCompile(`(?i)^\s*(from|van|von|de|exp(é|e)diteur)\s*:\s*\S`)
	reHdrSent    = regexp.MustCompile(`(?i)^\s*(sent|verzonden|gesendet|envoy(é|e)|date)\s*:\s*\S`)
	reHdrTo      = regexp.MustCompile(`(?i)^\s*(to|aan|an|(à|a))\s*:\s*\S`)
	reHdrSubject = regexp.MustCompile(`(?i)^\s*(subject|onderwerp|betreff|objet)\s*:\s*`)
	reHdrCC      = regexp.MustCompile(`(?i)^\s*(cc|kopie|copie)\s*:\s*`)
)

// Mobile client footers.
var sentFrom = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^\s*sent (from|via|with|using) my .{0,60}$`),
	regexp.MustCompile(`(?i)^\s*sent from (mail|my mail|outlook|yahoo mail|samsung|the .{0,40} app).{0,60}$`),
	regexp.MustCompile(`(?i)^\s*get outlook for (ios|android)\s*$`),
	regexp.MustCompile(`(?i)^\s*(verstuurd|verzonden) van(af)? mijn .{0,60}$`),
	regexp.MustCompile(`(?i)^\s*von meinem .{0,60} gesendet\.?$`),
	regexp.MustCompile(`(?i)^\s*gesendet (von|mit) .{0,60}$`),
	regexp.MustCompile(`(?i)^\s*envoy(é|e) (de|depuis) mon .{0,60}$`),
	regexp.MustCompile(`(?i)^\s*obtenez outlook pour (ios|android)\s*$`),
	regexp.MustCompile(`(?i)^\s*t(é|e)l(é|e)charg(é|e) outlook pour (ios|android)\s*$`),
}

var reQuoted = regexp.MustCompile(`^\s{0,3}>`)

// StripQuotes removes quoted replies, forwarded originals and signatures so
// only what the sender actually typed is left. It is a pure function and never
// returns an empty result for non-empty input: if the heuristics would remove
// everything, the trimmed original is returned instead.
func StripQuotes(text string) string {
	original := strings.TrimSpace(reCRLF.Replace(text))
	if original == "" {
		return ""
	}
	lines := strings.Split(original, "\n")

	if i := cutIndex(lines); i >= 0 {
		lines = lines[:i]
	}
	lines = cutSignature(lines)
	// A footer can sit either side of the quoted original ("Sent from my
	// iPhone" above it on Apple Mail, below it on some Android clients), so
	// peel the tail until nothing more comes off.
	for {
		n := len(lines)
		lines = dropMobileFooter(lines)
		lines = dropQuotedTail(lines)
		if len(lines) == n {
			break
		}
	}

	out := normalizeText(strings.Join(lines, "\n"))
	if strings.TrimSpace(out) == "" {
		return original
	}
	return out
}

// SplitQuote divides text into what its author wrote and the quoted material
// under it, at the first line from which everything is a quote: the
// attribution ("On …, X wrote:"), a reply separator or a forwarded header.
// Signatures are not touched, unlike StripQuotes — the split is for an editor
// putting the two halves back together, not for reading. quoted is empty
// when no such line is found, and own is then the whole text.
func SplitQuote(text string) (own, quoted string) {
	text = reCRLF.Replace(text)
	lines := strings.Split(text, "\n")
	i := cutIndex(lines)
	if i < 0 {
		return text, ""
	}
	return strings.Join(lines[:i], "\n"), strings.Join(lines[i:], "\n")
}

// cutIndex finds the first line from which everything is quoted material.
func cutIndex(lines []string) int {
	for i, raw := range lines {
		l := strings.TrimRight(raw, " \t")
		if l == "" {
			continue
		}
		for _, re := range replySeparator {
			if re.MatchString(l) {
				if strings.HasPrefix(strings.TrimSpace(l), "_") && !headerFollows(lines, i+1) {
					continue
				}
				return i
			}
		}
		if matchesAttribution(l) && attributionConfirmed(lines, i, l) {
			return i
		}
		// Wrapped attribution: "On <date>,\nJane <j@x> wrote:".
		if i+1 < len(lines) && looksLikeAttributionStart(l) {
			joined := l + " " + strings.TrimSpace(lines[i+1])
			if matchesAttribution(joined) && attributionConfirmed(lines, i, joined) {
				return i
			}
			if i+2 < len(lines) {
				joined += " " + strings.TrimSpace(lines[i+2])
				if matchesAttribution(joined) && attributionConfirmed(lines, i, joined) {
					return i
				}
			}
		}
		if outlookHeaderAt(lines, i) {
			return i
		}
	}
	return -1
}

func matchesAttribution(l string) bool {
	l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), ">"))
	if len(l) > 600 {
		return false
	}
	for _, re := range replyAttribution {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

// maxSignatureLines is how long a block under a bare "--" may be before it
// stops looking like a signature.
const maxSignatureLines = 8

// An attribution line proper carries a date, a time or an address; ordinary
// prose that happens to end in "wrote:" does not.
var (
	reAttrEmail = regexp.MustCompile(`[^\s<>@,;:"]+@[^\s<>@,;:"]+\.[A-Za-z]{2,}`)
	reAttrDate  = regexp.MustCompile(`\b\d{1,2}[:.\-/]\d{2}\b|\b(19|20)\d{2}\b`)
)

// attributionConfirmed guards against cutting a message in half at a sentence
// that merely reads like an attribution ("... here is what their lawyer
// wrote:"). A real attribution names a date, a time or an address, or is
// immediately followed by the quoted material it introduces.
func attributionConfirmed(lines []string, i int, text string) bool {
	if reAttrEmail.MatchString(text) || reAttrDate.MatchString(text) {
		return true
	}
	return quotedFollows(lines, i+1)
}

// quotedFollows reports whether one of the next three non-blank lines is quoted.
func quotedFollows(lines []string, from int) bool {
	seen := 0
	for i := from; i < len(lines) && seen < 3; i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		if reQuoted.MatchString(lines[i]) {
			return true
		}
	}
	return false
}

var reAttributionStart = regexp.MustCompile(`(?i)^(on|op|am|le)\s+\S`)

func looksLikeAttributionStart(l string) bool {
	return reAttributionStart.MatchString(strings.TrimSpace(l)) && !strings.HasSuffix(strings.TrimSpace(l), ".")
}

// headerFollows reports whether one of the next few non-blank lines is a
// "From:"-style header, which is what turns a rule of underscores into a quote
// separator rather than decoration.
func headerFollows(lines []string, from int) bool {
	seen := 0
	for i := from; i < len(lines) && seen < 4; i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		if reHdrFrom.MatchString(lines[i]) {
			return true
		}
	}
	return false
}

// outlookHeaderAt reports whether a quoted Outlook header block starts at i:
// a From: line followed within a few lines by Sent:/To:/Subject:.
func outlookHeaderAt(lines []string, i int) bool {
	if !reHdrFrom.MatchString(lines[i]) {
		return false
	}
	var sent, to, subject bool
	for j := i + 1; j < len(lines) && j <= i+5; j++ {
		l := lines[j]
		if strings.TrimSpace(l) == "" {
			continue
		}
		switch {
		case reHdrSent.MatchString(l):
			sent = true
		case reHdrTo.MatchString(l):
			to = true
		case reHdrSubject.MatchString(l):
			subject = true
		case reHdrCC.MatchString(l):
			// keep scanning
		default:
			return sent && to && subject
		}
	}
	return sent && to && subject
}

// dropQuotedTail removes a trailing block of quoted lines: the original message
// under a reply. A quoted block with the sender's own text after it is not a
// quote of the thread but something they typed - a pasted REPL transcript
// (">>> import x"), a markdown block quote, a diff - and is kept.
func dropQuotedTail(lines []string) []string {
	end := len(lines)
	quoted := 0
	for end > 0 {
		l := lines[end-1]
		if strings.TrimSpace(l) == "" {
			end--
			continue
		}
		if reQuoted.MatchString(l) {
			quoted++
			end--
			continue
		}
		break
	}
	if quoted == 0 {
		return lines
	}
	return lines[:end]
}

// cutSignature removes everything after an RFC 3676 "-- " signature delimiter.
// The bare forms ("--", an em dash, a rule of underscores) are also used as a
// visual separator in the middle of a message, so they only count when what
// follows is shaped like a signature.
func cutSignature(lines []string) []string {
	for i, l := range lines {
		t := strings.TrimRight(l, " \t")
		if i == 0 || !isSignatureDelimiter(t) {
			continue // a message that opens with a rule is not a signature
		}
		// RFC 3676: exactly "-- " (with the trailing space) is the delimiter and
		// needs no corroboration.
		if t == "--" && l != t {
			return lines[:i]
		}
		if signatureBlock(lines[i+1:]) {
			return lines[:i]
		}
	}
	return lines
}

func isSignatureDelimiter(t string) bool {
	switch t {
	case "--", "—", "–", "__":
		return true
	}
	return false
}

// signatureBlock reports whether rest looks like a signature: a short,
// uninterrupted run of lines at the very end of the message. A blank line or a
// quoted line means the message carries on past the delimiter, so the delimiter
// was a rule rather than a signature marker.
func signatureBlock(rest []string) bool {
	for len(rest) > 0 && strings.TrimSpace(rest[len(rest)-1]) == "" {
		rest = rest[:len(rest)-1]
	}
	if len(rest) == 0 || len(rest) > maxSignatureLines {
		return false
	}
	for _, l := range rest {
		if strings.TrimSpace(l) == "" || reQuoted.MatchString(l) {
			return false
		}
	}
	return true
}

func dropMobileFooter(lines []string) []string {
	for len(lines) > 0 {
		last := len(lines) - 1
		l := strings.TrimSpace(lines[last])
		if l == "" {
			lines = lines[:last]
			continue
		}
		matched := false
		for _, re := range sentFrom {
			if re.MatchString(l) {
				lines = lines[:last]
				matched = true
				break
			}
		}
		if !matched {
			return lines
		}
	}
	return lines
}
