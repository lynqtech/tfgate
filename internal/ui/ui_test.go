package ui

import (
	"bytes"
	"strings"
	"testing"
)

func newTestUI(input string) (*UI, *bytes.Buffer, *bytes.Buffer) {
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	return NewWithIO(strings.NewReader(input), out, errOut), out, errOut
}

func TestAsk(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"lowercase is upper-cased", "y\n", "Y"},
		{"uppercase", "Y\n", "Y"},
		{"only first character counts", "yes please\n", "Y"},
		{"surrounding whitespace ignored", "  r  \n", "R"},
		{"no trailing newline", "o", "O"},
		{"empty answer", "\n", ""},
		{"whitespace only", "   \n", ""},
		{"closed input", "", ""},
		{"crlf line ending", "y\r\n", "Y"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, out, _ := newTestUI(tt.input)
			if got := u.Ask("Continue?"); got != tt.want {
				t.Errorf("Ask() = %q, want %q", got, tt.want)
			}
			if !strings.Contains(out.String(), "Continue?") {
				t.Errorf("question not printed, output: %q", out)
			}
		})
	}
}

func TestAskReadsOneLinePerCall(t *testing.T) {
	u, _, _ := newTestUI("y\nn\n")
	if got := u.Ask("first"); got != "Y" {
		t.Errorf("first answer = %q, want Y", got)
	}
	if got := u.Ask("second"); got != "N" {
		t.Errorf("second answer = %q, want N", got)
	}
	if got := u.Ask("third"); got != "" {
		t.Errorf("answer after input ran out = %q, want empty", got)
	}
}

func TestOutputGoesToTheRightStream(t *testing.T) {
	u, out, errOut := newTestUI("")

	u.Println("a", "b")
	u.Printf("%d-%s\n", 1, "x")
	u.Red("red %s", "text")
	u.Yellow("yellow %s", "text")
	u.Error("failed: %v", "boom")

	for _, want := range []string{"a b\n", "1-x\n", "\x1b[31mred text\x1b[0m\n", "\x1b[33myellow text\x1b[0m\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(out.String(), "boom") {
		t.Errorf("errors must not go to stdout, got %q", out)
	}
	if want := "\x1b[31mError: failed: boom\x1b[0m\n"; errOut.String() != want {
		t.Errorf("stderr = %q, want %q", errOut, want)
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text unchanged", "Plan: 1 to add, 0 to change.\n\t+ resource", "Plan: 1 to add, 0 to change.\n\t+ resource"},
		{"umlauts unchanged", "Größe: 3 ✓", "Größe: 3 ✓"},
		{"color codes removed", "\x1b[31m- destroy\x1b[0m", "- destroy"},
		{"erase line removed", "keep\x1b[2K\x1b[1Atext", "keeptext"},
		{"cursor movement removed", "a\x1b[10;5Hb", "ab"},
		{"osc title removed (bel)", "x\x1b]0;owned\x07y", "xy"},
		{"osc title removed (st)", "x\x1b]0;owned\x1b\\y", "xy"},
		{"carriage return cannot overwrite a line", "+ create\r- destroy", "+ create- destroy"},
		{"crlf becomes lf", "a\r\nb", "a\nb"},
		{"bare escape removed", "a\x1bb", "ab"},
		{"c0 controls removed", "a\x00\x07\x08\x7fb", "ab"},
		{"c1 controls removed", "a\u009b2Jb", "a2Jb"},
		{"bidi overrides removed", "safe\u202Etxt.exe\u202C", "safetxt.exe"},
		{"bidi isolates removed", "a\u2066b\u2069", "ab"},
		{"lrm and rlm removed", "a\u200Eb\u200Fc", "abc"},
		{"arabic letter mark removed", "a\u061Cb", "ab"},
		{"zero width chars removed", "a\u200Bb\u200Cc\u200Dd", "abcd"},
		{"word joiner removed", "a\u2060b", "ab"},
		{"bom removed", "\uFEFFa\uFEFFb", "ab"},
		{"line separator removed", "a\u2028b", "ab"},
		{"paragraph separator removed", "a\u2029b", "ab"},
		{"tag characters removed", "a\U000E0041\U000E0042\U000E007Fb", "ab"},
		{"soft hyphen removed", "a\u00ADb", "ab"},
		{"hidden char cannot disguise a name", "pro\u200Bd-db", "prod-db"},
		{"zwj in emoji sequence dropped", "\U0001F468\u200D\U0001F469", "\U0001F468\U0001F469"},
		{"non-ascii letters unchanged", "Ünïcödé ß 日本語 Привет العربية", "Ünïcödé ß 日本語 Привет العربية"},
		{"newlines and tabs kept next to format chars", "a\u200B\n\t\u200Eb", "a\n\tb"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sanitize(tt.in); got != tt.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
