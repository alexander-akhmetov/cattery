package setup

import (
	"fmt"
	"strings"
)

// The managed block's fences. Everything between them belongs to setup and is
// replaced on every run; everything outside them is the user's. A marked block
// is used instead of rewriting kitty.conf because it stays visible, survives a
// re-run without duplicating itself, and leaves the rest of the file alone.
const (
	blockStart = "# >>> cattery >>>"
	blockEnd   = "# <<< cattery <<<"
)

// renderBlock builds the managed kitty.conf block. The picker is launched by
// absolute path rather than through a shell script, so nothing outside the
// binary has to be installed.
func renderBlock(binary string) string {
	return strings.Join([]string{
		blockStart,
		"# Managed by `cattery setup`: this block is replaced on every run.",
		"# Put your own settings outside it.",
		"watcher cattery_watcher.py",
		"allow_remote_control yes",
		"listen_on unix:/tmp/kitty-{kitty_pid}",
		"tab_bar_style powerline",
		`tab_title_template "{custom}"`,
		"map opt+a>opt+a launch --type=overlay --cwd=current --copy-colors " + shellQuote(binary),
		blockEnd,
	}, "\n") + "\n"
}

// mergeBlock puts block into conf: replacing the existing managed block when
// there is one, appending it otherwise. Every byte outside the two markers is
// left alone.
//
// Markers that do not pair up are reported instead of guessed at. Repairing
// them means deciding which half of the file the user meant to keep, and
// appending a second block would leave kitty applying two of them.
func mergeBlock(conf, block string) (string, error) {
	lines := strings.Split(conf, "\n")
	var starts, ends []int
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case blockStart:
			starts = append(starts, i)
		case blockEnd:
			ends = append(ends, i)
		}
	}

	switch {
	case len(starts) == 0 && len(ends) == 0:
		return appendBlock(conf, block), nil
	case len(starts) == 1 && len(ends) == 1 && starts[0] < ends[0]:
		// The block carries its own trailing newline, and the split already ate
		// the one that ended the marker line, so head and tail go back on either
		// side untouched. Trimming a newline off the tail here would eat a blank
		// line the user left below the block, one per run.
		head := strings.Join(lines[:starts[0]], "\n")
		tail := strings.Join(lines[ends[0]+1:], "\n")
		if head != "" {
			head += "\n"
		}
		return head + block + tail, nil
	default:
		return "", fmt.Errorf("kitty.conf has %d %q and %d %q lines; it needs one of each, in that order",
			len(starts), blockStart, len(ends), blockEnd)
	}
}

// appendBlock puts the block at the end of the file, separated from whatever
// came before by exactly one blank line.
func appendBlock(conf, block string) string {
	if strings.TrimSpace(conf) == "" {
		return block
	}
	return strings.TrimRight(conf, "\n") + "\n\n" + block
}

// shellQuote makes a path safe to embed in a command line. kitty's `launch` and
// Claude's hook command both split their string shell-style, so a path holding
// a space or a quote has to be quoted.
func shellQuote(s string) string {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:@+,="
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !strings.ContainsRune(safe, r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
