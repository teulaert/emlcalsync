package mime

import "strings"

// unflow undoes RFC 3676 format=flowed: it joins the lines a sender wrapped
// softly back into the paragraphs that were typed.
//
// Apple Mail and Thunderbird in plain-text mode send text/plain this way,
// wrapped at about 72 columns with a trailing space on every soft break.
// Indexed as is, such a mail reads, searches and quotes as a narrow column.
//
// The rules (§4.1-4.5): a line's quote depth is its count of leading ">"; one
// space after them (or at the start of an unquoted line) is space-stuffing and
// goes; a line ending in a space is soft and joins the next line of the same
// depth, except the signature separator "-- "; with DelSp=yes that trailing
// space was added by the wrapper and is dropped too. Quoted paragraphs come
// back out with "> " per level, the way emlcal quotes.
func unflow(s string, delsp bool) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var out strings.Builder
	var para strings.Builder
	depth, open := 0, false
	flush := func() {
		if !open {
			return
		}
		text := para.String()
		if text != "-- " { // the signature separator keeps its space
			text = strings.TrimRight(text, " ")
		}
		prefix := strings.Repeat("> ", depth)
		if text == "" {
			prefix = strings.TrimRight(prefix, " ")
		}
		out.WriteString(prefix + text + "\n")
		para.Reset()
		open = false
	}

	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // the final newline is not an empty line
	}
	for _, line := range lines {
		d := 0
		for d < len(line) && line[d] == '>' {
			d++
		}
		rest := strings.TrimPrefix(line[d:], " ")
		soft := strings.HasSuffix(rest, " ") && rest != "-- "
		if soft && delsp {
			rest = rest[:len(rest)-1]
		}
		if open && d != depth {
			flush() // a change of depth ends a paragraph, soft or not (§4.5)
		}
		depth, open = d, true
		para.WriteString(rest)
		if !soft {
			flush()
		}
	}
	flush()
	return out.String()
}
