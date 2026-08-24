package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexander-akhmetov/cattery/internal/agent"
)

// printLister stands in for the merged inventory.
type printLister struct {
	agents []agent.Agent
	err    error
}

func (p printLister) ListAgents(context.Context) ([]agent.Agent, error) { return p.agents, p.err }

// A printed row says which host the agent runs in, and a tmux row carries the
// target `cattery attach` takes. The columns are how the contract is checked by
// hand when the picker shows nothing, so `cattery list` with no flags has to
// keep printing exactly what `cattery -print` printed.
func TestRunListColumns(t *testing.T) {
	var out strings.Builder
	code := runList(printLister{agents: []agent.Agent{
		{
			ID: 17, Host: agent.HostTmux, Kind: "claude", Display: "working",
			Project: "myapp", Branch: "wt/feat-42",
			CWD:    "/Users/x/.worktrees/myapp/feat-42",
			Target: "dev:3.%17",
		},
		{
			ID: 12, Host: agent.HostKitty, Kind: "pi", Display: "idle",
			Project: "dotfiles", Branch: "main", CWD: "/Users/x/projects/dotfiles",
		},
		{
			ID: 21, Host: agent.HostKitty, Kind: "opencode", Display: "stalled",
			Project: "cattery", Branch: "main", CWD: "/Users/x/projects/cattery",
		},
	}}, &out, nil)
	if code != 0 {
		t.Fatalf("exit code: got %d, want 0", code)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(lines), out.String())
	}
	for _, want := range []string{"opencode", "stalled"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("opencode row %q is missing %q", lines[2], want)
		}
	}
	// "opencode" is the longest kind any agent publishes, and a kind that
	// overruns its column shifts the rest of that row against every other one.
	for _, field := range []string{"host=", "id="} {
		if strings.Index(lines[2], field) != strings.Index(lines[1], field) {
			t.Errorf("%q starts at a different column in\n %q\nand\n %q", field, lines[1], lines[2])
		}
	}
	for _, want := range []string{"host=tmux", "id=17", "target=dev:3.%17", "myapp", "working"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("tmux row %q is missing %q", lines[0], want)
		}
	}
	if !strings.Contains(lines[1], "host=kitty") || strings.Contains(lines[1], "target=") {
		t.Errorf("kitty row %q should name its host and no target", lines[1])
	}
}

// One host can fail while the other answers. Its rows are the whole inventory
// on a machine with no kitty running, so they print and the failure still
// decides the exit code.
func TestRunListColumnsPartialFailure(t *testing.T) {
	var out strings.Builder
	code := runList(printLister{
		agents: []agent.Agent{{ID: 17, Host: agent.HostTmux, Display: "working", Target: "dev:3.%17"}},
		err:    errors.New("kitty: no listening socket"),
	}, &out, nil)

	if code != 1 {
		t.Fatalf("exit code: got %d, want 1", code)
	}
	if !strings.Contains(out.String(), "host=tmux") {
		t.Errorf("dropped the rows the working host returned: %q", out.String())
	}
}

// jsonAgents runs the command and decodes what it wrote into maps, so every
// assertion below is on the wire name rather than on a Go field.
func jsonAgents(t *testing.T, out string) (map[string]any, []map[string]any) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	raw, ok := payload["agents"].([]any)
	if !ok {
		t.Fatalf("no agents array: %v", payload)
	}
	rows := make([]map[string]any, len(raw))
	for i, r := range raw {
		rows[i] = r.(map[string]any)
	}
	return payload, rows
}

func TestRunListJSON(t *testing.T) {
	toolSince := time.Unix(1787562558, 0)
	since := time.Unix(1787562500, 0)
	created := time.Unix(1787522022, 0)
	// A prompt carrying && and angle brackets is the ordinary case, not an
	// exotic one, and the default encoder would hand a consumer &amp;&amp;.
	prompt := "go test ./... && echo <ok>"

	var out strings.Builder
	code := runList(printLister{agents: []agent.Agent{
		{
			ID: 324, Host: agent.HostKitty, Kind: "pi", Self: true,
			State: "working", Display: "stalled",
			Title: "π - cattery", CWD: "/Users/x/projects/cattery", Msg: prompt,
			Tool: "bash: go test ./...", ToolSince: toolSince, Since: since, CreatedAt: created,
			Resume:  "pi --session /Users/x/.pi/s.jsonl",
			Project: "cattery", ProjectKey: "/Users/x/projects/cattery/.git",
			Root: "/Users/x/projects/cattery", Branch: "main",
			PID: 25636,
			Procs: []agent.Process{
				{PID: 23053, Cmdline: []string{"/usr/bin/log", "stream"}, CWD: "/Users/x/projects/cattery"},
				{PID: 23051, Cmdline: []string{"nono", "run", "--", "pi"}, CWD: "/Users/x/projects/cattery"},
			},
		},
		{
			ID: 17, Host: agent.HostTmux, Kind: "claude",
			State: "idle", Display: "done",
			Title: "◐ Run /code-review", CWD: "/Users/x/.worktrees/myapp/feat-42",
			Since: since, Target: "dev:3.%17", Resume: "claude --resume abc-123",
			Project: "myapp", Branch: "wt/feat-42",
			PID: 86369, Command: "claude",
		},
		// A window whose agent was killed: `cattery state clear` dropped the
		// state, and nothing clears the display the watcher derived from it.
		{ID: 400, Host: agent.HostKitty, Display: "done"},
	}}, &out, []string{"-json"})
	if code != 0 {
		t.Fatalf("exit code: got %d, want 0", code)
	}

	payload, rows := jsonAgents(t, out.String())
	if _, ok := payload["errors"]; ok {
		t.Errorf("nothing failed, so there should be no errors key: %v", payload["errors"])
	}
	if payload["cattery"] != versionString() {
		t.Errorf("cattery: got %v, want %q", payload["cattery"], versionString())
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	kittyRow, tmuxRow, killedRow := rows[0], rows[1], rows[2]

	// A consumer has to be able to tell "the agent said idle" from "there is no
	// agent", so the killed window carries no state key rather than an empty
	// one, while its display stands.
	if _, ok := killedRow["state"]; ok {
		t.Errorf("a window with no agent carries a state key: %v", killedRow["state"])
	}
	if killedRow["display"] != "done" {
		t.Errorf("killed row display: got %v, want done", killedRow["display"])
	}

	// The two words disagree on purpose, on both rows: no agent publishes
	// "stalled" or "done", so a caller gating on what the agent said has to
	// read "state" and not "display".
	for _, tc := range []struct {
		row                 map[string]any
		key, state, display string
	}{
		{kittyRow, "kitty:324", "working", "stalled"},
		{tmuxRow, "tmux:%17", "idle", "done"},
	} {
		if tc.row["key"] != tc.key {
			t.Errorf("key: got %v, want %q", tc.row["key"], tc.key)
		}
		if tc.row["state"] != tc.state || tc.row["display"] != tc.display {
			t.Errorf("%s: got state=%v display=%v, want %q / %q",
				tc.key, tc.row["state"], tc.row["display"], tc.state, tc.display)
		}
	}

	// At most one row is the caller, and the other must carry no self key at
	// all: a consumer excluding itself reads a missing key as "not me", and a
	// false one would say the same thing about a window nobody checked.
	if kittyRow["self"] != true {
		t.Errorf("self: got %v, want true", kittyRow["self"])
	}
	if _, ok := tmuxRow["self"]; ok {
		t.Errorf("the other row carries a self key: %v", tmuxRow["self"])
	}

	// Each host reports what it can and nothing it cannot.
	for _, absent := range []string{"target", "command"} {
		if _, ok := kittyRow[absent]; ok {
			t.Errorf("kitty row has a %q key: %v", absent, kittyRow[absent])
		}
	}
	for _, absent := range []string{"created_at", "foreground_processes"} {
		if _, ok := tmuxRow[absent]; ok {
			t.Errorf("tmux row has a %q key: %v", absent, tmuxRow[absent])
		}
	}
	if tmuxRow["pid"] != float64(86369) || tmuxRow["command"] != "claude" || tmuxRow["target"] != "dev:3.%17" {
		t.Errorf("tmux fingerprint: got %+v", tmuxRow)
	}

	// The sandbox's log reader comes first and the agent is behind it, so the
	// whole list has to reach the wire with its argv.
	wantProcs := []any{
		map[string]any{"pid": float64(23053), "cmdline": []any{"/usr/bin/log", "stream"}, "cwd": "/Users/x/projects/cattery"},
		map[string]any{"pid": float64(23051), "cmdline": []any{"nono", "run", "--", "pi"}, "cwd": "/Users/x/projects/cattery"},
	}
	if !reflect.DeepEqual(kittyRow["foreground_processes"], wantProcs) {
		t.Errorf("foreground_processes:\n got %+v\nwant %+v", kittyRow["foreground_processes"], wantProcs)
	}

	for _, tc := range []struct {
		field string
		want  time.Time
	}{
		{"since", since}, {"tool_since", toolSince}, {"created_at", created},
	} {
		if kittyRow[tc.field] != float64(tc.want.Unix()) {
			t.Errorf("%s: got %v, want %d", tc.field, kittyRow[tc.field], tc.want.Unix())
		}
	}

	if kittyRow["msg"] != prompt {
		t.Errorf("msg: got %q, want %q", kittyRow["msg"], prompt)
	}
	if strings.Contains(out.String(), "\\u0026") {
		t.Errorf("the encoder escaped HTML:\n%s", out.String())
	}
}

// A consumer that reads only stdout must not be able to mistake a broken
// listing for an empty one, so the failure is in the payload as well as in the
// exit code, and the host that answered keeps its rows.
func TestRunListJSONPartialFailure(t *testing.T) {
	var out strings.Builder
	code := runList(printLister{
		agents: []agent.Agent{{ID: 17, Host: agent.HostTmux, Display: "working", Target: "dev:3.%17"}},
		err:    errors.Join(errors.New("kitty: no listening socket")),
	}, &out, []string{"-json"})

	if code != 1 {
		t.Fatalf("exit code: got %d, want 1", code)
	}
	payload, rows := jsonAgents(t, out.String())
	if len(rows) != 1 || rows[0]["key"] != "tmux:%17" {
		t.Errorf("dropped the rows the working host returned: %v", rows)
	}
	errs, ok := payload["errors"].([]any)
	if !ok || len(errs) != 1 {
		t.Fatalf("errors: got %v, want one entry", payload["errors"])
	}
	if msg, _ := errs[0].(string); !strings.HasPrefix(msg, "kitty: ") {
		t.Errorf("errors[0] should name the host that failed: %q", msg)
	}
}

// A whole tmux server can be missing without anything having failed, and then
// a caller must not see an errors key at all.
func TestRunListJSONNoErrorsKey(t *testing.T) {
	var out strings.Builder
	if code := runList(printLister{}, &out, []string{"-json"}); code != 0 {
		t.Fatalf("exit code: got %d, want 0", code)
	}
	if strings.Contains(out.String(), "errors") {
		t.Errorf("an empty listing should carry no errors key:\n%s", out.String())
	}
}

func TestRunListRejectsOperands(t *testing.T) {
	var out strings.Builder
	if code := runList(printLister{}, &out, []string{"kitty:324"}); code != 2 {
		t.Fatalf("exit code: got %d, want 2", code)
	}
	if out.Len() > 0 {
		t.Errorf("a rejected command line should print nothing: %q", out.String())
	}
}
