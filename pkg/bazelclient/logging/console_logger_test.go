package logging

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"bonanza.build/pkg/bazelclient/formatted"
)

func TestNoCursesPreservesProgressLines(t *testing.T) {
	var out bytes.Buffer
	logger := NewConsoleLogger(&out, formatted.WriteVT100).(*consoleLogger)
	logger.curses = false
	logger.Info(formatted.Text("first progress"))
	logger.RemovePreviousLines(1)
	logger.Info(formatted.Text("second progress"))
	if got := out.String(); !strings.Contains(got, "first progress") || !strings.Contains(got, "second progress") || strings.Contains(got, "\r") || strings.Contains(got, "\x1b[J") || strings.Contains(got, "\x1b[1A") {
		t.Fatalf("--nocurses should preserve both lines without cursor control, got %q", got)
	}
}

func TestShowTimestampsPrefixesDiagnostics(t *testing.T) {
	var out bytes.Buffer
	logger := NewConsoleLogger(&out, formatted.WritePlainText).(*consoleLogger)
	logger.showTimestamps = true
	logger.Error(formatted.Text("build failed"))
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d ERROR: build failed\n$`).MatchString(out.String()) {
		t.Fatalf("--show_timestamps should prefix diagnostics, got %q", out.String())
	}
}
