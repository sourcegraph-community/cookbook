// A terminal dashboard for Sourcegraph Search Jobs.
//
// A Search Job runs one query exhaustively across every repository, branch, and
// revision. Jobs are asynchronous and can run for hours, so this watches several
// at once instead of blocking on one.
//
// Usage:
//
//	export SRC_ENDPOINT="https://demo.sourcegraph.com"
//	export SRC_ACCESS_TOKEN="sgp_..."   # externalapi:read + externalapi:write
//
//	go run .
//	go run . --query 'context:global patterntype:keyword TODO count:all'
//
// For a scriptable, pipeable version of the same API calls, see the
// search-jobs-api recipe next door.

package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

const defaultEndpoint = "https://demo.sourcegraph.com"

func main() {
	var (
		query  = flag.String("query", "", "submit this query at startup")
		poll   = flag.Duration("poll", 5*time.Second, "how often to check job status")
		outDir = flag.String("out-dir", ".", "where downloaded results and logs are written")
		store  = flag.String("state", DefaultStorePath(), "file remembering jobs between runs")
	)
	flag.Parse()

	if err := run(*query, *poll, *outDir, *store); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(query string, poll time.Duration, outDir, storePath string) error {
	// A dashboard has no non-interactive mode. Rather than render escape codes
	// into a pipe, point at the recipe that was built for that.
	if st, err := os.Stdout.Stat(); err == nil && st.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("stdout is not a terminal; for scripting use the search-jobs-api recipe:\n" +
			"  node ../search-jobs-api/search_job.ts --out -")
	}

	endpoint := os.Getenv("SRC_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	token := os.Getenv("SRC_ACCESS_TOKEN")
	if token == "" {
		return fmt.Errorf("SRC_ACCESS_TOKEN is not set. " +
			"Create a token with externalapi:read and externalapi:write scopes.")
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("out-dir: %w", err)
	}
	if poll < time.Second {
		poll = time.Second
	}

	m := newModel(NewClient(endpoint, token), outDir, storePath, poll, query)

	// A query passed on the command line is submitted as soon as the program
	// starts, so `go run . --query ...` behaves like the script version.
	var boot tea.Cmd
	if query != "" {
		boot = createJobCmd(m.client, query)
	}

	p := tea.NewProgram(bootModel{model: m, boot: boot})
	_, err := p.Run()
	return err
}

// bootModel runs one extra command on startup without complicating newModel.
type bootModel struct {
	model
	boot tea.Cmd
}

func (b bootModel) Init() tea.Cmd {
	if b.boot == nil {
		return b.model.Init()
	}
	return tea.Batch(b.model.Init(), b.boot)
}

func (b bootModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := b.model.Update(msg)
	b.model = next.(model)
	return b, cmd
}
