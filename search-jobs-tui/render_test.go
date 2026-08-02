package main

import (
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

func testModel(w, h int, jobs ...*SearchJob) model {
	m := newModel(NewClient("https://demo.sourcegraph.com", "t"), ".", "", 5*time.Second, "")
	for _, j := range jobs {
		m.jobs = append(m.jobs, &jobEntry{job: j, createdAt: time.Now().Add(-90 * time.Second)})
	}
	m.syncList()
	m.width, m.height = w, h
	m.layout()
	return m
}

// Every mode has to draw exactly the window, not just the list. Checking only
// modeList is how the help screen came to render three rows too many: its footer
// grew to four rows and the chrome budget still said one.
func TestRenderFillsExactlyTheWindow(t *testing.T) {
	cases := []struct{ w, h int }{{80, 24}, {40, 24}, {200, 60}, {40, 12}}
	jobs := []*SearchJob{
		{Name: "users/a/searchJobs/1", Query: "context:global TODO count:all", State: StateProcessing},
		{Name: "users/a/searchJobs/2", Query: "panic( lang:go", State: StateCompleted, ResultsURL: "/x"},
		{Name: "users/a/searchJobs/3", Query: ".* count:all", State: StateFailed, LogsURL: "/logs"},
	}
	// The key that opens each mode, pressed from the list. An empty list has
	// nothing to delete, so modeConfirm is only reachable in the three-job case;
	// enterMode checks it landed rather than assuming.
	modes := []struct {
		name string
		key  tea.KeyPressMsg
		want mode
	}{
		{"list", tea.KeyPressMsg{}, modeList},
		{"input", tea.KeyPressMsg{Code: 'n', Text: "n"}, modeInput},
		{"help", tea.KeyPressMsg{Code: '?', Text: "?"}, modeHelp},
		{"confirm", tea.KeyPressMsg{Code: 'x', Text: "x"}, modeConfirm},
	}

	for _, tc := range cases {
		for _, n := range []int{0, 3} {
			for _, md := range modes {
				m := testModel(tc.w, tc.h, jobs[:n]...)
				if md.want != modeList {
					next, _ := m.handleKey(md.key)
					m = next.(model)
					if m.mode != md.want {
						continue
					}
				}
				got := strings.Count(m.render(), "\n") + 1
				if got != tc.h {
					t.Errorf("%s w=%d h=%d jobs=%d: rendered %d lines, want %d",
						md.name, tc.w, tc.h, n, got, tc.h)
				}
			}
		}
	}
}

func TestRenderNeverExceedsWidth(t *testing.T) {
	jobs := []*SearchJob{
		{Name: "users/averylongusername/searchJobs/12345", State: StateProcessing,
			Query: "context:global patterntype:keyword TODO OR FIXME OR XXX count:all lang:go"},
	}
	for _, w := range []int{40, 60, 80} {
		m := testModel(w, 24, jobs...)
		for i, line := range strings.Split(m.render(), "\n") {
			if n := len([]rune(stripANSI(line))); n > w {
				t.Errorf("w=%d line %d is %d cells: %q", w, i, n, stripANSI(line))
			}
		}
	}
}

// The selected-row prefix "› " is 4 bytes but 2 display cells. Measuring it in
// bytes silently shifts every column on the selected row.
func TestJobRowsAlignRegardlessOfSelection(t *testing.T) {
	jobs := []*SearchJob{
		{Name: "users/a/searchJobs/1", Query: "context:global TODO count:all", State: StateProcessing},
		{Name: "users/a/searchJobs/2", Query: "panic( lang:go", State: StateCompleted},
		{Name: "users/a/searchJobs/3", Query: ".* count:all", State: StateFailed},
	}
	for sel := range jobs {
		m := testModel(80, 24, jobs...)
		m.list.Select(sel)
		var widths []int
		for _, line := range strings.Split(m.list.View(), "\n") {
			plain := stripANSI(line)
			if strings.TrimSpace(plain) == "" {
				continue
			}
			widths = append(widths, len([]rune(plain)))
		}
		for i, w := range widths {
			if w != widths[0] {
				t.Errorf("selection=%d: row %d is %d cells, row 0 is %d", sel, i, w, widths[0])
			}
		}
	}
}

// The running job's marker is a spinner frame. A frame wider than one cell
// shifts every column to its right, and it would do so only while a job is
// running, which is the hardest case to notice by eye.
func TestSpinnerFrameKeepsRowWidth(t *testing.T) {
	jobs := []*SearchJob{
		{Name: "users/a/searchJobs/1", Query: "context:global TODO count:all", State: StateProcessing},
		{Name: "users/a/searchJobs/2", Query: "panic( lang:go", State: StateCompleted},
	}
	m := testModel(80, 24, jobs...)
	var want int
	for _, frame := range spinner.MiniDot.Frames {
		m.list.SetDelegate(jobDelegate{spin: frame})
		for i, line := range strings.Split(m.list.View(), "\n") {
			plain := stripANSI(line)
			if strings.TrimSpace(plain) == "" {
				continue
			}
			got := len([]rune(plain))
			if want == 0 {
				want = got
				continue
			}
			if got != want {
				t.Errorf("frame %q: row %d is %d cells, want %d", frame, i, got, want)
			}
		}
	}
}

// Labels that do not sit over the cells they name are worse than no labels. Both
// are built from columnsFor, so this checks the two agree at several widths: the
// header is the same width as a row, and each right-hand label ends where its
// cell does.
func TestColumnLabelsLineUpWithRows(t *testing.T) {
	jobs := []*SearchJob{
		{Name: "users/a/searchJobs/1", Query: "context:global TODO count:all", State: StateCompleted},
	}
	for _, w := range []int{40, 60, 80, 200} {
		m := testModel(w, 24, jobs...)
		m.jobs[0].endedAt = time.Now()

		header := stripANSI(columnLabels(w))
		row := stripANSI(strings.SplitN(m.list.View(), "\n", 2)[0])
		if len([]rune(header)) != len([]rune(row)) {
			t.Errorf("w=%d: header is %d cells, row is %d: %q vs %q",
				w, len([]rune(header)), len([]rune(row)), header, row)
		}

		c := columnsFor(w)
		if got, want := strings.Index(header, "status"), c.prefix+c.glyph; got != want {
			t.Errorf("w=%d: status label at %d, state cell starts at %d", w, got, want)
		}
		// "finished" is right-aligned in its cell, as the timestamps under it are.
		if got, want := strings.Index(header, "finished")+len("finished"),
			len([]rune(header)); got != want {
			t.Errorf("w=%d: finished label ends at %d, its cell ends at %d", w, got, want)
		}
	}
}

// The one-line help bar is cut off at its right-hand end, which is where q quit
// sits, so adding one more key can silently push quit off an 80-column terminal.
func TestShortHelpFitsAnEightyColumnTerminal(t *testing.T) {
	m := testModel(80, 24)
	bar := stripANSI(m.help.ShortHelpView(m.keys.ShortHelp()))
	if n := len([]rune(bar)); n > 80 {
		t.Errorf("short help is %d cells: %q", n, bar)
	}
	for _, want := range []string{"rerun", "logs", "delete", "quit"} {
		if !strings.Contains(bar, want) {
			t.Errorf("short help does not mention %q: %q", want, bar)
		}
	}
}

// The help screen's own footer is truncated at the right like any other, and the
// one thing it cannot lose is how to get out.
func TestHelpFooterSurvivesFortyColumns(t *testing.T) {
	m := testModel(40, 24)
	bar := stripANSI(m.help.ShortHelpView(m.keys.HelpModeHelp()))
	if n := len([]rune(bar)); n > 40 {
		t.Errorf("help footer is %d cells: %q", n, bar)
	}
	if !strings.Contains(bar, "close") {
		t.Errorf("help footer does not say how to close: %q", bar)
	}
}

// The help screen is the only place several keys are written down at all, so a
// key that stops appearing here stops being discoverable.
func TestHelpDocumentsEveryActionKey(t *testing.T) {
	m := testModel(80, 24)
	text := stripANSI(m.helpText(80))

	for _, s := range m.helpSections() {
		for _, e := range s.entries {
			if !strings.Contains(text, e.label()) {
				t.Errorf("help text is missing the %q row", e.label())
			}
		}
	}

	// The paging keys come from the list's keymap and were documented nowhere.
	for _, want := range []string{"pgup", "pgdn", "g home", "G end"} {
		if !strings.Contains(text, want) {
			t.Errorf("help text does not mention %q", want)
		}
	}

	// c means two things. Both have to be spelled out, each with its condition,
	// or the row that does not apply reads as the one that does.
	for _, want := range []string{"while a transfer is running", "with no transfer running"} {
		if !strings.Contains(text, want) {
			t.Errorf("help text does not distinguish the two meanings of c: missing %q", want)
		}
	}

	// The list binds l to next page and d to half a page down, but neither ever
	// reaches it: the log and download keys are matched first. Offering them as
	// paging keys would document a shadow.
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "page") && !strings.Contains(line, "job") {
			continue
		}
		for _, shadowed := range []string{"→/l", "/l ", "pgdn f d", " d "} {
			if strings.Contains(line, "next page") && strings.Contains(line, shadowed) {
				t.Errorf("help offers the shadowed key %q for paging: %q", shadowed, line)
			}
		}
	}
}

// Three things the dashboard knows and never shows: where it is pointed, where
// downloads land, and which of these keys this instance actually implements.
func TestHelpReportsThisSession(t *testing.T) {
	m := testModel(80, 24)
	text := stripANSI(m.helpText(80))
	for _, want := range []string{m.client.Endpoint, "searchjob-<id>.jsonl", "SRC_ACCESS_TOKEN", "every 5s"} {
		if !strings.Contains(text, want) {
			t.Errorf("help text does not report %q", want)
		}
	}
	// An empty store path means this run forgets its jobs on exit, which is worth
	// saying rather than printing as a blank.
	if !strings.Contains(text, "does not remember jobs") {
		t.Error("help text does not say the job cache is off")
	}

	m.canStop, m.canDelete = false, false
	if n := strings.Count(stripANSI(m.helpText(80)), "not supported by this instance"); n != 2 {
		t.Errorf("unsupported instance: %d rows say so, want 2", n)
	}
	// Read from the sections rather than the rendered body: the sentence is long
	// enough that wrapping puts a newline through the middle of it.
	var descs strings.Builder
	for _, s := range m.helpSections() {
		for _, e := range s.entries {
			descs.WriteString(e.desc + "\n")
		}
	}
	for _, want := range []string{"does not support canceling jobs", "does not support deleting jobs"} {
		if !strings.Contains(descs.String(), want) {
			t.Errorf("help does not explain %q on an instance that lacks it", want)
		}
	}
}

// The hanging indent under a key is arithmetic, and the viewport would hide a
// mistake in it by cutting the line to width.
func TestHelpWrapsWithoutOverflowing(t *testing.T) {
	m := testModel(80, 24)
	for _, w := range []int{40, 60, 80, 200} {
		for i, line := range strings.Split(m.helpText(w), "\n") {
			if n := len([]rune(stripANSI(line))); n > w {
				t.Errorf("w=%d: help line %d is %d cells: %q", w, i, n, stripANSI(line))
			}
		}
	}
}

// Help used to close on any key, which is what made it unscrollable. Only the
// three keys that mean "out" close it now; everything else moves the text.
func TestHelpScrollsInsteadOfClosing(t *testing.T) {
	open := func() model {
		m := testModel(40, 12)
		next, _ := m.handleKey(tea.KeyPressMsg{Code: '?', Text: "?"})
		return next.(model)
	}

	base := open()
	if base.mode != modeHelp {
		t.Fatalf("? left mode = %v, want modeHelp", base.mode)
	}
	first := stripANSI(base.render())

	for _, k := range []tea.KeyPressMsg{
		{Code: 'j', Text: "j"},
		{Code: tea.KeyPgDown},
		// No Text on a modified key: Key.String reports Text when it is set, so
		// "d" would arrive as a plain d.
		{Code: 'd', Mod: tea.ModCtrl},
	} {
		next, _ := open().handleKey(k)
		m := next.(model)
		if m.mode != modeHelp {
			t.Errorf("%v closed help", k)
			continue
		}
		if stripANSI(m.render()) == first {
			t.Errorf("%v did not scroll the help text", k)
		}
	}

	// An unbound key is not an exit. Pressing one while reading should do
	// nothing at all, not dismiss the text.
	next, _ := open().handleKey(tea.KeyPressMsg{Code: 'z', Text: "z"})
	if m := next.(model); m.mode != modeHelp {
		t.Error("an unbound key closed help")
	} else if stripANSI(m.render()) != first {
		t.Error("an unbound key scrolled the help text")
	}

	for _, k := range []tea.KeyPressMsg{
		{Code: '?', Text: "?"},
		{Code: tea.KeyEscape},
		{Code: 'q', Text: "q"},
	} {
		next, _ := open().handleKey(k)
		if m := next.(model); m.mode != modeList {
			t.Errorf("%v left mode = %v, want modeList", k, m.mode)
		}
	}
}

// q closes the help screen rather than the program, so ctrl+c is the only way
// out from here — and it still owes an in-flight download its cleanup.
func TestForceQuitFromHelpStopsDownloads(t *testing.T) {
	jobs := []*SearchJob{{Name: "users/a/searchJobs/1", Query: "TODO", State: StateProcessing}}
	m := testModel(80, 24, jobs...)

	canceled := false
	m.jobs[0].dl = &download{cancel: func() { canceled = true }}

	next, _ := m.handleKey(tea.KeyPressMsg{Code: '?', Text: "?"})
	m = next.(model)
	_, cmd := m.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("ctrl+c in help issued no command, want quit")
	}
	if !canceled {
		t.Error("ctrl+c in help quit without canceling the download")
	}
}

// esc used to reach the list, which answered it with tea.Quit — skipping the
// download cleanup and taking the dashboard down on a key that means "back"
// everywhere else.
func TestEscapeDoesNotQuitTheJobList(t *testing.T) {
	jobs := []*SearchJob{{Name: "users/a/searchJobs/1", Query: "TODO", State: StateProcessing}}
	m := testModel(80, 24, jobs...)
	next, cmd := m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := next.(model).mode; got != modeList {
		t.Errorf("esc left mode = %v, want modeList", got)
	}
	if cmd != nil {
		t.Error("esc in the job list issued a command, want nothing")
	}
}

// x never deletes on its own. It opens a prompt, and only y answers it; every
// other key backs out, including q, which must not quit out from under an
// unanswered question.
func TestDeleteAsksFirst(t *testing.T) {
	jobs := []*SearchJob{
		{Name: "users/a/searchJobs/145", Query: "context:global TODO count:all", State: StateCompleted},
		{Name: "users/a/searchJobs/146", Query: "panic( lang:go", State: StateProcessing},
	}
	x := tea.KeyPressMsg{Code: 'x', Text: "x"}

	for _, deny := range []tea.KeyPressMsg{
		{Code: 'n', Text: "n"},
		{Code: tea.KeyEscape},
		{Code: 'q', Text: "q"},
	} {
		m := testModel(80, 24, jobs...)
		next, cmd := m.handleKey(x)
		m = next.(model)
		if m.mode != modeConfirm {
			t.Fatalf("x left mode = %v, want modeConfirm", m.mode)
		}
		if cmd != nil {
			t.Error("x issued a command before the prompt was answered")
		}
		if m.confirm != "users/a/searchJobs/145" {
			t.Errorf("confirm = %q, want the selected job", m.confirm)
		}
		frame := stripANSI(m.render())
		for _, want := range []string{"delete job 145", "cannot be undone", "y delete it"} {
			if !strings.Contains(frame, want) {
				t.Errorf("prompt frame does not contain %q", want)
			}
		}

		next, cmd = m.handleKey(deny)
		m = next.(model)
		if m.mode != modeList {
			t.Errorf("%v left mode = %v, want modeList", deny, m.mode)
		}
		if cmd != nil {
			t.Errorf("%v issued a command; nothing should happen on a denied delete", deny)
		}
		if len(m.jobs) != 2 {
			t.Errorf("%v: %d jobs left, want 2", deny, len(m.jobs))
		}
	}

	// y is the only key that sends the request.
	m := testModel(80, 24, jobs...)
	next, _ := m.handleKey(x)
	m = next.(model)
	next, cmd := m.handleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})
	m = next.(model)
	if cmd == nil {
		t.Error("y issued no delete command")
	}
	if m.mode != modeList || m.confirm != "" {
		t.Errorf("y left mode = %v confirm = %q, want modeList and no pending job", m.mode, m.confirm)
	}
	// The row stays until the server says it is gone.
	if len(m.jobs) != 2 {
		t.Errorf("%d jobs after y, want 2 until the reply arrives", len(m.jobs))
	}
	if !strings.Contains(m.status, "deleting 145") {
		t.Errorf("status = %q, want it to say the delete is in flight", m.status)
	}
	m.list.SetDelegate(jobDelegate{spin: "⠋"})
	row := stripANSI(strings.SplitN(m.list.View(), "\n", 2)[0])
	if !strings.Contains(row, "⠋ Deleting") {
		t.Errorf("row = %q, want deleting state and spinner", row)
	}

	// A running job is worth flagging: the prompt is the last chance to notice.
	m = testModel(80, 24, jobs...)
	m.list.Select(1)
	next, _ = m.handleKey(x)
	m = next.(model)
	if frame := stripANSI(m.render()); !strings.Contains(frame, "still processing") {
		t.Error("prompt for an unfinished job does not say it is still running")
	}
}

// The prompt takes the status row instead of a pane of its own, so asking must
// not move anything else.
func TestConfirmPromptKeepsTheFrameSize(t *testing.T) {
	for _, tc := range []struct{ w, h int }{{80, 24}, {40, 12}, {200, 60}} {
		m := testModel(tc.w, tc.h, &SearchJob{
			Name:  "users/a/searchJobs/145",
			Query: "context:global patterntype:keyword TODO OR FIXME count:all lang:go",
			State: StateCompleted,
		})
		next, _ := m.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
		m = next.(model)

		out := m.render()
		if got := strings.Count(out, "\n") + 1; got != tc.h {
			t.Errorf("w=%d h=%d: prompt frame is %d lines, want %d", tc.w, tc.h, got, tc.h)
		}
		for i, line := range strings.Split(out, "\n") {
			if n := len([]rune(stripANSI(line))); n > tc.w {
				t.Errorf("w=%d: prompt frame line %d is %d cells: %q", tc.w, i, n, stripANSI(line))
			}
		}
	}
}

// What the server's reply does to the row. A 404 means two different things
// here, and the code tells them apart: the job is gone, or the method is.
func TestDeleteReplyHandling(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantJobs      int
		wantCanDelete bool
		wantStatus    string
	}{
		{"deleted", nil, 1, true, "deleted 145"},
		{"already gone", &APIError{Status: 404, Code: "not_found"}, 1, true, "already gone"},
		{"method missing", &APIError{Status: 404, Code: "unimplemented"}, 2, false, "does not support"},
		{"server error", &APIError{Status: 500}, 2, true, "delete failed"},
	}
	for _, tc := range cases {
		m := testModel(80, 24,
			&SearchJob{Name: "users/a/searchJobs/145", Query: "x", State: StateCompleted},
			&SearchJob{Name: "users/a/searchJobs/146", Query: "y", State: StateCompleted},
		)
		next, _ := m.Update(deleteDoneMsg{name: "users/a/searchJobs/145", err: tc.err})
		m = next.(model)

		if len(m.jobs) != tc.wantJobs {
			t.Errorf("%s: %d jobs left, want %d", tc.name, len(m.jobs), tc.wantJobs)
		}
		if len(m.list.Items()) != tc.wantJobs {
			t.Errorf("%s: list shows %d rows, want %d", tc.name, len(m.list.Items()), tc.wantJobs)
		}
		if m.canDelete != tc.wantCanDelete {
			t.Errorf("%s: canDelete = %v, want %v", tc.name, m.canDelete, tc.wantCanDelete)
		}
		if !strings.Contains(m.status, tc.wantStatus) {
			t.Errorf("%s: status = %q, want it to contain %q", tc.name, m.status, tc.wantStatus)
		}
		if tc.wantJobs == 2 && m.jobs[0].deleting {
			t.Errorf("%s: row remained in deleting state after reply", tc.name)
		}
	}

	// An instance that answered "unimplemented" once is not asked again.
	m := testModel(80, 24, &SearchJob{Name: "users/a/searchJobs/145", Query: "x", State: StateCompleted})
	m.canDelete = false
	next, cmd := m.handleKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = next.(model)
	if m.mode != modeList || cmd != nil {
		t.Error("x prompted on an instance that cannot delete")
	}
	if !strings.Contains(m.status, "does not support") {
		t.Errorf("status = %q, want it to say deleting is unsupported", m.status)
	}
}

// A job with a transfer running is still deletable, and the transfer has to stop
// with it: nothing else would ever close that goroutine's file.
func TestDeleteStopsAnInFlightDownload(t *testing.T) {
	m := testModel(80, 24, &SearchJob{Name: "users/a/searchJobs/145", Query: "x", State: StateCompleted})
	canceled := false
	m.jobs[0].dl = &download{kind: transferResults, cancel: func() { canceled = true }, total: -1}

	next, _ := m.Update(deleteDoneMsg{name: "users/a/searchJobs/145"})
	m = next.(model)
	if !canceled {
		t.Error("deleting a job left its download running")
	}
	if len(m.jobs) != 0 {
		t.Errorf("%d jobs left, want 0", len(m.jobs))
	}
}

// stripANSI removes escape sequences so widths can be measured.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestFmtBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 512: "512 B", 1024: "1.0 KB", 1536: "1.5 KB",
		10 * 1024: "10 KB", 1024 * 1024: "1.0 MB", 66 * 1024 * 1024: "66 MB"}
	for in, want := range cases {
		if got := fmtBytes(in); got != want {
			t.Errorf("fmtBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFmtDuration(t *testing.T) {
	cases := map[time.Duration]string{
		1500 * time.Millisecond: "1.5s",
		45 * time.Second:        "45s",
		134 * time.Second:       "2m 14s",
		3 * time.Hour:           "3h 0m",
	}
	for in, want := range cases {
		if got := fmtDuration(in); got != want {
			t.Errorf("fmtDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestFmtWhen(t *testing.T) {
	done := time.Date(2026, 8, 1, 9, 18, 3, 0, time.Local)
	running := &jobEntry{createdAt: time.Now().Add(-90 * time.Second)}
	finished := &jobEntry{createdAt: done.Add(-time.Minute), endedAt: done}

	cases := []struct {
		name     string
		e        *jobEntry
		state    string
		withDate bool
		want     string
	}{
		{"finished wide", finished, StateCompleted, true, "2026-08-01 | 09:18:03"},
		{"finished narrow", finished, StateCompleted, false, "09:18:03"},
		{"running shows elapsed", running, StateProcessing, true, "1m 30s"},
		// Terminal before this process ever saw it: no finish time exists, and
		// timing from the create time would invent one.
		{"terminal without a finish time", running, StateCompleted, true, ""},
	}
	for _, tc := range cases {
		if got := fmtWhen(tc.e, tc.state, tc.withDate); got != tc.want {
			t.Errorf("%s: fmtWhen = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFmtCount(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1000: "1,000", 12904: "12,904", 1234567: "1,234,567"}
	for in, want := range cases {
		if got := fmtCount(in); got != want {
			t.Errorf("fmtCount(%d) = %q, want %q", in, got, want)
		}
	}
}

// Instances return resultsUrl absolute. Concatenating the endpoint onto it used
// to produce https://host/https:/host/api/... .
func TestResolveURL(t *testing.T) {
	c := NewClient("https://demo.sourcegraph.com", "t")
	want := "https://demo.sourcegraph.com/api/users/129/searchJobs/142/results.jsonl"
	for _, in := range []string{
		want,
		"/api/users/129/searchJobs/142/results.jsonl",
		"api/users/129/searchJobs/142/results.jsonl",
	} {
		got, err := c.ResolveURL(in)
		if err != nil {
			t.Fatalf("ResolveURL(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ResolveURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// A terminal delivers cmd+v as one bracketed-paste event, not as a run of key
// presses, so a model that switches only on tea.KeyPressMsg drops it.
func TestPasteFillsTheQueryInput(t *testing.T) {
	m := testModel(80, 24)

	next, _ := m.Update(tea.PasteMsg{Content: "context:global TODO\ncount:all"})
	m = next.(model)
	if m.mode != modeInput {
		t.Errorf("pasting from the list left mode = %v, want modeInput", m.mode)
	}
	// The query box is one line, so newlines have to collapse to spaces.
	if got, want := m.input.Value(), "context:global TODO count:all"; got != want {
		t.Errorf("input = %q, want %q", got, want)
	}

	// Already in the box, a second paste appends at the cursor.
	next, _ = m.Update(tea.PasteMsg{Content: " lang:go"})
	m = next.(model)
	if got, want := m.input.Value(), "context:global TODO count:all lang:go"; got != want {
		t.Errorf("input after second paste = %q, want %q", got, want)
	}
}

// r resubmits the selected job's query without touching the original job. The
// created job comes back from the server, so this checks the key is wired to a
// command and that a row with no query to resubmit says so instead.
func TestRerunSubmitsTheSelectedQuery(t *testing.T) {
	m := testModel(80, 24,
		&SearchJob{Name: "users/a/searchJobs/1", Query: "context:global TODO count:all", State: StateCompleted},
		&SearchJob{Name: "users/a/searchJobs/2", State: StateUnspecified},
	)
	r := tea.KeyPressMsg{Code: 'r', Text: "r"}

	next, cmd := m.handleKey(r)
	m = next.(model)
	if cmd == nil {
		t.Error("r on a job with a query issued no command")
	}
	if !strings.Contains(m.status, "context:global TODO count:all") {
		t.Errorf("status = %q, want the query in it", m.status)
	}
	if n := len(m.jobs); n != 2 {
		t.Errorf("rerun changed the list to %d jobs; the original should be untouched", n)
	}

	// Row two is a name recovered from the cache with no query behind it.
	m.list.Select(1)
	next, cmd = m.handleKey(r)
	m = next.(model)
	if cmd != nil {
		t.Error("r on a job with no query issued a command")
	}
	if got, want := m.status, "nothing to rerun"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
}

func TestOutFileNameFor(t *testing.T) {
	if got := outFileNameFor("users/alice/searchJobs/42", ".jsonl"); got != "searchjob-42.jsonl" {
		t.Errorf("got %q", got)
	}
	if got := outFileNameFor("users/alice/searchJobs/42", ".log"); got != "searchjob-42.log" {
		t.Errorf("got %q", got)
	}
}

// The web UI's "View logs" button ignores logsUrl and GETs
// /.api/search/export/{id}.log. Instances that send logsUrl are taken at their
// word; the rest get that path built from the id in the job name.
func TestLogsURLFor(t *testing.T) {
	c := NewClient("https://demo.sourcegraph.com", "t")
	cases := []struct {
		name string
		job  *SearchJob
		want string
	}{
		{"no logsUrl", &SearchJob{Name: "users/129/searchJobs/145"}, "/.api/search/export/145.log"},
		{"logsUrl wins", &SearchJob{Name: "users/129/searchJobs/145", LogsURL: "/api/users/129/searchJobs/145/logs.log"},
			"/api/users/129/searchJobs/145/logs.log"},
		{"no id", &SearchJob{Name: ""}, ""},
		{"no job", nil, ""},
	}
	for _, tc := range cases {
		if got := c.LogsURLFor(tc.job); got != tc.want {
			t.Errorf("%s: LogsURLFor = %q, want %q", tc.name, got, tc.want)
		}
	}

	// The fallback is a reference, so it has to resolve against the endpoint.
	got, err := c.ResolveURL(c.LogsURLFor(&SearchJob{Name: "users/129/searchJobs/145"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://demo.sourcegraph.com/.api/search/export/145.log"; got != want {
		t.Errorf("resolved = %q, want %q", got, want)
	}
}

// Only one transfer runs per job, so pressing l during a results download says
// so instead of starting a second one that would fight over the same slot.
func TestOneTransferPerJob(t *testing.T) {
	m := testModel(80, 24, &SearchJob{
		Name: "users/a/searchJobs/1", Query: "x", State: StateCompleted,
		ResultsURL: "/api/users/a/searchJobs/1/results.jsonl",
	})
	e := m.jobs[0]
	e.dl = &download{kind: transferResults, total: -1}

	msg := m.beginDownload(e, transferLogs)()
	if got, want := string(msg.(statusMsg)), "already downloading results"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	if e.logPath != "" {
		t.Errorf("logPath = %q, want empty", e.logPath)
	}
}
