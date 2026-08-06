// Package cattery embeds the kitty files that `cattery setup` copies into the
// kitty config directory, so a `go install` carries everything an install
// needs.
//
// The package sits at the module root because go:embed cannot reach outside its
// own package directory, and the Python files stay in kitty/ where the Python
// tests read them.
package cattery

import (
	"embed"
	"io/fs"
)

//go:embed kitty/cattery_watcher.py kitty/cattery_tab.py kitty/cattery_jump.py kitty/tab_bar.py
var embedded embed.FS

// ManagedFiles are the kitty files setup owns. It overwrites them on every run,
// and the picker warns when an installed copy no longer matches the binary.
var ManagedFiles = []string{"cattery_watcher.py", "cattery_tab.py", "cattery_jump.py"}

// TabBarFile is kitty's tab title renderer. Setup writes it only when the kitty
// config directory has none: an existing one belongs to the user.
const TabBarFile = "tab_bar.py"

// KittyFiles is the embedded kitty directory, rooted so entries are named
// "cattery_tab.py" instead of "kitty/cattery_tab.py".
func KittyFiles() fs.FS {
	sub, err := fs.Sub(embedded, "kitty")
	if err != nil {
		// The embed pattern is a compile-time constant, so "kitty" is always a
		// directory in the embedded tree.
		panic(err)
	}
	return sub
}
