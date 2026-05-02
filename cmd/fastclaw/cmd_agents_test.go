package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailLogReturnsLastLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	body := strings.Builder{}
	for i := 1; i <= 200; i++ {
		body.WriteString("line-")
		body.WriteString(itoa(i))
		body.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	var buf bytes.Buffer
	if err := tailLog(path, 5, false, &buf); err != nil {
		t.Fatalf("tailLog: %v", err)
	}
	got := strings.TrimRight(buf.String(), "\n")
	want := "line-196\nline-197\nline-198\nline-199\nline-200"
	if got != want {
		t.Fatalf("tail output mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestTailLogHandlesShortFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte("only-line\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	var buf bytes.Buffer
	if err := tailLog(path, 50, false, &buf); err != nil {
		t.Fatalf("tailLog: %v", err)
	}
	if buf.String() != "only-line\n" {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

func TestTailLogHandlesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	var buf bytes.Buffer
	if err := tailLog(path, 5, false, &buf); err != nil {
		t.Fatalf("tailLog on empty file: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected empty output, got %q", buf.String())
	}
}

func TestTailLogReportsMissingFile(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	err := tailLog(filepath.Join(dir, "nope"), 5, false, &buf)
	if err == nil {
		t.Fatal("expected missing-file error")
	}
	if !strings.Contains(err.Error(), "no log file found") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
