package shellquote

import (
	"os/exec"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	cases := []struct{ name, in, want string }{
		// The common inputs: a pi session path and a Claude session id. Both
		// come back untouched, as the AGENT_RESUME examples show.
		{name: "session path", in: "/tmp/pi-session.jsonl", want: "/tmp/pi-session.jsonl"},
		{name: "uuid", in: "abc-123", want: "abc-123"},
		{name: "every safe character", in: "@%+=:,./_-azAZ09", want: "@%+=:,./_-azAZ09"},

		{name: "empty", in: "", want: "''"},
		{name: "space", in: "/tmp/my sessions/a.jsonl", want: `'/tmp/my sessions/a.jsonl'`},
		{name: "single quote", in: "it's", want: `'it'\''s'`},
		{name: "only a single quote", in: "'", want: `''\'''`},
		{name: "dollar", in: "$HOME", want: `'$HOME'`},
		{name: "semicolon", in: "a;rm -rf /", want: `'a;rm -rf /'`},
		{name: "backslash", in: `a\b`, want: `'a\b'`},
		{name: "double quote", in: `a"b`, want: `'a"b'`},
		{name: "newline", in: "a\nb", want: "'a\nb'"},
		{name: "tilde, which the shell would expand", in: "~/x", want: `'~/x'`},
		{name: "non-ascii", in: "café", want: "'café'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Quote(tc.in); got != tc.want {
				t.Fatalf("Quote(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A quoted argument has to survive a real shell, because restore types
// AGENT_RESUME at the user's prompt.
func TestQuoteRoundTripsThroughSh(t *testing.T) {
	inputs := []string{
		"/tmp/pi-session.jsonl",
		"abc-123",
		"/tmp/my sessions/a.jsonl",
		"it's",
		"'",
		"$HOME",
		"a;rm -rf /",
		`a\b`,
		`a"b`,
		"~/x",
		"café",
		"*",
		"a b\tc",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			// printf %s writes the argument with nothing added, so whatever the
			// shell did to it on the way in shows in the output.
			out, err := exec.Command("sh", "-c", "printf %s "+Quote(in)).Output()
			if err != nil {
				t.Fatalf("sh rejected %s: %v", Quote(in), err)
			}
			if got := string(out); got != in {
				t.Fatalf("sh saw %q, want %q (quoted as %s)", got, in, Quote(in))
			}
		})
	}
}

// The empty string is the one input that cannot stay bare. Without quotes the
// shell drops the word, and the next argument shifts into its place.
func TestQuoteEmptyStaysOneArgument(t *testing.T) {
	out, err := exec.Command("sh", "-c", "printf '%s\n' "+Quote("")+" second").Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	if got := strings.Split(strings.TrimRight(string(out), "\n"), "\n"); len(got) != 2 || got[0] != "" || got[1] != "second" {
		t.Fatalf("sh saw %q, want an empty first argument then \"second\"", got)
	}
}
