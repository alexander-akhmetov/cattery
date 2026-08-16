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

`kitty/` holds the five files kitty loads directly. `extensions/cattery.ts` is
the pi-side state writer. `cmd/` and `internal/` build the binary: the picker,
`cattery setup`, `cattery state <x>` (the Claude-side writer),
`cattery save`/`cattery restore` (the tab-tree snapshot), `cattery attach`
(a read-only view of a tmux agent), and `cattery events` (the transitions the
watcher pushes, as JSON lines).

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
- The read-write drawer types at an agent, which reverses what the rest of the
  tmux path is careful about: `cattery attach` is read-only on purpose.
  `send-keys` is a server-side command and is not constrained by that, so the
  drawer can type into a pane the user is watching in a read-only viewer tab.
- `tmux send-keys -H` reads each argument as one **byte**, not a code point. A
  value above 0xff is dropped without a word, and 0xe9 arrives as a raw byte
  rather than as UTF-8. `internal/tmux` encodes per byte, so anything non-ASCII
  goes out as the UTF-8 it already is. `-H` is also why no argument can end in
  ";" and why NUL is expressible at all.
- tmux from 3.3 to at least 3.7 resolves a target client for `send-keys` even
  when none was asked for, and refuses if that client is read-only. With no
  `$TMUX` to go on, as the picker has, it picks the most recently active client
  of the most recently active session, and `cattery attach` creates exactly such
  a client. So one open viewer tab makes every send fail, whatever session the
  target pane is in. An empty `-c` matches no client, and `send-keys` carries
  CMD_CLIENT_CANFAIL, so the lookup fails quietly and the command runs with no
  client. `retryUnclaimed` does that once and remembers the answer.
- Neither host reports a send that went nowhere. kitty documents `send-text` as
  always succeeding, even when its match found no window, and tmux delivers to a
  pane in copy mode without the program ever seeing it. So `Send` returning nil
  does not mean the agent received anything, and read-write is bound to an agent
  key that every reload rechecks.
- The watcher runs inside kitty's own Python, so it notifies through
  `boss.notification_manager` rather than an OSC 99 escape. The escape would
  need `a=report` for its buttons, and kitty writes that activation response
  into the channel that sent it, which here is the *agent's* stdin. The manager
  takes an `on_activation` callback instead, and calls it whatever `actions`
  holds. `actions = frozenset()` is what stops kitty focusing the agent window
  itself when the picker button is the thing that was pressed.
- `_activated` has to pass `cmd.activation_token` to `set_active_window`.
  kitty's own focus path is the only other thing that passes one, and
  `actions = frozenset()` turns it off. A Wayland compositor with
  focus-stealing prevention discards a raise that carries no token, so without
  it the click neither raises the window nor marks the agent seen. Empty on
  macOS and X11, which is why it is easy to miss.
- `cmd.urgency` does nothing on macOS. kitty's `schedule_notification` in
  `kitty/cocoa_window.m` switches on urgency with no `break` in any case, so
  every notification arrives at the `Active` level. Setting it is still right
  for Linux. The fall-through is also what keeps `Urgency.Critical` safe:
  `UNNotificationInterruptionLevelCritical` needs the Critical Alerts
  entitlement, which kitty.app does not carry, so a fixed switch could make
  macOS reject exactly the `blocked` banners.
- `env CATTERY_BIN` in the managed kitty.conf block is how the installed watcher
  learns where the binary is. It is a static file with no way to know, and it
  cannot look the binary up on PATH for the same reason the picker cannot. That
  line must not be `shellQuote`d: kitty's `env` takes the rest of the line as
  the value, quotes included, while `launch` on the line above splits
  shell-style and needs them. It does expand `$VAR` in the value.
- `setup.Stale` compares the installed kitty files *and* the managed kitty.conf
  block, because the block carries settings those files depend on. A release
  that changes only `renderBlock` leaves every file identical, and the watcher
  would then lose the new line with nothing said. The comparison re-renders the
  block with the binary path the installed block itself names, since that part
  belongs to the install rather than to the release.
- The kitty watcher keeps its seen windows in `boss._agent_seen`, a set inside
  kitty's own process. Nothing outside kitty can reach it, so the picker
  publishes `AGENT_SEEN` and `on_set_user_var` picks it up and clears it. That
  path only exists after `cattery setup` has installed the new watcher.
- A `tea.KeyMsg` carries no raw bytes, so `internal/overlay/keys.go` rebuilds
  them. The control keys are free: bubbletea defines `KeyCtrlA` as SOH and
  `KeyBackspace` as DEL, so for a type of 0-31 or 127 the type *is* the byte.
  Arrows go out in CSI form (`\x1b[A`), never SS3: the picker cannot know the
  target's application-cursor-key mode, and CSI is the form every parser has to
  accept.
- A row's prompt wraps, so a row is not two lines any more. `blocks` takes each
  row's height from `rowHeight`, which runs the same wrap `renderRow` will run.
  If those two disagree the viewport scrolls by the wrong amount and the list
  tears at the bottom edge.
- `ansi.Wrap` returns text already broken at the width you gave it. A row's
  first prompt line is narrower than the lines under it, because the cwd shares
  it, so `splitWrap` flattens the remainder before it is wrapped again. Without
  that the wider lines keep the narrow line's breaks and sit half empty.
- The drawer's box costs exactly what the plain rule cost, a space and two
  edges in place of " │ ". That is why `previewWidths`, `previewFits` and the
  91-column threshold did not move when the box arrived. Keep it that way, or
  the drawer shows a different amount of screen in each mode.
- `AGENT_STATE` is written last in every batch, by both writers. Writing it is
  what wakes the watcher, so a variable written after it is missing from the
  transition the watcher publishes: with the old order every `working` event
  carried the previous prompt. The order is asserted in
  `internal/state/state_test.go`, `internal/state/hook_test.go` and
  `tests/cattery_extension_test.ts`.
- The watcher's `_publish` runs on kitty's own thread, so it may neither raise
  nor block. A blocking send would freeze the terminal, which is why the socket
  is a non-blocking datagram and the whole body sits in a `try`, the way
  `_write_os_title` does. `sendto` answers `ENOENT` for a missing path,
  `ECONNREFUSED` for a dead owner and `ENOBUFS` for a full receiver, and none
  of the three blocks. The first two mean the subscriber has gone and its path
  is dropped; the third means it is alive and behind, so the datagram goes
  instead.
- An event has to fit in one datagram. macOS refuses a unix datagram over
  `net.local.dgram.maxdgram`, 2048 bytes by default, with `EMSGSIZE`, which
  reads as "alive and behind" and drops the event with nothing said. So the
  watcher writes compact UTF-8 JSON, not the six bytes per non-ASCII character
  `json.dumps` writes by default, and cuts the title and the prompt at 200
  characters each.
- `_derive_display` returns `"done"` only when the previous display is in
  `_WORKED`. Any new state a working agent can pass through has to join that
  tuple, or the agent goes idle with no marker and no notification. The failure
  is silent: no alert, no error.
- `time.Time{}` is older than every threshold, so a "has this run too long" rule
  needs `!IsZero()` as well as the comparison, and every unix-seconds variable
  needs a `secs > 0` guard: `"0"` parses, and `time.Unix(0, 0)` is not
  `IsZero()`. `agent.UnixSeconds` is that guard, and both parsers read every
  timestamp through it.
- `agent.PublishesTool` gates the tool pair on the agent kind, not on the
  display alone. `cattery state clear` drops the state, the kind and the message
  and nothing else, so a pi killed mid-call leaves `AGENT_TOOL` standing, and
  the Claude started in that window would wear the dead pi's command and read as
  stalled inside its first second. The watcher's `_tool_since` makes the same
  check.
- pi runs sibling tool calls concurrently and holds several open at once, so
  anything counting them needs a map keyed by `toolCallId`. It has to publish
  the earliest-started entry, or a fast `read` restamps the timestamp of a
  `bash` that has hung for 19 minutes, and it has to reset on `agent_start`: an
  interrupt tears the process down with no `tool_execution_end`.
- `tool_execution_start` fires before `prepareToolCall`, which is where pi's
  approval gate runs, so a tool waiting for the user publishes and ages towards
  stalled.
- `\x1f` separates the fields of a `tmux list-panes` row and `parseAgents` drops
  a row whose field count is wrong, so one in any `AGENT_*` value takes the
  agent out of the picker rather than showing bad text. The extension strips C0
  and C1 in `setUserVars`, the batched publish path, so every value is covered
  and not only the ones `sanitizeMessage` builds: JavaScript's `\s` matches
  neither `\x1f` nor `\x1b`.
- A number that grows inside wrapped text moves the wrap, which breaks the
  `rowHeight`/`renderRow` agreement above. `ansi.Wrap` eats one space at each
  break, so padding the number to a fixed width does not survive `splitWrap`.
  The running tool keeps the first activity line to itself and is cut rather
  than wrapped instead.
- `toolCell` cuts the tool label, never the elapsed time beside it. Cutting the
  composed line as one string is what a row does everywhere else, and here it
  drops the number off exactly the calls the feature exists for: a `bash`
  command long enough to overrun the line is the one worth timing. At 80 columns
  the label has about 50 cells.
- `add_timer` exists on kitty 0.48.1 but appears in no documentation page, so
  `_start_sweep` imports inside a `try`. The timer id
  lives on `boss`, never in a module global: a config reload re-executes the
  watcher module against the same `boss`, and a module global would leak one
  timer per reload. The watcher is a global watcher, so `on_load` runs once per
  kitty process and `boss` is the only shared state.
- `_apply` calls `_redraw` and `_update_os_title` unconditionally on its "no
  display" path, so anything sweeping windows on a timer has to filter to agent
  windows first. `_sweep` takes `AGENT_STATE == "working"` only.
- A colour name `fmt.fg` does not carry raises inside `agent_prefix`, whose
  `except Exception` returns `""`, so a typo in `_AGENT_STATE_STYLE` costs the
  tab marker with nothing said. `magenta` is in kitty 0.48.1's table.
- The pi extension is a second rollout. `cattery setup` offers to run
  `pi install git:github.com/alexander-akhmetov/cattery`, which fetches the
  published copy and not this checkout, and `setup.Stale` covers only the
  installed `kitty/*.py`, so nothing notices an out-of-date extension. Test a
  local one with `pi --no-extensions -e ./extensions/cattery.ts`.
- A display value missing from `filters` is reachable only from the `all` tab
  and is missing from the footer's "N active".
- `cattery events` asks for `SIGPIPE`. Go makes a broken pipe on stdout fatal
  unless a program does, so `cattery events | head -1` would die on the spot,
  leaving its path in kitty's registry and its socket file on disk. A socket
  file left behind is not harmless: `Close` does not unlink a unixgram socket,
  and the next run with the same pid binds the same name and fails
  `EADDRINUSE`, so `internal/events` clears a path nobody answers on before it
  binds.

@README.md
