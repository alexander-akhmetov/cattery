package overlay

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestEncodeKey(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyMsg
		want string
	}{
		{"a letter", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}, "a"},
		{"a multibyte rune", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("界")}, "界"},
		{"several runes at once", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("界面")}, "界面"},
		{"space carries a rune beside its type", tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}, " "},
		{"a paste is its runes", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("run the tests"), Paste: true}, "run the tests"},

		{"enter", tea.KeyMsg{Type: tea.KeyEnter}, "\r"},
		{"tab", tea.KeyMsg{Type: tea.KeyTab}, "\t"},
		{"backspace", tea.KeyMsg{Type: tea.KeyBackspace}, "\x7f"},
		{"escape", tea.KeyMsg{Type: tea.KeyEsc}, "\x1b"},
		{"ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}, "\x03"},
		{"ctrl+d", tea.KeyMsg{Type: tea.KeyCtrlD}, "\x04"},
		{"ctrl+at is NUL", tea.KeyMsg{Type: tea.KeyCtrlAt}, "\x00"},
		{"ctrl+] is the escape hatch's own byte", tea.KeyMsg{Type: tea.KeyCtrlCloseBracket}, "\x1d"},

		{"up", tea.KeyMsg{Type: tea.KeyUp}, "\x1b[A"},
		{"down", tea.KeyMsg{Type: tea.KeyDown}, "\x1b[B"},
		{"right", tea.KeyMsg{Type: tea.KeyRight}, "\x1b[C"},
		{"left", tea.KeyMsg{Type: tea.KeyLeft}, "\x1b[D"},
		{"shift+tab", tea.KeyMsg{Type: tea.KeyShiftTab}, "\x1b[Z"},
		{"home", tea.KeyMsg{Type: tea.KeyHome}, "\x1b[H"},
		{"end", tea.KeyMsg{Type: tea.KeyEnd}, "\x1b[F"},
		{"pgup", tea.KeyMsg{Type: tea.KeyPgUp}, "\x1b[5~"},
		{"delete", tea.KeyMsg{Type: tea.KeyDelete}, "\x1b[3~"},
		{"ctrl+left", tea.KeyMsg{Type: tea.KeyCtrlLeft}, "\x1b[1;5D"},
		{"f1", tea.KeyMsg{Type: tea.KeyF1}, "\x1bOP"},
		{"f5", tea.KeyMsg{Type: tea.KeyF5}, "\x1b[15~"},

		{"alt+a", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a"), Alt: true}, "\x1ba"},
		{"alt+backspace", tea.KeyMsg{Type: tea.KeyBackspace, Alt: true}, "\x1b\x7f"},
		{"alt+left prefixes the whole sequence", tea.KeyMsg{Type: tea.KeyLeft, Alt: true}, "\x1b\x1b[D"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := encodeKey(tc.key)
			if !ok {
				t.Fatalf("%s has no encoding", tc.key)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// A control key's bubbletea type is the byte the terminal sent, so the encoder
// computes it rather than listing thirty-two constants. If that ever stops
// being true, every ctrl+letter silently turns into the wrong byte.
func TestEncodeKeyControlBytes(t *testing.T) {
	for b := range 0x20 {
		key := tea.KeyMsg{Type: tea.KeyType(b)}
		got, ok := encodeKey(key)
		if !ok {
			t.Fatalf("byte %#x has no encoding", b)
		}
		if want := string([]byte{byte(b)}); got != want {
			t.Fatalf("byte %#x: got %q, want %q", b, got, want)
		}
	}
}

// A key with no encoding in the legacy scheme is dropped. Guessing at one sends
// the agent bytes no terminal would have produced.
func TestEncodeKeyRefusesWhatItCannotSend(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyF20},
		{Type: tea.KeyRunes},
	} {
		if got, ok := encodeKey(key); ok {
			t.Fatalf("%v encoded as %q, want a refusal", key.Type, got)
		}
	}
}
