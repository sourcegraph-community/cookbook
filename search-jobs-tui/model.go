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
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// How many jobs to remember on disk between runs.
const storeLimit = 200

type mode int

const (
	modeList mode = iota
	modeInput
	modeHelp
	modeConfirm
)

// transferKind distinguishes the two things a job has to download. They share
// one code path; only the URL, the file extension, and the wording differ.
type transferKind int

const (
	transferResults transferKind = iota
	transferLogs
)

func (k transferKind) label() string {
	if k == transferLogs {
		return "logs"
	}
	return "results"
}

func (k transferKind) ext() string {
	if k == transferLogs {
		return ".log"
	}
	return ".jsonl"
}

// jobEntry is one row: the server's view of a job plus what this process knows
// about it.
type jobEntry struct {
	job       *SearchJob
	createdAt time.Time
	endedAt   time.Time
	deleting  bool
	outPath   string
	stats     *DownloadStats
	logPath   string
	logStats  *DownloadStats
	// Which transfer finished most recently, so the detail pane reports that one
	// when a job has both a result set and a log on disk.
	lastDone transferKind
	err      error
	dl       *download
}

// donePath and doneStats report the finished transfer the detail pane should
// show: the most recent one, falling back to whichever exists.
func (e *jobEntry) donePath() (string, *DownloadStats, transferKind) {
	if e.lastDone == transferLogs && e.logStats != nil {
		return e.logPath, e.logStats, transferLogs
	}
	if e.stats != nil {
		return e.outPath, e.stats, transferResults
	}
	if e.logStats != nil {
		return e.logPath, e.logStats, transferLogs
	}
	return "", nil, transferResults
}

// elapsed is how long the job has been running, frozen once it finishes.
func (e *jobEntry) elapsed() time.Duration {
	if !e.endedAt.IsZero() {
		return e.endedAt.Sub(e.createdAt)
	}
	return time.Since(e.createdAt)
}

// download tracks one in-flight transfer. A job runs at most one at a time, so
// there is a single progress bar to read and cancel is unambiguous.
type download struct {
	kind      transferKind
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
type jobsLoadedMsg struct {
	jobs []*SearchJob
	// Finish times recovered from the store, keyed by job name. The server does
	// not report one, so this is the only way a restart keeps them.
	ended map[string]time.Time
}
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
type deleteDoneMsg struct {
	name string
	err  error
}
type dlProgressMsg struct {
	name        string
	done, total int64
}
type dlDoneMsg struct {
	name  string
	kind  transferKind
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
	Rerun    key.Binding
	Download key.Binding
	Logs     key.Binding
	Cancel   key.Binding
	Delete   key.Binding
	Open     key.Binding
	Help     key.Binding
	Quit     key.Binding
	Escape   key.Binding
	Confirm  key.Binding
	Deny     key.Binding
	// ForceQuit is matched inside the help screen, where q closes the screen
	// rather than the program. Elsewhere Quit already covers ctrl+c.
	ForceQuit key.Binding
	CloseHelp key.Binding
	// Scroll and Page are the help screen's footer labels. The viewport owns the
	// keys themselves; these two only say so, the way Deny does.
	Scroll key.Binding
	Page   key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		New:      key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new")),
		Submit:   key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "submit")),
		Rerun:    key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "rerun")),
		Download: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "download")),
		// Short labels here, spelled out in FullHelp: the one-line help bar has to
		// fit an 80-column terminal, and anything past the edge is cut off the
		// right-hand end, which is where q lives.
		Logs:   key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "logs")),
		Cancel: key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "cancel")),
		Delete: key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "delete")),
		Open:   key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open")),
		Help:   key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:   key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Escape: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		// Only y confirms a delete, and it is the only key that does: enter is not
		// wired up, because enter is submit everywhere else in this dashboard and
		// muscle memory should not be able to destroy a job. Every other key backs
		// out, so the listed n is one way to say no rather than the way.
		Confirm: key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "delete it")),
		Deny:    key.NewBinding(key.WithKeys("n", "esc"), key.WithHelp("n/esc", "keep it")),

		ForceQuit: key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
		CloseHelp: key.NewBinding(key.WithKeys("?", "esc", "q"), key.WithHelp("?/esc/q", "close")),
		Scroll:    key.NewBinding(key.WithKeys("up", "down", "k", "j"), key.WithHelp("↑/↓", "scroll")),
		Page:      key.NewBinding(key.WithKeys("pgup", "pgdown", "b", "f"), key.WithHelp("pgup/pgdn", "page")),
	}
}

// ShortHelp is the one-line footer. It leaves out o open in browser, which the
// full list carries: the bar is cut off at its right-hand end, where q quit
// sits, so on an 80-column terminal there is room for eight entries and no more.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.New, k.Rerun, k.Download, k.Logs, k.Cancel, k.Delete, k.Help, k.Quit}
}

// HelpModeHelp is the footer while the help screen is open.
//
// CloseHelp goes first because the bar is cut off at its right-hand end on a
// narrow terminal, and how to get out is the one thing that has to survive the
// cut. There is no FullHelp: the four-column grid it fed said too little to be
// worth a mode of its own, and helpSections replaced it.
func (k keyMap) HelpModeHelp() []key.Binding {
	return []key.Binding{k.CloseHelp, k.Scroll, k.Page, k.ForceQuit}
}

// ConfirmHelp replaces the footer while a delete is waiting on an answer.
func (k keyMap) ConfirmHelp() []key.Binding {
	return []key.Binding{k.Confirm, k.Deny}
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
	// The help screen's body. It scrolls, so the text can say more than a
	// terminal is tall.
	helpVP viewport.Model
	keys   keyMap

	mode   mode
	status string
	width  int
	height int
	// The job name a pending delete is waiting on, empty outside modeConfirm. A
	// name rather than a *jobEntry: the answer arrives at least one message later,
	// and looking the row up again then is what keeps a stale pointer impossible.
	confirm   string
	canStop   bool // instance implements CancelSearchJob
	canDelete bool // instance implements DeleteSearchJob
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
	// The list answers both q and esc with tea.Quit. q never reaches it, because
	// handleKey matches Quit first, but esc did — and quitting that way skipped
	// the download cleanup, leaving a half-written file behind.
	l.DisableQuitKeybindings()

	sp := spinner.New()
	// One cell wide, unlike Dot, which pads a trailing space. The same frame
	// draws the running job's marker in the list, where anything wider than one
	// cell would shift every column to its right.
	sp.Spinner = spinner.MiniDot

	// Key letters take the spinner's cyan so the pressable part of the help bar
	// separates from its label at a glance.
	h := help.New()
	h.Styles.ShortKey = styleCyan
	h.Styles.FullKey = styleCyan

	// The help text is word-wrapped before it goes in, so the viewport's own
	// wrapping is left off: it cuts mid-word. That also makes horizontal
	// scrolling meaningless, and its keys are h and l, which the dashboard wants
	// for other things.
	vp := viewport.New()
	vp.KeyMap.Left.SetEnabled(false)
	vp.KeyMap.Right.SetEnabled(false)

	m := model{
		client:    c,
		outDir:    outDir,
		storePath: storePath,
		pollEvery: pollEvery,
		list:      l,
		input:     in,
		// The percentage is rendered alongside the bar, not inside it.
		progress:  progress.New(progress.WithDefaultBlend(), progress.WithoutPercentage()),
		spinner:   sp,
		help:      h,
		helpVP:    vp,
		keys:      defaultKeys(),
		mode:      modeList,
		canStop:   true,
		canDelete: true,
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

		// Read the cache either way: even when the server answers, it is the only
		// source of finish times.
		stored := LoadStore(storePath)
		ended := make(map[string]time.Time, len(stored))
		for _, s := range stored {
			if !s.EndedAt.IsZero() {
				ended[s.Name] = s.EndedAt
			}
		}

		if jobs, err := c.ListSearchJobs(ctx); err == nil {
			return jobsLoadedMsg{jobs: jobs, ended: ended}
		} else if Unauthorized(err) {
			return statusMsg("auth failed: " + err.Error())
		}

		var jobs []*SearchJob
		for _, s := range stored {
			jobs = append(jobs, &SearchJob{Name: s.Name, Query: s.Query, State: StateUnspecified})
		}
		return jobsLoadedMsg{jobs: jobs, ended: ended}
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

func deleteJobCmd(c *Client, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return deleteDoneMsg{name: name, err: c.DeleteSearchJob(ctx, name)}
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

// dropJob takes one row out of the dashboard once the server no longer has it.
// The selection is left to the list, which clamps it, so removing the last row
// does not leave the cursor past the end.
func (m *model) dropJob(name string) tea.Cmd {
	kept := make([]*jobEntry, 0, len(m.jobs))
	for _, e := range m.jobs {
		if e.job != nil && e.job.Name == name {
			if e.dl != nil {
				e.dl.cancel()
			}
			continue
		}
		kept = append(kept, e)
	}
	m.jobs = kept
	m.persist()
	return m.syncList()
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
		jobs = append(jobs, StoredJob{
			Name:      e.job.Name,
			Query:     e.job.Query,
			CreatedAt: e.createdAt,
			EndedAt:   e.endedAt,
		})
	}
	_ = SaveStore(m.storePath, jobs, storeLimit)
}

// outFileNameFor turns users/alice/searchJobs/42 into searchjob-42.jsonl, or
// searchjob-42.log for a log transfer.
func outFileNameFor(name, ext string) string {
	id := JobID(name)
	if id == "" {
		id = "results"
	}
	return "searchjob-" + id + ext
}

func status(s string) tea.Cmd {
	return func() tea.Msg { return statusMsg(s) }
}

func (m *model) beginDownload(e *jobEntry, kind transferKind) tea.Cmd {
	if e.job == nil {
		return nil
	}
	if e.dl != nil {
		return status("already downloading " + e.dl.kind.label())
	}

	var srcURL string
	switch kind {
	case transferLogs:
		srcURL = m.client.LogsURLFor(e.job)
		if srcURL == "" {
			return status("no logs for this job")
		}
	default:
		srcURL = e.job.ResultsURL
		if srcURL == "" {
			return status("no results to download yet")
		}
	}
	outPath := path.Join(m.outDir, outFileNameFor(e.job.Name, kind.ext()))

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan tea.Msg, 64)
	e.dl = &download{kind: kind, cancel: cancel, ch: ch, startedAt: time.Now(), total: -1}
	if kind == transferLogs {
		e.logPath = outPath
	} else {
		e.outPath = outPath
	}

	name := e.job.Name
	client := m.client

	go func() {
		stats, err := client.Download(ctx, srcURL, outPath, func(done, total int64) {
			// Non-blocking: if the UI has not drained the last sample yet,
			// drop this one. A fresher number is always right behind it.
			select {
			case ch <- dlProgressMsg{name: name, done: done, total: total}:
			default:
			}
		})
		ch <- dlDoneMsg{name: name, kind: kind, stats: stats, err: err}
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

// cancelDownloads stops every transfer still in flight.
//
// Quitting has to do this, or the goroutine writing the file outlives the
// program that started it and leaves a partial download looking like a whole
// one. There are two ways out — q from the list, ctrl+c from anywhere — and
// both go through here.
func (m *model) cancelDownloads() {
	for _, e := range m.jobs {
		if e.dl != nil {
			e.dl.cancel()
		}
	}
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
			m.jobs = append(m.jobs, &jobEntry{
				job:       j,
				createdAt: parseCreateTime(j),
				endedAt:   msg.ended[j.Name],
			})
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
			m.persist()
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

	case deleteDoneMsg:
		if e := m.find(msg.name); e != nil {
			e.deleting = false
		}
		switch {
		case msg.err == nil:
			m.status = "deleted " + path.Base(msg.name)
			cmds = append(cmds, m.dropJob(msg.name))
		case NotFound(msg.err):
			// Someone else deleted it, or it was a name recovered from the local
			// cache that the instance no longer has. Either way the row is wrong.
			m.status = path.Base(msg.name) + " was already gone; removed it from the list"
			cmds = append(cmds, m.dropJob(msg.name))
		case Unsupported(msg.err):
			m.canDelete = false
			m.status = "this instance does not support deleting jobs"
		default:
			m.status = "delete failed: " + msg.err.Error()
		}

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
				if msg.kind == transferLogs {
					if e.logPath != "" {
						_ = os.Remove(e.logPath)
					}
					e.logPath = ""
				} else {
					if e.outPath != "" {
						_ = os.Remove(e.outPath)
					}
					e.outPath = ""
				}
				m.status = msg.kind.label() + " download canceled"
			} else {
				e.err = msg.err
				m.status = msg.kind.label() + " download failed: " + msg.err.Error()
			}
			break
		}
		stats := msg.stats
		outPath := e.outPath
		if msg.kind == transferLogs {
			e.logStats = &stats
			outPath = e.logPath
		} else {
			e.stats = &stats
		}
		e.lastDone = msg.kind
		m.status = fmt.Sprintf("wrote %s line(s) of %s to %s (%s)",
			fmtCount(stats.Lines), msg.kind.label(), outPath, fmtBytes(stats.Bytes))

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
		switch {
		case key.Matches(msg, m.keys.ForceQuit):
			// q closes the help screen, so ctrl+c is the only way out of the
			// program from here, and it still owes the downloads their cleanup.
			m.cancelDownloads()
			return m, tea.Quit
		case key.Matches(msg, m.keys.CloseHelp):
			m.mode = modeList
			return m, nil
		}
		// Anything else scrolls. A stray key must not dismiss the one screen that
		// explains what the other keys do.
		var cmd tea.Cmd
		m.helpVP, cmd = m.helpVP.Update(msg)
		return m, cmd

	case modeConfirm:
		// One question, two outcomes, and only y takes the destructive one. The
		// prompt is dismissed either way, so a stray key never leaves the dashboard
		// waiting on an answer nobody knows it wants.
		name := m.confirm
		m.mode, m.confirm = modeList, ""
		if !key.Matches(msg, m.keys.Confirm) {
			m.status = "kept " + path.Base(name)
			return m, nil
		}
		e := m.find(name)
		if e == nil {
			return m, nil
		}
		e.deleting = true
		m.status = "deleting " + path.Base(name) + "…"
		return m, deleteJobCmd(m.client, name)
	}

	// modeList
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.cancelDownloads()
		return m, tea.Quit

	case key.Matches(msg, m.keys.Help):
		m.mode = modeHelp
		// Rebuilt on the way in rather than only on resize: canStop and canDelete
		// can have flipped since the last time the text was written, and they are
		// two of the lines worth reading.
		m.helpVP.SetContent(m.helpText(m.width))
		m.helpVP.GotoTop()
		return m, nil

	case key.Matches(msg, m.keys.New):
		m.mode = modeInput
		m.input.Focus()
		return m, textinput.Blink

	case key.Matches(msg, m.keys.Rerun):
		// Same as the web UI's rerun: submit the selected job's query again as a
		// new job. The original is left alone, so a finished run and its rerun sit
		// side by side in the list and stay comparable.
		e := m.selected()
		if e == nil || e.job == nil || e.job.Query == "" {
			// A job recovered from the cache with no query is the one case where
			// there is nothing to resubmit.
			m.status = "nothing to rerun"
			return m, nil
		}
		m.status = "rerunning " + truncate(e.job.Query, max(20, m.width-11)) + "…"
		return m, createJobCmd(m.client, e.job.Query)

	case key.Matches(msg, m.keys.Download):
		e := m.selected()
		if e == nil {
			return m, nil
		}
		if e.job == nil || e.job.State != StateCompleted {
			m.status = "only completed jobs have results"
			return m, nil
		}
		return m, m.beginDownload(e, transferResults)

	case key.Matches(msg, m.keys.Logs):
		e := m.selected()
		if e == nil || e.job == nil {
			return m, nil
		}
		// A queued job has not run yet, so there is nothing to log. Every other
		// state has a log worth reading, and it is the only explanation a failed
		// job ever gives.
		if e.job.State == StateQueued {
			m.status = "no logs until the job starts"
			return m, nil
		}
		return m, m.beginDownload(e, transferLogs)

	case key.Matches(msg, m.keys.Cancel):
		e := m.selected()
		if e == nil || e.job == nil {
			return m, nil
		}
		if e.dl != nil {
			m.status = "canceling " + e.dl.kind.label() + " download…"
			e.dl.cancel()
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

	case key.Matches(msg, m.keys.Delete):
		e := m.selected()
		if e == nil || e.job == nil || e.job.Name == "" {
			return m, nil
		}
		if !m.canDelete {
			m.status = "this instance does not support deleting jobs"
			return m, nil
		}
		// Nothing is sent yet. The prompt below the list has to be answered first,
		// and the answer is handled in modeConfirm above.
		m.mode = modeConfirm
		m.confirm = e.job.Name
		return m, nil

	case key.Matches(msg, m.keys.Open):
		// The web UI page, not the job's resultsUrl. See SearchJobsPageURL.
		return m, openURLCmd(m.client.SearchJobsPageURL())

	case key.Matches(msg, m.keys.Escape):
		// esc backs out of the query box, the delete prompt, and the help screen.
		// In the list there is nothing to back out of, so it does nothing — but
		// it has to be claimed here, because otherwise it reaches the list, and
		// the list answers esc with tea.Quit.
		return m, nil
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
