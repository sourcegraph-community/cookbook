// Rendering. Layout is computed from the window size on every resize so the
// dashboard stays readable down to a narrow terminal.

package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Rows outside the list that always exist: header, blank, input, blank,
// blank, detail, progress, status, help.
const chromeHeight = 9

var (
	styleTitle    = lipgloss.NewStyle().Bold(true)
	styleDim      = lipgloss.NewStyle().Faint(true)
	styleCyan     = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleGreen    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleRed      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleYellow   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleSelected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
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

	glyph, glyphStyle := stateGlyph(state, d.spin)

	// Right-hand columns are fixed width so they line up; the query takes
	// whatever is left.
	count := ""
	if e.stats != nil {
		count = fmtCount(e.stats.Lines)
	} else if e.dl != nil {
		count = "…"
	} else if state == StateFailed || state == StateErrored {
		count = "logs"
	}

	const (
		prefixW = 2 // display cells, not bytes: "› " is 4 bytes wide
		glyphW  = 2 // glyph plus its trailing space
		stateW  = 11
		countW  = 9
		queryW  = 7 // the query never gets squeezed below this
		stampW  = 22
		clockW  = 9 // "15:04:05" plus the gap that keeps it off the query
	)
	prefix := "  "
	if selected {
		prefix = "› "
	}

	// The date is the first thing worth dropping when the terminal is narrow.
	timeW := stampW
	if m.Width() < prefixW+glyphW+stateW+stampW+countW+queryW {
		timeW = clockW
	}
	when := fmtWhen(e, state, timeW == stampW)

	avail := m.Width() - prefixW - glyphW - stateW - timeW - countW
	if avail < queryW {
		avail = queryW
	}

	stateText := pad(PrettyState(state), stateW)
	queryText := pad(truncate(query, avail), avail)
	line := fmt.Sprintf("%s%s %s%s%s%s",
		prefix,
		glyphStyle.Render(glyph),
		stateText,
		queryText,
		padLeft(when, timeW),
		padLeft(count, countW),
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
	h := m.height - chromeHeight
	if h < 3 {
		h = 3
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

	// Job list, or an empty-state hint.
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

	// Status line.
	status := m.status
	if m.mode == modeHelp {
		status = "press any key to close help"
	}
	b.WriteString(styleDim.Render(truncate(status, m.width)))
	b.WriteString("\n")

	// Footer.
	if m.mode == modeHelp {
		b.WriteString(m.help.FullHelpView(m.keys.FullHelp()))
	} else {
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
