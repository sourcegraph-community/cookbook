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

func TestRenderFillsExactlyTheWindow(t *testing.T) {
	cases := []struct{ w, h int }{{80, 24}, {40, 24}, {200, 60}, {40, 12}}
	jobs := []*SearchJob{
		{Name: "users/a/searchJobs/1", Query: "context:global TODO count:all", State: StateProcessing},
		{Name: "users/a/searchJobs/2", Query: "panic( lang:go", State: StateCompleted, ResultsURL: "/x"},
		{Name: "users/a/searchJobs/3", Query: ".* count:all", State: StateFailed, LogsURL: "/logs"},
	}
	for _, tc := range cases {
		for _, n := range []int{0, 3} {
			m := testModel(tc.w, tc.h, jobs[:n]...)
			out := m.render()
			got := strings.Count(out, "\n") + 1
			if got != tc.h {
				t.Errorf("w=%d h=%d jobs=%d: rendered %d lines, want %d", tc.w, tc.h, n, got, tc.h)
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

// The full help pane replaces a one-line footer, so its height is what decides
// whether the frame still fits. Four rows is the budget; a fifth column is free.
func TestFullHelpStaysFourRows(t *testing.T) {
	m := testModel(80, 24)
	pane := stripANSI(m.help.FullHelpView(m.keys.FullHelp()))
	if rows := strings.Count(pane, "\n") + 1; rows > 4 {
		t.Errorf("full help is %d rows:\n%s", rows, pane)
	}
	for i, line := range strings.Split(pane, "\n") {
		if n := len([]rune(line)); n > 80 {
			t.Errorf("full help line %d is %d cells: %q", i, n, line)
		}
	}
	// Every key the list acts on has to appear here, since this is the only place
	// the ones missing from the one-line bar are documented.
	for _, want := range []string{"delete this job", "open in browser", "rerun this query"} {
		if !strings.Contains(pane, want) {
			t.Errorf("full help does not mention %q:\n%s", want, pane)
		}
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
