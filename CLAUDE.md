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
`cattery setup`, `cattery state <x>` (the Claude-side writer), and
`cattery save`/`cattery restore` (the tab-tree snapshot).

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

@README.md
