package overlay

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// Every preview line is exactly as wide as its column, whatever the screen
// carried. The sidebar sits inside the picker's own frame: a line one cell too
// wide drags the rule out of place on that line alone, and a line one cell too
// narrow leaves a hole the list shows through.
func TestPreviewLinesGeometry(t *testing.T) {
	cases := []struct {
		name   string
		screen string
	}{
		{name: "plain text", screen: "one\ntwo\nthree"},
		{name: "colour", screen: "\x1b[38;5;39mbuilding\x1b[39m\n\x1b[1mdone\x1b[0m"},
		{name: "longer than the column", screen: strings.Repeat("x", 200)},
		{name: "coloured and longer than the column", screen: "\x1b[31m" + strings.Repeat("x", 200)},
		{name: "shorter than the column", screen: "hi"},
		{name: "fewer lines than the column", screen: "only one"},
		{name: "more lines than the column", screen: strings.Repeat("line\n", 60)},
		{name: "wide runes", screen: "界界界界界界界界界界界界界界界界界界界界界界界界界界"},
		{name: "a wide rune straddling the cut", screen: strings.Repeat("界", 13) + "x"},
		{name: "empty", screen: ""},
		{name: "blank lines only", screen: "\n\n\n"},
		{name: "trailing blank lines", screen: "content\n\n\n\n\n"},
		{name: "combining marks", screen: "ééé"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, w := range []int{1, 2, 25, 40, 80} {
				for _, n := range []int{1, 3, 12} {
					lines := previewLines(tc.screen, w, n)
					if len(lines) != n {
						t.Fatalf("%dx%d: got %d lines, want %d", w, n, len(lines), n)
					}
					for i, line := range lines {
						if got := ansi.StringWidth(line); got != w {
							t.Errorf("%dx%d line %d: %d cells, want %d: %q", w, n, i, got, w, line)
						}
					}
				}
			}
		})
	}

	t.Run("a column with no room renders nothing", func(t *testing.T) {
		for _, size := range [][2]int{{0, 5}, {5, 0}, {-1, 5}, {5, -1}} {
			if lines := previewLines("hello", size[0], size[1]); lines != nil {
				t.Errorf("%v: got %q, want nil", size, lines)
			}
		}
	})
}

// The bottom of an agent's screen is where the prompt and the activity are, so
// a screen taller than the column keeps its last lines, not its first.
func TestPreviewLinesKeepsTheBottom(t *testing.T) {
	lines := previewLines("first\nsecond\nthird\nfourth", 10, 2)
	for i, want := range []string{"third", "fourth"} {
		if !strings.Contains(ansi.Strip(lines[i]), want) {
			t.Errorf("line %d: got %q, want it to hold %q", i, ansi.Strip(lines[i]), want)
		}
	}
}

// Trailing blanks are what an agent leaves under a short frame. Keeping them
// would preview a working agent as an empty column.
func TestPreviewLinesDropsSurroundingBlanks(t *testing.T) {
	lines := previewLines("\n\n  \ncontent\n\n\n", 12, 3)
	if !strings.Contains(ansi.Strip(lines[0]), "content") {
		t.Fatalf("first line: %q", ansi.Strip(lines[0]))
	}
}

// The whitelist is the point of sanitize. This text is a pane's contents drawn
// inside the picker's frame, so one cursor move, one erase, or one OSC would
// corrupt the whole picker rather than one column, and nothing here could undo
// it.
func TestSanitizeKeepsOnlyColour(t *testing.T) {
	cases := []struct {
		name  string
		line  string
		want  string
		keeps string // a colour sequence that has to survive
	}{
		{name: "plain", line: "hello", want: "hello"},
		{name: "sgr survives", line: "\x1b[31mred\x1b[0m", want: "\x1b[31mred\x1b[0m", keeps: "\x1b[31m"},
		{name: "truecolor sgr survives", line: "\x1b[38:2:1:2:3mx\x1b[39m", want: "\x1b[38:2:1:2:3mx\x1b[39m"},
		{name: "cursor movement goes", line: "a\x1b[2Ab", want: "ab"},
		{name: "erase goes", line: "a\x1b[2Kb", want: "ab"},
		{name: "cursor position goes", line: "a\x1b[10;20Hb", want: "ab"},
		{name: "osc title goes, bell and all", line: "a\x1b]0;pwned\x07b", want: "ab"},
		{name: "osc terminated by ST goes", line: "a\x1b]8;;https://x\x1b\\link", want: "alink"},
		{name: "dcs goes", line: "a\x1bPq#0\x1b\\b", want: "ab"},
		{name: "two-byte escape goes", line: "a\x1bMb", want: "ab"},
		{name: "a bare escape at the end goes", line: "ab\x1b", want: "ab"},
		{name: "a truncated csi goes", line: "ab\x1b[38;5", want: "ab"},
		{name: "control bytes go", line: "a\tb\x00c\x7fd", want: "abcd"},
		{name: "a lone carriage return goes", line: "abc\rxyz", want: "abcxyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitize(tc.line)
			if got != tc.want {
				t.Fatalf("sanitize: got %q, want %q", got, tc.want)
			}
			if tc.keeps != "" && !strings.Contains(got, tc.keeps) {
				t.Fatalf("colour dropped: %q does not hold %q", got, tc.keeps)
			}
		})
	}
}

// The same guarantee, through the function the sidebar actually calls: whatever
// the screen carried, what reaches the terminal is text and SGR.
func TestPreviewLinesEmitOnlyColour(t *testing.T) {
	hostile := strings.Join([]string{
		"\x1b[2J\x1b[H wiped the screen",
		"\x1b]0;a new window title\x07",
		"\x1b]8;;https://example.com\x1b\\a hyperlink\x1b]8;;\x1b\\",
		"\x1b[?1049h alternate screen",
		"a\tb\x00c",
		"\x1b[31mlegitimate colour\x1b[0m",
	}, "\n")

	for _, line := range previewLines(hostile, 40, 8) {
		for _, seq := range escapes(line) {
			if !strings.HasSuffix(seq, "m") || !strings.HasPrefix(seq, "\x1b[") {
				t.Errorf("non-SGR escape reached the terminal: %q in %q", seq, line)
			}
		}
		if strings.ContainsAny(ansi.Strip(line), "\x00\x07\t\r") {
			t.Errorf("control byte reached the terminal: %q", line)
		}
	}
}

// Bubble Tea repaints individual lines, so a preview line can reach the
// terminal without the one above it. kitty happens to start each captured line
// with a reset and tmux does not, so every line carries its own on both sides.
func TestPreviewLinesAreSelfContained(t *testing.T) {
	for _, line := range previewLines("\x1b[41mred background\nsecond", 30, 2) {
		if !strings.HasPrefix(line, reset) {
			t.Errorf("line does not open with a reset: %q", line)
		}
		if !strings.Contains(line, reset+" ") && !strings.HasSuffix(line, reset) {
			t.Errorf("line does not close its styling before the padding: %q", line)
		}
	}
}

// escapes lists the escape sequences in s, each as the bytes from ESC to its
// final byte inclusive.
func escapes(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			continue
		}
		end := i + 1
		if end < len(s) && s[end] == '[' {
			end++ // the CSI introducer, before the parameter bytes
		}
		for end < len(s) && s[end] >= 0x20 && s[end] <= 0x3f {
			end++
		}
		if end < len(s) {
			end++
		}
		out = append(out, s[i:end])
		i = end - 1
	}
	return out
}
