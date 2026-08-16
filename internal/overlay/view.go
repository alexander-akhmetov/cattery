package overlay

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alexander-akhmetov/cattery/internal/agent"
)

// Catppuccin Mocha palette (the subset the picker uses).
var (
	cMantle     = lipgloss.Color("#181825")
	cSurface0   = lipgloss.Color("#313244")
	cSurfaceSel = lipgloss.Color("#2a2b3c")
	cBorder     = lipgloss.Color("#26273a")
	cText       = lipgloss.Color("#cdd6f4")
	cSubtext    = lipgloss.Color("#a6adc8")
	cOverlay2   = lipgloss.Color("#9399b2")
	cOverlay1   = lipgloss.Color("#7f849c")
	cOverlay0   = lipgloss.Color("#6c7086")
	cFaint      = lipgloss.Color("#45475a")
	cBlue       = lipgloss.Color("#89b4fa")
	cLavender   = lipgloss.Color("#b4befe")
	cRed        = lipgloss.Color("#f38ba8")
	cMaroon     = lipgloss.Color("#eba0ac")
	cGreen      = lipgloss.Color("#a6e3a1")
	cYellow     = lipgloss.Color("#f9e2af")
	cPeach      = lipgloss.Color("#fab387")
	cMauve      = lipgloss.Color("#cba6f7")
	cSky        = lipgloss.Color("#89dceb")
)

// spinnerFrames is the braille spinner shown on a working agent's activity line.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// Row geometry. Line 2 (cwd + activity) is indented to start under the status
// label on line 1: bar + 2-wide number + gutter + dot + gutter.
const (
	colNum    = 2 // right-aligned row number
	colStatus = 7 // "working" / "stalled" / "blocked" / "done" (padded)
	rowIndent = 1 + colNum + 1 + 1 + 1
	maxName   = 26
	maxBranch = 22
	maxCwd    = 40

	// A project heading needs its label plus a stub of rule to read as a
	// heading. Below this width it is only the label.
	minGroupRuleWidth = 4

	// A project heading takes its own line plus the blank before its first
	// row. A row takes two lines, plus one for each wrapped activity line.
	headerBlockLines = 2
	rowContentLines  = 2

	// A row's prompt wraps onto at most this many lines, and every line after
	// the first is indented past the row's status column. A prompt longer than
	// that is truncated on its last line, as before.
	maxActivityLines = 3
	activityIndent   = rowIndent + 2

	// Below this inner width the padded status word is dropped from a row. A
	// row carrying a host chip needs hostChipWidth more before it fits.
	minStatusWordWidth = 30

	// Below this inner width a row shows either its time or the action hint,
	// never both.
	minJumpHintWidth = 70

	// hostChipWidth is the width of the " tmux " chip plus the space chips draws
	// in front of it, which only a tmux row carries. The row's width thresholds
	// add it, so a narrow overlay sheds the status word before the chip crowds
	// out the agent's name.
	hostChipWidth = 7

	// Below this terminal height the chrome sheds its blank lines and the
	// footer's selected-agent summary, leaving the list every row it can.
	minRoomyHeight = 12

	// The preview sidebar's geometry. The list keeps enough for a row's indent
	// and a full-length name; the sidebar needs enough that an agent's screen is
	// worth reading. previewGutter is " │ ", the rule between the two columns.
	minListWidth    = 44
	minPreviewWidth = 40
	previewGutter   = 3
	previewShare    = 56 // percent of the inner width the sidebar asks for

	// What Enter does, per host. A kitty window is focused; a tmux pane opens a
	// read-only view, and the hint says so before the user presses it.
	jumpHint   = "\u23ce jump"
	attachHint = "\u23ce attach (ro)"
)

// actionHint is what Enter does to this agent.
func actionHint(a agent.Agent) string {
	if a.Host == agent.HostTmux {
		return attachHint
	}
	return jumpHint
}

// actionVerb is the in-flight label for the same action, so the row does not
// promise an attach and then report a jump.
func actionVerb(a agent.Agent) string {
	if a.Host == agent.HostTmux {
		return "attaching\u2026"
	}
	return "focusing\u2026"
}

// chips are the labels after a row's name: the agent kind always, and the host
// when the agent is not a kitty window. Blue keeps the host apart from the
// status colours, which own red, green, and yellow.
func chips(a agent.Agent) []span {
	out := []span{{" " + a.Kind + " ", modelColor(a.Kind), false, cSurface0}}
	if a.Host == agent.HostTmux {
		out = append(out, span{" ", cText, false, ""}, span{" tmux ", cBlue, false, cSurface0})
	}
	return out
}

func statusColor(display string) lipgloss.Color {
	switch display {
	case "blocked":
		return cRed
	case "done":
		return cGreen
	case "working":
		return cYellow
	case "stalled":
		return cMauve
	default:
		return cOverlay2
	}
}

// statusGlyph matches _AGENT_STATE_STYLE in kitty/cattery_tab.py, so a green
// dot means the same thing in the tab bar and in the picker. Only idle differs:
// the tab bar draws nothing, the picker draws a middot to hold the column.
func statusGlyph(display string) string {
	switch display {
	case "blocked":
		return "◆"
	case "stalled":
		return "◐"
	case "done", "working":
		return "●"
	default:
		return "·"
	}
}

func modelColor(kind string) lipgloss.Color {
	switch kind {
	case "claude":
		return cPeach
	case "codex":
		return cMauve
	case "pi":
		return cSky
	default:
		return cSubtext
	}
}

func activityColor(display string) lipgloss.Color {
	switch display {
	case "blocked":
		return cMaroon
	case "done":
		return cGreen
	case "stalled":
		return cMauve
	default:
		return cSubtext
	}
}

// activityGlyph leads the line-2 task with a steady status glyph. A working
// agent shows the animated braille spinner instead. An idle agent gets none,
// because its middot repeats the separator before it and reads as a typo.
//
// A stalled agent loses the spinner. An animation claims progress, which is the
// claim this state exists to doubt.
func activityGlyph(a agent.Agent, spin int) string {
	switch a.Display {
	case "working":
		return string(spinnerFrames[spin%len(spinnerFrames)])
	case "blocked", "done", "stalled":
		return statusGlyph(a.Display)
	default:
		return ""
	}
}

// toolCell is what the agent is running right now, laid into the columns the
// row has for it: the tool it published and, once that one call has run past
// minToolElapsed, how long it has taken. Empty for an agent that publishes no
// tool, which is every Claude and Codex agent and every pi between calls.
//
// The label is what gets cut, never the time. A label cut in the middle still
// names the tool, and the number is the reason the line is drawn at all: a long
// `bash` command is exactly the call worth timing, and cutting the line as one
// string drops the time off every one of them. Below minToolLabel the label
// goes alone, because a line of number and ellipsis names nothing.
func toolCell(now time.Time, a agent.Agent, width int) string {
	tool := oneLine(a.Tool)
	if tool == "" {
		return ""
	}
	if a.ToolSince.IsZero() || now.Sub(a.ToolSince) < minToolElapsed {
		return truncate(tool, width)
	}
	took := elapsed(now, a.ToolSince)
	room := width - ansi.StringWidth(took) - 1
	if room < minToolLabel {
		return truncate(tool, width)
	}
	return truncate(tool, room) + " " + took
}

// prompt is what this agent was asked to do: the message it published
// (AGENT_MSG), or the window title when it has published none.
func prompt(a agent.Agent) string {
	if msg := strings.TrimSpace(a.Msg); msg != "" {
		return msg
	}
	return strings.TrimSpace(a.Title)
}

// activity is the prompt text on line 2, or a word for the state when the agent
// has published neither a message nor a title.
func activity(a agent.Agent) string {
	if p := prompt(a); p != "" {
		return p
	}
	switch a.Display {
	case "blocked":
		return "waiting for input"
	case "done":
		return "finished"
	case "working":
		return "working"
	case "stalled":
		// Only reachable in the tick between a tool ending and the reload that
		// takes the row out of this state.
		return "may be stuck"
	default:
		// An idle agent has nothing to report, and line 1 already says "idle",
		// so line 2 drops the activity clause.
		return ""
	}
}

// timeLabel is the right-hand time on line 1, phrased per status.
func timeLabel(now time.Time, a agent.Agent) string {
	switch a.Display {
	case "blocked":
		if e := elapsed(now, a.Since); e != "" {
			return "waiting " + e
		}
		return "waiting"
	case "done":
		if ag := ago(now, a.Since); ag != "" {
			return ag
		}
		return "done"
	default:
		return elapsed(now, a.Since)
	}
}

// metaRight is the footer's right-hand summary for the selected agent.
func metaRight(now time.Time, a agent.Agent) string {
	switch a.Display {
	case "working", "stalled":
		return elapsed(now, a.Since)
	case "blocked":
		return timeLabel(now, a)
	case "done":
		if ag := ago(now, a.Since); ag != "" {
			return "finished " + ag
		}
		return "finished"
	default:
		return ""
	}
}

// minToolElapsed is how long a tool call has to have run before the row times
// it. A fast tool would otherwise flicker a single-digit second count on every
// tick, the way _agent_elapsed in kitty/cattery_tab.py shows nothing under a
// minute.
const minToolElapsed = 10 * time.Second

// minToolLabel is how many cells of the tool label a row keeps before it drops
// the elapsed time instead. Eight holds "bash: g…", which still says which tool
// is running.
const minToolLabel = 8

func elapsed(now, since time.Time) string {
	if since.IsZero() {
		return ""
	}
	d := max(now.Sub(since), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// ago is a coarse "finished N ago" form for done agents.
func ago(now, since time.Time) string {
	if since.IsZero() {
		return ""
	}
	d := max(now.Sub(since), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func shortenHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

// truncate shortens s to width terminal cells without splitting graphemes.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// span is one styled run of text on a line. bg overrides the row background for
// that run, as the model chip does. An empty bg inherits the row's.
type span struct {
	text string
	fg   lipgloss.Color
	bold bool
	bg   lipgloss.Color
}

func spansWidth(spans []span) int {
	w := 0
	for _, s := range spans {
		w += lipgloss.Width(s.text)
	}
	return w
}

func fitSpans(spans []span, width int) []span {
	if width <= 0 {
		return nil
	}
	fitted := make([]span, 0, len(spans))
	remaining := width
	for _, s := range spans {
		w := lipgloss.Width(s.text)
		if w <= remaining {
			fitted = append(fitted, s)
			remaining -= w
			continue
		}
		s.text = truncate(s.text, remaining)
		if s.text != "" {
			fitted = append(fitted, s)
		}
		break
	}
	return fitted
}

// renderSpans renders runs left to right. It applies bg, when set, to every
// run, so a highlighted row reads as one continuous band. A terminal reset
// would drop a background applied only to the outer string.
func renderSpans(spans []span, bg lipgloss.Color) string {
	var b strings.Builder
	for _, s := range spans {
		st := lipgloss.NewStyle().Foreground(s.fg).Bold(s.bold)
		cellBg := s.bg
		if cellBg == "" {
			cellBg = bg
		}
		if cellBg != "" {
			st = st.Background(cellBg)
		}
		b.WriteString(st.Render(s.text))
	}
	return b.String()
}

func bgSpaces(n int, bg lipgloss.Color) string {
	if n < 1 {
		return ""
	}
	if bg == "" {
		return strings.Repeat(" ", n)
	}
	return lipgloss.NewStyle().Background(bg).Render(strings.Repeat(" ", n))
}

// composeLine joins left and right span groups without exceeding inner cells.
// The right group keeps at most one third of a narrow line, so the status and
// the agent's identity stay visible.
func composeLine(left, right []span, inner int, bg lipgloss.Color) string {
	if inner <= 0 {
		return ""
	}
	if len(right) == 0 {
		left = fitSpans(left, inner)
		return renderSpans(left, bg) + bgSpaces(inner-spansWidth(left), bg)
	}

	rightMax := spansWidth(right)
	if spansWidth(left)+rightMax+1 > inner && rightMax > inner/3 {
		rightMax = inner / 3
	}
	right = fitSpans(right, rightMax)
	left = fitSpans(left, inner-spansWidth(right)-1)
	gap := inner - spansWidth(left) - spansWidth(right)
	return renderSpans(left, bg) + bgSpaces(gap, bg) + renderSpans(right, bg)
}

// View renders the picker full-screen: header pinned to the top, footer pinned
// to the bottom, the agent list filling the middle.
func (m Model) View() string {
	if m.quitting {
		return ""
	}
	width, height := viewWidth(m.width), viewHeight(m.height)
	if width < 20 || height < 7 {
		return smallView(width, height)
	}

	pad := lipgloss.NewStyle().PaddingLeft(2)
	inner := width - 4
	roomy := height >= minRoomyHeight
	header := m.headerLines(width, inner, roomy, pad)
	footer := []string{fullWidth(divider(inner), width), m.renderHints(width)}
	if roomy {
		footer = []string{fullWidth(divider(inner), width), m.renderSummary(width), m.renderHints(width)}
	}

	avail := height - len(header) - len(footer)
	// The sidebar splits the body horizontally and leaves the vertical
	// geometry alone, so the list keeps every line it had.
	listInner, sideInner, sidebar := inner, 0, false
	if m.previewOpen() {
		listInner, sideInner, sidebar = previewWidths(inner)
	}
	body := m.listLines(listInner, avail)
	if len(body) > avail {
		body = body[:avail]
	}
	if sidebar {
		body = m.joinPreview(body, listInner, sideInner, avail)
	}
	for i, line := range body {
		body[i] = pad.Render(line)
	}

	lines := make([]string, 0, height)
	lines = append(lines, header...)
	lines = append(lines, body...)
	for len(lines)+len(footer) < height {
		lines = append(lines, "")
	}
	lines = append(lines, footer...)
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, width, "")
	}
	return strings.Join(lines, "\n")
}

// viewWidth and viewHeight are the geometry the picker assumes before kitty has
// reported a size.
func viewWidth(w int) int {
	if w <= 0 {
		return 100
	}
	return w
}

func viewHeight(h int) int {
	if h <= 0 {
		return 30
	}
	return h
}

// previewWidths splits the inner width between the list and the sidebar. The
// third result is false when the terminal cannot hold both, and then the list
// keeps the whole width.
func previewWidths(inner int) (list, preview int, ok bool) {
	if inner < minListWidth+previewGutter+minPreviewWidth {
		return inner, 0, false
	}
	preview = max(inner*previewShare/100, minPreviewWidth)
	preview = min(preview, inner-previewGutter-minListWidth)
	return inner - previewGutter - preview, preview, true
}

// previewFits reports whether a terminal this wide can hold the sidebar. The
// key handler asks before it opens one, and the fetchers ask before they spawn
// a process for a column nobody can see.
func previewFits(width int) bool {
	_, _, ok := previewWidths(viewWidth(width) - 4)
	return ok
}

// previewFrameColour is what the drawer's box is drawn in: red while the
// keyboard belongs to the agent, and the picker's own border colour otherwise.
// The glyphs are the same either way, so the mode costs no space and moves
// nothing.
func (m Model) previewFrameColour() lipgloss.Color {
	if m.previewWriting() {
		return cRed
	}
	return cBorder
}

// joinPreview puts the drawer beside the list, one line at a time.
//
// Each list line is padded to its own column first. A row pads itself through
// composeLine, but the banner lines above the list are plain truncations, and
// an unpadded one would drag the frame left on that line alone.
//
// The box costs exactly what the plain rule cost: a space, a left edge, and a
// right edge in place of " │ ". So the geometry, and every width threshold that
// depends on it, is the same whether the drawer is boxed or not.
func (m Model) joinPreview(body []string, listWidth, previewWidth, avail int) []string {
	frame := lipgloss.NewStyle().Foreground(m.previewFrameColour())

	listLine := func(i int) string {
		if i < len(body) {
			return padTo(body[i], listWidth)
		}
		return strings.Repeat(" ", listWidth)
	}

	// Two lines is not a box. Below that the drawer keeps the plain rule it had
	// before, still recoloured, so the mode is never invisible.
	if avail < 3 {
		side := m.previewColumn(previewWidth, avail)
		rule := frame.Render("│")
		out := make([]string, avail)
		for i := range out {
			out[i] = listLine(i) + " " + rule + " " + side[i]
		}
		return out
	}

	side := m.previewColumn(previewWidth, avail-2)
	var (
		top    = frame.Render("┌" + strings.Repeat("─", previewWidth) + "┐")
		bottom = frame.Render("└" + strings.Repeat("─", previewWidth) + "┘")
		edge   = frame.Render("│")
	)

	out := make([]string, avail)
	for i := range out {
		switch i {
		case 0:
			out[i] = listLine(i) + " " + top
		case avail - 1:
			out[i] = listLine(i) + " " + bottom
		default:
			// padTo defends the right edge: a preview line already ends reset,
			// and if that ever stops being true a stray background would run
			// out through the frame rather than stop at it.
			out[i] = listLine(i) + " " + edge + padTo(side[i-1], previewWidth) + edge
		}
	}
	return out
}

// previewColumn is the sidebar: a heading naming the agent, a rule, then the
// last lines of its screen. It always returns exactly avail lines.
func (m Model) previewColumn(width, avail int) []string {
	if width < 1 || avail < 1 {
		return nil
	}
	blankLine := strings.Repeat(" ", width)

	a, ok := m.selectedAgent()
	if !ok {
		return fill(nil, blankLine, avail)
	}

	out := []string{m.previewHead(a, width)}
	if avail >= 4 {
		out = append(out, divider(width))
	}
	body := avail - len(out)
	if body < 1 {
		return out
	}

	switch {
	case m.previewErr != nil:
		return append(out, note(width, body, oneLine(m.previewErr.Error()), cRed)...)
	case m.previewKey != a.Key():
		return append(out, note(width, body, "loading…", cOverlay0)...)
	case blank(m.previewScreen):
		return append(out, note(width, body, "no output", cOverlay0)...)
	}
	return append(out, previewLines(m.previewScreen, width, body)...)
}

// previewHead names the agent and says what the drawer is doing with it. In
// read-only that is the key that starts typing. In read-write it is an "R/W"
// mark in the colour of the frame, then the two ways back out.
//
// esc is the way back out of read-write, so it is the one key the agent cannot
// be sent, and Claude and vim both want it. ctrl+] carries it instead. The hint
// is the only place that says so within reach of someone already typing, so of
// the two it is the one that survives a narrow column.
func (m Model) previewHead(a agent.Agent, width int) string {
	name := lipgloss.NewStyle().Foreground(cSubtext).Bold(true)
	dim := lipgloss.NewStyle().Foreground(cOverlay0)
	label := agentName(a)

	// Barest first for the mark, richest first for the hint: the mode is what
	// the head is for, and the keys are what it can afford to shorten.
	mark, hints := "", []string{"v type", ""}
	if m.previewWriting() {
		mark = "R/W"
		hints = []string{"^] esc · esc read-only", "^] esc · esc back", "^] esc", ""}
	}

	for _, hint := range hints {
		plain, right := mark, ""
		if mark != "" {
			right = lipgloss.NewStyle().Foreground(m.previewFrameColour()).Bold(true).Render(mark)
		}
		if plain != "" && hint != "" {
			plain, right = plain+" · ", right+dim.Render(" · ")
		}
		if hint != "" {
			plain, right = plain+hint, right+dim.Render(hint)
		}
		if plain == "" {
			break
		}
		if gap := width - ansi.StringWidth(label) - ansi.StringWidth(plain); gap >= 1 {
			return name.Render(label) + strings.Repeat(" ", gap) + right
		}
	}
	return padTo(name.Render(truncate(label, width)), width)
}

// note fills the sidebar with one dim line, for a state with no screen behind
// it: a capture in flight, an agent that could not be read, an empty pane.
func note(width, lines int, text string, colour lipgloss.Color) []string {
	if lines < 1 {
		return nil
	}
	first := padTo(lipgloss.NewStyle().Foreground(colour).Render(truncate(text, width)), width)
	return fill([]string{first}, strings.Repeat(" ", width), lines)
}

// padTo renders s as exactly width cells, cutting or padding as needed. The
// padding is plain, so it carries no background from whatever s ended with.
func padTo(s string, width int) string {
	s = ansi.Truncate(s, width, "")
	if pad := width - ansi.StringWidth(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

func fill(lines []string, blank string, n int) []string {
	for len(lines) < n {
		lines = append(lines, blank)
	}
	return lines
}

// headerLines is the top chrome: title bar, filter tabs, the search field once
// it has a query, and a rule. The title shares the bar and the search row
// appears only when used, so an untouched picker spends those lines on agents.
func (m Model) headerLines(width, inner int, roomy bool, pad lipgloss.Style) []string {
	lines := []string{m.renderTitleBar(width)}
	if roomy {
		lines = append(lines, "")
	}
	lines = append(lines, pad.Render(m.renderTabs(inner)))
	if m.searchActive() {
		lines = append(lines, pad.Render(m.renderSearch(inner)))
	}
	lines = append(lines, pad.Render(divider(inner)))
	if roomy {
		lines = append(lines, "")
	}
	return lines
}

func smallView(width, height int) string {
	lines := []string{"AGENTS", "", "terminal too small", "resize or press esc"}
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, width, "")
	}
	return strings.Join(lines, "\n")
}

func divider(width int) string {
	if width < 1 {
		width = 1
	}
	return lipgloss.NewStyle().Foreground(cBorder).Render(strings.Repeat("─", width))
}

// fullWidth left-pads content by two columns to align with the padded body.
func fullWidth(content string, width int) string {
	gap := max(width-2-lipgloss.Width(content), 0)
	return "  " + content + strings.Repeat(" ", gap)
}

// fitPair picks the richest label pair that fits on one line. It degrades the
// right side first and the left side only when it must. A truncated fragment
// ("esc clo…") carries no information, so a shorter complete label wins.
func fitPair(left, right []string, inner int) (string, string) {
	for _, l := range left {
		for _, r := range right {
			if ansi.StringWidth(l)+ansi.StringWidth(r)+1 <= inner {
				return l, r
			}
		}
	}
	return left[len(left)-1], right[len(right)-1]
}

// renderTitleBar names the picker and the key that leaves it. The counts live
// in the filter tabs and the footer summary.
func (m Model) renderTitleBar(width int) string {
	rightTiers := []string{"esc close"}
	switch {
	case m.focusing:
		verb := "focusing…"
		if a, ok := m.selectedAgent(); ok {
			verb = actionVerb(a)
		}
		rightTiers = []string{verb}
	case m.previewWriting():
		rightTiers = []string{"esc read-only"}
	case m.searching:
		rightTiers = []string{"esc clear · ^c close", "esc clear"}
	case m.previewOpen():
		rightTiers = []string{"esc close preview", "esc preview"}
	}

	inner := width - 4
	leftText, rightText := fitPair([]string{"AGENTS", "AG"}, rightTiers, inner)
	left := span{leftText, cText, true, ""}
	right := span{rightText, cOverlay1, false, ""}
	body := composeLine([]span{left}, []span{right}, inner, cMantle)
	line := bgSpaces(2, cMantle) + body + bgSpaces(2, cMantle)
	return lipgloss.NewStyle().Background(cMantle).MaxWidth(width).Render(line)
}

func styleFB(fg, bg lipgloss.Color) lipgloss.Style {
	st := lipgloss.NewStyle().Foreground(fg)
	if bg != "" {
		st = st.Background(bg)
	}
	return st
}

// renderTabs shows every status tab when the row fits. Otherwise the tabs
// collapse to the active filter plus the key that cycles them, so a narrow
// terminal still shows which filter is hiding rows.
func (m Model) renderTabs(inner int) string {
	if full := m.renderAllTabs(); ansi.StringWidth(full) <= inner {
		return full
	}
	return m.renderActiveTab(inner)
}

func (m Model) renderAllTabs() string {
	parts := make([]string, 0, len(filters))
	counts := m.counts()
	for _, f := range filters {
		labelColor, countColor := cOverlay1, cOverlay1
		var bg lipgloss.Color
		if f == m.filter {
			labelColor, countColor, bg = cText, cOverlay1, cSurface0
		}
		label := styleFB(labelColor, bg).Render(f)
		count := styleFB(countColor, bg).Render(fmt.Sprintf(" %d", counts[f]))
		p := lipgloss.NewStyle().Padding(0, 1)
		if bg != "" {
			p = p.Background(bg)
		}
		parts = append(parts, p.Render(label+count))
	}
	return strings.Join(parts, " ")
}

func (m Model) renderActiveTab(inner int) string {
	active := lipgloss.NewStyle().Foreground(cText).Background(cSurface0).Padding(0, 1).
		Render(fmt.Sprintf("%s %d", m.filter, m.counts()[m.filter]))
	hint := lipgloss.NewStyle().Foreground(cOverlay1).Render(" · f next")
	if ansi.StringWidth(active)+ansi.StringWidth(hint) > inner {
		return truncate(active, inner)
	}
	return active + hint
}

func (m Model) renderSearch(inner int) string {
	prompt := lipgloss.NewStyle().Foreground(cBlue).Render("/ ")
	matchText := m.searchMatchText()
	search := m.search
	search.Width = m.searchFieldWidth(inner)
	left := prompt + search.View()
	if matchText == "" {
		return left
	}
	match := lipgloss.NewStyle().Foreground(cOverlay1).Render(matchText)
	gap := max(inner-lipgloss.Width(left)-lipgloss.Width(match), 1)
	return left + strings.Repeat(" ", gap) + match
}

// renderGroupHeader draws a project heading: the repo name, how many of its
// agents the active filter and query leave, and a rule filling the line.
func renderGroupHeader(label string, count, inner int) string {
	head := lipgloss.NewStyle().Foreground(cLavender).Bold(true).Render(truncate(label, inner)) +
		lipgloss.NewStyle().Foreground(cOverlay0).Render(fmt.Sprintf(" %d", count))
	rule := inner - lipgloss.Width(head) - 1
	if rule < minGroupRuleWidth {
		return truncate(head, inner)
	}
	return head + " " + lipgloss.NewStyle().Foreground(cBorder).Render(strings.Repeat("─", rule))
}

// block is one unit of the scrollable body: a project heading, or an agent's
// two lines. Blocks scroll whole, so the viewport edge never splits a row.
// Separators trail their block instead of leading the next one, so the viewport
// never opens on a blank line.
//
// A block knows its height without drawing anything, and windowBlocks picks the
// run that fits from heights alone. Only the blocks that reach the screen are
// rendered.
type block struct {
	render func() []string
	// count is the block's height, trailing separators included. content is the
	// height without them, and the bottom of the viewport can drop the
	// difference without losing anything.
	count   int
	content int
	// header is the project heading this block sits under, empty for a heading
	// itself. The picker redraws it at the top of the viewport once the real
	// heading has scrolled away.
	header string
}

// blocks lays the visible agents out as project headings and rows. It returns
// the blocks and the index of the block holding the selected agent.
func (m Model) blocks(vis []agent.Agent, inner int) ([]block, int) {
	var out []block
	selected := 0
	header := ""
	for i, a := range vis {
		if i == 0 || !sameProject(a, vis[i-1]) {
			header = renderGroupHeader(groupLabel(a), groupSize(vis, i), inner)
			lines := []string{header, ""}
			out = append(out, block{
				render:  func() []string { return lines },
				count:   len(lines),
				content: 1,
			})
		}
		if i == m.selected {
			selected = len(out)
		}
		// Close the last row of a group with a second blank, so the gap before a
		// heading is wider than the gap between two rows.
		trailing := 1
		if i+1 < len(vis) && !sameProject(vis[i+1], a) {
			trailing = 2
		}
		// A wrapped prompt makes a row taller than two lines, so the block takes
		// its height from the same wrap renderRow will do.
		height := rowHeight(m.now, a, inner)
		out = append(out, block{
			render: func() []string {
				lines := m.renderRow(i, a, inner, true)
				for range trailing {
					lines = append(lines, "")
				}
				return lines
			},
			count:   height + trailing,
			content: height,
			header:  header,
		})
	}
	return out, selected
}

// windowBlocks renders the run of blocks that fits in avail lines. It scrolls
// the selected block to the bottom edge, so rows arrive from below, and it
// redraws the project heading when the viewport starts mid-group.
//
// listLines calls this only with room for a heading and a row, so the selected
// block always fits and one agent row is always drawn.
func windowBlocks(blocks []block, selected, avail int) []string {
	if len(blocks) == 0 || avail <= 0 {
		return nil
	}
	if selected < 0 || selected >= len(blocks) {
		selected = 0
	}

	// A viewport starting mid-group spends a heading's worth of lines on
	// redrawing it.
	pin := func(i int) int {
		if i > 0 && blocks[i].header != "" {
			return headerBlockLines
		}
		return 0
	}

	start := selected
	used := blocks[selected].content + pin(selected)
	for start > 0 {
		next := used - pin(start) + blocks[start-1].count + pin(start-1)
		if next > avail {
			break
		}
		start--
		used = next
	}

	var out []string
	if pin(start) > 0 {
		out = append(out, blocks[start].header, "")
	}
	for i := start; i < len(blocks); i++ {
		need := blocks[i].content
		// A heading draws only when a row fits under it. Its own blank line
		// counts here, unlike a trailing one, because the row follows it.
		if blocks[i].header == "" && i+1 < len(blocks) {
			need = headerBlockLines + blocks[i+1].content
		}
		if len(out)+need > avail {
			break
		}
		for _, line := range blocks[i].render() {
			if len(out) >= avail {
				break
			}
			out = append(out, line)
		}
	}
	return out
}

// listLines lays the agent list into avail lines, grouped by project.
func (m Model) listLines(inner, avail int) []string {
	if avail <= 0 {
		return nil
	}
	if !m.loaded {
		return centeredState(inner, avail, "loading agents…", "")
	}
	if m.reloadErr != nil && len(m.agents) == 0 {
		return centeredState(inner, avail, "kitty remote control unavailable", "retrying automatically · "+oneLine(m.reloadErr.Error()))
	}

	var lines []string
	if m.focusErr != nil {
		banner := "jump failed: " + oneLine(m.focusErr.Error()) + " · press enter to retry"
		lines = append(lines, lipgloss.NewStyle().Foreground(cRed).Render(truncate(banner, inner)))
	}
	// What "s" or "R" just did, until its tick clears it.
	if m.notice != "" {
		var colour lipgloss.Color
		switch m.noticeLevel {
		case noticeOK:
			colour = cGreen
		case noticeShort:
			colour = cYellow
		case noticeErr:
			colour = cRed
		}
		lines = append(lines, lipgloss.NewStyle().Foreground(colour).Render(truncate(m.notice, inner)))
	}
	// One warning row, and a failed refresh owns it. A failed refresh explains
	// the rows on screen now; stale kitty files are a background chore.
	if warning := m.warning(); warning != "" {
		lines = append(lines, lipgloss.NewStyle().Foreground(cYellow).Render(truncate(warning, inner)))
	}
	if len(lines) >= avail {
		return lines[:avail]
	}

	vis := m.visible()
	remaining := avail - len(lines)
	if len(vis) == 0 {
		return append(lines, m.emptyState(inner, remaining)...)
	}
	m.clampSelection()
	if m.selected < 0 || m.selected >= len(vis) {
		return append(lines, m.emptyState(inner, remaining)...)
	}
	// Too short for a project heading and a row together. The list drops to the
	// selected row alone, which names itself by directory.
	if remaining < headerBlockLines+rowContentLines {
		for _, line := range m.renderRow(m.selected, vis[m.selected], inner, false) {
			if len(lines) >= avail {
				break
			}
			lines = append(lines, line)
		}
		return lines
	}

	body, selected := m.blocks(vis, inner)
	return append(lines, windowBlocks(body, selected, remaining)...)
}

// warning is the yellow line above the list, or empty. A stale install is worth
// one line, and never worth hiding why the list is out of date.
func (m Model) warning() string {
	switch {
	case m.reloadErr != nil:
		return "refresh failed · some agents may be stale or missing · retrying automatically"
	case m.staleAssets:
		return "kitty files are out of date · run cattery setup"
	default:
		return ""
	}
}

// emptyState explains why the list is empty: no inventory at all, a filter that
// excludes everything, a query that matches nothing, or both. The filter and
// query cases name the key that recovers. No key recovers from an empty
// inventory, so that case says what is missing.
func (m Model) emptyState(inner, avail int) []string {
	q := strings.TrimSpace(m.search.Value())
	filtered := m.filter != "all"

	if len(m.agents) > 0 {
		switch {
		case q != "" && filtered:
			return centeredState(inner, avail,
				fmt.Sprintf("no %s agents match %q.", m.filter, q),
				"press esc to clear the search · f to change filter")
		case q != "":
			return centeredState(inner, avail, fmt.Sprintf("no agents match %q.", q), "press esc to clear the search")
		case filtered:
			return centeredState(inner, avail, fmt.Sprintf("no %s agents.", m.filter), "press f to change filter")
		}
	}
	return centeredState(inner, avail, "no agents.", "nothing has published agent state yet")
}

// oneLine collapses text to a single terminal line. Errors reach the banners
// from several sources, and one embedded newline would push the view past its
// height budget.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func centeredState(inner, avail int, message, hint string) []string {
	message, hint = truncate(message, inner), truncate(hint, inner)
	center := func(s string, color lipgloss.Color) string {
		left := max((inner-lipgloss.Width(s))/2, 0)
		return strings.Repeat(" ", left) + lipgloss.NewStyle().Foreground(color).Render(s)
	}
	lines := []string{center(message, cOverlay2)}
	if hint != "" {
		lines = append(lines, center(hint, cOverlay1))
	}
	if len(lines) > avail {
		lines = lines[:avail]
	}
	top := (avail - len(lines)) / 2
	out := make([]string, 0, avail)
	for range top {
		out = append(out, "")
	}
	return append(out, lines...)
}

// activityWidths are the columns a row gives its prompt: what is left of line 2
// after the cwd, and what each wrapped line after it gets.
func activityWidths(a agent.Agent, inner int) (first, cont int) {
	head := rowIndent + lipgloss.Width(rowCwd(a, inner))
	if rowCwd(a, inner) != "" {
		head += 3 // " · "
	}
	if activityGlyph(a, 0) != "" {
		head += 2 // the glyph and the space after it
	}
	return max(inner-head, 1), max(inner-activityIndent, 1)
}

func rowCwd(a agent.Agent, inner int) string {
	return truncate(shortenHome(a.CWD), max(min(inner/3, maxCwd), 4))
}

// splitWrap returns the first wrapped line of s at this width and the rest of
// the text. ansi.Wrap breaks on spaces and falls back to breaking a word that
// is longer than the whole line, which a path or a URL in a prompt can be.
//
// The remainder is flattened again. It comes out of ansi.Wrap already broken at
// this width, and wrapping it a second time at a wider one would keep those
// breaks instead of filling the wider line.
func splitWrap(s string, width int) (string, string) {
	head, rest, _ := strings.Cut(ansi.Wrap(s, width, ""), "\n")
	return head, oneLine(rest)
}

// wrapActivity lays a prompt over at most maxLines lines. The first shares line
// 2 with the cwd and so is narrower; the rest are indented under it. Text past
// the last line is dropped and the line ends in an ellipsis, which is what the
// row did with the whole overflow before.
func wrapActivity(text string, first, cont, maxLines int) []string {
	if text == "" || maxLines < 1 {
		return nil
	}
	head, rest := splitWrap(text, first)
	if rest == "" {
		return []string{head}
	}
	if maxLines == 1 {
		return []string{truncate(text, first)}
	}
	out := []string{head}
	for line := range strings.SplitSeq(ansi.Wrap(rest, cont, ""), "\n") {
		if len(out) == maxLines {
			last := out[len(out)-1]
			out[len(out)-1] = truncate(last+" "+strings.TrimSpace(line), cont)
			break
		}
		out = append(out, strings.TrimSpace(line))
	}
	return out
}

// rowActivity is a row's line-2 text and the lines it wraps onto: the running
// tool, then the prompt it is running for.
//
// The tool keeps the first line to itself and is cut rather than wrapped. The
// elapsed time on it gains a cell at "10m 00s" and again at "10h 00m", and a
// number that grows inside wrapped text moves the wrap: the row would gain and
// lose a line on its own, and rowHeight and renderRow agreeing is what keeps
// the viewport from tearing.
func rowActivity(now time.Time, a agent.Agent, inner int) []string {
	first, cont := activityWidths(a, inner)
	tool := toolCell(now, a, first)
	if tool == "" {
		return wrapActivity(oneLine(activity(a)), first, cont, maxActivityLines)
	}
	// Only the prompt follows a tool: activity()'s state words would repeat the
	// tool line above in less detail.
	lines := make([]string, 0, maxActivityLines)
	lines = append(lines, tool)
	return append(lines, wrapActivity(oneLine(prompt(a)), cont, cont, maxActivityLines-1)...)
}

// rowHeight is how many lines renderRow returns for this agent, without drawing
// it. windowBlocks sizes blocks it may never render.
func rowHeight(now time.Time, a agent.Agent, inner int) int {
	return 1 + max(len(rowActivity(now, a, inner)), 1)
}

// renderRow returns the lines for one agent row: the status line, the cwd and
// prompt line, and one more line for each line the prompt wraps onto. grouped
// says whether a project heading sits above it, which decides how the row names
// itself.
func (m Model) renderRow(i int, a agent.Agent, inner int, grouped bool) []string {
	sc := statusColor(a.Display)
	selected := i == m.selected

	var bg lipgloss.Color
	bar := " "
	barColor := cOverlay0
	if selected {
		bg = cSurfaceSel
		bar = "▌"
		barColor = sc
	}

	// Line 1: number, glyph, status, name, kind chip, branch, then time and a
	// jump hint on the selected row.
	left1 := []span{
		{bar, barColor, false, ""},
		{fmt.Sprintf("%*d", colNum, i+1), cOverlay1, false, ""},
		{" ", cText, false, ""},
		{statusGlyph(a.Display), sc, false, ""},
		{" ", cText, false, ""},
	}
	// The glyph already carries the status, so a narrow row drops the padded
	// status word instead of crowding out the agent's name.
	statusWordWidth := minStatusWordWidth
	if a.Host == agent.HostTmux {
		statusWordWidth += hostChipWidth
	}
	if inner >= statusWordWidth {
		left1 = append(left1,
			span{fmt.Sprintf("%-*s", colStatus, a.Display), sc, true, ""},
			span{" ", cText, false, ""},
		)
	}
	if grouped {
		// The heading above names the project, so the row only has to identify
		// itself within that group.
		left1 = append(left1,
			span{truncate(rowLabel(a), maxName), cText, true, ""},
			span{" ", cText, false, ""},
		)
		left1 = append(left1, chips(a)...)
	} else {
		left1 = append(left1,
			span{truncate(agentName(a), maxName), cText, true, ""},
			span{" ", cText, false, ""},
		)
		left1 = append(left1, chips(a)...)
		left1 = append(left1,
			span{" ", cText, false, ""},
			span{truncate(a.Branch, maxBranch), cOverlay1, false, ""},
		)
	}
	hint := actionHint(a)
	right1 := []span{{timeLabel(m.now, a), cOverlay2, false, ""}}
	switch {
	case selected && m.focusing:
		right1 = []span{{actionVerb(a), cLavender, false, ""}}
	case selected && inner < minJumpHintWidth:
		//nolint:prealloc // every branch holds at most three spans, so a
		// preallocated capacity would be a guess.
		right1 = []span{{hint, cLavender, false, ""}}
	case selected:
		right1 = append(right1, span{"  ", cText, false, ""}, span{hint, cLavender, false, ""})
	case inner >= minJumpHintWidth:
		// Reserve this row's own hint, so the time column holds still while the
		// cursor moves onto it.
		right1 = append(right1, span{strings.Repeat(" ", lipgloss.Width(hint)+2), cText, false, ""})
	}
	line1 := composeLine(left1, right1, inner, bg)

	// Line 2: cwd · glyph + the agent's current prompt, which wraps onto the
	// lines after it rather than being cut at the right edge.
	cwd := rowCwd(a, inner)
	sep := " · "
	if cwd == "" {
		sep = ""
	}
	glyph := activityGlyph(a, m.spin)
	left2 := []span{
		{strings.Repeat(" ", rowIndent), cText, false, ""},
		{cwd, cOverlay0, false, ""},
	}
	act := rowActivity(m.now, a, inner)
	if len(act) > 0 {
		left2 = append(left2, span{sep, cFaint, false, ""})
		if glyph != "" {
			left2 = append(left2, span{glyph, sc, false, ""}, span{" ", cText, false, ""})
		}
		left2 = append(left2, span{act[0], activityColor(a.Display), false, ""})
	}

	lines := []string{line1, composeLine(left2, nil, inner, bg)}
	for n := 1; n < len(act); n++ {
		lines = append(lines, composeLine([]span{
			{strings.Repeat(" ", activityIndent), cText, false, ""},
			{act[n], activityColor(a.Display), false, ""},
		}, nil, inner, bg))
	}
	return lines
}

// renderSummary is the footer's selected-agent line: status glyph, name,
// cwd · branch, and a right-aligned status summary, on the mantle bar.
func (m Model) renderSummary(width int) string {
	inner := width - 4
	vis := m.visible()
	if m.selected < 0 || m.selected >= len(vis) {
		return lipgloss.NewStyle().Background(cMantle).MaxWidth(width).Render(bgSpaces(width, cMantle))
	}
	a := vis[m.selected]
	sc := statusColor(a.Display)
	loc := truncate(shortenHome(a.CWD), maxCwd)
	if a.Branch != "" {
		loc += " · " + truncate(a.Branch, maxBranch)
	}
	// The target is what `cattery attach` takes, and the only identity a tmux
	// agent has outside the picker. A kitty agent is reached by window id, which
	// the user never types.
	if a.Target != "" {
		loc += " · " + a.Target
	}
	left := []span{
		{statusGlyph(a.Display), sc, false, ""},
		{" ", cText, false, ""},
		{agentName(a), cText, true, ""},
		{"  ", cText, false, ""},
		{loc, cOverlay1, false, ""},
	}
	right := []span{{metaRight(m.now, a), cOverlay1, false, ""}}
	body := composeLine(left, right, inner, cMantle)
	line := bgSpaces(2, cMantle) + body + bgSpaces(2, cMantle)
	return lipgloss.NewStyle().Background(cMantle).MaxWidth(width).Render(line)
}

// hintTiers are the footer keybinds from fullest to barest. Truncating the
// longest list would cut the close key on the narrow terminals that need it, so
// a shorter complete list wins.
// The action names what Enter does to the selected agent, so it changes with
// the cursor: "jump" for a kitty window, "attach (ro)" for a tmux pane.
func (m Model) hintTiers() []string {
	action := jumpHint
	verb := "focusing"
	if a, ok := m.selectedAgent(); ok {
		action, verb = actionHint(a), strings.TrimSuffix(actionVerb(a), "…")
	}
	switch {
	case m.focusing:
		return []string{verb + " selected agent…", verb + "…"}
	case m.previewWriting():
		// Every key goes to the agent, ctrl+c included, so esc is the only way
		// back and the footer has to say so on every tier.
		name := "the agent"
		if a, ok := m.selectedAgent(); ok {
			name = agentName(a)
		}
		return []string{
			"typing into " + name + " · every key goes to it · ^] esc · esc read-only",
			"typing into " + name + " · ^] esc · esc read-only",
			"typing · ^] esc · esc read-only",
			"esc read-only",
		}
	case m.searching:
		return []string{"↑↓ move · " + action + " · esc clear · ^c close", action + " · esc clear", "esc clear"}
	case m.sessionBusy:
		// The verb names which key is running. "R" restores a snapshot; "s"
		// makes one.
		return []string{m.sessionVerb + " the session snapshot…", m.sessionVerb + "…"}
	default:
		// In read-only esc closes the drawer rather than the picker, so the
		// tiers have to name the key that does leave, or they lie.
		preview, closeKey := "v preview", "esc close"
		if m.previewOpen() {
			preview, closeKey = "v type", "esc close preview · q quit"
		}
		return []string{
			"↑↓ / j k move · 1-9 select · " + action + " · " + preview + " · / search · f filter · s save · R restore · " + closeKey,
			"j k move · " + action + " · " + preview + " · / search · f filter · s save · R restore · " + closeKey,
			"j k move · " + action + " · " + preview + " · / search · f filter · " + closeKey,
			"j k move · " + action + " · / search · f filter · " + closeKey,
			action + " · / search · " + closeKey,
			closeKey,
			"esc close",
		}
	}
}

// pickHints mirrors composeLine's split so the chosen tier renders whole.
func pickHints(tiers []string, rightWidth, inner int) string {
	for _, t := range tiers {
		w := ansi.StringWidth(t)
		right := rightWidth
		if w+right+1 > inner && right > inner/3 {
			right = inner / 3
		}
		if w <= inner-right-1 {
			return t
		}
	}
	return tiers[len(tiers)-1]
}

// renderHints is the footer keybind line with a right-aligned scan summary.
func (m Model) renderHints(width int) string {
	inner := width - 4
	c := m.counts()

	// Position leads the summary. Truncation takes the right side first, and
	// the cursor position matters most while scrolling. The working count
	// carries the stalled agents, which are still turns nobody has finished.
	summary := fmt.Sprintf("%d active · %d agents", c["working"]+c["blocked"], c["all"])
	vis := m.visible()
	if len(vis) > 0 && m.selected >= 0 && m.selected < len(vis) {
		summary = fmt.Sprintf("%d/%d · %s", m.selected+1, len(vis), summary)
	}
	right := []span{{summary, cOverlay1, false, ""}}

	hints := pickHints(m.hintTiers(), spansWidth(right), inner)
	left := []span{{hints, cOverlay1, false, ""}}
	body := composeLine(left, right, inner, cMantle)
	line := bgSpaces(2, cMantle) + body + bgSpaces(2, cMantle)
	return lipgloss.NewStyle().Background(cMantle).MaxWidth(width).Render(line)
}
