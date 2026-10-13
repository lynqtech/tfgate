// Package ui contains terminal output helpers and interactive prompts.
package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"
)

// ansiSequence matches CSI (colors, cursor movement, erase) and OSC (window
// title, hyperlinks) escape sequences.
var ansiSequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

const (
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorReset  = "\033[0m"
)

// UI writes colored output and reads answers from the terminal.
type UI struct {
	Out io.Writer
	Err io.Writer
	in  *bufio.Reader
}

// New returns a UI that reads answers from /dev/tty, falling back to stdin
// when no terminal is attached.
func New() *UI {
	var in io.Reader = os.Stdin
	if tty, err := os.Open("/dev/tty"); err == nil {
		in = tty
	}
	return NewWithIO(in, os.Stdout, os.Stderr)
}

// NewWithIO returns a UI using the given streams. Used in tests.
func NewWithIO(in io.Reader, out, errOut io.Writer) *UI {
	return &UI{Out: out, Err: errOut, in: bufio.NewReader(in)}
}

func (u *UI) Println(a ...any) { _, _ = fmt.Fprintln(u.Out, a...) }

func (u *UI) Printf(format string, a ...any) { _, _ = fmt.Fprintf(u.Out, format, a...) }

func (u *UI) Red(format string, a ...any) {
	_, _ = fmt.Fprintf(u.Out, colorRed+format+colorReset+"\n", a...)
}

func (u *UI) Yellow(format string, a ...any) {
	_, _ = fmt.Fprintf(u.Out, colorYellow+format+colorReset+"\n", a...)
}

// Error prints a red message to stderr.
func (u *UI) Error(format string, a ...any) {
	_, _ = fmt.Fprintf(u.Err, colorRed+"Error: "+format+colorReset+"\n", a...)
}

// Sanitize makes text from the cluster safe to print to a terminal. It removes
// escape sequences and control characters other than newline and tab, so that
// a plan cannot move the cursor, erase lines or overwrite text the user has
// already seen. Carriage returns are dropped for the same reason. Bidi
// override and isolate characters are removed because they reorder text
// visually. Colors in the original text are lost.
func Sanitize(s string) string {
	s = ansiSequence.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r):
			return -1
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, s)
}

// Ask prints the question and returns the first character of the answer,
// upper-cased. An empty answer or a read error returns "".
func (u *UI) Ask(question string) string {
	_, _ = fmt.Fprintln(u.Out, question)
	line, err := u.in.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	return strings.ToUpper(line[:1])
}
