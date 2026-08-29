package doctext

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func ctx() context.Context { return context.Background() }

// fakePdftotext puts a script named pdftotext on the path lookPath sees,
// with the given body, for one test.
func fakePdftotext(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	script := filepath.Join(t.TempDir(), "pdftotext")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	old := lookPath
	lookPath = func(name string) (string, error) {
		if name == "pdftotext" {
			return script, nil
		}
		return "", exec.ErrNotFound
	}
	t.Cleanup(func() { lookPath = old })
}

func noPdftotext(t *testing.T) {
	t.Helper()
	old := lookPath
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookPath = old })
}

// pdftotext reads the PDF from stdin with the arguments that keep an
// invoice's columns together, and its stdout is the text.
func TestExtractPDFRunsPdftotext(t *testing.T) {
	fakePdftotext(t, "echo \"args: $*\"\nn=$(wc -c)\necho \"stdin: $n bytes\"\necho 'FACTUUR   Totaal   1.234,56'\n")
	got, err := Extract(ctx(), "application/pdf", "360954.pdf", []byte("%PDF-1.4 twelve bytes"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for _, want := range []string{"args: -layout -enc UTF-8 - -", "stdin: 21 bytes", "1.234,56"} {
		if !strings.Contains(got, want) {
			t.Errorf("text lacks %q:\n%s", want, got)
		}
	}
	// The extension is enough when the type says nothing.
	if _, err := Extract(ctx(), "application/octet-stream", "x.pdf", []byte("x")); err != nil {
		t.Errorf("by extension: %v", err)
	}
	if !HavePDFReader() {
		t.Error("HavePDFReader should see the fake")
	}
}

func TestExtractPDFWithoutText(t *testing.T) {
	fakePdftotext(t, "cat >/dev/null\necho '   '\n")
	if _, err := Extract(ctx(), "application/pdf", "scan.pdf", []byte("x")); !errors.Is(err, ErrNoText) {
		t.Errorf("err = %v, want ErrNoText", err)
	}
}

func TestExtractPDFReportsPdftotextFailure(t *testing.T) {
	fakePdftotext(t, "cat >/dev/null\necho 'Syntax Error: Couldn'\\''t read xref table' >&2\nexit 1\n")
	_, err := Extract(ctx(), "application/pdf", "x.pdf", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "could not read this PDF") || !strings.Contains(err.Error(), "xref") {
		t.Errorf("err = %v", err)
	}
}

func TestExtractPDFStopsAHangingReader(t *testing.T) {
	fakePdftotext(t, "sleep 10\n")
	old := PDFTimeout
	PDFTimeout = 200 * time.Millisecond
	t.Cleanup(func() { PDFTimeout = old })
	start := time.Now()
	_, err := Extract(ctx(), "application/pdf", "x.pdf", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s; the child was not killed", time.Since(start))
	}
}

func TestExtractPDFNeedsPdftotext(t *testing.T) {
	noPdftotext(t)
	_, err := Extract(ctx(), "application/pdf", "x.pdf", []byte("x"))
	if !errors.Is(err, ErrNoPDFReader) || !strings.Contains(err.Error(), "poppler-utils") {
		t.Errorf("err = %v", err)
	}
	if HavePDFReader() {
		t.Error("HavePDFReader should say no")
	}
}

func TestExtractTextAndHTML(t *testing.T) {
	got, err := Extract(ctx(), "text/csv; charset=utf-8", "report.csv", []byte("\xEF\xBB\xBFa,b\r\n1,2\r\n"))
	if err != nil || got != "a,b\n1,2" {
		t.Errorf("csv = %q, %v", got, err)
	}
	got, err = Extract(ctx(), "text/html", "", []byte("<html><body><p>Hoi <b>Anna</b></p></body></html>"))
	if err != nil || !strings.Contains(got, "Hoi Anna") {
		t.Errorf("html = %q, %v", got, err)
	}
	got, err = Extract(ctx(), "application/octet-stream", "notes.md", []byte("# hi"))
	if err != nil || got != "# hi" {
		t.Errorf("md = %q, %v", got, err)
	}
}

func TestExtractUnsupported(t *testing.T) {
	_, err := Extract(ctx(), "image/png", "photo.png", []byte{0x89, 'P', 'N', 'G'})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "photo.png") {
		t.Errorf("err = %v", err)
	}
	if Supported("image/png", "photo.png") || !Supported("application/pdf", "") {
		t.Error("Supported disagrees with Extract")
	}
}
