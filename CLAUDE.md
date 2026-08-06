# cattery

Agent state in the kitty tab bar, plus a picker for jumping between agents.

## Build & test

```bash
make build    # go build -o cattery ./cmd/cattery
make test     # Go, Python, and TypeScript tests
make lint     # golangci-lint, govulncheck, deadcode, tsc
```

`make test-python` needs no kitty. The watcher and tab-glyph tests stub
`kitty.boss`, `kitty.window`, and `kitty.fast_data_types` and load the module by
path. `tests/cattery_tab_bar_test.py` copies `kitty/tab_bar.py` into a temporary
directory, with and without `cattery_tab.py` beside it, because which of those
holds is what decides whether the guarded import succeeds.

`make test-ts` and `make lint-ts` install `node_modules` first. That toolchain
covers `extensions/cattery.ts` and nothing else: no installed file needs node.

## Layout

`kitty/` holds the four files kitty loads directly, `extensions/cattery.ts` is
the pi-side state writer, and `cmd/`+`internal/` build the binary: the picker,
`cattery setup`, and `cattery state <x>`, the Claude-side state writer.

`cattery setup` writes copies of the `kitty/` files, not symlinks into a
checkout, so an install stops depending on where the source lives. The picker
compares the installed copies with the embedded ones on startup and warns when
they differ, because a copy does not follow a binary upgrade.

## Things that bite

- The user variables are named `AGENT_*`, not `CATTERY_*`. They are a live
  contract with running pi and Claude sessions; renaming them breaks sessions
  mid-flight.
- kitty loads `tab_bar.py` with `runpy.run_path`, which does not extend
  `sys.path`. `cattery_tab.py` cannot be imported from a `tab_bar.py` that does
  not insert its own directory first, and that import must be guarded.
- `cattery state clear` must return before it reads stdin. The fish wrappers call
  it with whatever stdin they inherited, which can be a pipe that never closes.
- `go:embed` cannot reach outside its own package directory, so `assets.go` sits
  at the module root rather than in `internal/setup`. The Python files stay in
  `kitty/`, where the Python tests read them.

@README.md
