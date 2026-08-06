# cattery

Coding agents management overlay for [Kitty](https://sw.kovidgoyal.net/kitty/).

Coding agents run for minutes in windows you are not watching. Cattery marks
each tab with what its agent is doing, notifies you when one needs an answer,
and lists them all in a picker.

## Install

With Homebrew:

```bash
brew tap alexander-akhmetov/cattery https://github.com/alexander-akhmetov/cattery
brew install cattery
cattery setup
```

Or with Go:

```bash
go install github.com/alexander-akhmetov/cattery/cmd/cattery@latest
cattery setup
```

Reload kitty afterwards. Press `opt+a` twice to open the picker.

`cattery setup` installs copies of the kitty files, and an upgrade of the binary
does not update those copies. Run it again after every upgrade.

`cattery -version` prints the release the binary was built from.

## Tab markers

| Display state | Marker | Meaning |
|---|---:|---|
| `blocked` | red `◆` | the agent needs input |
| `done` | green `●` | the agent finished while you were elsewhere |
| `working` | yellow `●` | the agent is running |
| `idle` | none | nothing to report |

A `working` or `blocked` tab also shows elapsed minutes. An OS window holding a
`blocked` or `done` agent gets a `(N need you)` title prefix, which the Dock and
the ⌘-Tab switcher pick up. A notification fires on entry to `blocked` or
`done`, unless that window has focus.

No agent writes `done`. The watcher derives it when the agent goes idle after
working and you have not focused the window since. Focusing the window marks it
seen and drops the marker.

## Overlay

`opt+a` `opt+a` opens the management overlay. It lists all running agents, grouped 
by git repository.
