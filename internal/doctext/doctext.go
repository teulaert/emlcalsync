// Package doctext turns an attachment into text a person or a model can
// read: a PDF's words, an HTML file's prose, a text file as it is.
//
// It exists because an invoice's amount lives in the PDF, not the mail.
//
// PDFs are the hard part. The pure-Go reader (rsc.io/pdf's lineage) keeps
// the binary static and handles simple documents, but it is known to loop
// forever or panic on real-world ones, so it is never run in this process:
// pdftotext (poppler) is used when it is on PATH -- it reads everything --
// and otherwise the binary re-runs itself with a hidden command that does
// the pure-Go extraction, with a deadline that kills it. A scanned page is a
// picture and comes back as nothing, which is reported rather than returned
// empty.
package doctext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dslipak/pdf"

	emime "github.com/teulaert/emlcalsync/internal/mime"
)

// ErrUnsupported is returned for a type there is no extraction for.
var ErrUnsupported = errors.New("no text extraction for this type")

// ErrNoText is returned for a document that holds no text to extract -- a
// scanned PDF, an image.
var ErrNoText = errors.New("the document holds no text (scanned, or an image)")

// PDFTimeout bounds one PDF extraction, whichever reader runs it.
var PDFTimeout = 30 * time.Second

// SelfCommand is the command that runs the pure-Go PDF reader in a child
// process -- this binary with its hidden pdf-text command -- reading the PDF
// on stdin and writing the text on stdout. The binary's main sets it; when
// it is empty and pdftotext is absent, the reader runs in-process, which is
// only safe for a PDF you trust.
var SelfCommand []string

// lookPath is exec.LookPath, replaceable by tests.
var lookPath = exec.LookPath

// Extract returns the text of an attachment. The type is taken from
// contentType, or from the file name's extension when the type says nothing
// (application/octet-stream is what many senders label everything).
func Extract(ctx context.Context, contentType, filename string, data []byte) (string, error) {
	kind := kindOf(contentType, filename)
	switch kind {
	case "pdf":
		return fromPDF(ctx, data)
	case "html":
		return strings.TrimSpace(emime.HTMLToText(string(data))), nil
	case "text":
		s := string(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})) // a BOM is not text
		if !utf8.ValidString(s) {
			s = strings.ToValidUTF8(s, "�")
		}
		return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")), nil
	}
	return "", fmt.Errorf("%w: %s", ErrUnsupported, describe(contentType, filename))
}

// Supported reports whether Extract has anything to say for the type.
func Supported(contentType, filename string) bool {
	return kindOf(contentType, filename) != ""
}

func kindOf(contentType, filename string) string {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	}
	ext := strings.ToLower(filepath.Ext(filename))
	switch {
	case mt == "application/pdf" || ext == ".pdf":
		return "pdf"
	case mt == "text/html" || mt == "application/xhtml+xml" || ext == ".html" || ext == ".htm":
		return "html"
	case strings.HasPrefix(mt, "text/"),
		mt == "application/json", mt == "application/xml",
		ext == ".txt", ext == ".md", ext == ".csv", ext == ".json", ext == ".xml", ext == ".log":
		return "text"
	}
	return ""
}

func describe(contentType, filename string) string {
	if filename != "" {
		return filename + " (" + contentType + ")"
	}
	return contentType
}

// fromPDF picks the reader: pdftotext when it is there, else this binary's
// own reader in a child process, else in-process as a last resort.
func fromPDF(ctx context.Context, data []byte) (string, error) {
	if path, err := lookPath("pdftotext"); err == nil {
		// -layout keeps table columns -- an invoice's lines -- side by side.
		return runReader(ctx, data, path, "-layout", "-enc", "UTF-8", "-", "-")
	}
	if len(SelfCommand) > 0 {
		return runReader(ctx, data, SelfCommand[0], SelfCommand[1:]...)
	}
	return PDFInProcess(data)
}

// runReader runs a reader that takes the PDF on stdin and gives text on
// stdout, within PDFTimeout, and kills it otherwise.
func runReader(ctx context.Context, data []byte, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, PDFTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(data)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("reading this PDF took longer than %s and was stopped; install poppler-utils (pdftotext) for a reader that copes with more documents", PDFTimeout)
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, ErrNoText.Error()) {
			return "", ErrNoText
		}
		return "", fmt.Errorf("could not read this PDF: %s", msg)
	}
	text := strings.TrimSpace(out.String())
	if text == "" {
		return "", ErrNoText
	}
	return text, nil
}

// PDFInProcess reads the text a PDF carries with the pure-Go reader, here,
// now. It is what the hidden child command runs; anything else should go
// through Extract, which keeps a hang or a panic out of the process.
func PDFInProcess(data []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("could not read this PDF: %v", r)
		}
	}()
	rd, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("could not read this PDF: %w", err)
	}
	var b strings.Builder
	for i := 1; i <= rd.NumPage(); i++ {
		p := rd.Page(i)
		if p.V.IsNull() {
			continue
		}
		s, err := p.GetPlainText(nil)
		if err != nil {
			return "", fmt.Errorf("could not read page %d of this PDF: %w", i, err)
		}
		if strings.TrimSpace(s) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.TrimSpace(s))
	}
	if b.Len() == 0 {
		return "", ErrNoText
	}
	return b.String(), nil
}
