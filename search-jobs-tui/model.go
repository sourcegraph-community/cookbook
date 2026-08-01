// State and the update loop.
//
// The interesting part of this file is how two things that Bubble Tea does not
// model directly are made to fit:
//
//  1. Many jobs polled at once. Each tick fans out one command per unfinished
//     job, so a slow or failing job cannot hold up the others.
//  2. Streaming download progress. A tea.Cmd returns exactly one message, so it
//     cannot report progress as it goes. The download runs in a goroutine that
//     writes onto a channel, and a command that reads one value from that
//     channel re-issues itself after every read.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// How many jobs to remember on disk between runs.
const storeLimit = 200

type mode int

const (
	modeList mode = iota
	modeInput
	modeHelp
)

// jobEntry is one row: the server's view of a job plus what this process knows
// about it.
type jobEntry struct {
	job       *SearchJob
	createdAt time.Time
	endedAt   time.Time
	outPath   string
	stats     *DownloadStats
	err       error
	dl        *download
}

// elapsed is how long the job has been running, frozen once it finishes.
func (e *jobEntry) elapsed() time.Duration {
	if !e.endedAt.IsZero() {
		return e.endedAt.Sub(e.createdAt)
	}
	return time.Since(e.createdAt)
}

// download tracks one in-flight results transfer.
type download struct {
	cancel    context.CancelFunc
	ch        chan tea.Msg
	startedAt time.Time
	done      int64
	total     int64 // -1 when the server sent no Content-Length
}

// jobItem adapts a jobEntry to the list. It holds a pointer, so field updates
// on the entry show up in the list without rebuilding it.
type jobItem struct{ entry *jobEntry }

func (i jobItem) FilterValue() string {
	if i.entry.job == nil {
		return ""
	}
	return i.entry.job.Query
}

// --- messages ---------------------------------------------------------------

type tickMsg time.Time
type jobsLoadedMsg struct{ jobs []*SearchJob }
type jobCreatedMsg struct {
	job *SearchJob
	err error
}
type jobUpdatedMsg struct {
	name string
	job  *SearchJob
	err  error
}
type cancelDoneMsg struct {
	name string
	err  error
}
type dlProgressMsg struct {
	name        string
	done, total int64
}
type dlDoneMsg struct {
	name  string
	stats DownloadStats
	err   error
}
type statusMsg string

// --- key map ----------------------------------------------------------------

type keyMap struct {
	Up       key.Binding
	Down     key.Binding
	New      key.Binding
	Submit   key.Binding
	Download key.Binding
	Cancel   key.Binding
	Open     key.Binding
	Help     key.Binding
	Quit     key.Binding
	Escape   key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		New:      key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new")),
		Submit:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "submit")),
		Download: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "download")),
		Cancel:   key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "cancel")),
		Open:     key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open in browser")),
		Help:     key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Escape:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	}
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.New, k.Download, k.Cancel, k.Open, k.Help, k.Quit}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.New, k.Submit},
		{k.Download, k.Cancel, k.Open},
		{k.Help, k.Escape, k.Quit},
	}
}

// --- model ------------------------------------------------------------------

type model struct {
	client    *Client
	outDir    string
	storePath string
	pollEvery time.Duration

	jobs     []*jobEntry
	lastPoll time.Time

	list     list.Model
	input    textinput.Model
	progress progress.Model
	spinner  spinner.Model
	help     help.Model
	keys     keyMap

	mode    mode
	status  string
	width   int
	height  int
	canStop bool // instance implements CancelSearchJob
}

func newModel(c *Client, outDir, storePath string, pollEvery time.Duration, initialQuery string) model {
	in := textinput.New()
	in.Placeholder = "context:global patterntype:keyword TODO count:all"
	in.Prompt = "query> "
	in.SetValue(initialQuery)

	l := list.New(nil, jobDelegate{}, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(false)
	l.SetFilteringEnabled(false)

	sp := spinner.New()
	// One cell wide, unlike Dot, which pads a trailing space. The same frame
	// draws the running job's marker in the list, where anything wider than one
	// cell would shift every column to its right.
	sp.Spinner = spinner.MiniDot

	m := model{
		client:    c,
		outDir:    outDir,
		storePath: storePath,
		pollEvery: pollEvery,
		list:      l,
		input:     in,
		// The percentage is rendered alongside the bar, not inside it.
		progress: progress.New(progress.WithDefaultBlend(), progress.WithoutPercentage()),
		spinner:  sp,
		help:     help.New(),
		keys:     defaultKeys(),
		mode:     modeList,
		canStop:  true,
	}

	// Start at a conventional size so there is always a frame to draw. A real
	// terminal corrects this with a WindowSizeMsg before the first render.
	m.width, m.height = 80, 24
	m.layout()
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, tickCmd(), loadJobsCmd(m.client, m.storePath))
}

// --- commands ---------------------------------------------------------------

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// loadJobsCmd prefers the server's list and falls back to the local cache when
// the instance does not implement ListSearchJobs.
func loadJobsCmd(c *Client, storePath string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if jobs, err := c.ListSearchJobs(ctx); err == nil {
			return jobsLoadedMsg{jobs: jobs}
		} else if Unauthorized(err) {
			return statusMsg("auth failed: " + err.Error())
		}

		var jobs []*SearchJob
		for _, s := range LoadStore(storePath) {
			jobs = append(jobs, &SearchJob{Name: s.Name, Query: s.Query, State: StateUnspecified})
		}
		return jobsLoadedMsg{jobs: jobs}
	}
}

func createJobCmd(c *Client, query string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		job, err := c.CreateSearchJob(ctx, query)
		return jobCreatedMsg{job: job, err: err}
	}
}

func pollJobCmd(c *Client, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		job, err := c.GetSearchJob(ctx, name)
		return jobUpdatedMsg{name: name, job: job, err: err}
	}
}

func cancelJobCmd(c *Client, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return cancelDoneMsg{name: name, err: c.CancelSearchJob(ctx, name)}
	}
}

// waitForDownload turns the next value on a download's channel into a message.
// Update re-issues it after each progress message, which is what keeps a
// streaming transfer visible in a one-message-per-command world.
func waitForDownload(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func openURLCmd(url string) tea.Cmd {
	return func() tea.Msg {
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("open", url)
		case "windows":
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
		default:
			cmd = exec.Command("xdg-open", url)
		}
		if err := cmd.Start(); err != nil {
			return statusMsg("could not open browser: " + err.Error())
		}
		return statusMsg("opened " + url)
	}
}

// --- helpers ----------------------------------------------------------------

func (m *model) find(name string) *jobEntry {
	for _, e := range m.jobs {
		if e.job != nil && e.job.Name == name {
			return e
		}
	}
	return nil
}

func (m *model) selected() *jobEntry {
	item, ok := m.list.SelectedItem().(jobItem)
	if !ok {
		return nil
	}
	return item.entry
}

func (m *model) syncList() tea.Cmd {
	items := make([]list.Item, 0, len(m.jobs))
	for _, e := range m.jobs {
		items = append(items, jobItem{entry: e})
	}
	return m.list.SetItems(items)
}

func (m *model) persist() {
	jobs := make([]StoredJob, 0, len(m.jobs))
	for _, e := range m.jobs {
		if e.job == nil {
			continue
		}
		jobs = append(jobs, StoredJob{Name: e.job.Name, Query: e.job.Query, CreatedAt: e.createdAt})
	}
	_ = SaveStore(m.storePath, jobs, storeLimit)
}

// outFileNameFor turns users/alice/searchJobs/42 into searchjob-42.jsonl.
func outFileNameFor(name string) string {
	id := path.Base(name)
	if id == "" || id == "." || id == "/" {
		id = "results"
	}
	return "searchjob-" + id + ".jsonl"
}

func (m *model) beginDownload(e *jobEntry) tea.Cmd {
	if e.job == nil || e.job.ResultsURL == "" {
		return func() tea.Msg { return statusMsg("no results to download yet") }
	}
	if e.dl != nil {
		return func() tea.Msg { return statusMsg("already downloading") }
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan tea.Msg, 64)
	e.dl = &download{cancel: cancel, ch: ch, startedAt: time.Now(), total: -1}
	e.outPath = path.Join(m.outDir, outFileNameFor(e.job.Name))

	name, url, outPath := e.job.Name, e.job.ResultsURL, e.outPath
	client := m.client

	go func() {
		stats, err := client.DownloadResults(ctx, url, outPath, func(done, total int64) {
			// Non-blocking: if the UI has not drained the last sample yet,
			// drop this one. A fresher number is always right behind it.
			select {
			case ch <- dlProgressMsg{name: name, done: done, total: total}:
			default:
			}
		})
		ch <- dlDoneMsg{name: name, stats: stats, err: err}
		close(ch)
	}()

	return waitForDownload(ch)
}

// pollDue returns commands for every job that still needs watching.
func (m *model) pollDue() tea.Cmd {
	var cmds []tea.Cmd
	for _, e := range m.jobs {
		if e.job == nil || IsTerminal(e.job.State) {
			continue
		}
		cmds = append(cmds, pollJobCmd(m.client, e.job.Name))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// --- update -----------------------------------------------------------------

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// A terminal that cannot report its size sends zeroes; keep the default.
		if msg.Width > 0 && msg.Height > 0 {
			m.width, m.height = msg.Width, msg.Height
			m.layout()
		}

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.PasteMsg:
		// cmd+v, right-click paste, and middle-click all arrive as a bracketed
		// paste event rather than a run of key presses, so a model that only
		// handles tea.KeyPressMsg drops the text on the floor. Pasting from the
		// list opens the query box first: the paste is almost always a query.
		if m.mode == modeList {
			m.mode = modeInput
			m.input.Focus()
			cmds = append(cmds, textinput.Blink)
		}
		if m.mode == modeInput {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			cmds = append(cmds, cmd)
		}

	case tickMsg:
		cmds = append(cmds, tickCmd())
		if time.Since(m.lastPoll) >= m.pollEvery {
			if c := m.pollDue(); c != nil {
				m.lastPoll = time.Now()
				cmds = append(cmds, c)
			}
		}

	case jobsLoadedMsg:
		for _, j := range msg.jobs {
			if m.find(j.Name) != nil {
				continue
			}
			m.jobs = append(m.jobs, &jobEntry{job: j, createdAt: parseCreateTime(j)})
		}
		cmds = append(cmds, m.syncList())
		if c := m.pollDue(); c != nil {
			m.lastPoll = time.Now()
			cmds = append(cmds, c)
		}

	case jobCreatedMsg:
		if msg.err != nil {
			m.status = "create failed: " + msg.err.Error()
			break
		}
		m.jobs = append(m.jobs, &jobEntry{job: msg.job, createdAt: time.Now()})
		m.status = "created " + msg.job.Name
		m.persist()
		cmds = append(cmds, m.syncList())
		m.list.Select(len(m.jobs) - 1)
		m.lastPoll = time.Now()
		cmds = append(cmds, pollJobCmd(m.client, msg.job.Name))

	case jobUpdatedMsg:
		e := m.find(msg.name)
		if e == nil {
			break
		}
		if msg.err != nil {
			// Transient failures are normal on a long poll. Say so and keep going.
			m.status = "poll " + path.Base(msg.name) + ": " + msg.err.Error()
			break
		}
		wasTerminal := IsTerminal(e.job.State)
		e.job = msg.job
		if !wasTerminal && IsTerminal(msg.job.State) {
			e.endedAt = time.Now()
		}

	case cancelDoneMsg:
		if msg.err != nil {
			if Unsupported(msg.err) {
				m.canStop = false
				m.status = "this instance does not support canceling jobs"
			} else {
				m.status = "cancel failed: " + msg.err.Error()
			}
			break
		}
		m.status = "canceled " + path.Base(msg.name)

	case dlProgressMsg:
		e := m.find(msg.name)
		if e == nil || e.dl == nil {
			break
		}
		e.dl.done, e.dl.total = msg.done, msg.total
		cmds = append(cmds, waitForDownload(e.dl.ch))

	case dlDoneMsg:
		e := m.find(msg.name)
		if e == nil {
			break
		}
		e.dl = nil
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) {
				// A canceled transfer leaves a truncated file that looks like a
				// complete result set. Remove it rather than leave the trap.
				if e.outPath != "" {
					_ = os.Remove(e.outPath)
				}
				e.outPath = ""
				m.status = "download canceled"
			} else {
				e.err = msg.err
				m.status = "download failed: " + msg.err.Error()
			}
			break
		}
		stats := msg.stats
		e.stats = &stats
		m.status = fmt.Sprintf("wrote %s line(s) to %s (%s)",
			fmtCount(stats.Lines), e.outPath, fmtBytes(stats.Bytes))

	case statusMsg:
		m.status = string(msg)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
		// A list delegate cannot reach the model, so the frame is pushed into it
		// on every tick. Without this the running job's marker sits still and a
		// stalled job looks exactly like a working one.
		m.list.SetDelegate(jobDelegate{spin: m.spinner.View()})

	default:
		// The text input's cursor blink and its clipboard reply are unexported
		// message types, so no case above can name them. Anything unrecognized
		// goes to the input while the query box is open. Without this, ctrl+v
		// does nothing: the input answers the key press with a command that
		// reads the clipboard, and the reply has nowhere to land.
		if m.mode == modeInput {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeInput:
		switch {
		case key.Matches(msg, m.keys.Escape):
			m.mode = modeList
			return m, nil
		case key.Matches(msg, m.keys.Submit):
			query := m.input.Value()
			if query == "" {
				query = m.input.Placeholder
			}
			m.mode = modeList
			m.input.Blur()
			m.input.SetValue("")
			m.status = "creating job…"
			return m, createJobCmd(m.client, query)
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case modeHelp:
		m.mode = modeList
		return m, nil
	}

	// modeList
	switch {
	case key.Matches(msg, m.keys.Quit):
		for _, e := range m.jobs {
			if e.dl != nil {
				e.dl.cancel()
			}
		}
		return m, tea.Quit

	case key.Matches(msg, m.keys.Help):
		m.mode = modeHelp
		return m, nil

	case key.Matches(msg, m.keys.New):
		m.mode = modeInput
		m.input.Focus()
		return m, textinput.Blink

	case key.Matches(msg, m.keys.Download):
		e := m.selected()
		if e == nil {
			return m, nil
		}
		if e.job == nil || e.job.State != StateCompleted {
			m.status = "only completed jobs have results"
			return m, nil
		}
		return m, m.beginDownload(e)

	case key.Matches(msg, m.keys.Cancel):
		e := m.selected()
		if e == nil || e.job == nil {
			return m, nil
		}
		if e.dl != nil {
			e.dl.cancel()
			m.status = "canceling download…"
			return m, nil
		}
		if !m.canStop {
			m.status = "this instance does not support canceling jobs"
			return m, nil
		}
		if IsTerminal(e.job.State) {
			m.status = "job already finished"
			return m, nil
		}
		return m, cancelJobCmd(m.client, e.job.Name)

	case key.Matches(msg, m.keys.Open):
		// The web UI page, not the job's resultsUrl. See SearchJobsPageURL.
		return m, openURLCmd(m.client.SearchJobsPageURL())
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func parseCreateTime(j *SearchJob) time.Time {
	if j.CreateTime != "" {
		if t, err := time.Parse(time.RFC3339, j.CreateTime); err == nil {
			return t
		}
	}
	return time.Now()
}
