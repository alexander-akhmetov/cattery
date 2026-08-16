package setup

import (
	"fmt"
	"strings"
)

// The managed block's fences. Setup owns everything between them and replaces
// it on every run. Everything outside them belongs to the user. A marked block
// stays visible, survives a re-run without duplicating itself, and leaves the
// rest of kitty.conf alone.
const (
	blockStart = "# >>> cattery >>>"
	blockEnd   = "# <<< cattery <<<"
)

// envBinary starts the line naming the picker binary for the installed watcher.
const envBinary = "env CATTERY_BIN="

// renderBlock builds the managed kitty.conf block. It launches the picker by
// absolute path instead of through a shell script, so an install needs nothing
// outside the binary.
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
		// How the installed watcher finds the picker: it is a static file with
		// no way to know where the binary is. Unquoted, unlike the `map` line
		// above, because kitty's `env` takes the rest of the line as the value,
		// quotes included, while `launch` splits shell-style. It does expand
		// $VAR in that value, so a path holding a literal `$` arrives rewritten.
		// This also exports CATTERY_BIN into every shell kitty starts.
		envBinary + binary,
		blockEnd,
	}, "\n") + "\n"
}

// blockStale reports whether the managed block in conf has fallen behind what
// renderBlock writes now. The installed files are only half the install: a
// release that changes only renderBlock leaves every one of them identical,
// while the installed watcher silently loses whatever the new line was meant to
// give it, which today is the picker's path.
//
// The comparison re-renders with the binary the block itself names, because
// that is the one part of the block that belongs to the install and not to the
// release. A conf with no managed block is left alone: setup reports that case
// while it runs, and there is nothing the picker could add.
func blockStale(conf string) bool {
	block, ok := extractBlock(conf)
	if !ok {
		return false
	}
	return block != renderBlock(blockBinary(block))
}

// extractBlock returns the managed block, exactly as renderBlock would have
// written it. Markers that do not pair up are the case mergeBlock refuses, so
// there is no block to compare.
func extractBlock(conf string) (string, bool) {
	lines := strings.Split(conf, "\n")
	start, end := -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case blockStart:
			if start >= 0 {
				return "", false
			}
			start = i
		case blockEnd:
			if end >= 0 {
				return "", false
			}
			end = i
		}
	}
	if start < 0 || end < start {
		return "", false
	}
	return strings.Join(lines[start:end+1], "\n") + "\n", true
}

// blockBinary is the picker path a managed block names. A block written before
// the env line has none, and renders as a block for the empty path, which
// matches nothing and is therefore stale.
func blockBinary(block string) string {
	for line := range strings.SplitSeq(block, "\n") {
		if path, ok := strings.CutPrefix(line, envBinary); ok {
			return path
		}
	}
	return ""
}

// mergeBlock puts block into conf. It replaces an existing managed block, or
// appends when there is none. Every byte outside the two markers stays.
//
// Markers that do not pair up are reported instead of guessed at. Repairing
// them means deciding which half of the file the user meant to keep, and
// appending a second block would leave kitty applying two.
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
		// the one ending the marker line, so head and tail go back untouched.
		// Trimming a newline off the tail would eat one blank line the user left
		// below the block, on every run.
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

// shellQuote makes a path safe to embed in a command line. kitty's `launch`
// splits its command shell-style, so a path with a space or a quote needs
// quoting. Nothing else setup writes does: `env` on the line below takes the
// rest of the line as the value, and the hook manifests name the binary by its
// bare name.
func shellQuote(s string) string {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:@+,="
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !strings.ContainsRune(safe, r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
