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

## tmux agents

An agent in a tmux pane publishes the same states as one in a kitty window, as
pane options: `@AGENT_STATE`, `@AGENT_KIND`, `@AGENT_MSG`. `cattery state` and
the pi extension write there whenever `$TMUX` and `$TMUX_PANE` are set, which
covers a pane nobody is attached to. To see what an agent published:

```bash
tmux list-panes -a -F '#{pane_id} #{@AGENT_STATE} #{@AGENT_KIND}'
```

The picker lists those panes beside the kitty agents, in the same repository
groups, marked with a `tmux` chip. There is no tab marker and no notification
for them: nothing pushes tmux events, and a pane has no kitty tab to mark.

Enter on a tmux agent opens a kitty tab showing that pane **read-only**: keys do
nothing, and the viewer's terminal size does not resize the agent's pane.
`prefix d` detaches and closes the tab. A second Enter on the same agent focuses
the tab already showing it.

The view is its own tmux session, grouped with the agent's, so two viewers never
fight over which window the shared session shows. `cattery attach
<session>:<window>.<pane id>` is the same thing from a shell, for example
`cattery attach kontora:3.%17`.

Attaching drops the `done` marker, because the attach marks the pane seen. A
plain `tmux attach` does not, so an agent watched that way keeps its marker
until it works again.

Snapshots stay kitty-only. `cattery save` records kitty tabs, and a tmux agent
belongs to whatever started it.
