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
			len([]rune(header))-c.count; got != want {
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
	for _, want := range []string{"logs", "quit"} {
		if !strings.Contains(bar, want) {
			t.Errorf("short help does not mention %q: %q", want, bar)
		}
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
