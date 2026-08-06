// Package overlay implements the picker TUI, which kitty runs in an overlay
// window: a two-lines-per-agent list with status filters, search, and
// jump-to-focus.
package overlay

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/alexander-akhmetov/cattery/internal/kitty"
	"github.com/alexander-akhmetov/cattery/internal/setup"
)

// filters are the status tabs, in cycle order for the "f" key.
var filters = []string{"all", "working", "blocked", "done", "idle"}

// KittyClient is the part of kitty remote control the picker needs. It is an
// interface so the model can be tested without a live kitty instance.
type KittyClient interface {
	ListAgents(context.Context) ([]kitty.Agent, error)
	FocusWindow(context.Context, int) error
}

type (
	agentsMsg struct {
		generation uint64
		agents     []kitty.Agent
	}
	reloadErrMsg struct {
		generation uint64
		err        error
	}
	focusMsg struct{ err error }
	staleMsg struct{ stale bool }
	tickMsg  time.Time
	spinMsg  struct{}
)

// spinInterval drives the braille activity spinner on working agents. It is
// faster than the data reload so the animation stays smooth without re-running
// `kitten @ ls` and git on every frame. The tick runs only while an agent is
// working: with nothing to animate it would redraw the whole view nine times a
// second to produce the same frame.
const spinInterval = 110 * time.Millisecond

// Model is the Bubble Tea model for the picker.
type Model struct {
	client KittyClient

	agents           []kitty.Agent
	filter           string
	search           textinput.Model
	searching        bool
	selected         int
	loaded           bool
	loading          bool
	reloadGeneration uint64
	reloadErr        error
	focusErr         error
	focusing         bool
	focusCancel      context.CancelFunc

	// staleAssets is set when the installed kitty files no longer match the
	// copies this binary carries, which means the user upgraded the binary and
	// has not re-run setup.
	staleAssets bool

	width    int
	height   int
	now      time.Time
	spin     int  // braille spinner frame, advanced by spinMsg
	spinning bool // a spin tick is scheduled

	quitting bool
}

// New builds a Model with an initial reload scheduled in Init.
func New(client KittyClient) Model {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = "search by name, path, branch, prompt…"
	in.CharLimit = 120

	return Model{
		client:           client,
		filter:           "all",
		search:           in,
		loading:          true,
		reloadGeneration: 1,
		now:              time.Now(),
	}
}

// Init kicks off the first agent load, the refresh tick, and the one-shot check
// for stale kitty files. The spinner tick starts with the first working agent.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.reload(m.reloadGeneration), tick(), checkAssets())
}

// checkAssets compares the installed kitty files with the copies embedded in
// this binary, once. It is a command rather than part of New so the constructor
// does no file I/O.
func checkAssets() tea.Cmd {
	return func() tea.Msg {
		dir, err := setup.KittyDir()
		if err != nil {
			return staleMsg{}
		}
		return staleMsg{stale: setup.Stale(dir)}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func spin() tea.Cmd {
	return tea.Tick(spinInterval, func(time.Time) tea.Msg { return spinMsg{} })
}

func (m Model) reload(generation uint64) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		agents, err := client.ListAgents(ctx)
		if err != nil {
			return reloadErrMsg{generation: generation, err: err}
		}
		return agentsMsg{generation: generation, agents: agents}
	}
}

func focusCmd(ctx context.Context, client KittyClient, id int) tea.Cmd {
	return func() tea.Msg {
		return focusMsg{err: client.FocusWindow(ctx, id)}
	}
}

// Update handles messages and key input.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeSearch()
		return m, nil
	case tickMsg:
		m.now = time.Time(msg)
		if m.loading {
			return m, tick()
		}
		m.loading = true
		m.reloadGeneration++
		return m, tea.Batch(m.reload(m.reloadGeneration), tick())
	case spinMsg:
		m.spin++
		if !m.anyWorking() {
			m.spinning = false
			return m, nil
		}
		return m, spin()
	case agentsMsg:
		if msg.generation != m.reloadGeneration {
			return m, nil
		}
		m.replaceAgents(msg.agents)
		m.resizeSearch()
		m.loaded = true
		m.loading = false
		m.reloadErr = nil
		if !m.spinning && m.anyWorking() {
			m.spinning = true
			return m, spin()
		}
		return m, nil
	case reloadErrMsg:
		if msg.generation != m.reloadGeneration {
			return m, nil
		}
		m.loaded = true
		m.loading = false
		m.reloadErr = msg.err
		return m, nil
	case staleMsg:
		m.staleAssets = msg.stale
		return m, nil
	case focusMsg:
		m.cancelFocus()
		m.focusing = false
		if msg.err != nil {
			m.focusErr = msg.err
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit
	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	if m.searching {
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		m.resizeSearch()
		return m, cmd
	}
	return m, nil
}

func (m *Model) resizeSearch() {
	m.search.Width = m.searchFieldWidth(m.width - 4)
	m.search.SetCursor(m.search.Position())
}

func (m Model) searchFieldWidth(inner int) int {
	width := inner - 2 // search prompt
	if match := m.searchMatchText(); match != "" {
		width -= ansi.StringWidth(match) + 1
	}
	if width < 1 {
		return 1
	}
	return width
}

// searchMatchText is the count shown beside the query. Matches are counted
// inside the active filter, so the scope is named whenever it is not "all".
func (m Model) searchMatchText() string {
	if strings.TrimSpace(m.search.Value()) == "" {
		return ""
	}
	count := len(m.visible())
	noun := "matches"
	if count == 1 {
		noun = "match"
	}
	if m.filter != "all" {
		return fmt.Sprintf("%d %s in %s", count, noun, m.filter)
	}
	return fmt.Sprintf("%d %s", count, noun)
}

// searchActive reports whether the search field belongs on screen: while the
// user is typing, and for as long as a query keeps narrowing the list.
func (m Model) searchActive() bool {
	return m.searching || strings.TrimSpace(m.search.Value()) != ""
}

// selectedID is the kitty window under the cursor, or 0 when nothing is
// visible. It identifies the selection across filter, search, and inventory
// changes, where the row index alone does not.
func (m Model) selectedID() int {
	vis := m.visible()
	if m.selected < 0 || m.selected >= len(vis) {
		return 0
	}
	return vis[m.selected].ID
}

func (m *Model) replaceAgents(agents []kitty.Agent) {
	selectedID := m.selectedID()

	m.agents = agents
	if selectedID != 0 {
		for i, agent := range m.visible() {
			if agent.ID == selectedID {
				m.selected = i
				return
			}
		}
	}
	m.clampSelection()
}

// handleKey dispatches a key press. While a focus command is in flight only the
// close keys act; every other key waits for the result. With no focus in flight,
// a stale jump error is dropped as soon as the selection moves, so "press enter
// to retry" always names the agent the cursor is on.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.focusing {
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.cancelFocus()
			m.quitting = true
			return m, tea.Quit
		default:
			return m, nil
		}
	}

	before := m.selectedID()
	next, cmd := m.handleActiveKey(msg)
	if next.focusErr != nil && next.selectedID() != before {
		next.focusErr = nil
	}
	return next, cmd
}

func (m Model) handleActiveKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	if m.searching {
		switch msg.Type {
		case tea.KeyCtrlC:
			m.quitting = true
			return m, tea.Quit
		case tea.KeyEsc:
			m.searching = false
			m.search.Blur()
			m.search.SetValue("")
			m.clampSelection()
			m.resizeSearch()
			return m, nil
		case tea.KeyEnter:
			return m.focusSelected()
		case tea.KeyUp:
			m.move(-1)
			return m, nil
		case tea.KeyDown:
			m.move(1)
			return m, nil
		}
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		m.clampSelection()
		m.resizeSearch()
		return m, cmd
	}

	switch msg.String() {
	case "q", "esc", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "/":
		m.searching = true
		m.search.Focus()
		m.resizeSearch()
		return m, textinput.Blink
	case "f":
		m.cycleFilter()
		return m, nil
	case "j", "down":
		m.move(1)
		return m, nil
	case "k", "up":
		m.move(-1)
		return m, nil
	case "g", "home":
		m.selected = 0
		return m, nil
	case "G", "end":
		m.selected = len(m.visible()) - 1
		m.clampSelection()
		return m, nil
	case "enter":
		return m.focusSelected()
	}

	if len(msg.Runes) == 1 && msg.Runes[0] >= '1' && msg.Runes[0] <= '9' {
		if idx := int(msg.Runes[0] - '1'); idx < len(m.visible()) {
			m.selected = idx
		}
	}
	return m, nil
}

func (m Model) focusSelected() (Model, tea.Cmd) {
	vis := m.visible()
	if m.selected < 0 || m.selected >= len(vis) {
		return m, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	m.focusErr = nil
	m.focusing = true
	m.focusCancel = cancel
	return m, focusCmd(ctx, m.client, vis[m.selected].ID)
}

func (m *Model) cancelFocus() {
	if m.focusCancel != nil {
		m.focusCancel()
		m.focusCancel = nil
	}
}

func (m *Model) move(delta int) {
	n := len(m.visible())
	if n == 0 {
		return
	}
	m.selected = (m.selected + delta + n) % n
}

func (m *Model) clampSelection() {
	n := len(m.visible())
	switch {
	case n == 0:
		m.selected = 0
	case m.selected >= n:
		m.selected = n - 1
	case m.selected < 0:
		m.selected = 0
	}
}

func (m *Model) cycleFilter() {
	for i, f := range filters {
		if f == m.filter {
			m.filter = filters[(i+1)%len(filters)]
			m.selected = 0
			return
		}
	}
	m.filter = "all"
}

// anyWorking reports whether the inventory holds an agent whose row animates.
func (m Model) anyWorking() bool {
	for _, a := range m.agents {
		if a.Display == "working" {
			return true
		}
	}
	return false
}

// visible returns the agents passing the active filter and search query.
func (m Model) visible() []kitty.Agent {
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	var out []kitty.Agent
	for _, a := range m.agents {
		if m.filter != "all" && a.Display != m.filter {
			continue
		}
		if q != "" && !matches(a, q) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func matches(a kitty.Agent, q string) bool {
	hay := strings.ToLower(strings.Join([]string{
		a.Display, a.Kind, a.Project, agentName(a), a.Branch, a.CWD, a.Title, a.Msg,
	}, " "))
	return strings.Contains(hay, q)
}

// groupLabel names a project group. An agent in a directory git knows nothing
// about is grouped by that folder, so only a window with no cwd has no label.
func groupLabel(a kitty.Agent) string {
	if a.Project != "" {
		return a.Project
	}
	return "unknown"
}

// rowLabel identifies an agent inside its project group, where the heading
// already names the project. The branch does that in a normal checkout and in
// most worktrees. A detached HEAD has no branch, so the row falls back to the
// worktree directory, which is what separates it from its siblings.
func rowLabel(a kitty.Agent) string {
	if a.Branch != "" {
		return a.Branch
	}
	if a.Root != "" {
		return filepath.Base(a.Root)
	}
	return agentName(a)
}

// agentName is the agent's full identity, for rows with no project heading above
// them: the cwd basename, or the title when cwd is empty.
func agentName(a kitty.Agent) string {
	if a.CWD != "" {
		return filepath.Base(a.CWD)
	}
	return a.Title
}

// groupSize counts the agents sharing start's project from start onward. The
// list is sorted by project, so a group is one consecutive run.
func groupSize(agents []kitty.Agent, start int) int {
	n := 0
	for i := start; i < len(agents) && sameProject(agents[i], agents[start]); i++ {
		n++
	}
	return n
}

func sameProject(a, b kitty.Agent) bool {
	return a.ProjectKey == b.ProjectKey && a.Project == b.Project
}

// counts is the number of agents per status, plus "all". The keys are the
// filter names, so the status tabs and the footer index it directly.
func (m Model) counts() map[string]int {
	c := map[string]int{"all": len(m.agents)}
	for _, a := range m.agents {
		c[a.Display]++
	}
	return c
}
