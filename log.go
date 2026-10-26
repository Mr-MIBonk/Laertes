package main

import (
	"io"
	"log"
	"os"
	"regexp"
	"strings"

	"golang.org/x/sys/windows"
)

// Colored console logging. Laertes logs with the standard log package; setupConsoleColor routes that
// output through colorWriter, which dims the timestamp and tints each line by what it reports (errors
// red, warnings yellow, successes green, HTTP replies by status). It is enabled only when stderr is a
// real console that accepts ANSI escapes, so a redirected or captured log stays plain text.

const (
	cReset  = "\x1b[0m"
	cDim    = "\x1b[90m" // gray, for the timestamp
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
)

var (
	tsPrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)
	httpLine = regexp.MustCompile(`-> (\d{3}) in `)
)

// setupConsoleColor turns on ANSI colors for log output when stderr is a console that supports them.
func setupConsoleColor() {
	h := windows.Handle(os.Stderr.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return // stderr is redirected or captured: leave the log plain
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
		if windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) != nil {
			return // an older console without ANSI support
		}
	}
	log.SetOutput(colorWriter{os.Stderr})
}

type colorWriter struct{ w io.Writer }

func (c colorWriter) Write(p []byte) (int, error) {
	s := strings.TrimSuffix(string(p), "\n")
	nl := ""
	if len(s) != len(string(p)) {
		nl = "\n"
	}
	ts := tsPrefix.FindString(s)
	msg := s[len(ts):]
	if _, err := io.WriteString(c.w, cDim+ts+cReset+lineColor(msg)+msg+cReset+nl); err != nil {
		return 0, err
	}
	return len(p), nil
}

// lineColor picks a color for a log message from what it reports; "" leaves the default foreground.
func lineColor(msg string) string {
	if m := httpLine.FindStringSubmatch(msg); m != nil {
		switch m[1][0] {
		case '2', '3':
			return cGreen
		case '4':
			return cYellow
		case '5':
			return cRed
		}
	}
	m := strings.ToLower(msg)
	switch {
	case containsAny(m, "fail", "error", "cannot", "not reachable", "refused", "could not",
		"not registered", "no such", "unable", "panic", "✘"):
		return cRed
	case containsAny(m, "not installed", "not updated", "stays in use", "have to be pasted",
		"closed", "disconnected", "taken over", "ignored", "not supported"):
		return cYellow
	case containsAny(m, "done", "connected", "running", "installed", "opened", "renamed",
		"restored", "listening", "requested", "✔"):
		return cGreen
	}
	return ""
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
