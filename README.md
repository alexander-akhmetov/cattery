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

`v` opens a drawer beside the list, showing the screen of the agent under the
cursor. It works on a kitty window you are not looking at and on a tmux pane
nobody is attached to.

The drawer opens **read-only**, framed in grey: the keys still belong to the
picker, so you can move down the list and watch each agent in turn.

A second `v` switches it to **read-write**, framed in red and marked `R/W` in
the heading. From then on every key you press goes to the agent instead of to
the picker, so you can answer a blocked agent without leaving the list. That
includes `q`, `enter` and `ctrl+c`, which interrupts the agent rather than
closing the picker. The cursor cannot move while you type.

`esc` walks back out one step at a time:

| From | `esc` does |
|---|---|
| read-write (red frame) | gives the keyboard back to the picker |
| read-only (grey frame) | closes the drawer |
| no drawer | closes the picker |

`q` and `ctrl+c` still close the picker outright from anywhere except
read-write.

`esc` is the way out, so it is the one key the agent cannot be sent, and Claude
and vim both want it. **`ctrl+]` sends a literal escape** and stays in
read-write. The drawer's heading says so while you are typing.

Read-only changes nothing, and does not mark the agent seen: a `done`
marker survives being watched. The first key you send does mark it seen, the
same way jumping to it does, because answering an agent is looking at it.

The drawer refreshes every second in read-only and four times a second in
read-write, plus once right after each keystroke.

The drawer shows the leftmost columns of a screen written for a wider terminal,
so a boxed frame is cut off on the right, and it draws no cursor: you see that
the frame changed, not where your text landed. It needs about 91 columns to open
at all, and says so instead of opening when the terminal is narrower. Neither
host reports a keystroke that went nowhere, so a window that closes under the
drawer is noticed by the next reload rather than by the send itself.

Marking a kitty agent seen goes through the watcher, so that half only works
once `cattery setup` has installed this version's copy of it.

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

The viewer tab is read-only, but the picker's drawer in read-write is not: `v`
`v` types at a tmux pane the same way it types at a kitty window. So you can
watch a pane in a viewer tab and answer it from the drawer at the same time.
This needs a tmux with `send-keys -H`, which is tmux 3.1 and later.

Attaching drops the `done` marker, because the attach marks the pane seen. A
plain `tmux attach` does not, so an agent watched that way keeps its marker
until it works again.

Snapshots stay kitty-only. `cattery save` records kitty tabs, and a tmux agent
belongs to whatever started it.
