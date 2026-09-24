package logging

import (
	"bytes"
	"io"
	"os"
	"time"

	"bonanza.build/pkg/bazelclient/formatted"
)

// FormattedNodeWriter is called into by the console logger to format
// messages before writing them. For example, if the console supports
// VT100 escape sequences, the FormattedNodeWriter may format the
// message using Set Graphic Rendition (SGR) escape sequences.
type FormattedNodeWriter func(message formatted.Node, w io.StringWriter) (int, error)

type consoleLogger struct {
	w              io.Writer
	writeFormatted FormattedNodeWriter
	curses         bool
	showTimestamps bool
}

// NewConsoleLogger creates a Logger that writes log messages to a
// textual console, like a terminal or a simple log file. Messages are
// prefixed with their severity.
func NewConsoleLogger(w io.Writer, writeFormatted FormattedNodeWriter) Logger {
	return &consoleLogger{
		w:              w,
		writeFormatted: writeFormatted,
		curses:         true,
	}
}

func (l *consoleLogger) timestampPrefix() formatted.Node {
	if l.showTimestamps {
		return formatted.Text(time.Now().Format("2006-01-02 15:04:05") + " ")
	}
	return formatted.Text("")
}

func (l *consoleLogger) Error(message formatted.Node) {
	var b bytes.Buffer
	l.writeFormatted(
		formatted.Join(
			l.timestampPrefix(),
			formatted.Bold(formatted.Red(formatted.Text("ERROR: "))),
			message,
			formatted.Text("\n"),
		),
		&b,
	)
	l.w.Write(b.Bytes())
}

func (l *consoleLogger) Fatal(message formatted.Node) {
	l.Error(message)
	os.Exit(1)
}

func (l *consoleLogger) Warning(message formatted.Node) {
	var b bytes.Buffer
	l.writeFormatted(
		formatted.Join(
			l.timestampPrefix(),
			formatted.Bold(formatted.Yellow(formatted.Text("WARNING: "))),
			message,
			formatted.Text("\n"),
		),
		&b,
	)
	l.w.Write(b.Bytes())
}

func (l *consoleLogger) Info(message formatted.Node) {
	var b bytes.Buffer
	l.writeFormatted(
		formatted.Join(
			l.timestampPrefix(),
			formatted.Green(formatted.Text("INFO: ")),
			message,
			formatted.Text("\n"),
		),
		&b,
	)
	l.w.Write(b.Bytes())
}

func (l *consoleLogger) RemovePreviousLines(linesCount int) {
	if !l.curses || linesCount == 0 {
		return
	}
	var b bytes.Buffer
	l.writeFormatted(
		formatted.Join(
			formatted.Text("\r"),
			formatted.CursorUp(linesCount),
			formatted.EraseDisplay,
		),
		&b,
	)
	l.w.Write(b.Bytes())
}
