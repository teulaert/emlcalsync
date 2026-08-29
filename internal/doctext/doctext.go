// Package doctext turns an attachment into text a person or a model can
// read: a PDF's words, an HTML file's prose, a text file as it is.
//
// It exists because an invoice's amount lives in the PDF, not the mail.
//
// PDFs are read by pdftotext (poppler-utils), and only by it. A pure-Go
// reader was tried and looped forever on the first real invoice it met; a
// PDF is the one document type where the mature tool is worth a runtime
// dependency, and it is what an agent on the shell would reach for anyway.
// It runs under a deadline all the same. A scanned page is a picture and
// comes back as nothing, which is reported rather than returned empty.
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

	emime "github.com/teulaert/emlcalsync/internal/mime"
)

// ErrUnsupported is returned for a type there is no extraction for.
var ErrUnsupported = errors.New("no text extraction for this type")

// ErrNoText is returned for a document that holds no text to extract -- a
// scanned PDF, an image.
var ErrNoText = errors.New("the document holds no text (scanned, or an image)")

// ErrNoPDFReader is returned when pdftotext is not installed.
var ErrNoPDFReader = errors.New("pdftotext is not installed; PDF attachments need poppler-utils (apt install poppler-utils)")

// PDFTimeout bounds one PDF extraction.
var PDFTimeout = 30 * time.Second

// lookPath is exec.LookPath, replaceable by tests.
var lookPath = exec.LookPath

// HavePDFReader reports whether pdftotext is on PATH -- what `doctor` asks.
func HavePDFReader() bool {
	_, err := lookPath("pdftotext")
	return err == nil
}

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

// fromPDF runs pdftotext on the document, within PDFTimeout. -layout keeps
// table columns -- an invoice's lines -- side by side.
func fromPDF(ctx context.Context, data []byte) (string, error) {
	path, err := lookPath("pdftotext")
	if err != nil {
		return "", ErrNoPDFReader
	}
	ctx, cancel := context.WithTimeout(ctx, PDFTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-layout", "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// Once the deadline kills the reader, stop waiting for its pipes too: a
	// grandchild holding stdout open must not turn the kill into a hang.
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("reading this PDF took longer than %s and was stopped", PDFTimeout)
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("could not read this PDF: %s", msg)
	}
	text := strings.TrimSpace(out.String())
	if text == "" {
		return "", ErrNoText
	}
	return text, nil
}
