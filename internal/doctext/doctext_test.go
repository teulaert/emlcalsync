package doctext

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess is not a test: it is what the test binary runs when it
// is started as a child by the tests below, standing in for the hidden
// pdf-text command. It reads a PDF on stdin and writes its text.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("DOCTEXT_HELPER") != "1" {
		return
	}
	defer os.Exit(0)
	if d := os.Getenv("DOCTEXT_HELPER_SLEEP"); d != "" {
		dur, _ := time.ParseDuration(d)
		time.Sleep(dur)
	}
	data, _ := io.ReadAll(os.Stdin)
	text, err := PDFInProcess(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprint(os.Stdout, text)
}

// useSelf points SelfCommand at this test binary's helper for one test,
// with no pdftotext in sight.
func useSelf(t *testing.T, env ...string) {
	t.Helper()
	old, oldLook := SelfCommand, lookPath
	SelfCommand = []string{os.Args[0], "-test.run=TestHelperProcess", "--"}
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	t.Setenv("DOCTEXT_HELPER", "1")
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	t.Cleanup(func() { SelfCommand, lookPath = old, oldLook })
}

func ctx() context.Context { return context.Background() }

func TestExtractPDFInProcess(t *testing.T) {
	got, err := PDFInProcess(TinyPDF("Totaal 1.234,56 EUR vervaldatum 25-09-2026"))
	if err != nil {
		t.Fatalf("PDFInProcess: %v", err)
	}
	if !strings.Contains(got, "1.234,56") || !strings.Contains(got, "25-09-2026") {
		t.Errorf("text = %q", got)
	}
	if _, err := PDFInProcess(TinyPDF("")); !errors.Is(err, ErrNoText) {
		t.Errorf("empty: err = %v, want ErrNoText", err)
	}
	if _, err := PDFInProcess([]byte("%PDF-1.4 this is not a pdf")); err == nil {
		t.Error("garbage should not extract")
	}
}

func TestExtractPDFThroughTheChildProcess(t *testing.T) {
	useSelf(t)
	got, err := Extract(ctx(), "application/pdf", "360954.pdf", TinyPDF("Totaal 1.234,56 EUR"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(got, "1.234,56") {
		t.Errorf("text = %q", got)
	}
	// The extension is enough when the type says nothing.
	if _, err := Extract(ctx(), "application/octet-stream", "x.pdf", TinyPDF("hi")); err != nil {
		t.Errorf("by extension: %v", err)
	}
	// A child that finds no text says so, as the same error.
	if _, err := Extract(ctx(), "application/pdf", "scan.pdf", TinyPDF("")); !errors.Is(err, ErrNoText) {
		t.Errorf("scan: err = %v, want ErrNoText", err)
	}
	// And garbage is an error, not a crash.
	if _, err := Extract(ctx(), "application/pdf", "x.pdf", []byte("nope")); err == nil || !strings.Contains(err.Error(), "could not read") {
		t.Errorf("garbage: err = %v", err)
	}
}

// A reader that hangs is killed: the whole reason it runs in a child.
func TestExtractPDFStopsAHangingReader(t *testing.T) {
	useSelf(t, "DOCTEXT_HELPER_SLEEP=10s")
	old := PDFTimeout
	PDFTimeout = 200 * time.Millisecond
	t.Cleanup(func() { PDFTimeout = old })
	start := time.Now()
	_, err := Extract(ctx(), "application/pdf", "x.pdf", TinyPDF("hi"))
	if err == nil || !strings.Contains(err.Error(), "stopped") || !strings.Contains(err.Error(), "poppler-utils") {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s; the child was not killed", time.Since(start))
	}
}

// pdftotext, when it is there, is what reads PDFs; its arguments are the
// ones that keep an invoice's columns together.
func TestExtractPDFPrefersPdftotext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "pdftotext")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"args: $*\"\ncat >/dev/null\necho 'FACTUUR   Totaal   1.234,56'\n"), 0o755); err != nil {
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

	got, err := Extract(ctx(), "application/pdf", "x.pdf", []byte("whatever"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !strings.Contains(got, "args: -layout -enc UTF-8 - -") || !strings.Contains(got, "1.234,56") {
		t.Errorf("text = %q", got)
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
