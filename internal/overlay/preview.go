package overlay

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// reset ends every preview line and starts it again. Bubble Tea repaints
// individual lines, so a line can reach the terminal without the one above it,
// and tmux does not begin a captured line with an SGR reset the way kitty does.
// Self-contained lines are what keep an agent's colours out of the picker's
// own chrome.
const reset = "\x1b[0m"

// previewLines fits a captured screen into exactly n lines of exactly w cells.
//
// It keeps the last lines rather than the first: the bottom of an agent's
// screen is where the prompt and the activity are. Long lines are cut on the
// right, so a screen written for a wide terminal shows its leftmost w columns.
func previewLines(screen string, w, n int) []string {
	if w < 1 || n < 1 {
		return nil
	}

	raw := trimBlank(strings.Split(strings.ReplaceAll(screen, "\r\n", "\n"), "\n"))
	if len(raw) > n {
		raw = raw[len(raw)-n:]
	}

	out := make([]string, 0, n)
	for _, line := range raw {
		out = append(out, fitLine(line, w))
	}
	for len(out) < n {
		out = append(out, strings.Repeat(" ", w))
	}
	return out
}

// fitLine renders one captured line as exactly w cells. The trailing reset
// comes before the padding, so the spaces that fill the column carry no
// background from the agent's screen.
func fitLine(line string, w int) string {
	line = sanitize(line)
	line = ansi.Truncate(line, w, "")
	pad := max(w-ansi.StringWidth(line), 0)
	return reset + line + reset + strings.Repeat(" ", pad)
}

// trimBlank drops leading and trailing lines with nothing on them, so a mostly
// empty screen is not previewed as a column of blanks.
func trimBlank(lines []string) []string {
	start, end := 0, len(lines)
	for start < end && blank(lines[start]) {
		start++
	}
	for end > start && blank(lines[end-1]) {
		end--
	}
	return lines[start:end]
}

func blank(line string) bool {
	return strings.TrimSpace(ansi.Strip(line)) == ""
}

// sanitize keeps SGR sequences and drops every other escape and control byte.
//
// Both hosts emit SGR alone today, but this text is a pane's contents rendered
// inside the picker's own frame. One cursor movement, one erase, or one OSC
// would corrupt the whole picker rather than one column, and there is no way to
// undo that from here. A whitelist makes the class of problem impossible
// instead of unlikely.
func sanitize(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		c := line[i]
		if c != 0x1b {
			// Tabs and other C0 bytes move the cursor too. The hosts render a
			// screen, so there should be none left, and a stray one would
			// misalign the column.
			if c < 0x20 || c == 0x7f {
				i++
				continue
			}
			b.WriteByte(c)
			i++
			continue
		}
		seq, width, ok := sgr(line[i:])
		if !ok {
			// Not an SGR sequence: skip the escape and whatever it introduces.
			i += width
			continue
		}
		b.WriteString(seq)
		i += width
	}
	return b.String()
}

// sgr matches one escape sequence at the start of s. It returns the sequence
// and true when that sequence is SGR ("ESC [ ... m"), and the number of bytes
// to skip and false for anything else, including a truncated sequence at the
// end of the input.
func sgr(s string) (seq string, width int, ok bool) {
	if len(s) < 2 {
		return "", len(s), false
	}
	if s[1] != '[' {
		// A two-byte escape (ESC M and friends), or the start of an OSC or
		// another string. Skip to its terminator so its payload is not
		// mistaken for text.
		return "", skipOther(s), false
	}
	for i := 2; i < len(s); i++ {
		c := s[i]
		// Parameter and intermediate bytes of a CSI sequence.
		if c >= 0x20 && c <= 0x3f {
			continue
		}
		if c == 'm' {
			return s[:i+1], i + 1, true
		}
		// Any other final byte: a real CSI, but not one we pass through.
		return "", i + 1, false
	}
	return "", len(s), false
}

// skipOther consumes an escape sequence that is not CSI. OSC and the other
// string sequences run until BEL or ST; everything else is two bytes.
func skipOther(s string) int {
	switch s[1] {
	case ']', 'P', 'X', '^', '_':
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	default:
		return 2
	}
}
