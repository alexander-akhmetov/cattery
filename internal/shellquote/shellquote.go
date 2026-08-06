// Package shellquote quotes one argument for a POSIX shell.
//
// Two callers build a string that something else splits back into words. The
// agent-state writer appends a session path or id to AGENT_RESUME, which
// restore types at a shell prompt. The session package puts a snapshot path
// inside the single string kitty's `action` command takes.
package shellquote

import "strings"

// safe holds the characters that need no quoting anywhere in a POSIX shell.
// One character outside this set sends the whole argument through quotes.
const safe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789" + "@%+=:,./_-"

// Quote returns s ready to paste into a shell command line.
//
// A string that needs no quoting comes back unchanged, so a session path or a
// UUID stays readable. An empty string becomes a pair of quotes, because a bare
// empty word would vanish.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsFunc(s, needsQuote) {
		return s
	}
	// A single quote cannot appear inside single quotes, so close, escape it,
	// and reopen: the POSIX '\'' idiom.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func needsQuote(r rune) bool {
	return !strings.ContainsRune(safe, r)
}
