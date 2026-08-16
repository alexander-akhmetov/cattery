// Package cattery embeds the files that `cattery setup` copies into place: the
// kitty ones, and the opencode plugin. A `go install` then carries everything
// an install needs.
//
// The package sits at the module root because go:embed cannot reach outside its
// own package directory, and the Python files stay in kitty/ where the Python
// tests read them, the plugin in opencode/ where the TypeScript tests do.
package cattery

import (
	"embed"
	"io/fs"
)

//go:embed kitty/cattery_watcher.py kitty/cattery_tab.py kitty/cattery_jump.py kitty/cattery_events.py kitty/tab_bar.py
//go:embed opencode/cattery.ts
var embedded embed.FS

// EventsFile is the kitten that adds and removes event subscribers. `cattery
// events` runs it by path through kitty remote control, so the binary needs the
// name setup installed it under.
const EventsFile = "cattery_events.py"

// ManagedFiles are the kitty files setup owns. It overwrites them on every run,
// and the picker warns when an installed copy no longer matches the binary.
var ManagedFiles = []string{"cattery_watcher.py", "cattery_tab.py", "cattery_jump.py", EventsFile}

// TabBarFile is kitty's tab title renderer. Setup writes it only when the kitty
// config directory has none: an existing one belongs to the user.
const TabBarFile = "tab_bar.py"

// OpencodeFile is the opencode plugin, as it is named under the plugin
// directory opencode auto-loads. Unlike the pi extension and the Codex plugin,
// which both come from the published repository, this one ships inside the
// binary, so setup can install it and the picker can warn when it goes stale.
const OpencodeFile = "cattery.ts"

// OpencodePluginDir is the directory under opencode's configuration that it
// scans. It loads "{plugin,plugins}/*.{ts,js}"; setup writes into the singular
// one.
const OpencodePluginDir = "plugin"

// KittyFiles is the embedded kitty directory, rooted so entries are named
// "cattery_tab.py" instead of "kitty/cattery_tab.py".
func KittyFiles() fs.FS { return sub("kitty") }

// OpencodeFiles is the embedded opencode directory, rooted the same way.
func OpencodeFiles() fs.FS { return sub("opencode") }

func sub(dir string) fs.FS {
	out, err := fs.Sub(embedded, dir)
	if err != nil {
		// The embed patterns are compile-time constants, so both names are
		// always directories in the embedded tree.
		panic(err)
	}
	return out
}
