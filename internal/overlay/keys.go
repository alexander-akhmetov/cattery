package overlay

import (
	tea "github.com/charmbracelet/bubbletea"
)

// This file is the mirror of preview.go. That one takes an agent's screen and
// makes it safe to draw inside the picker; this one takes a key the picker
// received and rebuilds the bytes the agent's terminal would have delivered.

// namedKeys are the keys with no byte of their own, in the encoding a terminal
// emits by default.
//
// The arrows and the navigation keys go out in CSI form ("\x1b[A"), not SS3
// ("\x1bOA"). A full-screen agent has usually enabled application cursor mode
// and would emit SS3 itself, but the picker never sees the agent's terminal,
// only a captured screen, so it cannot know. CSI is the form a terminal sends
// by default, which makes it the half of the pair every parser has to accept:
// bubbletea's own table reads both back as KeyUp.
var namedKeys = map[tea.KeyType]string{
	tea.KeyUp:    "\x1b[A",
	tea.KeyDown:  "\x1b[B",
	tea.KeyRight: "\x1b[C",
	tea.KeyLeft:  "\x1b[D",

	tea.KeyShiftUp:    "\x1b[1;2A",
	tea.KeyShiftDown:  "\x1b[1;2B",
	tea.KeyShiftRight: "\x1b[1;2C",
	tea.KeyShiftLeft:  "\x1b[1;2D",

	tea.KeyCtrlUp:    "\x1b[1;5A",
	tea.KeyCtrlDown:  "\x1b[1;5B",
	tea.KeyCtrlRight: "\x1b[1;5C",
	tea.KeyCtrlLeft:  "\x1b[1;5D",

	tea.KeyCtrlShiftUp:    "\x1b[1;6A",
	tea.KeyCtrlShiftDown:  "\x1b[1;6B",
	tea.KeyCtrlShiftRight: "\x1b[1;6C",
	tea.KeyCtrlShiftLeft:  "\x1b[1;6D",

	tea.KeyShiftTab: "\x1b[Z",

	tea.KeyHome:          "\x1b[H",
	tea.KeyEnd:           "\x1b[F",
	tea.KeyCtrlHome:      "\x1b[1;5H",
	tea.KeyCtrlEnd:       "\x1b[1;5F",
	tea.KeyShiftHome:     "\x1b[1;2H",
	tea.KeyShiftEnd:      "\x1b[1;2F",
	tea.KeyCtrlShiftHome: "\x1b[1;6H",
	tea.KeyCtrlShiftEnd:  "\x1b[1;6F",
	tea.KeyPgUp:          "\x1b[5~",
	tea.KeyPgDown:        "\x1b[6~",
	tea.KeyCtrlPgUp:      "\x1b[5;5~",
	tea.KeyCtrlPgDown:    "\x1b[6;5~",
	tea.KeyInsert:        "\x1b[2~",
	tea.KeyDelete:        "\x1b[3~",

	tea.KeyF1:  "\x1bOP",
	tea.KeyF2:  "\x1bOQ",
	tea.KeyF3:  "\x1bOR",
	tea.KeyF4:  "\x1bOS",
	tea.KeyF5:  "\x1b[15~",
	tea.KeyF6:  "\x1b[17~",
	tea.KeyF7:  "\x1b[18~",
	tea.KeyF8:  "\x1b[19~",
	tea.KeyF9:  "\x1b[20~",
	tea.KeyF10: "\x1b[21~",
	tea.KeyF11: "\x1b[23~",
	tea.KeyF12: "\x1b[24~",
}

// encodeKey turns one key press back into the bytes a terminal would have sent
// for it. The second result is false for a key with no encoding in the legacy
// scheme, which is dropped rather than guessed at.
func encodeKey(msg tea.KeyMsg) (string, bool) {
	base, ok := keyBytes(msg)
	if !ok {
		return "", false
	}
	// xterm's metaSendsEscape, which every agent's input parser reads back the
	// same way. It applies to a rune and to a sequence alike, so alt+left goes
	// out as ESC, then ESC [ D.
	if msg.Alt {
		return "\x1b" + base, true
	}
	return base, true
}

func keyBytes(msg tea.KeyMsg) (string, bool) {
	// Runes first, because bubbletea fills them in beside the type for space
	// and leaves them as the only content for ordinary text. A paste arrives
	// this way too, with every rune intact: the bracketed-paste wrapper is
	// deliberately not added, because it is correct only if the agent asked for
	// bracketed paste and prints a literal "200~" if it did not.
	if len(msg.Runes) > 0 {
		return string(msg.Runes), true
	}
	// The control keys carry their own byte as their type: bubbletea defines
	// KeyCtrlA as SOH and KeyBackspace as DEL. Computing the byte rather than
	// listing it keeps enter, tab, every ctrl+letter and their aliases exact,
	// and emits what the terminal itself delivered.
	if msg.Type >= 0 && msg.Type <= 0x1f {
		return string([]byte{byte(msg.Type)}), true
	}
	if msg.Type == 0x7f {
		return "\x7f", true
	}
	seq, ok := namedKeys[msg.Type]
	return seq, ok
}
