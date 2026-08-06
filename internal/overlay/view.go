package overlay

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alexander-akhmetov/cattery/internal/kitty"
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
	colStatus = 7 // "working" / "blocked" / "done" (padded)
	rowIndent = 1 + colNum + 1 + 1 + 1
	maxName   = 26
	maxBranch = 22
	maxCwd    = 40

	// A project heading needs its label plus a stub of rule to read as a
	// heading; below this width it is only the label.
	minGroupRuleWidth = 4

	// A project heading occupies its own line plus the blank separating it
	// from its first row; a row occupies two.
	headerBlockLines = 2
	rowContentLines  = 2

	// Below this inner width the padded status word is dropped from a row.
	minStatusWordWidth = 30

	// Below this inner width a row shows either its time or the jump hint,
	// never both.
	minJumpHintWidth = 70

	// Below this terminal height the chrome sheds its blank lines and the
	// footer's selected-agent summary, leaving the list every row it can.
	minRoomyHeight = 12

	jumpHint = "\u23ce jump"
)

func statusColor(display string) lipgloss.Color {
	switch display {
	case "blocked":
		return cRed
	case "done":
		return cGreen
	case "working":
		return cYellow
	default:
		return cOverlay2
	}
}

// statusGlyph matches _AGENT_STATE_STYLE in kitty/cattery_tab.py, so a green dot
// means the same thing in the tab bar and in the picker. Only idle differs: the
// tab bar draws nothing, the picker draws a middot to hold the column.
func statusGlyph(display string) string {
	switch display {
	case "blocked":
		return "◆"
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
	default:
		return cSubtext
	}
}

// activityGlyph leads the line-2 task: a steady status glyph, except a working
// agent shows the animated braille spinner. An idle agent gets none: its glyph
// is the same middot as the separator before it, which reads as a typo.
func activityGlyph(a kitty.Agent, spin int) string {
	switch a.Display {
	case "working":
		return string(spinnerFrames[spin%len(spinnerFrames)])
	case "blocked", "done":
		return statusGlyph(a.Display)
	default:
		return ""
	}
}

// activity is the prompt text on line 2. It prefers the agent's published user
// message (AGENT_MSG) and falls back to the window title.
func activity(a kitty.Agent) string {
	if msg := strings.TrimSpace(a.Msg); msg != "" {
		return msg
	}
	if title := strings.TrimSpace(a.Title); title != "" {
		return title
	}
	switch a.Display {
	case "blocked":
		return "waiting for input"
	case "done":
		return "finished"
	case "working":
		return "working"
	default:
		// An idle agent has nothing to report and the row already says "idle";
		// line 2 drops the whole activity clause rather than repeating it.
		return ""
	}
}

// timeLabel is the right-hand time on line 1, phrased per status.
func timeLabel(now time.Time, a kitty.Agent) string {
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
func metaRight(now time.Time, a kitty.Agent) string {
	switch a.Display {
	case "working":
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

// span is one styled run of text on a line. bg overrides the row background
// for that run (used for the model chip); an empty bg inherits the row's.
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

// renderSpans renders runs left-to-right; bg (when set) is applied to every run
// so a highlighted row reads as one continuous band (a terminal reset would
// otherwise drop a background applied only to the outer string).
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
// The right group keeps at most one third of narrow lines so status and agent
// identity remain visible.
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
	width := m.width
	if width <= 0 {
		width = 100
	}
	height := m.height
	if height <= 0 {
		height = 30
	}
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
	body := m.listLines(inner, avail)
	if len(body) > avail {
		body = body[:avail]
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

// headerLines is the top chrome: title bar, filter tabs, the search field when
// there is a query, and a rule. The title sits in the bar instead of on its own
// line, and the search row appears only once it has a query, so an untouched
// picker spends those lines on agents.
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

// fitPair picks the richest label pair that fits on one line, degrading the
// right side first and the left side only when it must. A truncated fragment
// ("esc clo…") carries no information, so a shorter complete label beats it.
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

// renderTitleBar names the picker and the key that leaves it. Counts live in
// the filter tabs and the footer summary, so they stay out of here.
func (m Model) renderTitleBar(width int) string {
	rightTiers := []string{"esc close"}
	switch {
	case m.focusing:
		rightTiers = []string{"focusing…"}
	case m.searching:
		rightTiers = []string{"esc clear · ^c close", "esc clear"}
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

// renderTabs shows every status tab when the row fits. When it does not, the
// tabs collapse to the active filter alone plus the key that cycles them, so a
// narrow terminal never hides which filter is hiding rows.
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
// two lines. Blocks scroll whole, so a row is never split across the top or
// bottom edge. Separators trail their block rather than leading the next one,
// which keeps the viewport from ever opening on a blank line.
//
// A block knows its height without drawing anything, and windowBlocks needs
// only heights to pick the run that fits, so render runs for the blocks that
// reach the screen and not for the rest of the list.
type block struct {
	render func() []string
	// count is the block's height, trailing separators included. content is the
	// height without them, which may be dropped at the bottom of the viewport
	// without losing anything.
	count   int
	content int
	// header is the project heading this block sits under, empty for a heading
	// itself. The picker re-draws it at the top of the viewport when the real
	// heading has scrolled away.
	header string
}

// blocks lays the visible agents out as project headings and rows. It returns
// the blocks and the index of the block holding the selected agent.
func (m Model) blocks(vis []kitty.Agent, inner int) ([]block, int) {
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
		// Close the last row of a group with a second blank, so the gap before
		// the next heading is wider than the gap between two rows.
		trailing := 1
		if i+1 < len(vis) && !sameProject(vis[i+1], a) {
			trailing = 2
		}
		out = append(out, block{
			render: func() []string {
				l1, l2 := m.renderRow(i, a, inner, true)
				lines := make([]string, 0, rowContentLines+trailing)
				lines = append(lines, l1, l2)
				for range trailing {
					lines = append(lines, "")
				}
				return lines
			},
			count:   rowContentLines + trailing,
			content: rowContentLines,
			header:  header,
		})
	}
	return out, selected
}

// windowBlocks renders the run of blocks that fits in avail lines. It scrolls
// the selected block to the bottom edge, so the newest rows a user scrolls into
// arrive from below, and keeps the project heading on screen by re-drawing it
// when the viewport starts mid-group.
//
// listLines calls this only with room for a heading and a row, so the selected
// block always fits and at least one agent row is always drawn.
func windowBlocks(blocks []block, selected, avail int) []string {
	if len(blocks) == 0 || avail <= 0 {
		return nil
	}
	if selected < 0 || selected >= len(blocks) {
		selected = 0
	}

	// A viewport starting mid-group spends a heading's worth of lines on
	// re-drawing it.
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
		// A heading at the bottom edge with no room for its first row is noise,
		// so it draws only when a row fits under it. Its own blank line counts
		// here, unlike a trailing one, because the row follows it.
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
	// One warning row, and a failed refresh owns it: it explains the rows on
	// screen right now, while stale kitty files are a background chore.
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
	// Too short for a project heading and a row together, so the list drops to
	// the selected row alone, which names itself by directory instead of
	// relying on a heading.
	if remaining < headerBlockLines+rowContentLines {
		l1, l2 := m.renderRow(m.selected, vis[m.selected], inner, false)
		lines = append(lines, l1)
		if remaining > 1 {
			lines = append(lines, l2)
		}
		return lines
	}

	body, selected := m.blocks(vis, inner)
	return append(lines, windowBlocks(body, selected, remaining)...)
}

// warning is the yellow line above the list, or empty. A stale install is worth
// saying once, but not at the cost of hiding why the list is out of date.
func (m Model) warning() string {
	switch {
	case m.reloadErr != nil:
		return "refresh failed · showing cached agents · retrying automatically"
	case m.staleAssets:
		return "kitty files are out of date · run cattery setup"
	default:
		return ""
	}
}

// emptyState explains why the list is empty in the scope the emptiness comes
// from: no inventory at all, a filter that excludes everything, a query that
// matches nothing, or both. The filter and query cases name the key that
// recovers; nothing recovers from an empty inventory, so that case says what is
// missing instead.
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
// from several sources, and an embedded newline would push the view past its
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

// renderRow returns the two lines for one agent row. grouped says whether a
// project heading sits above it, which decides how the row names itself.
func (m Model) renderRow(i int, a kitty.Agent, inner int, grouped bool) (string, string) {
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
	// The glyph already carries the status, so a very narrow row drops the
	// padded status word instead of crowding out the agent's name.
	if inner >= minStatusWordWidth {
		left1 = append(left1,
			span{fmt.Sprintf("%-*s", colStatus, a.Display), sc, true, ""},
			span{" ", cText, false, ""},
		)
	}
	if grouped {
		// The heading above already names the project, so the row only has to
		// identify itself within it.
		left1 = append(left1,
			span{truncate(rowLabel(a), maxName), cText, true, ""},
			span{" ", cText, false, ""},
			span{" " + a.Kind + " ", modelColor(a.Kind), false, cSurface0},
		)
	} else {
		left1 = append(left1,
			span{truncate(agentName(a), maxName), cText, true, ""},
			span{" ", cText, false, ""},
			span{" " + a.Kind + " ", modelColor(a.Kind), false, cSurface0},
			span{" ", cText, false, ""},
			span{truncate(a.Branch, maxBranch), cOverlay1, false, ""},
		)
	}
	right1 := []span{{timeLabel(m.now, a), cOverlay2, false, ""}}
	switch {
	case selected && m.focusing:
		right1 = []span{{"focusing…", cLavender, false, ""}}
	case selected && inner < minJumpHintWidth:
		//nolint:prealloc // every branch here holds at most three spans, so a
		// preallocated capacity would be a guess, not a saving.
		right1 = []span{{jumpHint, cLavender, false, ""}}
	case selected:
		right1 = append(right1, span{"  ", cText, false, ""}, span{jumpHint, cLavender, false, ""})
	case inner >= minJumpHintWidth:
		// Reserve the hint's cells on every other row so the time column holds
		// still while the cursor moves through the list.
		right1 = append(right1, span{strings.Repeat(" ", lipgloss.Width(jumpHint)+2), cText, false, ""})
	}
	line1 := composeLine(left1, right1, inner, bg)

	// Line 2: cwd · glyph + the agent's current prompt.
	cwdMax := max(min(inner/3, maxCwd), 4)
	cwd := truncate(shortenHome(a.CWD), cwdMax)
	sep := " · "
	if cwd == "" {
		sep = ""
	}
	glyph := activityGlyph(a, m.spin)
	glyphWidth := 0
	if glyph != "" {
		glyphWidth = lipgloss.Width(glyph) + 1 // glyph and the space after it
	}
	actWidth := max(inner-(rowIndent+lipgloss.Width(cwd)+lipgloss.Width(sep)+glyphWidth), 1)
	left2 := []span{
		{strings.Repeat(" ", rowIndent), cText, false, ""},
		{cwd, cOverlay0, false, ""},
	}
	if act := truncate(activity(a), actWidth); act != "" {
		left2 = append(left2, span{sep, cFaint, false, ""})
		if glyph != "" {
			left2 = append(left2, span{glyph, sc, false, ""}, span{" ", cText, false, ""})
		}
		left2 = append(left2, span{act, activityColor(a.Display), false, ""})
	}
	line2 := composeLine(left2, nil, inner, bg)

	return line1, line2
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
// longest list would cut the close key on exactly the narrow terminals that
// need it, so a shorter complete list wins.
func (m Model) hintTiers() []string {
	switch {
	case m.focusing:
		return []string{"focusing selected agent…", "focusing…"}
	case m.searching:
		return []string{"↑↓ move · ⏎ jump · esc clear · ^c close", "⏎ jump · esc clear", "esc clear"}
	default:
		return []string{
			"↑↓ / j k move · 1-9 select · ⏎ jump · / search · f filter · esc close",
			"j k move · ⏎ jump · / search · f filter · esc close",
			"⏎ jump · / search · esc close",
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

	// Position leads the summary: truncation takes the right side first, and
	// knowing where the cursor sits matters most while scrolling.
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
