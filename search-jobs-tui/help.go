// The help screen.
//
// It is a screen rather than a pane because there is more to say than a footer
// holds: two keys mean different things depending on what is running, the list
// brings paging keys this file did not write, and the answer to "why did
// nothing happen when I pressed c" is a property of the instance, not of the
// keyboard. All of that is data here, in helpSections, so the keys shown cannot
// drift from the keys the update loop matches.

package main

import (
	"fmt"
	"path"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"
)

// helpChromeHeight is the rows renderHelp spends outside the scrolling body:
// header, blank, blank, footer.
//
// Unlike chromeHeight this is not a budget that has to be kept in step with
// what the panes below it draw. The viewport pads itself to exactly the height
// it is given, so the frame comes out at m.height by construction.
const helpChromeHeight = 4

// helpKeyCol is the width of the key column. Counted in runes by pad, which is
// right only while every label is ASCII or a one-cell arrow; an emoji here
// would push the descriptions out of line.
const helpKeyCol = 12

// helpGutter is the indent every help line starts with.
const helpGutter = 2

// helpEntry is one row of the help screen.
type helpEntry struct {
	// binding is the key this row documents. Its Help().Key is the label, so a
	// rebind moves the help text with it.
	binding key.Binding
	// keys overrides that label. Two kinds of row need it: those documenting a
	// component's own keymap, where the component's label would be wrong (the
	// list calls its next-page binding "→/l/pgdn", but l never reaches the list
	// — the log key claims it first), and the rows of THIS SESSION, which are
	// not keys at all.
	keys string
	desc string
}

func (e helpEntry) label() string {
	if e.keys != "" {
		return e.keys
	}
	return e.binding.Help().Key
}

type helpSection struct {
	title   string
	entries []helpEntry
}

// helpSections is the whole help screen as data.
//
// A model method rather than a keyMap one: two of these rows depend on what the
// instance supports, and the last section is what this particular run is
// pointed at.
func (m model) helpSections() []helpSection {
	k := m.keys

	// The two capability-dependent rows. A key that does nothing is worth a line
	// saying so; the alternative, hiding it, leaves someone pressing it.
	cancelJob := "with no transfer running, ask the server to cancel the job. Canceling stops the work and keeps the record: the job stays in the list with whatever it had reached."
	if !m.canStop {
		cancelJob = "would cancel the running job, but this instance does not support canceling jobs."
	}
	deleteJob := "delete the job and its results on the server. Asks first, and cannot be undone. Anything already downloaded is a local copy and is left alone."
	if !m.canDelete {
		deleteJob = "would delete the job, but this instance does not support deleting jobs."
	}

	return []helpSection{{
		title: "MOVING AROUND",
		entries: []helpEntry{
			{binding: k.Up, keys: "↑/k ↓/j", desc: "move the selection"},
			{keys: "←/h pgup", desc: "previous page; b and u do the same. The list pages when there are more jobs than rows for them, and there is no page marker: the selection jumps."},
			{keys: "→ pgdn", desc: "next page; f does the same"},
			{keys: "g home", desc: "first job"},
			{keys: "G end", desc: "last job"},
		},
	}, {
		title: "RUNNING A QUERY",
		entries: []helpEntry{
			{binding: k.New, desc: "open the query box"},
			{binding: k.Submit, desc: "submit what is in the box. An empty box submits the greyed-out example, which is a reasonable first query."},
			{binding: k.Escape, desc: "close the box without submitting"},
			{keys: "ctrl+v", desc: "paste a query; cmd+v works too. Pasting from the list opens the box first, so a copied query lands in it either way."},
			{binding: k.Rerun, desc: "rerun the selected job's query as a new job, as the web UI's rerun does. The original is left alone, so a run and its rerun sit side by side and stay comparable."},
		},
	}, {
		title: "RESULTS AND LOGS",
		entries: []helpEntry{
			{binding: k.Download, desc: "download the selected job's results as JSONL. Only a completed job has results."},
			{binding: k.Logs, desc: "download its log: a CSV row per repository and revision, with a status for each. It is the only explanation a failed job ever gives, and it is worth reading on one that succeeded, because partial coverage shows up there. Nothing to read until the job starts."},
			{binding: k.Cancel, desc: "while a transfer is running, cancel the transfer. The partial file is removed rather than left looking like a complete result set."},
			{binding: k.Open, desc: "open the Search Jobs page in a browser. Downloads there work off your browser session; the URLs here need the token."},
		},
	}, {
		title: "MANAGING JOBS",
		entries: []helpEntry{
			{binding: k.Cancel, desc: cancelJob},
			{binding: k.Delete, desc: deleteJob},
			{binding: k.Confirm, desc: "answers that prompt. Only y deletes. Every other key keeps the job, q included, so quit cannot fire out from under the question."},
		},
	}, {
		title: "THIS SCREEN",
		entries: []helpEntry{
			{keys: "↑/k ↓/j", desc: "scroll a line"},
			{keys: "pgup pgdn", desc: "scroll a page; b and f do the same"},
			{keys: "u d", desc: "half a page up, half a page down"},
			{binding: k.CloseHelp, desc: "close help and go back to the list"},
			{binding: k.ForceQuit, desc: "quit. q closes this screen, so ctrl+c is the way out of the program from here."},
		},
	}, {
		title: "QUITTING",
		entries: []helpEntry{
			{keys: "q ctrl+c", desc: "quit. A transfer still running is canceled and its partial file removed first."},
			{binding: k.Escape, desc: "does nothing in the job list. It closes the query box, the delete prompt, and this screen."},
		},
	}, {
		title:   "THIS SESSION",
		entries: m.sessionEntries(),
	}}
}

// sessionEntries is what this run is pointed at. None of it appears anywhere
// else in the dashboard, and the last two lines are the ones that explain a key
// that looks broken.
func (m model) sessionEntries() []helpEntry {
	supported := func(ok bool) string {
		if ok {
			return "supported by this instance"
		}
		return "not supported by this instance"
	}

	store := m.storePath
	if store == "" {
		store = "none — this run does not remember jobs between restarts"
	}

	return []helpEntry{
		{keys: "endpoint", desc: m.client.Endpoint},
		{keys: "token", desc: "from SRC_ACCESS_TOKEN"},
		{keys: "downloads", desc: path.Join(m.outDir, "searchjob-<id>.jsonl") + " for results, " + path.Join(m.outDir, "searchjob-<id>.log") + " for logs"},
		{keys: "job cache", desc: store},
		{keys: "polling", desc: "every " + m.pollEvery.String()},
		{keys: "cancel", desc: supported(m.canStop)},
		{keys: "delete", desc: supported(m.canDelete)},
	}
}

// helpIntro is what a search job is, for someone who opened this before running
// one. It says why the dashboard is shaped the way it is: jobs are slow enough
// that watching one at a time would be the wrong thing to build.
const helpIntro = "A search job runs one query exhaustively across every repository, branch, and revision on this instance: no result cap, no timeout. Jobs run on the server and can take hours, so this dashboard polls several at once instead of blocking on one. A finished job leaves a JSONL file of results to download, and a CSV log with a row per repository and revision it touched."

// helpText is the scrolling body, wrapped to w.
func (m model) helpText(w int) string {
	var b strings.Builder
	b.WriteString(wrapIndent(helpIntro, helpGutter, w))

	for _, s := range m.helpSections() {
		b.WriteString("\n\n")
		b.WriteString(styleTitle.Render(s.title))
		for _, e := range s.entries {
			b.WriteString("\n")
			b.WriteString(helpRow(e.label(), e.desc, w))
		}
	}
	return b.String()
}

// helpRow lays out one entry: the key in a fixed left column, the description
// wrapped in what is left and hanging under itself.
//
// A label too wide for the column, or a terminal too narrow to leave room for a
// sentence beside one, drops to two lines rather than truncating the key. A cut
// key is worse than a tall row: the row is still readable, the key is not.
func helpRow(label, desc string, w int) string {
	lead := helpGutter + helpKeyCol + 1
	if w-lead < 12 || lipgloss.Width(label) > helpKeyCol {
		return strings.Repeat(" ", helpGutter) + styleCyan.Render(label) + "\n" +
			wrapIndent(desc, helpGutter+2, w)
	}

	lines := strings.Split(wrapIndent(desc, lead, w), "\n")
	indent := strings.Repeat(" ", lead)
	lines[0] = strings.Repeat(" ", helpGutter) +
		styleCyan.Render(pad(label, helpKeyCol)) + " " +
		strings.TrimPrefix(lines[0], indent)
	return strings.Join(lines, "\n")
}

// wrapIndent word-wraps s into w columns and indents every line by indent.
func wrapIndent(s string, indent, w int) string {
	lines := strings.Split(lipgloss.Wrap(s, max(1, w-indent), "/-"), "\n")
	prefix := strings.Repeat(" ", indent)
	for i, line := range lines {
		lines[i] = prefix + styleDim.Render(line)
	}
	return strings.Join(lines, "\n")
}

// renderHelp is the whole frame while the help screen is open.
//
// Unlike render it does not budget a fixed chrome height against a body that
// might not fill it: the viewport pads to exactly the height it was given, so
// this comes out at m.height rows whatever the text does.
func (m model) renderHelp() string {
	var b strings.Builder

	b.WriteString(spread(
		styleTitle.Render("Sourcegraph search jobs · help"),
		styleDim.Render(m.client.Endpoint), m.width))
	b.WriteString("\n\n")
	b.WriteString(m.helpVP.View())
	b.WriteString("\n\n")

	// How far down the text you are, but only when there is text below the fold.
	// It is measured first so the key bar is offered only what is left and
	// truncates itself to fit.
	pct := ""
	if m.helpVP.TotalLineCount() > m.helpVP.VisibleLineCount() {
		pct = styleDim.Render(fmt.Sprintf("%d%%", int(m.helpVP.ScrollPercent()*100)))
	}
	h := m.help
	h.SetWidth(max(1, m.width-lipgloss.Width(pct)-1))
	b.WriteString(spread(h.ShortHelpView(m.keys.HelpModeHelp()), pct, m.width))

	return b.String()
}
