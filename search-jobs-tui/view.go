// Rendering. Layout is computed from the window size on every resize so the
// dashboard stays readable down to a narrow terminal.

package main

import (
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Rows outside the list that always exist: header, blank, input, blank,
// column labels, blank, detail, progress, status, help.
const chromeHeight = 10

var (
	styleTitle    = lipgloss.NewStyle().Bold(true)
	styleDim      = lipgloss.NewStyle().Faint(true)
	styleColumn   = lipgloss.NewStyle().Faint(true).Underline(true)
	styleCyan     = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleGreen    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleRed      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleYellow   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleSelected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	// The delete prompt. Bold, because it is the one line in this dashboard that
	// asks for something back instead of reporting what happened.
	styleWarn = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
)

// stateGlyph returns a marker and the style to draw it in.
//
// A processing job takes spin, the current spinner frame, so its marker moves.
// Every frame has to be one cell wide or the columns to its right shift; see the
// spinner choice in newModel. An empty spin falls back to a static marker, which
// is what the layout tests use.
func stateGlyph(state, spin string) (string, lipgloss.Style) {
	switch state {
	case StateCompleted:
		return "✔", styleGreen
	case StateFailed, StateErrored:
		return "✖", styleRed
	case StateCanceled:
		return "⊘", styleYellow
	case StateProcessing:
		if spin != "" {
			return spin, styleCyan
		}
		return "●", styleCyan
	case StateQueued:
		// Deliberately static. A queued job is not doing anything yet, and that
		// reads at a glance only if it is the one marker holding still.
		return "◌", styleDim
	default:
		return "·", styleDim
	}
}

// --- columns ----------------------------------------------------------------

// jobColumns is the width of every cell in a job row. The labels above the list
// and the rows themselves both derive theirs from this, so neither can drift out
// from under the other.
type jobColumns struct {
	prefix, glyph, state, query, when int
	// Whether the time column is wide enough for a date as well as a clock.
	withDate bool
}

func columnsFor(width int) jobColumns {
	const (
		prefixW = 2 // display cells, not bytes: "› " is 4 bytes wide
		glyphW  = 2 // glyph plus its trailing space
		stateW  = 11
		queryW  = 7 // the query never gets squeezed below this
		stampW  = 22
		clockW  = 9 // "15:04:05" plus the gap that keeps it off the query
	)

	// The date is the first thing worth dropping when the terminal is narrow.
	whenW := stampW
	if width < prefixW+glyphW+stateW+stampW+queryW {
		whenW = clockW
	}

	queryCol := width - prefixW - glyphW - stateW - whenW
	if queryCol < queryW {
		queryCol = queryW
	}

	return jobColumns{
		prefix:   prefixW,
		glyph:    glyphW,
		state:    stateW,
		query:    queryCol,
		when:     whenW,
		withDate: whenW == stampW,
	}
}

// columnLabels is the header row over the job list. The marker column has no
// label, so its cells are spaces.
//
// The labels are underlined instead of ruled off with a line of their own: a
// rule would cost a row of the list on every terminal, and a 24-row window has
// few enough to begin with.
func columnLabels(width int) string {
	c := columnsFor(width)
	return strings.Repeat(" ", c.prefix+c.glyph) +
		labelCell("status", c.state, false) +
		labelCell("query", c.query, false) +
		labelCell("finished", c.when, true)
}

// labelCell underlines the label but not the padding around it, so the rule sits
// under words rather than running the width of the window.
func labelCell(label string, w int, right bool) string {
	label = truncate(label, w)
	gap := strings.Repeat(" ", max(0, w-len([]rune(label))))
	if right {
		return gap + styleColumn.Render(label)
	}
	return styleColumn.Render(label) + gap
}

// --- list delegate ----------------------------------------------------------

type jobDelegate struct {
	// The current spinner frame, replaced on every tick by the update loop.
	spin string
}

func (jobDelegate) Height() int                         { return 1 }
func (jobDelegate) Spacing() int                        { return 0 }
func (jobDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d jobDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(jobItem)
	if !ok {
		return
	}
	e := it.entry
	selected := index == m.Index()

	state := StateUnspecified
	query := ""
	if e.job != nil {
		state, query = e.job.State, e.job.Query
	}
	stateLabel := PrettyState(state)
	glyphState := state
	if e.deleting {
		glyphState = StateProcessing
		stateLabel = "Deleting"
	}

	glyph, glyphStyle := stateGlyph(glyphState, d.spin)

	// Right-hand cells are fixed width so they line up under their labels; the
	// query takes whatever is left.
	c := columnsFor(m.Width())

	prefix := "  "
	if selected {
		prefix = "› "
	}

	line := fmt.Sprintf("%s%s %s%s%s",
		prefix,
		glyphStyle.Render(glyph),
		pad(stateLabel, c.state),
		pad(query, c.query),
		padLeft(fmtWhen(e, state, c.withDate), c.when),
	)

	if selected {
		line = styleSelected.Render(line)
	}
	fmt.Fprint(w, line)
}

// --- layout -----------------------------------------------------------------

func (m *model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	// One row is the floor. Clamping higher than that would make the list taller
	// than the space left for it and push the footer off a short window.
	h := m.height - chromeHeight
	if h < 1 {
		h = 1
	}
	m.list.SetSize(m.width, h)
	m.input.SetWidth(max(20, m.width-8))
	m.help.SetWidth(m.width)
}

// --- view -------------------------------------------------------------------

func (m model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m model) render() string {
	if m.width == 0 {
		return "starting…"
	}

	var b strings.Builder

	// Header: title on the left, instance on the right.
	title := styleTitle.Render("Sourcegraph search jobs")
	host := styleDim.Render(m.client.Endpoint)
	b.WriteString(spread(title, host, m.width))
	b.WriteString("\n\n")

	// Query line.
	if m.mode == modeInput {
		b.WriteString(m.input.View())
	} else {
		b.WriteString(styleDim.Render("query> press n to run a new search"))
	}
	b.WriteString("\n\n")

	// Column labels, then the job rows or an empty-state hint. The labels stay up
	// even with nothing under them, so the panes below do not move when the first
	// job appears.
	b.WriteString(columnLabels(m.width))
	b.WriteString("\n")

	if len(m.jobs) == 0 {
		b.WriteString(styleDim.Render("  no jobs yet — press n to create one"))
		// Pad to the same height the list would have occupied, so the panes
		// below it do not move when the first job appears.
		b.WriteString(strings.Repeat("\n", max(2, m.height-chromeHeight+1)))
	} else {
		b.WriteString(m.list.View())
		b.WriteString("\n\n")
	}

	// Detail pane for the selection.
	b.WriteString(m.renderDetail())
	b.WriteString("\n")

	// Status line. The delete prompt takes this row rather than a pane of its own,
	// so asking the question does not change the height of the frame.
	switch m.mode {
	case modeConfirm:
		b.WriteString(m.renderConfirm())
	case modeHelp:
		b.WriteString(styleDim.Render(truncate("press any key to close help", m.width)))
	default:
		b.WriteString(styleDim.Render(truncate(m.status, m.width)))
	}
	b.WriteString("\n")

	// Footer.
	switch m.mode {
	case modeHelp:
		b.WriteString(m.help.FullHelpView(m.keys.FullHelp()))
	case modeConfirm:
		b.WriteString(m.help.ShortHelpView(m.keys.ConfirmHelp()))
	default:
		b.WriteString(m.help.ShortHelpView(m.keys.ShortHelp()))
	}

	// Belt and braces: every pane truncates its own content, but one escaped
	// long line would wrap and shift the whole layout by a row.
	return clampWidth(b.String(), m.width)
}

// clampWidth trims each line to w, counting display cells and leaving escape
// sequences intact.
func clampWidth(s string, w int) string {
	if w <= 0 {
		return s
	}
	style := lipgloss.NewStyle().MaxWidth(w)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = style.Render(line)
	}
	return strings.Join(lines, "\n")
}

func (m model) renderDetail() string {
	e := m.selected()
	if e == nil || e.job == nil {
		return "\n"
	}

	name := styleDim.Render(truncate(e.job.Name, m.width))

	donePath, doneStats, doneKind := e.donePath()

	var second string
	switch {
	case e.dl != nil:
		second = m.renderProgress(e)
	case doneStats != nil:
		second = styleDim.Render(truncate(fmt.Sprintf("%s lines · %s · %s · %s",
			fmtCount(doneStats.Lines), fmtBytes(doneStats.Bytes), doneKind.label(), donePath), m.width))
	case e.err != nil:
		second = styleRed.Render(truncate(e.err.Error(), m.width))
	case e.job.State == StateCompleted:
		second = styleDim.Render("press d to download results, l for logs")
	case e.job.State == StateFailed:
		// Failed is where the log actually matters: it is the only explanation
		// the API ever gives for a rejected query.
		second = styleDim.Render("press l to download the logs")
	case !IsTerminal(e.job.State):
		// MiniDot frames carry no trailing space, unlike Dot, so the gap is
		// spelled out here.
		second = styleCyan.Render(m.spinner.View()) +
			styleDim.Render(fmt.Sprintf(" %s · %s", PrettyState(e.job.State), fmtDuration(e.elapsed())))
	default:
		second = styleDim.Render(PrettyState(e.job.State))
	}

	return name + "\n" + second
}

// renderConfirm is the question a pending delete is waiting on.
//
// It names the job and repeats its query, because an id on its own is not enough
// to tell two jobs apart, and the query goes last so a long one is what the
// truncation eats. A job that has not finished says so: deleting it throws away
// work that is still running.
func (m model) renderConfirm() string {
	label := path.Base(m.confirm)
	query := ""
	if e := m.find(m.confirm); e != nil && e.job != nil {
		query = e.job.Query
		if !IsTerminal(e.job.State) {
			label += " (still " + PrettyState(e.job.State) + ")"
		}
	}
	prompt := "delete job " + label + "? this cannot be undone"
	if query != "" {
		prompt += " · " + query
	}
	return styleWarn.Render(truncate(prompt, m.width))
}

func (m model) renderProgress(e *jobEntry) string {
	d := e.dl
	elapsed := time.Since(d.startedAt)
	var rate float64
	if elapsed > 0 {
		rate = float64(d.done) / elapsed.Seconds()
	}

	// Right-hand readout first, so the bar can take whatever is left.
	var readout string
	if d.total > 0 {
		readout = fmt.Sprintf("  %3d%%  %s / %s  %s/s",
			int(float64(d.done)/float64(d.total)*100),
			fmtBytes(d.done), fmtBytes(d.total), fmtBytes(int64(rate)))
	} else {
		// No Content-Length: report throughput instead of a percentage.
		readout = fmt.Sprintf("  %s  %s/s  %s",
			fmtBytes(d.done), fmtBytes(int64(rate)), fmtDuration(elapsed))
	}

	if d.total <= 0 {
		return styleDim.Render(truncate(strings.TrimLeft(readout, " "), m.width))
	}

	barWidth := m.width - lipgloss.Width(readout)
	if barWidth < 10 {
		// Too narrow for a bar; the numbers matter more.
		return styleDim.Render(truncate(strings.TrimLeft(readout, " "), m.width))
	}
	if barWidth > 40 {
		barWidth = 40
	}
	p := m.progress
	p.SetWidth(barWidth)
	return p.ViewAs(float64(d.done)/float64(d.total)) + styleDim.Render(readout)
}

// --- formatting -------------------------------------------------------------
//
// Ported from search-jobs-api/search_job.ts so both recipes format identically.

func fmtBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", int64(v), units[i])
	}
	if v < 10 {
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.0f %s", v, units[i])
}

// fmtWhen fills the row's time column: local wall-clock time for a job that has
// finished, a running total for one that has not.
//
// The API reports no end time, so a finish is only known for a transition this
// process watched or recorded in the store on an earlier run. A job that was
// already terminal when the dashboard first saw it gets a blank cell, which is
// the honest answer; timing it from its create time would invent a duration.
func fmtWhen(e *jobEntry, state string, withDate bool) string {
	if !e.endedAt.IsZero() {
		if withDate {
			return e.endedAt.Local().Format("2006-01-02 | 15:04:05")
		}
		return e.endedAt.Local().Format("15:04:05")
	}
	if IsTerminal(state) {
		return ""
	}
	return fmtDuration(e.elapsed())
}

func fmtDuration(d time.Duration) string {
	if d < 10*time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	secs := int(d.Round(time.Second).Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	mins := secs / 60
	if mins < 60 {
		return fmt.Sprintf("%dm %ds", mins, secs%60)
	}
	return fmt.Sprintf("%dh %dm", mins/60, mins%60)
}

// fmtCount groups thousands with commas, matching toLocaleString("en-US").
func fmtCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// --- small string helpers ---------------------------------------------------

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return string(r[:w-1]) + "…"
}

func pad(s string, w int) string {
	s = truncate(s, w)
	if n := w - len([]rune(s)); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

func padLeft(s string, w int) string {
	s = truncate(s, w)
	if n := w - len([]rune(s)); n > 0 {
		return strings.Repeat(" ", n) + s
	}
	return s
}

// spread puts left and right on one line, separated to the full width.
func spread(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return truncate(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}
