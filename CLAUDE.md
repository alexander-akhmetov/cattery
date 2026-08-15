# cattery

Agent state in the kitty tab bar, plus a picker for jumping between agents.

## Build & test

```bash
make build    # go build -o cattery ./cmd/cattery
make test     # Go, Python, and TypeScript tests
make lint     # golangci-lint, govulncheck, deadcode, tsc
```

`make test-python` needs no kitty. The tests stub `kitty.boss`, `kitty.window`,
and `kitty.fast_data_types`, then load the module by path.
`tests/cattery_tab_bar_test.py` copies `kitty/tab_bar.py` into a temporary
directory twice, once with `cattery_tab.py` beside it and once without, because
that is what decides whether the guarded import succeeds.

`make test-ts` and `make lint-ts` install `node_modules` first. Only
`extensions/cattery.ts` needs that toolchain; no installed file needs node.

## Layout

`kitty/` holds the four files kitty loads directly. `extensions/cattery.ts` is
the pi-side state writer. `cmd/` and `internal/` build the binary: the picker,
`cattery setup`, `cattery state <x>` (the Claude-side writer),
`cattery save`/`cattery restore` (the tab-tree snapshot), and `cattery attach`
(a read-only view of a tmux agent).

An agent runs in one of two hosts. `internal/agent` holds what both share: the
`Agent` struct, its `Key()`, and the git grouping. `internal/kitty` and
`internal/tmux` each list their own host, `internal/agents` merges them for the
picker, and `internal/state` picks the transport the state writer publishes on.

`cattery setup` installs copies of the `kitty/` files, so an install does not
depend on where the source lives. A copy does not follow a binary upgrade, so
the picker compares the installed files with the embedded ones and warns.

## Things that bite

- The user variables are named `AGENT_*`. They are a live contract with running
  pi and Claude sessions, so renaming them breaks sessions mid-flight.
- kitty loads `tab_bar.py` with `runpy.run_path`, which does not extend
  `sys.path`. `tab_bar.py` must insert its own directory before importing
  `cattery_tab`, and must guard that import.
- `cattery state clear` must return before it reads stdin. The fish wrappers
  call it with whatever stdin they inherited, which can be a pipe that never
  closes.
- `go:embed` cannot reach outside its own package directory, so `assets.go` sits
  at the module root. The Python files stay in `kitty/`, where the Python tests
  read them.
- `save_as_session` records the picker's own overlay window, and `--match`
  cannot exclude it. `internal/session` drops `launch` lines whose `--type`
  starts with `overlay`. kitty writes `overlay-main` too, so the check is a
  prefix test.
- Never pass `--use-foreground-process` to `save_as_session`. It records the
  process the window is running, which is the sandbox's log reader for a
  sandboxed agent and the last command the user ran for a plain shell. Restore
  would start that again.
- `goto_session` is not idempotent: a second run on one file builds a second
  copy of every tab. `Restore` refuses when a window already reports the
  snapshot's `session_name`.
- kitty names those windows after the file's basename minus `.kitty-session`,
  `.session`, or `.kitty_session`, and no other suffix
  (`SESSION_FILE_EXTENSIONS` in kitty's source). `filepath.Ext` computes a
  different name for any other suffix, and then the typing pass and the
  duplicate guard both fail silently.
- Snapshot paths must be absolute before kitty sees them. `save_as_session`
  resolves a relative path against the kitty process's working directory and
  `goto_session` against the kitty configuration directory, so one string names
  three files. `session.Abs` handles it.
- A kitty action and its arguments go to `kitten @ action` as one string.
  Separate argv entries make kitty open a window titled "Invalid <action>
  command line" instead of reporting an error.
- `kitten @ send-text` reads Python escapes out of a positional text argument,
  which mangles the shell quoting in `AGENT_RESUME`: `'\''` collapses to `'''`,
  and a `\n` runs the command. Send the text on `--stdin`, which kitty documents
  as sent as is.
- `kitten @ ls` reports `session_name` on the window. Its tab dictionaries carry
  no such key.
- A tmux server inherits the environment of whatever started it, so a detached
  pane can carry a `KITTY_WINDOW_ID` belonging to an unrelated window. Both
  writers check `$TMUX` and `$TMUX_PANE` first. Otherwise the state goes to that
  window and the pane stays blank.
- Grouped sessions share their windows, so `tmux list-panes -a` reports one pane
  once per session. `internal/tmux` deduplicates by `pane_id`, keeping the row
  whose session is not a `cattery-view-` viewer. Keeping the other row lists
  every watched agent twice, under a target that dies with the viewer.
- `tmux attach -r` is `read-only,ignore-size`. Dropping `ignore-size` lets the
  viewer's terminal resize the agent's live pane.
- `cattery attach` runs tmux as a child process, never `syscall.Exec`. The
  grouped view session has to be killed after the detach, and an exec'd process
  has nothing left to do it. That cleanup only runs if the process survives the
  signal that ends the attach. kitty sends SIGHUP when the viewer tab closes,
  and Go terminates on SIGHUP by default, so `Attach` takes SIGHUP, SIGINT, and
  SIGTERM through `signal.NotifyContext`.
- An agent's target is `<session>:<window index>.<pane id>`, not
  `<session>:<window index>`. A window target resolves to whichever pane is
  active, so `@AGENT_SEEN` would go to the other agent in a split window, and
  the picker would match a viewer tab showing a different pane. tmux allows both
  ":" and "." in a session name and reads its own targets from the end;
  `splitTarget` does the same.
- The picker and `cattery attach` run as the command of a kitty window, with no
  shell in between, so they get kitty's own PATH. A kitty started from the Dock
  has launchd's (`/usr/bin:/bin:/usr/sbin:/sbin`), which has no Homebrew.
  `internal/kitty` and `internal/tmux` both fall back to known install prefixes
  when the lookup fails. Without that, `noServer` reads the missing binary as
  "no tmux on this machine" and every tmux agent leaves the picker silently.
- The preview sidebar draws text the picker did not write, inside its own
  frame. `internal/overlay/preview.go` passes SGR through and drops every other
  escape and control byte. One cursor movement or one OSC would corrupt the
  whole picker, not one column, and nothing downstream could undo it.
- Every preview line carries its own reset on both sides. Bubble Tea repaints
  individual lines, so a line can reach the terminal without the one above it.
  kitty happens to begin each captured line with a reset and tmux does not.
- `kitten @ get-text --extent screen` returns the *visible* screen, which for an
  agent in the alternate screen is its TUI frame. That is the whole reason the
  preview shows anything useful. `--extent all` would return the main screen and
  its scrollback instead, which for those agents is the shell they started in.
- tmux ends a command at any argument that ends in ";", not only at one that is
  nothing else. `@AGENT_MSG` carries raw prompt text, so both writers escape a
  trailing ";" as `\;`. Without that escape a prompt ending in ";" loses that
  character, and a prompt that is only ";" drops every update chained behind it.

@README.md
