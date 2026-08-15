package overlay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alexander-akhmetov/cattery"
	"github.com/alexander-akhmetov/cattery/internal/agent"
	"github.com/alexander-akhmetov/cattery/internal/kitty"
)

type fakeClient struct {
	agents   []agent.Agent
	listErr  error
	focused  []string
	focusErr error

	// The preview half. previewed records the agent keys asked for, so a test
	// can prove which agent the sidebar went after and how often.
	previewed  []string
	screen     string
	previewErr error

	// The read-write half. typed records what each send carried, in order, so
	// a test can prove both the bytes and that nothing was reordered. inFlight
	// counts the sends running at once, which must never exceed one.
	typed    []string
	sendErr  error
	inFlight int
	maxSends int
	marked   []string

	// The session half of the interface. actionErr makes save or restore fail
	// without a kitty. windows is what goto_session produced, so it stays empty
	// until then and the duplicate guard sees nothing.
	actions   []string
	actionErr error
	windows   []kitty.Window
	restored  bool
	sent      []string
}

func (f *fakeClient) ListAgents(context.Context) ([]agent.Agent, error) {
	return f.agents, f.listErr
}

func (f *fakeClient) Focus(_ context.Context, a agent.Agent) error {
	f.focused = append(f.focused, a.Key())
	return f.focusErr
}

func (f *fakeClient) Preview(_ context.Context, a agent.Agent) (string, error) {
	f.previewed = append(f.previewed, a.Key())
	return f.screen, f.previewErr
}

func (f *fakeClient) Send(_ context.Context, _ agent.Agent, data string) error {
	f.inFlight++
	f.maxSends = max(f.maxSends, f.inFlight)
	f.inFlight--
	f.typed = append(f.typed, data)
	return f.sendErr
}

func (f *fakeClient) MarkSeen(_ context.Context, a agent.Agent) error {
	f.marked = append(f.marked, a.Key())
	return nil
}

func (f *fakeClient) Action(_ context.Context, arg string) error {
	f.actions = append(f.actions, arg)
	if f.actionErr != nil {
		return f.actionErr
	}
	// Stand in for kitty's save_as_session, which writes the file it is given.
	if path, ok := strings.CutPrefix(arg, "save_as_session --save-only "); ok {
		return os.WriteFile(strings.Trim(path, "'"), []byte("\nnew_tab\ncd /tmp\nlaunch\n"), 0o600)
	}
	if strings.HasPrefix(arg, "goto_session ") {
		f.restored = true
	}
	return nil
}

func (f *fakeClient) SendText(_ context.Context, _ int, text string) error {
	f.sent = append(f.sent, text)
	return nil
}

func (f *fakeClient) Windows(context.Context) ([]kitty.Window, error) {
	if !f.restored {
		return nil, nil
	}
	return f.windows, nil
}

// checkout builds an agent in the primary checkout of its project, the shape
// ListAgents produces for a plain `cd ~/projects/x` session.
func checkout(id int, kind, display, project, branch string) agent.Agent {
	root := "/p/" + project
	return agent.Agent{
		ID: id, Kind: kind, Display: display,
		CWD: root, Project: project, ProjectKey: root + "/.git", Root: root, Branch: branch,
	}
}

func sampleModel() Model {
	m := New(&fakeClient{}, &fakeClient{})
	m.loaded = true
	m.loading = false
	// Project order, the way ListAgents delivers them.
	m.agents = []agent.Agent{
		checkout(3, "pi", "working", "astra-l", "feat/oauth"),
		checkout(2, "pi", "working", "dotfiles", "main"),
		checkout(1, "claude", "blocked", "llm-proxy", "master"),
		checkout(4, "codex", "done", "qmp-relay", "qmp"),
	}
	return m
}

// The status filter and the query narrow the same list. The query searches
// inside the filter.
func TestVisible(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		query  string
		want   []int
	}{
		{"no filter, no query", "all", "", []int{3, 2, 1, 4}},
		{"filter keeps only its status", "working", "", []int{3, 2}},
		{"query matches a branch", "all", "oauth", []int{3}},
		{"query matches a kind", "all", "pi", []int{3, 2}},
		{"query matches nothing", "all", "nomatch", nil},
		{"query searches inside the filter", "working", "master", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.filter = tc.filter
			m.search.SetValue(tc.query)
			vis := m.visible()
			got := make([]int, 0, len(vis))
			for _, a := range vis {
				got = append(got, a.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("visible ids: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMoveWraps(t *testing.T) {
	m := sampleModel()
	m.selected = 0
	m.move(-1)
	if m.selected != 3 {
		t.Errorf("move up from top: got %d, want 3", m.selected)
	}
	m.move(1)
	if m.selected != 0 {
		t.Errorf("move down wraps to top: got %d, want 0", m.selected)
	}
}

func TestClampSelectionAfterFilter(t *testing.T) {
	m := sampleModel()
	m.selected = 3
	m.filter = "blocked" // only one match now
	m.clampSelection()
	if m.selected != 0 {
		t.Errorf("clamp after shrink: got %d, want 0", m.selected)
	}
}

func TestCycleFilterResetsSelection(t *testing.T) {
	m := sampleModel()
	m.selected = 2
	m.cycleFilter()
	if m.filter != "working" {
		t.Errorf("cycle from all: got %q, want working", m.filter)
	}
	if m.selected != 0 {
		t.Errorf("cycle should reset selection, got %d", m.selected)
	}
	m.cycleFilter()
	m.cycleFilter()
	if m.filter != "done" {
		t.Errorf("third cycle: got %q, want done", m.filter)
	}
	m.cycleFilter()
	if m.filter != "idle" {
		t.Errorf("fourth cycle: got %q, want idle", m.filter)
	}
	m.cycleFilter()
	if m.filter != "all" {
		t.Errorf("cycle wraps to all, got %q", m.filter)
	}
}

func TestNumberKeySelectsVisibleRow(t *testing.T) {
	m := sampleModel()
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	if updated.(Model).selected != 2 {
		t.Errorf("number key 3: got %d, want 2", updated.(Model).selected)
	}
}

func TestFocusSelectedTargetsVisibleRow(t *testing.T) {
	m := sampleModel()
	m.filter = "working"
	m.selected = 1 // second working agent => id 2

	// The selected row in the filtered view must map back to the right window.
	if got := m.visible()[m.selected].ID; got != 2 {
		t.Fatalf("selected visible id: got %d, want 2", got)
	}

	model, cmd := m.focusSelected()
	if !model.focusing || model.quitting {
		t.Error("focusSelected should wait for the focus result before quitting")
	}
	if cmd == nil {
		t.Fatal("focusSelected should return a command")
	}
}

func TestFocusCmdCallsClient(t *testing.T) {
	cases := []struct {
		name string
		in   agent.Agent
		want string
	}{
		{name: "a kitty window", in: agent.Agent{ID: 3, Host: agent.HostKitty}, want: "kitty:3"},
		// The whole agent goes to the client, because reaching a tmux agent
		// means attaching to its pane rather than focusing a window id.
		{name: "a tmux pane", in: agent.Agent{ID: 17, Host: agent.HostTmux, Target: "kontora:3.%17"}, want: "tmux:%17"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeClient{}
			msg := focusCmd(context.Background(), fc, tc.in)()
			if got := msg.(focusMsg).err; got != nil {
				t.Errorf("focusCmd error: got %v, want nil", got)
			}
			if !slices.Equal(fc.focused, []string{tc.want}) {
				t.Errorf("focused: got %v, want [%s]", fc.focused, tc.want)
			}
		})
	}
}

func TestFocusSelectedEmptyNoop(t *testing.T) {
	m := sampleModel()
	m.filter = "working"
	m.search.SetValue("nomatch")
	model, cmd := m.focusSelected()
	if model.quitting || cmd != nil {
		t.Error("focusSelected with no visible rows should be a no-op")
	}
}

func TestSearchEscClears(t *testing.T) {
	m := sampleModel()
	m.searching = true
	m.search.Focus()
	m.search.SetValue("oauth")
	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	got := model.(Model)
	if got.searching {
		t.Error("esc should leave search mode")
	}
	if got.search.Value() != "" {
		t.Errorf("esc should clear query, got %q", got.search.Value())
	}
}

func TestCounts(t *testing.T) {
	m := sampleModel()
	m.agents = append(m.agents, agent.Agent{ID: 5, Display: "idle"})
	c := m.counts()
	want := map[string]int{"all": 5, "working": 2, "blocked": 1, "done": 1, "idle": 1}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("counts[%q]: got %d, want %d", k, c[k], v)
		}
	}
}

// The spinner tick is the only thing that redraws a still list. It stops when
// no agent is working and starts again when one appears.
func TestSpinTickFollowsWorkingAgents(t *testing.T) {
	idle := []agent.Agent{{ID: 1, Display: "idle"}}
	working := []agent.Agent{{ID: 1, Display: "working"}}

	m := sampleModel()
	m.agents, m.spinning = nil, false
	afterIdle, cmd := m.Update(agentsMsg{generation: m.reloadGeneration, agents: idle})
	if cmd != nil || afterIdle.(Model).spinning {
		t.Errorf("an idle inventory must not schedule a spin tick")
	}

	started, cmd := afterIdle.(Model).Update(agentsMsg{generation: m.reloadGeneration, agents: working})
	if cmd == nil || !started.(Model).spinning {
		t.Fatalf("a working agent must schedule a spin tick")
	}

	still, cmd := started.(Model).Update(spinMsg{})
	if cmd == nil || !still.(Model).spinning {
		t.Errorf("the tick must keep going while an agent works")
	}

	stopping, _ := started.(Model).Update(agentsMsg{generation: m.reloadGeneration, agents: idle})
	stopped, cmd := stopping.(Model).Update(spinMsg{})
	if cmd != nil || stopped.(Model).spinning {
		t.Errorf("the tick must stop once no agent is working")
	}
}

func TestElapsed(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name  string
		since time.Time
		want  string
	}{
		{"zero", time.Time{}, ""},
		{"seconds", now.Add(-47 * time.Second), "47s"},
		{"minutes", now.Add(-(2*time.Minute + 18*time.Second)), "2m 18s"},
		{"hours", now.Add(-(1*time.Hour + 5*time.Minute)), "1h 05m"},
		{"future", now.Add(30 * time.Second), "0s"},
	}
	for _, tc := range cases {
		if got := elapsed(now, tc.since); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestActivity(t *testing.T) {
	cases := []struct {
		name string
		in   agent.Agent
		want string
	}{
		{"message wins over title", agent.Agent{Display: "working", Title: "zsh", Msg: "refactor the prompt"}, "refactor the prompt"},
		{"blocked with title", agent.Agent{Display: "blocked", Title: "build"}, "build"},
		{"blocked no title", agent.Agent{Display: "blocked"}, "waiting for input"},
		{"working with title", agent.Agent{Display: "working", Title: "writing tests"}, "writing tests"},
		{"done no title", agent.Agent{Display: "done"}, "finished"},
		{"idle with message", agent.Agent{Display: "idle", Msg: "ship the release"}, "ship the release"},
		{"idle with title", agent.Agent{Display: "idle", Title: "fish"}, "fish"},
		// The status column already says "idle", so the row has nothing to add.
		{"idle bare", agent.Agent{Display: "idle"}, ""},
	}
	for _, tc := range cases {
		if got := activity(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The glyphs stay in step with _AGENT_STATE_STYLE in kitty/cattery_tab.py, so a
// dot means the same thing in the tab bar and in the picker and only the colour
// separates a finished agent from a running one. Idle is picker-only, because
// the tab bar draws nothing for it.
func TestStatusGlyph(t *testing.T) {
	cases := map[string]string{
		"working": "●",
		"blocked": "◆",
		"done":    "●",
		"idle":    "·",
	}
	for display, want := range cases {
		if got := statusGlyph(display); got != want {
			t.Errorf("statusGlyph(%q): got %q, want %q", display, got, want)
		}
	}
}

func TestActivityGlyph(t *testing.T) {
	wrk := agent.Agent{Display: "working"}
	if a, b := activityGlyph(wrk, 0), activityGlyph(wrk, 1); a == b {
		t.Errorf("working glyph should advance with spin: %q == %q", a, b)
	}
	cases := map[string]string{
		"blocked": "◆",
		"done":    "●",
		// Idle's status glyph repeats the middot separator in front of it, so
		// the row leaves it out.
		"idle": "",
	}
	for display, want := range cases {
		if got := activityGlyph(agent.Agent{Display: display}, 3); got != want {
			t.Errorf("activityGlyph(%q): got %q, want %q", display, got, want)
		}
	}
}

func TestTimeLabel(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name string
		in   agent.Agent
		want string
	}{
		{"working", agent.Agent{Display: "working", Since: now.Add(-90 * time.Second)}, "1m 30s"},
		{"blocked", agent.Agent{Display: "blocked", Since: now.Add(-30 * time.Second)}, "waiting 30s"},
		{"done", agent.Agent{Display: "done", Since: now.Add(-6 * time.Minute)}, "6m ago"},
		{"blocked unknown", agent.Agent{Display: "blocked"}, "waiting"},
	}
	for _, tc := range cases {
		if got := timeLabel(now, tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMetaRight(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name string
		in   agent.Agent
		want string
	}{
		{"working", agent.Agent{Display: "working", Since: now.Add(-(13*time.Minute + 41*time.Second))}, "13m 41s"},
		{"done", agent.Agent{Display: "done", Since: now.Add(-2 * time.Minute)}, "finished 2m ago"},
		{"done unknown", agent.Agent{Display: "done"}, "finished"},
		{"idle has no summary", agent.Agent{Display: "idle", Since: now.Add(-time.Minute)}, ""},
	}
	for _, tc := range cases {
		if got := metaRight(now, tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAgentName(t *testing.T) {
	if got := agentName(agent.Agent{CWD: "/p/dotfiles"}); got != "dotfiles" {
		t.Errorf("name from cwd: %q", got)
	}
	if got := agentName(agent.Agent{Title: "fallback"}); got != "fallback" {
		t.Errorf("name fallback to title: %q", got)
	}
}

// Inside a project group the row says which checkout it is. The branch does
// that until there is no branch to show.
func TestRowLabel(t *testing.T) {
	cases := []struct {
		name  string
		agent agent.Agent
		want  string
	}{
		{
			name:  "branch names the checkout",
			agent: agent.Agent{CWD: "/p/dotfiles", Root: "/p/dotfiles", Branch: "main"},
			want:  "main",
		},
		{
			name:  "worktree branch beats the directory",
			agent: agent.Agent{CWD: "/wt/feat-oauth", Root: "/wt/feat-oauth", Branch: "feat/oauth"},
			want:  "feat/oauth",
		},
		{
			name:  "detached HEAD falls back to the worktree",
			agent: agent.Agent{CWD: "/tmp/sig-review/sub", Root: "/tmp/sig-review"},
			want:  "sig-review",
		},
		{
			name:  "outside git falls back to the folder",
			agent: agent.Agent{CWD: "/home/x/scratch"},
			want:  "scratch",
		},
		{
			name:  "no path at all falls back to the title",
			agent: agent.Agent{Title: "pi"},
			want:  "pi",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowLabel(tc.agent); got != tc.want {
				t.Errorf("rowLabel: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGroupLabel(t *testing.T) {
	if got := groupLabel(agent.Agent{Project: "dotfiles"}); got != "dotfiles" {
		t.Errorf("group label: %q", got)
	}
	if got := groupLabel(agent.Agent{}); got != "unknown" {
		t.Errorf("group label without a project: %q", got)
	}
}

func TestGroupSize(t *testing.T) {
	agents := []agent.Agent{
		{ID: 1, Project: "a", ProjectKey: "/a/.git"},
		{ID: 2, Project: "a", ProjectKey: "/a/.git"},
		// The same label from another repository, so a separate group.
		{ID: 3, Project: "a", ProjectKey: "/other/a/.git"},
		{ID: 4, Project: "b", ProjectKey: "/b/.git"},
	}
	for start, want := range map[int]int{0: 2, 1: 1, 2: 1, 3: 1} {
		if got := groupSize(agents, start); got != want {
			t.Errorf("groupSize(%d): got %d, want %d", start, got, want)
		}
	}
}

// Grouping puts several worktrees of one repository under a single heading,
// each identified by its branch.
func TestViewGroupsWorktreesUnderOneHeading(t *testing.T) {
	m := sampleModel()
	m.width, m.height = 100, 40
	m.agents = []agent.Agent{
		{ID: 1, Kind: "pi", Display: "working", CWD: "/p/dotfiles", Project: "dotfiles",
			ProjectKey: "/p/dotfiles/.git", Root: "/p/dotfiles", Branch: "main"},
		{ID: 2, Kind: "pi", Display: "idle", CWD: "/wt/dotfiles/feat-oauth", Project: "dotfiles",
			ProjectKey: "/p/dotfiles/.git", Root: "/wt/dotfiles/feat-oauth", Branch: "feat/oauth"},
		{ID: 3, Kind: "pi", Display: "idle", CWD: "/tmp/dotfiles-review", Project: "dotfiles",
			ProjectKey: "/p/dotfiles/.git", Root: "/tmp/dotfiles-review"},
		{ID: 4, Kind: "pi", Display: "working", CWD: "/p/sigil", Project: "sigil",
			ProjectKey: "/p/sigil/.git", Root: "/p/sigil", Branch: "main"},
	}

	out := ansi.Strip(m.View())

	// One heading per project, counting only that project's agents.
	if !strings.Contains(out, "dotfiles 3") {
		t.Errorf("missing dotfiles heading with 3 agents, got:\n%s", out)
	}
	if !strings.Contains(out, "sigil 1") {
		t.Errorf("missing sigil heading with 1 agent, got:\n%s", out)
	}
	if got := strings.Count(out, "dotfiles 3"); got != 1 {
		t.Errorf("dotfiles should head exactly one group, headed %d:\n%s", got, out)
	}
	// Each worktree names itself: two branches, then the detached one's
	// directory.
	for _, want := range []string{"feat/oauth", "dotfiles-review"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing row label %q, got:\n%s", want, out)
		}
	}
}

// A heading draws only when a row fits under it. A viewport that starts
// mid-group redraws the heading it scrolled past.
func TestViewGroupHeadingsSurviveScrolling(t *testing.T) {
	heading := func(project string, count int) string { return fmt.Sprintf("%s %d", project, count) }

	cases := []struct {
		name     string
		height   int
		selected int
		want     []string
		absent   []string
	}{
		{
			name: "everything fits", height: 40, selected: 0,
			want: []string{heading("alpha", 2), heading("zulu", 2)},
		},
		{
			// Room for alpha's heading and one row, but not for zulu's heading.
			name: "no orphan heading at the bottom edge", height: 14, selected: 0,
			want: []string{heading("alpha", 2)}, absent: []string{heading("zulu", 2)},
		},
		{
			name: "heading re-drawn when the group scrolls off", height: 14, selected: 3,
			want: []string{heading("zulu", 2)}, absent: []string{heading("alpha", 2)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = 60, tc.height
			m.agents = []agent.Agent{
				checkout(1, "pi", "working", "alpha", "main"),
				checkout(2, "pi", "working", "alpha", "topic"),
				checkout(3, "pi", "working", "zulu", "main"),
				checkout(4, "pi", "working", "zulu", "topic"),
			}
			// Two rows of one project cannot share a ProjectKey built from the
			// branch, so name the same repo for both.
			m.agents[1].ProjectKey, m.agents[1].CWD = m.agents[0].ProjectKey, "/p/alpha-wt"
			m.agents[3].ProjectKey, m.agents[3].CWD = m.agents[2].ProjectKey, "/p/zulu-wt"
			m.selected = tc.selected

			out := ansi.Strip(m.View())
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q, got:\n%s", w, out)
				}
			}
			for _, w := range tc.absent {
				if strings.Contains(out, w) {
					t.Errorf("should not contain %q, got:\n%s", w, out)
				}
			}
		})
	}
}

func TestViewRenders(t *testing.T) {
	m := sampleModel()
	m.width, m.height = 120, 40
	out := m.View()
	want := []string{
		"AGENTS",
		"blocked", "working",
		"dotfiles",    // name on line 1
		"/p/dotfiles", // cwd on line 2
		"esc close",
		"4 agents", // footer inventory summary
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("view missing %q", w)
		}
	}
}

// Every chrome line costs the list a line, so the top chrome draws only what is
// in use. The title shares the title bar, and the search row waits for a query.
func TestHeaderLines(t *testing.T) {
	pad := lipgloss.NewStyle().PaddingLeft(2)
	cases := []struct {
		name      string
		roomy     bool
		searching bool
		query     string
		want      int
	}{
		{name: "roomy and untouched", roomy: true, want: 5},
		{name: "roomy while typing", roomy: true, searching: true, want: 6},
		{name: "roomy with a query", roomy: true, query: "oauth", want: 6},
		{name: "compact and untouched", want: 3},
		{name: "compact with a query", query: "oauth", want: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = 100, 30
			m.searching = tc.searching
			m.search.SetValue(tc.query)
			if got := len(m.headerLines(100, 96, tc.roomy, pad)); got != tc.want {
				t.Fatalf("header lines: got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSearchRowVisibility(t *testing.T) {
	cases := []struct {
		name      string
		searching bool
		query     string
		want      string // text that must appear, or "" for the placeholder's absence
	}{
		{name: "untouched hides the field"},
		{name: "typing shows the placeholder", searching: true, want: "search by name"},
		{name: "a query keeps the field on screen", query: "oauth", want: "oauth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = 100, 30
			m.searching = tc.searching
			m.search.SetValue(tc.query)
			out := ansi.Strip(m.View())
			if tc.want == "" {
				if strings.Contains(out, "search by name") {
					t.Fatalf("unused search field still drawn:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("view missing %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestTitleBarStates(t *testing.T) {
	cases := []struct {
		name      string
		searching bool
		focusing  bool
		want      string
	}{
		{name: "idle", want: "esc close"},
		{name: "searching", searching: true, want: "esc clear"},
		{name: "focusing", focusing: true, want: "focusing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.searching, m.focusing = tc.searching, tc.focusing
			bar := ansi.Strip(m.renderTitleBar(100))
			if !strings.Contains(bar, "AGENTS") {
				t.Errorf("title bar lost its name: %q", bar)
			}
			if !strings.Contains(bar, tc.want) {
				t.Errorf("title bar missing %q: %q", tc.want, bar)
			}
		})
	}
}

// Selecting a row must not slide the time column, or holding j shuffles the
// whole right edge on every keypress.
func TestRowTimeColumnHoldsStill(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	agent := agent.Agent{
		ID: 1, Kind: "pi", Display: "working",
		CWD: "/p/dotfiles", Branch: "main", Since: now.Add(-90 * time.Second),
	}
	label := timeLabel(now, agent)

	cases := []struct {
		name            string
		inner           int
		wantTimeOnFocus bool
	}{
		{name: "wide row keeps time and hint together", inner: 100, wantTimeOnFocus: true},
		{name: "narrow row trades time for the hint", inner: 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.now = now

			m.selected = 0
			selected := m.renderRow(0, agent, tc.inner, true)[0]
			m.selected = 1
			unselected := m.renderRow(0, agent, tc.inner, true)[0]
			selected, unselected = ansi.Strip(selected), ansi.Strip(unselected)

			if !strings.Contains(selected, jumpHint) {
				t.Fatalf("selected row missing the jump hint: %q", selected)
			}
			if !strings.Contains(unselected, label) {
				t.Fatalf("unselected row missing its time: %q", unselected)
			}
			if !tc.wantTimeOnFocus {
				if strings.Contains(selected, label) {
					t.Fatalf("narrow selected row should drop the time: %q", selected)
				}
				return
			}
			// Compare terminal cells. The selection bar is a wide rune, so byte
			// counts differ from what the terminal shows.
			column := func(line string) int {
				before, _, found := strings.Cut(line, label)
				if !found {
					t.Fatalf("row missing the time label %q: %q", label, line)
				}
				return ansi.StringWidth(before)
			}
			if got, want := column(selected), column(unselected); got != want {
				t.Fatalf("time column moved on selection: %d vs %d\n%q\n%q", got, want, selected, unselected)
			}
		})
	}
}

// Line 2 of an idle row. With nothing to report the row stops after the path,
// and a title or message prints without the repeated middot.
func TestIdleRowLine2(t *testing.T) {
	indent := strings.Repeat(" ", rowIndent)
	cases := []struct {
		name  string
		agent agent.Agent
		want  string
	}{
		{
			name:  "nothing to report",
			agent: agent.Agent{ID: 8, Kind: "pi", Display: "idle", CWD: "/p/org"},
			want:  indent + "/p/org",
		},
		{
			name:  "message without a status glyph",
			agent: agent.Agent{ID: 9, Kind: "pi", Display: "idle", CWD: "/p/org", Msg: "tidy the notes"},
			want:  indent + "/p/org · tidy the notes",
		},
		{
			name:  "title fallback",
			agent: agent.Agent{ID: 10, Kind: "pi", Display: "idle", CWD: "/p/org", Title: "π - org"},
			want:  indent + "/p/org · π - org",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			line2 := m.renderRow(0, tc.agent, 80, true)[1]
			if got := strings.TrimRight(ansi.Strip(line2), " "); got != tc.want {
				t.Fatalf("idle line 2: got %q, want %q", got, tc.want)
			}
		})
	}
}

// A prompt too long for line 2 continues on the lines under it instead of being
// cut at the right edge. Only the last line can still end in an ellipsis, and
// only once the prompt outruns maxActivityLines.
func TestRowWrapsTheActivityText(t *testing.T) {
	const prompt = "make the agent view wider by 25% and fix the left column so the " +
		"description text wraps onto the next line instead of being truncated at " +
		"the edge of the list, which loses the end of every prompt"
	cases := []struct {
		name      string
		msg       string
		inner     int
		wantLines int
		wantCut   bool
	}{
		{name: "short prompt stays on line 2", msg: "tidy the notes", inner: 80, wantLines: 2},
		{name: "long prompt wraps whole", msg: prompt, inner: 80, wantLines: 1 + maxActivityLines},
		{name: "wrapping stops at the cap", msg: strings.Repeat("word ", 200), inner: 80, wantLines: 1 + maxActivityLines, wantCut: true},
		{name: "a narrow list wraps too", msg: prompt, inner: 44, wantLines: 1 + maxActivityLines, wantCut: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := agent.Agent{ID: 1, Kind: "claude", Display: "working", CWD: "/p/cattery", Msg: tc.msg}
			m := sampleModel()
			lines := m.renderRow(0, a, tc.inner, true)

			if got := len(lines); got != tc.wantLines {
				t.Fatalf("row height: got %d lines, want %d:\n%s", got, tc.wantLines, strings.Join(lines, "\n"))
			}
			if got := rowHeight(a, tc.inner); got != len(lines) {
				t.Fatalf("rowHeight says %d, renderRow drew %d", got, len(lines))
			}
			for i, line := range lines {
				if w := ansi.StringWidth(line); w != tc.inner {
					t.Errorf("line %d is %d cells wide, want %d", i, w, tc.inner)
				}
			}

			last := strings.TrimRight(ansi.Strip(lines[len(lines)-1]), " ")
			if got := strings.HasSuffix(last, "…"); got != tc.wantCut {
				t.Errorf("last line cut: got %v, want %v: %q", got, tc.wantCut, last)
			}
			// Every word before the cut survives, in order, across the lines.
			text := strings.Join(lines[1:], " ")
			if !strings.Contains(oneLine(ansi.Strip(text)), firstWords(tc.msg, 12)) {
				t.Errorf("the prompt did not carry onto the wrapped lines:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

// The prompt's first line is narrow, because the cwd shares it with the text.
// The lines under it are wider, and have to fill that width: the wrap of the
// first line must not carry its breaks into them.
func TestWrapActivityFillsTheContinuationWidth(t *testing.T) {
	lines := wrapActivity(strings.Repeat("word ", 40), 20, 60, 3)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(lines), lines)
	}
	for i, line := range lines[1:] {
		if w := ansi.StringWidth(line); w < 55 || w > 60 {
			t.Errorf("continuation line %d is %d cells, want it to fill 60: %q", i+1, w, line)
		}
	}
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	return strings.Join(f[:min(n, len(f))], " ")
}

// The drawer takes more of the width than the list does, and the list still
// keeps enough for a full-length name. The width it opens at is unchanged: the
// share moves, the threshold does not.
func TestPreviewWidths(t *testing.T) {
	for inner := minListWidth + previewGutter + minPreviewWidth; inner < 400; inner++ {
		list, preview, ok := previewWidths(inner)
		if !ok {
			t.Fatalf("inner %d: the sidebar should fit", inner)
		}
		if list+previewGutter+preview != inner {
			t.Fatalf("inner %d: %d + %d + %d does not add up", inner, list, previewGutter, preview)
		}
		if list < minListWidth {
			t.Fatalf("inner %d: list squeezed to %d", inner, list)
		}
		if preview < minPreviewWidth {
			t.Fatalf("inner %d: sidebar squeezed to %d", inner, preview)
		}
		// At the narrowest the list holds its own minimum and the sidebar takes
		// what is left. Above that the sidebar is the wider of the two.
		if inner > 2*minListWidth+previewGutter && preview <= list {
			t.Fatalf("inner %d: sidebar %d is not wider than the list %d", inner, preview, list)
		}
	}
	if previewFits(90) {
		t.Error("the sidebar should not open at 90 columns")
	}
	if !previewFits(91) {
		t.Error("the sidebar should open at 91 columns")
	}
}

func TestViewSearchMatchCount(t *testing.T) {
	m := sampleModel()
	m.width, m.height = 120, 40
	m.search.SetValue("oauth")
	if out := m.View(); !strings.Contains(out, "1 match") {
		t.Errorf("view should show match count for an active query")
	}
}

func TestAgentsMsgPreservesSelectedID(t *testing.T) {
	m := sampleModel()
	m.selected = 1 // id 2
	updated, _ := m.Update(agentsMsg{
		generation: m.reloadGeneration,
		agents: []agent.Agent{
			{ID: 1, Display: "blocked", CWD: "/p/llm-proxy"},
			{ID: 4, Display: "done", CWD: "/p/qmp-relay"},
			{ID: 9, Display: "done", CWD: "/p/new"},
			{ID: 2, Display: "working", CWD: "/p/dotfiles"},
			{ID: 3, Display: "working", CWD: "/p/astra-l"},
		},
	})
	got := updated.(Model)
	if id := got.visible()[got.selected].ID; id != 2 {
		t.Fatalf("selected id after reorder: got %d, want 2", id)
	}
}

func TestStaleReloadResultIsIgnored(t *testing.T) {
	m := sampleModel()
	m.loading = true
	m.reloadGeneration = 2
	updated, _ := m.Update(agentsMsg{generation: 1, agents: nil})
	got := updated.(Model)
	if len(got.agents) != len(m.agents) || !got.loading {
		t.Fatalf("stale reload changed model: agents=%d loading=%v", len(got.agents), got.loading)
	}
}

func TestTickDoesNotStartOverlappingReload(t *testing.T) {
	m := sampleModel()
	m.loading = true
	generation := m.reloadGeneration
	updated, cmd := m.Update(tickMsg(time.Now()))
	got := updated.(Model)
	if got.reloadGeneration != generation || !got.loading || cmd == nil {
		t.Fatalf("tick while loading: generation=%d loading=%v cmd nil=%v", got.reloadGeneration, got.loading, cmd == nil)
	}
}

func TestFocusResult(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		quitting bool
	}{
		{name: "success", quitting: true},
		{name: "failure", err: errors.New("window gone")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.focusing = true
			updated, cmd := m.Update(focusMsg{err: tc.err})
			got := updated.(Model)
			if got.quitting != tc.quitting {
				t.Fatalf("quitting: got %v, want %v", got.quitting, tc.quitting)
			}
			if tc.err == nil {
				if cmd == nil || got.focusErr != nil {
					t.Fatalf("successful focus: cmd nil=%v err=%v", cmd == nil, got.focusErr)
				}
			} else if !errors.Is(got.focusErr, tc.err) || cmd != nil {
				t.Fatalf("failed focus: cmd nil=%v err=%v", cmd == nil, got.focusErr)
			}
		})
	}
}

func TestFocusCanBeCancelled(t *testing.T) {
	m := sampleModel()
	ctx, cancel := context.WithCancel(context.Background())
	m.focusing = true
	m.focusCancel = cancel
	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(Model)
	if !got.quitting || cmd == nil {
		t.Fatalf("esc while focusing: quitting=%v cmd nil=%v", got.quitting, cmd == nil)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("focus context was not cancelled")
	}
}

func TestKeysWhilePendingFocus(t *testing.T) {
	cases := []struct {
		name  string
		key   tea.KeyMsg
		quits bool
	}{
		{name: "q closes", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}, quits: true},
		{name: "esc closes", key: tea.KeyMsg{Type: tea.KeyEsc}, quits: true},
		{name: "ctrl+c closes", key: tea.KeyMsg{Type: tea.KeyCtrlC}, quits: true},
		{name: "down ignored", key: tea.KeyMsg{Type: tea.KeyDown}},
		{name: "j ignored", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}},
		{name: "enter ignored", key: tea.KeyMsg{Type: tea.KeyEnter}},
		{name: "f ignored", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}}},
		{name: "number ignored", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.focusing = true
			m.selected = 1
			updated, cmd := m.handleKey(tc.key)
			got := updated.(Model)
			if got.quitting != tc.quits {
				t.Fatalf("quitting: got %v, want %v", got.quitting, tc.quits)
			}
			if tc.quits {
				if cmd == nil {
					t.Fatal("close key should return a quit command")
				}
				return
			}
			if cmd != nil {
				t.Error("ignored key should not issue a command")
			}
			if got.selected != 1 || got.filter != "all" || !got.focusing {
				t.Errorf("ignored key changed state: selected=%d filter=%s focusing=%v",
					got.selected, got.filter, got.focusing)
			}
		})
	}
}

func TestPendingFocusCancelsOnClose(t *testing.T) {
	m := sampleModel()
	ctx, cancel := context.WithCancel(context.Background())
	m.focusing = true
	m.focusCancel = cancel
	if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}); ctx.Err() == nil {
		t.Fatal("q while focusing should cancel the pending focus context")
	}
}

// A jump error names one agent, so it must not outlive a move to another. It is
// keyed on the selected window rather than on keystrokes, so a key that leaves
// the cursor in place keeps the retry hint on screen.
func TestFocusErrClearsWhenSelectionChanges(t *testing.T) {
	cases := []struct {
		name     string
		selected int
		key      tea.KeyMsg
		cleared  bool
	}{
		{name: "move down", selected: 1, key: tea.KeyMsg{Type: tea.KeyDown}, cleared: true},
		{name: "move up", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}}, cleared: true},
		{name: "number select", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'4'}}, cleared: true},
		{name: "jump to top", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}, cleared: true},
		{name: "jump to end", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}}, cleared: true},
		// all -> working drops the blocked agent and resets to row 0, which
		// moves the cursor off window 1.
		{name: "cycle filter onto another agent", selected: 2, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}}, cleared: true},
		// all -> working keeps window 3 on row 0, so the retry stands.
		{name: "cycle filter onto the same agent", selected: 0, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}}},
		{name: "open search keeps error", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}}},
		{name: "unbound key keeps error", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.selected = tc.selected
			m.focusErr = errors.New("window gone")
			updated, _ := m.handleKey(tc.key)
			if got := updated.(Model).focusErr; (got == nil) != tc.cleared {
				t.Fatalf("focusErr after %s: got %v, want cleared=%v", tc.name, got, tc.cleared)
			}
		})
	}
}

func TestFocusErrClearsWhenSearchNarrowsSelection(t *testing.T) {
	m := sampleModel()
	m.searching = true
	m.search.Focus()
	m.selected = 2 // id 1, llm-proxy
	m.focusErr = errors.New("window 1 gone")

	// "f" matches dotfiles and feat/oauth but not the selected llm-proxy row,
	// so the cursor moves to another window and the stale error goes with it.
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	got := updated.(Model)
	if got.selectedKey() == "kitty:1" {
		t.Fatal("query should have moved the selection off window 1")
	}
	if got.focusErr != nil {
		t.Fatalf("focusErr after search edit: got %v, want nil", got.focusErr)
	}
}

func TestReplaceAgentsSelectionFallback(t *testing.T) {
	cases := []struct {
		name     string
		filter   string
		query    string
		selected int
		agents   []agent.Agent
		wantKey  string
	}{
		{
			name:     "selected window closes, index stays valid",
			selected: 1, // id 2
			agents: []agent.Agent{
				{ID: 1, Display: "blocked"},
				{ID: 3, Display: "working"},
				{ID: 4, Display: "done"},
			},
			wantKey: "kitty:3", // same index, next agent down
		},
		{
			name:     "selected window closes at the end, clamps",
			selected: 3, // id 4
			agents: []agent.Agent{
				{ID: 1, Display: "blocked"},
				{ID: 2, Display: "working"},
			},
			wantKey: "kitty:2",
		},
		{
			name:     "selection survives under an active filter",
			filter:   "working",
			selected: 1, // id 2
			agents: []agent.Agent{
				{ID: 9, Display: "working", CWD: "/p/new"},
				{ID: 2, Display: "working", CWD: "/p/dotfiles"},
				{ID: 3, Display: "working", CWD: "/p/astra-l"},
			},
			wantKey: "kitty:2",
		},
		{
			name:     "selection survives under an active query",
			query:    "working",
			selected: 1, // id 2
			agents: []agent.Agent{
				{ID: 3, Display: "working", CWD: "/p/astra-l"},
				{ID: 1, Display: "blocked", CWD: "/p/llm-proxy"},
				{ID: 2, Display: "working", CWD: "/p/dotfiles"},
			},
			wantKey: "kitty:2",
		},
		{
			// A tmux pane and a kitty window can carry the same number, and the
			// cursor must stay on the one it was on.
			name:     "a pane id equal to a window id does not steal the cursor",
			selected: 1, // id 2
			agents: []agent.Agent{
				{ID: 2, Host: agent.HostTmux, Display: "working", CWD: "/p/dotfiles", Target: "kontora:1.%2"},
				{ID: 2, Display: "working", CWD: "/p/dotfiles"},
			},
			wantKey: "kitty:2",
		},
		{
			name:     "empty inventory resets to the empty state",
			selected: 2,
			agents:   nil,
			wantKey:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			if tc.filter != "" {
				m.filter = tc.filter
			}
			if tc.query != "" {
				m.search.SetValue(tc.query)
			}
			m.selected = tc.selected

			m.replaceAgents(tc.agents)

			if got := m.selectedKey(); got != tc.wantKey {
				t.Fatalf("selected agent: got %q, want %q", got, tc.wantKey)
			}
			if vis := m.visible(); len(vis) > 0 && (m.selected < 0 || m.selected >= len(vis)) {
				t.Fatalf("selection %d outside visible list of %d", m.selected, len(vis))
			}
		})
	}
}

func TestSearchCtrlCQuits(t *testing.T) {
	m := sampleModel()
	m.searching = true
	m.search.Focus()
	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	got := updated.(Model)
	if !got.quitting || cmd == nil {
		t.Fatalf("ctrl+c in search: quitting=%v cmd nil=%v", got.quitting, cmd == nil)
	}
}

// The one-shot check reads the directory setup installs into. Any other
// directory makes the warning name a fix that changes nothing.
func TestCheckAssetsReadsTheKittyDirectory(t *testing.T) {
	cases := []struct {
		name    string
		install bool
		want    bool
	}{
		{name: "an install that does not match this binary", install: true, want: true},
		{name: "nothing installed", install: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("KITTY_CONFIG_DIRECTORY", dir)
			if tc.install {
				for _, name := range cattery.ManagedFiles {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("# stale\n"), 0o644); err != nil {
						t.Fatalf("write %s: %v", name, err)
					}
				}
			}
			msg, ok := checkAssets()().(staleMsg)
			if !ok {
				t.Fatalf("checkAssets returned %T, want staleMsg", msg)
			}
			if msg.stale != tc.want {
				t.Fatalf("stale: got %v, want %v", msg.stale, tc.want)
			}
		})
	}
}

func TestStaleMsgSetsTheFlag(t *testing.T) {
	for _, stale := range []bool{true, false} {
		m := sampleModel()
		updated, cmd := m.Update(staleMsg{stale: stale})
		if got := updated.(Model).staleAssets; got != stale {
			t.Errorf("staleAssets after staleMsg{%v}: got %v", stale, got)
		}
		// One shot. The check does not reschedule itself.
		if cmd != nil {
			t.Errorf("staleMsg{%v} returned a command", stale)
		}
	}
}

func TestViewLoadStates(t *testing.T) {
	cases := []struct {
		name   string
		set    func(*Model)
		want   string
		absent []string
	}{
		{
			name: "loading",
			set:  func(m *Model) { m.loaded = false },
			want: "loading agents",
			absent: []string{
				"no agents.",                 // loading is not an empty inventory
				"stale or missing",           // nor stale data
				"remote control unavailable", // nor a fatal failure
			},
		},
		{
			name:   "empty inventory",
			set:    func(m *Model) { m.agents = nil },
			want:   "nothing has published agent state yet",
			absent: []string{"loading agents", "stale or missing", "remote control unavailable"},
		},
		{
			// A failed first load is fatal, because nothing is cached to show.
			name:   "fatal first load",
			set:    func(m *Model) { m.agents = nil; m.reloadErr = errors.New("socket unavailable") },
			want:   "kitty remote control unavailable",
			absent: []string{"loading agents", "no agents.", "stale or missing"},
		},
		{
			name:   "stale",
			set:    func(m *Model) { m.reloadErr = errors.New("socket unavailable") },
			want:   "stale or missing",
			absent: []string{"loading agents", "remote control unavailable"},
		},
		{
			name: "focus failure",
			set:  func(m *Model) { m.focusErr = errors.New("window 42: no matching window") },
			want: "jump failed",
		},
		{
			name: "focus failure names the window and retry",
			set:  func(m *Model) { m.focusErr = errors.New("window 42: no matching window") },
			want: "window 42",
		},
		{
			name: "focus failure offers a retry",
			set:  func(m *Model) { m.focusErr = errors.New("window 42: no matching window") },
			want: "press enter to retry",
		},
		{
			// A binary upgraded without a second `cattery setup` leaves the
			// installed kitty files behind, and nothing else reports it.
			name:   "stale kitty files",
			set:    func(m *Model) { m.staleAssets = true },
			want:   "kitty files are out of date · run cattery setup",
			absent: []string{"stale or missing"},
		},
		{
			name:   "fresh kitty files say nothing",
			set:    func(m *Model) { m.staleAssets = false },
			absent: []string{"out of date"},
			want:   "astra-l", // a normal list, with no warning row
		},
		{
			// One warning row, and the failed refresh owns it. It explains the
			// rows on screen; stale files are a background chore.
			name:   "a failed refresh outranks stale files",
			set:    func(m *Model) { m.staleAssets = true; m.reloadErr = errors.New("socket unavailable") },
			want:   "stale or missing",
			absent: []string{"out of date"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = 120, 40
			tc.set(&m)
			out := ansi.Strip(m.View())
			if !strings.Contains(out, tc.want) {
				t.Fatalf("view missing %q, got:\n%s", tc.want, out)
			}
			for _, a := range tc.absent {
				if strings.Contains(out, a) {
					t.Errorf("view should not contain %q, got:\n%s", a, out)
				}
			}
		})
	}
}

func TestViewFitsTerminal(t *testing.T) {
	sizes := []struct{ width, height int }{
		{120, 40},
		{80, 24},
		{80, 14}, // full-header layout, minimum height
		{80, 13}, // compact-header layout, maximum height
		{60, 14},
		{40, 14},
		{40, 13},
		{40, 10},
		{20, 7}, // smallest supported terminal
		{19, 7}, // below the supported width
		{19, 6},
		{1, 1},
	}
	// Each variant stresses a different source of over-long content.
	variants := []struct {
		name string
		set  func(*Model)
	}{
		{name: "plain", set: func(*Model) {}},
		{name: "idle filter", set: func(m *Model) { m.filter = "idle" }},
		{name: "done filter", set: func(m *Model) { m.filter = "done" }},
		{name: "search", set: func(m *Model) { m.search.SetValue("oauth") }},
		{name: "search no match", set: func(m *Model) { m.search.SetValue("zzz") }},
		{
			name: "filtered search no match",
			set:  func(m *Model) { m.filter = "done"; m.search.SetValue("zzz") },
		},
		{name: "loading", set: func(m *Model) { m.loaded = false }},
		{
			name: "fatal reload",
			set:  func(m *Model) { m.agents = nil; m.reloadErr = errors.New("kitty socket unavailable at /tmp/mykitty") },
		},
		{name: "stale cache", set: func(m *Model) { m.reloadErr = errors.New("socket unavailable") }},
		{name: "stale kitty files", set: func(m *Model) { m.staleAssets = true }},
		{
			name: "focus failure",
			set:  func(m *Model) { m.focusErr = errors.New("window 42: no matching window found for id:42") },
		},
		{
			// Both banners plus rows squeeze the body to its minimum.
			name: "both banners",
			set: func(m *Model) {
				m.focusErr = errors.New("window 42: gone")
				m.reloadErr = errors.New("socket unavailable")
			},
		},
		{
			name: "multiline error",
			set:  func(m *Model) { m.focusErr = errors.New("first line\nsecond line\nthird line") },
		},
		{name: "wide unicode", set: func(m *Model) { m.agents = wideAgents() }},
		{name: "scrolled", set: func(m *Model) { m.agents = manyAgents(30); m.selected = 25 }},
		{name: "stale selection", set: func(m *Model) { m.selected = 99 }},
		{
			// The sidebar draws a captured screen the picker did not write. It
			// has to hold its column at every size, including the ones too
			// narrow to open it at all.
			name: "preview",
			set: func(m *Model) {
				m.preview = previewRead
				m.previewKey = m.selectedKey()
				m.previewScreen = "\x1b[31m" + strings.Repeat("wide ", 60) + "\n\x1b]0;title\x07\x1b[2Jhello\n界界界界"
			},
		},
		{name: "preview loading", set: func(m *Model) { m.preview = previewRead }},
		{
			name: "preview failed",
			set: func(m *Model) {
				m.preview = previewRead
				m.previewKey = m.selectedKey()
				m.previewErr = errors.New("window 42: no matching window found for id:42")
			},
		},
		{
			name: "preview with banners",
			set: func(m *Model) {
				m.preview = previewRead
				m.previewKey = m.selectedKey()
				m.previewScreen = "hello"
				m.focusErr = errors.New("window 42: gone")
				m.reloadErr = errors.New("socket unavailable")
			},
		},
		{
			name: "preview of nothing selected",
			set:  func(m *Model) { m.preview = previewRead; m.filter = "idle" },
		},
	}
	for _, size := range sizes {
		for _, v := range variants {
			t.Run(fmt.Sprintf("%dx%d/%s", size.width, size.height, v.name), func(t *testing.T) {
				m := sampleModel()
				m.width, m.height = size.width, size.height
				v.set(&m)
				lines := strings.Split(m.View(), "\n")
				if len(lines) != size.height {
					t.Fatalf("line count: got %d, want %d", len(lines), size.height)
				}
				for i, line := range lines {
					if width := ansi.StringWidth(line); width > size.width {
						t.Errorf("line %d width: got %d, max %d\n%q", i+1, width, size.width, line)
					}
				}
			})
		}
	}
}

// wideAgents exercises cell-width truncation with CJK, emoji ZWJ sequences, and
// combining marks in every field the row renders.
func wideAgents() []agent.Agent {
	return []agent.Agent{
		{
			ID: 1, Kind: "claude", Display: "blocked",
			CWD:    "/p/项目名称很长很长很长很长/子目录",
			Branch: "功能/认证",
			Msg:    "请帮我实现一个非常复杂的认证系统👩‍💻👨‍👩‍👧‍👦",
			Title:  "ééé combining",
		},
		{
			ID: 2, Kind: "pi", Display: "working",
			CWD:    "/p/🚀🚀🚀",
			Branch: "feat/🔥",
			Msg:    strings.Repeat("界", 200),
		},
	}
}

func manyAgents(n int) []agent.Agent {
	agents := make([]agent.Agent, 0, n)
	for i := range n {
		agents = append(agents, agent.Agent{
			ID:      i + 1,
			Kind:    "pi",
			Display: "working",
			CWD:     fmt.Sprintf("/p/project-%02d", i),
			Branch:  "main",
		})
	}
	return agents
}

func TestViewFilterTabs(t *testing.T) {
	cases := []struct {
		name       string
		width      int
		filter     string
		want       []string
		wantAbsent []string
	}{
		{
			name:  "all tabs fit at 120 columns",
			width: 120, filter: "all",
			want: []string{"all", "working", "blocked", "done", "idle"},
		},
		{
			name:  "idle stays visible when tabs collapse",
			width: 40, filter: "idle",
			want:       []string{"idle", "f next"},
			wantAbsent: []string{"blocked"},
		},
		{
			name:  "done stays visible when tabs collapse",
			width: 40, filter: "done",
			want:       []string{"done", "f next"},
			wantAbsent: []string{"blocked"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = tc.width, 24
			m.filter = tc.filter
			out := ansi.Strip(m.View())
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("view missing %q, got:\n%s", w, out)
				}
			}
			for _, w := range tc.wantAbsent {
				if strings.Contains(out, w) {
					t.Errorf("collapsed tabs should not show %q, got:\n%s", w, out)
				}
			}
		})
	}
}

func TestViewEmptyStates(t *testing.T) {
	cases := []struct {
		name   string
		set    func(*Model)
		want   []string
		absent []string
	}{
		{
			name: "no inventory at all",
			set:  func(m *Model) { m.agents = nil },
			want: []string{"no agents.", "nothing has published agent state yet"},
		},
		{
			name:   "filter excludes everything",
			set:    func(m *Model) { m.filter = "idle" },
			want:   []string{"no idle agents.", "press f to change filter"},
			absent: []string{"nothing has published agent state yet"},
		},
		{
			name: "query excludes everything",
			set:  func(m *Model) { m.search.SetValue("zzz-nope") },
			want: []string{`no agents match "zzz-nope".`, "press esc to clear the search"},
		},
		{
			name: "filter and query exclude everything",
			set:  func(m *Model) { m.filter = "done"; m.search.SetValue("zzz-nope") },
			want: []string{`no done agents match "zzz-nope".`, "press esc to clear the search", "f to change filter"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = 120, 40
			tc.set(&m)
			out := ansi.Strip(m.View())
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("view missing %q, got:\n%s", w, out)
				}
			}
			for _, w := range tc.absent {
				if strings.Contains(out, w) {
					t.Errorf("view should not contain %q, got:\n%s", w, out)
				}
			}
		})
	}
}

func TestSearchMatchTextNamesFilterScope(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		query  string
		want   string
	}{
		{name: "no query", filter: "all", want: ""},
		{name: "all scope plural", filter: "all", query: "p/", want: "4 matches"},
		{name: "all scope singular", filter: "all", query: "oauth", want: "1 match"},
		{name: "filtered scope", filter: "working", query: "oauth", want: "1 match in working"},
		{name: "filtered scope empty", filter: "done", query: "zzz", want: "0 matches in done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.filter = tc.filter
			m.search.SetValue(tc.query)
			if got := m.searchMatchText(); got != tc.want {
				t.Fatalf("searchMatchText: got %q, want %q", got, tc.want)
			}
		})
	}
}

// Position leads the footer summary. Truncation takes the right side first, and
// the position matters most in a scrolled list.
func TestFooterKeepsPositionWhenScrolled(t *testing.T) {
	for _, width := range []int{40, 80} {
		t.Run(fmt.Sprintf("%d columns", width), func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = width, 24
			m.agents = manyAgents(30)
			m.selected = 25

			out := ansi.Strip(m.View())
			if !strings.Contains(out, "26/30") {
				t.Errorf("footer missing selected position, got:\n%s", out)
			}

			// The selected row must also stay inside the rendered viewport.
			if !strings.Contains(out, "project-25") {
				t.Errorf("selected row scrolled out of view, got:\n%s", out)
			}
		})
	}
}

// The compact boundary keeps the four things needed to act: which filter is
// applied, which row is selected, where it sits, and how to get out.
func TestViewKeepsEssentialsWhenCompact(t *testing.T) {
	sizes := []struct{ width, height int }{
		{20, 7},
		{40, 13},
		{40, 14},
		{80, 13},
		{80, 14},
	}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = size.width, size.height
			m.filter = "working"
			m.selected = 0 // first working agent, id 3

			out := ansi.Strip(m.View())
			for _, want := range []string{
				"working",   // active filter stays visible
				"▌",         // the selected row is marked
				"ast",       // and is identifiable, even truncated
				"esc close", // close guidance survives whole
				"1/2",       // selected/visible position
			} {
				if !strings.Contains(out, want) {
					t.Errorf("compact view missing %q, got:\n%s", want, out)
				}
			}
		})
	}
}

func TestPickHints(t *testing.T) {
	tiers := []string{"aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbb", "cc"}
	cases := []struct {
		name       string
		rightWidth int
		inner      int
		want       string
	}{
		{name: "widest tier fits", rightWidth: 5, inner: 40, want: "aaaaaaaaaaaaaaaaaaaa"},
		{name: "falls to middle tier", rightWidth: 5, inner: 16, want: "bbbbbbbbbb"},
		{name: "falls to shortest tier", rightWidth: 20, inner: 9, want: "cc"},
		{name: "keeps shortest when nothing fits", rightWidth: 20, inner: 1, want: "cc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickHints(tiers, tc.rightWidth, tc.inner); got != tc.want {
				t.Fatalf("pickHints: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFitPair(t *testing.T) {
	left := []string{"AGENTS", "AG"}
	right := []string{"esc clear · ^c close", "esc clear"}
	cases := []struct {
		name                string
		inner               int
		wantLeft, wantRight string
	}{
		{name: "both full", inner: 40, wantLeft: "AGENTS", wantRight: "esc clear · ^c close"},
		{name: "right degrades first", inner: 18, wantLeft: "AGENTS", wantRight: "esc clear"},
		{name: "left degrades last", inner: 13, wantLeft: "AG", wantRight: "esc clear"},
		{name: "nothing fits keeps barest", inner: 2, wantLeft: "AG", wantRight: "esc clear"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLeft, gotRight := fitPair(left, right, tc.inner)
			if gotLeft != tc.wantLeft || gotRight != tc.wantRight {
				t.Fatalf("fitPair: got (%q, %q), want (%q, %q)", gotLeft, gotRight, tc.wantLeft, tc.wantRight)
			}
		})
	}
}

func TestComposeLine(t *testing.T) {
	sp := func(text string) []span { return []span{{text, cText, false, ""}} }
	cases := []struct {
		name        string
		left, right []span
		inner       int
		want        string
	}{
		{name: "exact fit", left: sp("abcd"), inner: 4, want: "abcd"},
		{name: "pads short left", left: sp("ab"), inner: 4, want: "ab  "},
		{name: "truncates overflowing left", left: sp("abcdefgh"), inner: 4, want: "abc…"},
		{name: "left and right with gap", left: sp("ab"), right: sp("yz"), inner: 6, want: "ab  yz"},
		{name: "clamps right to a third", left: sp("abcdef"), right: sp("wxyz"), inner: 9, want: "abcd… wx…"},
		{name: "zero width", left: sp("abc"), inner: 0, want: ""},
		{name: "negative width", left: sp("abc"), inner: -3, want: ""},
		{name: "single cell", left: sp("abc"), inner: 1, want: "…"},
		{name: "wide runes fit", left: sp("界界"), inner: 4, want: "界界"},
		// The ellipsis cannot split a wide rune, so the line pads out to inner.
		{name: "wide runes truncate", left: sp("界界界"), inner: 4, want: "界… "},
		{name: "wide runes pad odd width", left: sp("界"), inner: 3, want: "界 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ansi.Strip(composeLine(tc.left, tc.right, tc.inner, ""))
			if got != tc.want {
				t.Fatalf("composeLine: got %q, want %q", got, tc.want)
			}
			if tc.inner > 0 && ansi.StringWidth(got) > tc.inner {
				t.Fatalf("composeLine exceeded inner: %d > %d", ansi.StringWidth(got), tc.inner)
			}
		})
	}
}

func TestOneLine(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{name: "single line unchanged", in: "window 42: gone", want: "window 42: gone"},
		{name: "collapses newlines", in: "first\nsecond\nthird", want: "first second third"},
		{name: "collapses tabs and runs", in: "a\t\tb   c", want: "a b c"},
		{name: "trims surrounding space", in: "  padded  \n", want: "padded"},
		{name: "empty", in: " \n\t ", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := oneLine(tc.in); got != tc.want {
				t.Fatalf("oneLine: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTruncateUsesTerminalCells(t *testing.T) {
	cases := []struct {
		name string
		text string
		max  int
		want string
	}{
		{name: "ascii", text: "abcdef", max: 4, want: "abc…"},
		{name: "wide", text: "界界界", max: 3, want: "界…"},
		{name: "grapheme", text: "👩‍💻abc", max: 3, want: "👩‍💻…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncate(tc.text, tc.max); got != tc.want {
				t.Fatalf("truncate: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShortenHomeRequiresPathBoundary(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := home + "-other/project"
	if got := shortenHome(path); got != path {
		t.Fatalf("shortenHome(%q): got %q", path, got)
	}
}

// --- session keys ---------------------------------------------------------

// runes builds the key press for a single character.
func runes(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// sessionModel points the snapshot path at a scratch file, so pressing "s" in a
// test cannot touch the real one. It puts a snapshot there for restore to read.
func sessionModel(t *testing.T, client *fakeClient) Model {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.kitty-session")
	body := "\nnew_tab\ncd /tmp\nlaunch '--var=AGENT_RESUME=pi --session /tmp/a.jsonl'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CATTERY_SESSION_FILE", path)
	m := sampleModel()
	// Snapshots go to kitty alone, so "s" and "R" talk to the snapshot client
	// rather than the one listing agents.
	m.snapshots = client
	return m
}

// press sends one key and runs whatever command it produced, so the test sees
// the result the way Bubble Tea would deliver it.
func press(t *testing.T, m Model, key tea.KeyMsg) (Model, tea.Msg) {
	t.Helper()
	updated, cmd := m.handleKey(key)
	got := updated.(Model)
	if cmd == nil {
		return got, nil
	}
	return got, cmd()
}

func TestSessionKeys(t *testing.T) {
	cases := []struct {
		name    string
		key     tea.KeyMsg
		action  string
		summary string
	}{
		{name: "s saves", key: runes('s'), action: "save_as_session", summary: "saved 1 tab, 0 resumable agents"},
		{name: "R restores", key: runes('R'), action: "goto_session", summary: "restored 1 tab, typed 1 of 1 resume commands"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The window restore expects to find. Without it, restore waits out
			// its readiness deadline for a window that never appears.
			client := &fakeClient{windows: []kitty.Window{
				{ID: 9, SessionName: "agents", AtPrompt: true, UserVars: map[string]string{"AGENT_RESUME": "pi --session /tmp/a.jsonl"}},
			}}
			m := sessionModel(t, client)

			started, msg := press(t, m, tc.key)
			if !started.sessionBusy {
				t.Fatal("the key did not start a snapshot")
			}
			result, ok := msg.(sessionMsg)
			if !ok {
				t.Fatalf("got %T, want a sessionMsg", msg)
			}
			if result.err != nil {
				t.Fatalf("session failed: %v", result.err)
			}
			if len(client.actions) != 1 || !strings.HasPrefix(client.actions[0], tc.action) {
				t.Fatalf("actions: got %v, want one %s", client.actions, tc.action)
			}

			// Delivering the result shows the notice and stops the busy flag.
			done, _ := started.Update(result)
			final := done.(Model)
			if final.sessionBusy {
				t.Error("still busy after the result arrived")
			}
			if final.noticeLevel != noticeOK {
				t.Errorf("notice level %d for %q, want a plain success", final.noticeLevel, final.notice)
			}
			if !strings.Contains(final.notice, tc.summary) {
				t.Errorf("notice %q does not contain %q", final.notice, tc.summary)
			}
		})
	}
}

// Restore from the picker never presses return. One keystroke is easy to hit by
// accident, and a resume command can point at a session that is gone.
func TestPickerRestoreDoesNotRunAgents(t *testing.T) {
	client := &fakeClient{
		windows: []kitty.Window{
			{ID: 9, SessionName: "agents", AtPrompt: true, UserVars: map[string]string{"AGENT_RESUME": "pi --session /tmp/a.jsonl"}},
		},
	}
	m := sessionModel(t, client)

	_, msg := press(t, m, runes('R'))
	result, ok := msg.(sessionMsg)
	if !ok || result.err != nil {
		t.Fatalf("restore: %v (%T)", result.err, msg)
	}
	if len(client.sent) != 1 {
		t.Fatalf("sent %v, want one resume command", client.sent)
	}
	if strings.HasSuffix(client.sent[0], "\r") {
		t.Errorf("the picker ran the agent: %q", client.sent[0])
	}
	if client.sent[0] != "pi --session /tmp/a.jsonl" {
		t.Errorf("typed %q", client.sent[0])
	}
}

func TestSessionKeyReportsFailure(t *testing.T) {
	client := &fakeClient{actionErr: errors.New("no listening socket")}
	m := sessionModel(t, client)

	_, msg := press(t, m, runes('s'))
	result := msg.(sessionMsg)
	if result.err == nil {
		t.Fatal("expected the kitty failure to come back")
	}

	done, cmd := m.Update(result)
	final := done.(Model)
	if final.noticeLevel != noticeErr {
		t.Error("the notice is not marked an error")
	}
	if !strings.Contains(final.notice, "no listening socket") {
		t.Errorf("notice %q lost kitty's reason", final.notice)
	}
	if cmd == nil {
		t.Fatal("an error notice still needs a tick to clear it")
	}
}

// A notice is temporary. Its tick clears the notice it belongs to and no other,
// so a second action before the first tick keeps its own message.
func TestNoticeExpiry(t *testing.T) {
	m := sampleModel()

	shown, _ := m.Update(sessionMsg{summary: "saved 3 tabs"})
	first := shown.(Model)
	if first.notice == "" {
		t.Fatal("no notice after a result")
	}

	cleared, _ := first.Update(noticeExpiredMsg{id: first.noticeID})
	if got := cleared.(Model); got.notice != "" {
		t.Errorf("the tick did not clear the notice: %q", got.notice)
	}

	replaced, _ := first.Update(sessionMsg{summary: "restored 3 tabs"})
	second := replaced.(Model)
	stale, _ := second.Update(noticeExpiredMsg{id: first.noticeID})
	if got := stale.(Model); got.notice != "restored 3 tabs" {
		t.Errorf("an old tick cleared a newer notice: %q", got.notice)
	}
}

// A snapshot in flight must not stop the list refreshing, and must not queue a
// second one behind it.
func TestSessionDoesNotBlockTheReloadLoop(t *testing.T) {
	client := &fakeClient{}
	m := sessionModel(t, client)
	busy, _ := m.handleKey(runes('s'))
	model := busy.(Model)

	ticked, cmd := model.Update(tickMsg(time.Now()))
	if cmd == nil {
		t.Fatal("the reload tick stopped while a snapshot was running")
	}
	if !ticked.(Model).loading {
		t.Error("the tick did not start a reload")
	}

	// A second press while busy is dropped, never queued.
	before := len(client.actions)
	again, cmd2 := model.handleKey(runes('s'))
	if cmd2 != nil {
		t.Error("a second press started another snapshot")
	}
	if !again.(Model).sessionBusy {
		t.Error("the busy flag was lost")
	}
	if len(client.actions) != before {
		t.Errorf("actions grew to %v", client.actions)
	}
}

// The footer has to name the two keys, or nobody finds them.
func TestHintsNameTheSessionKeys(t *testing.T) {
	m := sampleModel()
	m.width, m.height = 160, 40
	hints := m.renderHints(m.width)
	for _, want := range []string{"s save", "R restore"} {
		if !strings.Contains(ansi.Strip(hints), want) {
			t.Errorf("the footer does not mention %q: %s", want, ansi.Strip(hints))
		}
	}
}

// A restore that passed its readiness deadline types fewer commands than the
// snapshot holds and returns no error. Green with no denominator would read as
// a complete restore.
func TestPartialRestoreNoticeIsNotASuccess(t *testing.T) {
	m := sampleModel()

	shown, _ := m.Update(sessionMsg{summary: "restored 11 tabs, typed 3 of 7 resume commands", short: true})
	partial := shown.(Model)
	if partial.noticeLevel != noticeShort {
		t.Errorf("notice level %d, want the shortfall level", partial.noticeLevel)
	}
	if !strings.Contains(partial.notice, "3 of 7") {
		t.Errorf("notice %q drops the denominator", partial.notice)
	}

	full, _ := m.Update(sessionMsg{summary: "restored 11 tabs, typed 7 of 7 resume commands"})
	if got := full.(Model).noticeLevel; got != noticeOK {
		t.Errorf("a complete restore got level %d", got)
	}
}

// The footer names which key is running. "R" restores a snapshot; "s" makes
// one.
func TestSessionHintNamesTheOperation(t *testing.T) {
	cases := []struct {
		key  tea.KeyMsg
		want string
	}{
		{key: runes('s'), want: "saving"},
		{key: runes('R'), want: "restoring"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			m := sessionModel(t, &fakeClient{})
			started, _ := m.handleKey(tc.key)
			for _, tier := range started.(Model).hintTiers() {
				if !strings.Contains(tier, tc.want) {
					t.Errorf("hint %q does not say %q", tier, tc.want)
				}
			}
		})
	}
}

// --- tmux rows ---------------------------------------------------------------

// tmuxAgent is a kontora agent: a detached pane in a ticket worktree.
func tmuxAgent() agent.Agent {
	return agent.Agent{
		ID: 17, Host: agent.HostTmux, Kind: "claude", Display: "working",
		CWD: "/p/astra-l/al-67je", Project: "astra-l", ProjectKey: "/p/astra-l/.git",
		Root: "/p/astra-l/al-67je", Branch: "kontora/al-67je",
		Target: "kontora:3.%17", Msg: "run the review",
	}
}

// The chip says how Enter reaches the agent, which is the one thing the rest of
// the row cannot show.
func TestRowHostChip(t *testing.T) {
	cases := []struct {
		name string
		in   agent.Agent
		want bool
	}{
		{name: "a tmux pane is marked", in: tmuxAgent(), want: true},
		{name: "a kitty window is not", in: checkout(3, "pi", "working", "dotfiles", "main")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			for _, grouped := range []bool{true, false} {
				line1 := m.renderRow(0, tc.in, 100, grouped)[0]
				got := ansi.Strip(line1)
				if strings.Contains(got, " tmux ") != tc.want {
					t.Errorf("grouped=%v row %q: tmux chip present=%v, want %v", grouped, got, !tc.want, tc.want)
				}
				// The kind chip stays either way: the host says where the agent
				// runs, not what it is.
				if !strings.Contains(got, " "+tc.in.Kind+" ") {
					t.Errorf("grouped=%v row %q lost its kind chip", grouped, got)
				}
			}
		})
	}
}

// Enter attaches to a tmux agent instead of jumping to it, and the row and the
// footer both have to say so before it is pressed.
func TestTmuxRowSaysItAttaches(t *testing.T) {
	m := sampleModel()
	m.width, m.height = 120, 40
	m.agents = []agent.Agent{tmuxAgent()}

	line1 := m.renderRow(0, m.agents[0], 100, true)[0]
	if got := ansi.Strip(line1); !strings.Contains(got, attachHint) {
		t.Errorf("selected tmux row %q does not offer %q", got, attachHint)
	}
	if got := ansi.Strip(m.renderHints(120)); !strings.Contains(got, attachHint) {
		t.Errorf("footer %q does not offer %q", got, attachHint)
	}
	// The summary names the target, which is what `cattery attach` takes.
	if got := ansi.Strip(m.renderSummary(120)); !strings.Contains(got, "kontora:3.%17") {
		t.Errorf("summary %q does not name the target", got)
	}

	// A kitty row keeps the old wording and shows no target.
	m.agents = []agent.Agent{checkout(3, "pi", "working", "dotfiles", "main")}
	line1 = m.renderRow(0, m.agents[0], 100, true)[0]
	if got := ansi.Strip(line1); !strings.Contains(got, jumpHint) || strings.Contains(got, attachHint) {
		t.Errorf("kitty row %q should offer %q", got, jumpHint)
	}
	if got := ansi.Strip(m.renderHints(120)); !strings.Contains(got, jumpHint) || strings.Contains(got, attachHint) {
		t.Errorf("footer %q should offer %q", got, jumpHint)
	}
	if got := ansi.Strip(m.renderSummary(120)); strings.Contains(got, ":") {
		t.Errorf("kitty summary %q should carry no target", got)
	}
}

// The action is in flight: the row must not promise an attach and then report a
// jump.
func TestTmuxRowInFlightVerb(t *testing.T) {
	m := sampleModel()
	m.width, m.height = 120, 40
	m.agents = []agent.Agent{tmuxAgent()}
	m.focusing = true

	line1 := m.renderRow(0, m.agents[0], 100, true)[0]
	for name, got := range map[string]string{
		"row":       ansi.Strip(line1),
		"footer":    ansi.Strip(m.renderHints(120)),
		"title bar": ansi.Strip(m.renderTitleBar(120)),
	} {
		if !strings.Contains(got, "attach") {
			t.Errorf("%s %q says nothing about attaching", name, got)
		}
	}
}

// The chip costs cells, so the thresholds that shed the status word and the
// hint have to account for it. Every width still has to draw the row.
func TestTmuxRowDegradesWithWidth(t *testing.T) {
	m := sampleModel()
	a := tmuxAgent()
	m.agents = []agent.Agent{a}

	cases := []struct {
		name           string
		inner          int
		wantStatusWord bool
		wantHint       bool
	}{
		{name: "wide", inner: 100, wantStatusWord: true, wantHint: true},
		{name: "no room for the time and the hint", inner: minJumpHintWidth - 1, wantStatusWord: true, wantHint: true},
		{name: "the status word goes with the chip in the way", inner: minStatusWordWidth + hostChipWidth - 1},
		{name: "narrow", inner: 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line1 := m.renderRow(0, a, tc.inner, true)[0]
			got := ansi.Strip(line1)
			if width := ansi.StringWidth(got); width > tc.inner {
				t.Fatalf("row is %d cells wide, over the %d it was given: %q", width, tc.inner, got)
			}
			if strings.Contains(got, "working ") != tc.wantStatusWord {
				t.Errorf("inner=%d row %q: status word present=%v, want %v",
					tc.inner, got, !tc.wantStatusWord, tc.wantStatusWord)
			}
			if strings.Contains(got, attachHint) != tc.wantHint {
				t.Errorf("inner=%d row %q: hint present=%v, want %v", tc.inner, got, !tc.wantHint, tc.wantHint)
			}
		})
	}
}

// Every footer tier has to fit the width it is meant for, with the longer
// attach wording too.
func TestHintTiersFitWithTheAttachAction(t *testing.T) {
	m := sampleModel()
	m.agents = []agent.Agent{tmuxAgent()}

	for _, width := range []int{40, 60, 80, 100, 120} {
		m.width, m.height = width, 40
		line := ansi.Strip(m.renderHints(width))
		if got := ansi.StringWidth(line); got > width {
			t.Errorf("footer at width %d is %d cells: %q", width, got, line)
		}
	}
}

// previewModel is a picker wide enough for the sidebar, with the client the
// sidebar will ask for screens.
func previewModel(client *fakeClient) Model {
	m := New(client, &fakeClient{})
	m.loaded = true
	m.loading = false
	m.agents = []agent.Agent{
		checkout(3, "pi", "working", "astra-l", "feat/oauth"),
		checkout(2, "pi", "working", "dotfiles", "main"),
	}
	m.width, m.height = 140, 40
	return m
}

// open presses "v" and runs whatever it scheduled, up to and including the
// capture, so a test can assert on what the drawer ended up with. One "v" opens
// read-only, which is where every test about the sidebar itself belongs.
func open(t *testing.T, m Model) (Model, *fakeClient) {
	t.Helper()
	client, ok := m.client.(*fakeClient)
	if !ok {
		t.Fatalf("client is %T, not a fakeClient", m.client)
	}
	m, msg := press(t, m, runes('v'))
	switch {
	case !m.previewOpen():
		t.Fatal(`"v" did not open the drawer`)
	case m.previewWriting():
		t.Fatal(`"v" opened the drawer for typing rather than read-only`)
	}
	return drain(t, m, msg), client
}

// openWrite walks the second rung too, so the keyboard ends up on the agent.
func openWrite(t *testing.T, m Model) (Model, *fakeClient) {
	t.Helper()
	m, client := open(t, m)
	m, msg := press(t, m, runes('v'))
	if !m.previewWriting() {
		t.Fatal(`a second "v" did not go into read-write`)
	}
	return drain(t, m, msg), client
}

// drain feeds a message back and follows the commands it produces, so a
// debounce tick and the capture behind it resolve inside one test step.
//
// A batch is expanded rather than fed in whole, and the read-write refresh tick
// is dropped: it re-arms itself, so following it would never end.
func drain(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	queue := []tea.Msg{msg}
	for step := 0; len(queue) > 0; step++ {
		if step > 16 {
			t.Fatal("the preview never settled")
		}
		next := queue[0]
		queue = queue[1:]

		switch next := next.(type) {
		case nil, writeTickMsg:
			continue
		case tea.BatchMsg:
			for _, cmd := range next {
				if cmd != nil {
					queue = append(queue, cmd())
				}
			}
			continue
		}

		updated, cmd := m.Update(next)
		m = updated.(Model)
		if cmd != nil {
			queue = append(queue, cmd())
		}
	}
	return m
}

func TestPreviewToggle(t *testing.T) {
	t.Run("v opens the sidebar and captures the selected agent", func(t *testing.T) {
		m, client := open(t, previewModel(&fakeClient{screen: "building the thing"}))

		if !slices.Equal(client.previewed, []string{"kitty:3"}) {
			t.Fatalf("captured: got %v, want [kitty:3]", client.previewed)
		}
		out := ansi.Strip(m.View())
		if !strings.Contains(out, "building the thing") {
			t.Fatalf("the screen is not on display:\n%s", out)
		}
	})

	// One "v" is a look, not a hand on the keyboard. The picker keeps its keys
	// until the second press asks for read-write.
	t.Run("the first v leaves the keyboard on the picker", func(t *testing.T) {
		m, client := open(t, previewModel(&fakeClient{screen: "building the thing"}))

		moved, _ := press(t, m, runes('j'))
		if moved.selected != 1 {
			t.Fatalf("selected %d after j, want the next row", moved.selected)
		}
		if len(client.typed) != 0 {
			t.Fatalf("read-only typed %v at the agent", client.typed)
		}
	})

	t.Run("esc closes it from read-only and forgets the screen", func(t *testing.T) {
		m, _ := open(t, previewModel(&fakeClient{screen: "building the thing"}))

		closed, _ := press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
		if closed.previewOpen() {
			t.Fatal("esc did not close the drawer")
		}
		if closed.previewScreen != "" || closed.previewKey != "" {
			t.Fatalf("kept %q for %q after closing", closed.previewScreen, closed.previewKey)
		}
		if out := ansi.Strip(closed.View()); strings.Contains(out, "building the thing") {
			t.Fatalf("the screen survived the close:\n%s", out)
		}
	})

	// The drawer keeps whatever it was showing, so the step into read-write does
	// not blink through "loading…" for a screen that is already on hand.
	t.Run("a second v enters read-write from read-only", func(t *testing.T) {
		m, client := open(t, previewModel(&fakeClient{screen: "building the thing"}))
		before := len(client.previewed)

		again, _ := press(t, m, runes('v'))
		if !again.previewWriting() {
			t.Fatal(`a second "v" did not go into read-write`)
		}
		if again.previewScreen != "building the thing" {
			t.Fatalf("screen: got %q, want the one already on display", again.previewScreen)
		}
		if len(client.previewed) != before {
			t.Fatalf("captured %v again for a screen it already had", client.previewed)
		}
	})

	// The picker has one text field, and while it has focus every letter
	// belongs to the query, as it does for "f", "s" and "R".
	t.Run("v is a letter while searching", func(t *testing.T) {
		m := previewModel(&fakeClient{})
		m.searching = true
		m.search.Focus()

		typed, _ := press(t, m, runes('v'))
		if typed.previewOpen() {
			t.Fatal(`"v" opened the sidebar instead of reaching the query`)
		}
		if typed.search.Value() != "v" {
			t.Fatalf("query: got %q, want %q", typed.search.Value(), "v")
		}
	})

	// Squeezing the list to nothing serves nobody, so the refusal is explicit
	// rather than a sidebar that silently never appears.
	t.Run("a terminal too narrow refuses and says so", func(t *testing.T) {
		m := previewModel(&fakeClient{})
		m.width = 80

		narrow, msg := press(t, m, runes('v'))
		if narrow.previewOpen() {
			t.Fatal("the sidebar opened on a terminal that cannot hold it")
		}
		if !strings.Contains(narrow.notice, "too narrow") {
			t.Fatalf("notice: got %q", narrow.notice)
		}
		if _, ok := msg.(noticeExpiredMsg); !ok {
			t.Fatalf("the notice was not scheduled to clear: got %T", msg)
		}
		if client := m.client.(*fakeClient); len(client.previewed) != 0 {
			t.Fatalf("captured %v for a sidebar that never opened", client.previewed)
		}
	})
}

func TestPreviewFollowsTheSelection(t *testing.T) {
	t.Run("moving the cursor drops the screen it came from", func(t *testing.T) {
		m, client := open(t, previewModel(&fakeClient{screen: "first agent"}))

		moved, msg := press(t, m, runes('j'))
		if moved.previewScreen != "" || moved.previewKey != "" {
			t.Fatalf("the previous agent's screen survived the move: %q", moved.previewScreen)
		}
		if out := ansi.Strip(moved.View()); strings.Contains(out, "first agent") {
			t.Fatalf("one agent's screen is shown under another's name:\n%s", out)
		}

		client.screen = "second agent"
		settled := drain(t, moved, msg)
		if !slices.Equal(client.previewed, []string{"kitty:3", "kitty:2"}) {
			t.Fatalf("captured: got %v, want [kitty:3 kitty:2]", client.previewed)
		}
		if !strings.Contains(ansi.Strip(settled.View()), "second agent") {
			t.Fatal("the sidebar did not follow the cursor")
		}
	})

	// A held movement key repeats far faster than a capture, and every capture
	// is a process. Only the generation still current gets to spawn one.
	t.Run("a superseded move never spawns a capture", func(t *testing.T) {
		m, client := open(t, previewModel(&fakeClient{}))
		before := len(client.previewed)

		first, due := press(t, m, runes('j'))
		second, _ := press(t, first, runes('k'))

		updated, cmd := second.Update(due)
		if cmd != nil {
			t.Fatalf("the stale timer started work: %T", cmd())
		}
		if got := updated.(Model); got.previewKey != "" {
			t.Fatalf("a stale timer filled the sidebar with %q", got.previewKey)
		}
		if len(client.previewed) != before {
			t.Fatalf("captured %v, one per keypress", client.previewed)
		}
	})

	// A capture is a round trip to another process. The cursor can move while
	// it is out, and the answer belongs to the agent that is gone from under it.
	t.Run("a capture for another agent is discarded", func(t *testing.T) {
		m, _ := open(t, previewModel(&fakeClient{screen: "first agent"}))

		stale := previewMsg{generation: m.previewGeneration, key: "kitty:2", screen: "second agent"}
		updated, _ := m.Update(stale)
		got := updated.(Model)
		if got.previewScreen != "first agent" {
			t.Fatalf("screen: got %q, want the selected agent's", got.previewScreen)
		}
	})
}

func TestPreviewStates(t *testing.T) {
	cases := []struct {
		name   string
		client *fakeClient
		want   string
	}{
		{
			name:   "a failure names the reason on one line",
			client: &fakeClient{previewErr: errors.New("window 3: no matching window\nfor id:3")},
			want:   "no matching window",
		},
		{
			name:   "a pane with nothing on it says so",
			client: &fakeClient{screen: "\n\n   \n"},
			want:   "no output",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := open(t, previewModel(tc.client))
			out := ansi.Strip(m.View())
			if !strings.Contains(out, tc.want) {
				t.Fatalf("view missing %q:\n%s", tc.want, out)
			}
		})
	}

	t.Run("a capture in flight says so rather than showing nothing", func(t *testing.T) {
		m := previewModel(&fakeClient{})
		opened, _ := press(t, m, runes('v'))
		if out := ansi.Strip(opened.View()); !strings.Contains(out, "loading") {
			t.Fatalf("view missing the loading state:\n%s", out)
		}
	})
}

// The list keeps every line it had: the sidebar splits the body across, not
// down, so the selected row cannot be pushed off the bottom.
func TestPreviewKeepsTheListWhole(t *testing.T) {
	plain := sampleModel()
	plain.agents = manyAgents(30)
	plain.selected = 25
	plain.width, plain.height = 140, 24

	with := plain
	with.preview = previewRead
	with.previewKey = with.selectedKey()
	with.previewScreen = "a screen"

	if got, want := len(strings.Split(with.View(), "\n")), len(strings.Split(plain.View(), "\n")); got != want {
		t.Fatalf("line count with the sidebar: got %d, want %d", got, want)
	}
	if out := ansi.Strip(with.View()); !strings.Contains(out, "project-25") {
		t.Fatalf("the selected row left the list:\n%s", out)
	}
}

// Read-write only changes the colour of the frame. If it changed the geometry
// too, walking the ladder would shuffle the whole picker under the cursor, and
// the drawer would show a different amount of the agent's screen in each mode.
func TestWriteMovesNothingButTheColour(t *testing.T) {
	ro := previewModel(&fakeClient{})
	ro.preview = previewRead
	ro.previewKey = ro.selectedKey()
	ro.previewScreen = "\x1b[31mbuilding\x1b[0m the thing\nand more"

	rw := ro
	rw.preview = previewWrite
	rw.writeKey = rw.selectedKey()

	roLines, rwLines := strings.Split(ro.View(), "\n"), strings.Split(rw.View(), "\n")
	if len(roLines) != len(rwLines) {
		t.Fatalf("line count: read-only %d, read-write %d", len(roLines), len(rwLines))
	}
	for i := range roLines {
		if a, b := ansi.StringWidth(roLines[i]), ansi.StringWidth(rwLines[i]); a != b {
			t.Errorf("line %d width: read-only %d, read-write %d", i, a, b)
		}
	}

	// The hint and the footer are the two places the mode is meant to show in
	// the text. Everything else has to read the same.
	roBody, rwBody := ansi.Strip(ro.View()), ansi.Strip(rw.View())
	if strings.Contains(roBody, "^] esc") {
		t.Error("read-only advertises the escape hatch it does not need")
	}
	if !strings.Contains(rwBody, "^] esc") {
		t.Errorf("read-write does not say how to send an escape:\n%s", rwBody)
	}
	if !strings.Contains(rwBody, "typing into") {
		t.Errorf("the footer does not say the keyboard is on the agent:\n%s", rwBody)
	}

	// The drawer opens read-only, so its heading has to name the mode it is in
	// and the key out of it. Nothing else on screen says which one is running.
	if !strings.Contains(rwBody, "R/W") || !strings.Contains(rwBody, "esc read-only") {
		t.Errorf("the drawer does not say it is in read-write and how to leave:\n%s", rwBody)
	}
	if strings.Contains(roBody, "R/W") {
		t.Errorf("read-only calls itself read-write:\n%s", roBody)
	}
	if !strings.Contains(roBody, "v type") {
		t.Errorf("read-only does not offer the key that starts typing:\n%s", roBody)
	}
}

// The frame is red only while the keyboard belongs to the agent, which is what
// makes the mode legible at a glance.
func TestWriteFrameIsRed(t *testing.T) {
	cases := []struct {
		mode previewMode
		want lipgloss.Color
	}{
		{previewOff, cBorder},
		{previewRead, cBorder},
		{previewWrite, cRed},
	}
	for _, tc := range cases {
		m := previewModel(&fakeClient{})
		m.preview = tc.mode
		if got := m.previewFrameColour(); got != tc.want {
			t.Errorf("mode %v: got %q, want %q", tc.mode, got, tc.want)
		}
	}
}

// The sidebar refreshes on the same tick as the list, so a working agent is
// watched live rather than sampled when it was opened.
func TestPreviewRefreshesOnTheTick(t *testing.T) {
	m, client := open(t, previewModel(&fakeClient{screen: "first frame"}))
	client.screen = "second frame"

	updated, cmd := m.Update(tickMsg(time.Now()))
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("the tick scheduled nothing")
	}
	// The tick batches a reload, a capture and the next tick. Only the capture
	// matters here, and Update ignores the rest of the batch's order.
	for _, msg := range batched(cmd) {
		if p, ok := msg.(previewMsg); ok {
			updated, _ = m.Update(p)
			m = updated.(Model)
		}
	}
	if m.previewScreen != "second frame" {
		t.Fatalf("screen: got %q, want the fresh one", m.previewScreen)
	}
	if len(client.previewed) != 2 {
		t.Fatalf("captured %v, want one per tick", client.previewed)
	}
}

// The ladder is the only way out of read-write, and each rung gives back one
// thing: the keyboard, then the drawer, then the picker.
func TestPreviewEscLadder(t *testing.T) {
	m, _ := openWrite(t, previewModel(&fakeClient{screen: "building the thing"}))

	esc := tea.KeyMsg{Type: tea.KeyEsc}

	ro, _ := press(t, m, esc)
	if ro.preview != previewRead {
		t.Fatalf("after one esc: got mode %v, want read-only", ro.preview)
	}
	if ro.previewScreen != "building the thing" {
		t.Fatalf("the first esc dropped the screen: %q", ro.previewScreen)
	}

	closed, _ := press(t, ro, esc)
	if closed.preview != previewOff {
		t.Fatalf("after two escs: got mode %v, want closed", closed.preview)
	}

	quit, _ := press(t, closed, esc)
	if !quit.quitting {
		t.Fatal("the third esc did not close the picker")
	}
}

// q and ctrl+c still leave outright, so a drawer left open is never a thing to
// escape twice before the picker will close.
func TestPreviewQuitKeysSkipTheLadder(t *testing.T) {
	for _, key := range []tea.KeyMsg{runes('q'), {Type: tea.KeyCtrlC}} {
		t.Run(key.String(), func(t *testing.T) {
			m, _ := open(t, previewModel(&fakeClient{screen: "building the thing"}))

			quit, _ := press(t, m, key)
			if !quit.quitting {
				t.Fatalf("%s did not close the picker from an open drawer", key)
			}
		})
	}
}

// typeAt presses keys in read-write and runs the commands they produced,
// without feeding the results back into the model. A test about what is still
// queued needs the send to have run with its answer still out.
func typeAt(t *testing.T, m Model, keys ...tea.KeyMsg) Model {
	t.Helper()
	for _, key := range keys {
		next, msg := press(t, m, key)
		runCmds(msg)
		m = next
	}
	return m
}

// runCmds executes whatever a batch is carrying and discards the answers. A
// lone command has already run by the time press returns.
func runCmds(msg tea.Msg) {
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return
	}
	for _, cmd := range batch {
		if cmd != nil {
			runCmds(cmd())
		}
	}
}

func TestWriteForwardsKeys(t *testing.T) {
	// Every picker key belongs to the agent in read-write, ctrl+c included:
	// interrupting an agent is a main reason to want this.
	cases := []struct {
		name string
		key  tea.KeyMsg
		want string
	}{
		{"a letter", runes('y'), "y"},
		{"a movement key does not move the cursor", runes('j'), "j"},
		{"the quit key", runes('q'), "q"},
		{"the search key", runes('/'), "/"},
		{"the restore key", runes('R'), "R"},
		{"a row number", runes('2'), "2"},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}, "\r"},
		{"ctrl+c interrupts the agent instead of closing the picker", tea.KeyMsg{Type: tea.KeyCtrlC}, "\x03"},
		{"an arrow", tea.KeyMsg{Type: tea.KeyUp}, "\x1b[A"},
		// esc is spoken for by the ladder, so this is the only way to send one.
		{"ctrl+] carries the escape esc cannot", tea.KeyMsg{Type: tea.KeyCtrlCloseBracket}, "\x1b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, client := openWrite(t, previewModel(&fakeClient{screen: "building the thing"}))
			before := m.selectedKey()

			typed := typeAt(t, m, tc.key)

			if !slices.Equal(client.typed, []string{tc.want}) {
				t.Fatalf("sent: got %q, want [%q]", client.typed, tc.want)
			}
			if typed.selectedKey() != before {
				t.Fatalf("the cursor moved to %s", typed.selectedKey())
			}
			if typed.quitting || typed.searching || !typed.previewWriting() {
				t.Fatal("the key reached the picker as well as the agent")
			}
			if len(client.focused) != 0 {
				t.Fatalf("focused %v from read-write", client.focused)
			}
		})
	}

	t.Run("a key with no encoding sends nothing", func(t *testing.T) {
		m, client := openWrite(t, previewModel(&fakeClient{}))

		typeAt(t, m, tea.KeyMsg{Type: tea.KeyF20})

		if len(client.typed) != 0 {
			t.Fatalf("sent %q for a key with no bytes", client.typed)
		}
	})
}

// Bubble Tea runs commands on their own goroutines, so two sends in flight
// could finish the other way round and scramble what the user typed.
func TestWriteKeepsOneSendInFlight(t *testing.T) {
	m, client := openWrite(t, previewModel(&fakeClient{screen: "building the thing"}))

	// Three keys with no result fed back between them: only the first can go.
	m = typeAt(t, m, runes('a'), runes('b'), runes('c'))
	if !slices.Equal(client.typed, []string{"a"}) {
		t.Fatalf("sent: got %q, want [a] while the first was out", client.typed)
	}
	if m.pending != "bc" {
		t.Fatalf("pending: got %q, want %q", m.pending, "bc")
	}

	// The result arrives and takes everything typed meanwhile, in one send and
	// in order.
	updated, cmd := m.Update(sentMsg{key: m.writeKey})
	m = updated.(Model)
	if cmd != nil {
		drain(t, m, cmd())
	}
	if !slices.Equal(client.typed, []string{"a", "bc"}) {
		t.Fatalf("sent: got %q, want [a bc]", client.typed)
	}
	if client.maxSends > 1 {
		t.Fatalf("%d sends ran at once", client.maxSends)
	}
	if m.pending != "" {
		t.Fatalf("pending: got %q, want it drained", m.pending)
	}
}

// Answering an agent is looking at it. Opening the drawer is not: the sidebar
// has always promised that reading changes nothing.
func TestWriteMarksTheAgentSeen(t *testing.T) {
	m, client := openWrite(t, previewModel(&fakeClient{screen: "building the thing"}))
	if len(client.marked) != 0 {
		t.Fatalf("marked %v for a drawer nobody has typed into", client.marked)
	}

	m = typeAt(t, m, runes('y'))
	if !slices.Equal(client.marked, []string{"kitty:3"}) {
		t.Fatalf("marked: got %v, want [kitty:3]", client.marked)
	}

	// Once per session, not once per keystroke.
	typeAt(t, m, runes('e'))
	if !slices.Equal(client.marked, []string{"kitty:3"}) {
		t.Fatalf("marked: got %v, want the one write", client.marked)
	}
}

func TestWriteStopsWhenItCannotReachTheAgent(t *testing.T) {
	// Typing at something unreachable is worse than stopping, and the keys that
	// never went must not be banked for whatever takes its place.
	t.Run("a failed send drops to read-only and says why", func(t *testing.T) {
		m, _ := openWrite(t, previewModel(&fakeClient{screen: "building the thing"}))
		m.pending = "still queued"

		updated, cmd := m.Update(sentMsg{key: m.writeKey, err: errors.New("window 3: no matching window\nfor id:3")})
		got := updated.(Model)

		if got.previewWriting() {
			t.Fatal("a failed send left the keyboard on the agent")
		}
		if !got.previewOpen() {
			t.Fatal("a failed send closed the drawer instead of dropping to read-only")
		}
		if got.pending != "" {
			t.Fatalf("pending: got %q, want the dead target's keys dropped", got.pending)
		}
		if !strings.Contains(got.notice, "no matching window") || strings.Contains(got.notice, "\n") {
			t.Fatalf("notice: got %q", got.notice)
		}
		if cmd == nil {
			t.Fatal("the notice was not scheduled to clear")
		}
	})

	// Neither host reports a send that went nowhere, so the reload is what
	// notices the window closing under the drawer.
	t.Run("the agent leaving the list ends read-write", func(t *testing.T) {
		m, _ := openWrite(t, previewModel(&fakeClient{screen: "building the thing"}))

		updated, _ := m.Update(agentsMsg{
			generation: m.reloadGeneration,
			agents:     []agent.Agent{checkout(2, "pi", "working", "dotfiles", "main")},
		})
		got := updated.(Model)

		if got.previewWriting() {
			t.Fatalf("still typing at %s after it left the list", got.writeKey)
		}
		if !strings.Contains(got.notice, "gone") {
			t.Fatalf("notice: got %q", got.notice)
		}
	})

	// A drawer the view no longer draws is one the user would be typing into
	// blind.
	t.Run("a terminal shrinking past the threshold ends read-write", func(t *testing.T) {
		m, _ := openWrite(t, previewModel(&fakeClient{screen: "building the thing"}))

		updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
		if got := updated.(Model); got.previewWriting() {
			t.Fatal("still typing at an agent the view cannot show")
		}
	})
}

// Three things ask for captures in read-write. Without the single-flight guard
// they queue up behind each other, and a guard cleared only on the accepted
// path latches and stops the drawer refreshing for good.
func TestWriteCapturesOneAtATime(t *testing.T) {
	m, client := openWrite(t, previewModel(&fakeClient{screen: "first frame"}))
	before := len(client.previewed)

	first := m.refreshPreview()
	if first == nil {
		t.Fatal("the first refresh did not run")
	}
	if second := m.refreshPreview(); second != nil {
		t.Fatal("a second capture started while one was in flight")
	}
	first()
	if len(client.previewed) != before+1 {
		t.Fatalf("captured %v, want the one capture", client.previewed)
	}

	// A result for an agent the cursor has left is dropped, and it still has to
	// free the guard.
	updated, _ := m.Update(previewMsg{generation: m.previewGeneration, key: "kitty:99", screen: "somewhere else"})
	m = updated.(Model)
	if m.previewBusy {
		t.Fatal("a dropped result left the guard set")
	}
	next := m.refreshPreview()
	if next == nil {
		t.Fatal("the drawer stopped refreshing after one dropped result")
	}
	next()
	if len(client.previewed) != before+2 {
		t.Fatalf("captured %v", client.previewed)
	}
}

// The fast tick is the one path that would spawn captures four times a second,
// so it is the one that most needs the guard to survive the return statement it
// is written in.
func TestWriteTickHoldsTheCaptureGuard(t *testing.T) {
	m, _ := openWrite(t, previewModel(&fakeClient{screen: "first frame"}))
	m.previewBusy = false

	updated, cmd := m.Update(writeTickMsg{})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("the tick scheduled nothing")
	}
	if !m.previewBusy {
		t.Fatal("the tick started a capture without holding the guard")
	}

	// And it re-arms itself, or read-write refreshes once and never again.
	updated, _ = m.Update(previewMsg{generation: m.previewGeneration, key: m.selectedKey(), screen: "second frame"})
	m = updated.(Model)
	if m.previewBusy {
		t.Fatal("the result did not free the guard")
	}
	if !m.previewTicking {
		t.Fatal("read-write stopped ticking")
	}
}

// batched runs a command and returns the messages it produced, flattening the
// BatchMsg a tea.Batch delivers.
func batched(cmd tea.Cmd) []tea.Msg {
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	out := make([]tea.Msg, 0, len(batch))
	for _, c := range batch {
		if c != nil {
			out = append(out, c())
		}
	}
	return out
}
