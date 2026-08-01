# Search Jobs TUI

A terminal dashboard for Sourcegraph Search Jobs. Submit several queries, watch them all run at once, and download each result set when it finishes. Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea).

Video: TODO

## What it does

A Search Job runs one query exhaustively across every repository, branch, and revision. Jobs are asynchronous and can run for hours. The [search-jobs-api](../search-jobs-api/) recipe creates one job and blocks until it finishes, which is the right shape for a script and the wrong shape for a person running an audit. You end up with one terminal per query and no way to see them together.

This recipe watches many jobs at once. It polls each unfinished job independently, so a slow or failing job never holds up the others, and it streams result downloads to disk with live progress.

| File | Use it for |
| --- | --- |
| `api.go` | The whole HTTP surface. Read this one if you want the API, not the terminal UI. It imports nothing from Bubble Tea. |
| `model.go` | State and the update loop: polling fan-out, download progress, key handling. |
| `view.go` | Rendering and layout. |
| `main.go` | Flags, environment, startup. |
| `store.go` | Remembers job names between runs. |
| `render_test.go` | Layout tests. They need no token and no network. |

This recipe takes a dependency, which the [cookbook rule](../README.md#adding-a-recipe) allows when the recipe is about the dependency. Building the TUI is the point here.

## Prerequisites

- Sourcegraph 7.0 or later.
- Go 1.25 or later.
- An access token with both the `externalapi:write` scope (to create and cancel jobs) and the `externalapi:read` scope (to read them).

Set two environment variables:

| Variable | Required | Default |
| --- | --- | --- |
| `SRC_ENDPOINT` | No | `https://demo.sourcegraph.com` |
| `SRC_ACCESS_TOKEN` | Yes | none |

## Quickstart

```sh
export SRC_ENDPOINT="https://demo.sourcegraph.com"
export SRC_ACCESS_TOKEN="sgp_..."

go run .
```

Press `n`, type a query, press enter. Repeat for as many queries as you want to run. Press `d` on a completed job to download its results.

Start with a query already running:

```sh
go run . --query 'context:global patterntype:keyword TODO count:all'
```

Build a binary instead:

```sh
go build -o search-jobs-tui .
```

## Keys

| Key | What it does |
| --- | --- |
| `j` `k` `↑` `↓` | Move the selection |
| `n` | New query |
| `cmd-v` `ctrl-v` | Paste a query. Works from the list too, which opens the query box |
| `enter` | Submit the query |
| `esc` | Leave the query box |
| `d` | Download the selected completed job |
| `c` | Cancel the selected running job, or an in-flight download |
| `o` | Open the Search Jobs page in a browser |
| `?` | Full key list |
| `q` `ctrl-c` | Quit |

## Flags

| Flag | Default | What it does |
| --- | --- | --- |
| `--query <q>` | none | Submit this query at startup. |
| `--poll <dur>` | `5s` | How long to wait between status checks. |
| `--out-dir <dir>` | `.` | Where downloaded JSONL is written. |
| `--state <path>` | platform cache dir | File remembering jobs between runs. |

## How it works

The same three calls as the [search-jobs-api](../search-jobs-api/) recipe, plus two more. Every request carries the header `Authorization: token <token>`.

1. `POST /api/searchjobs.v1.Service/CreateSearchJob` with body `{"parent": "users/-", "searchJob": {"query": "..."}}`. The `users/-` parent means the authenticated user. Needs `externalapi:write`.
2. `POST /api/searchjobs.v1.Service/GetSearchJob` with body `{"name": "..."}`. Needs `externalapi:read`.
3. `GET` the completed job's `resultsUrl`, which streams JSONL.
4. `POST /api/searchjobs.v1.Service/ListSearchJobs` populates the dashboard at startup.
5. `POST /api/searchjobs.v1.Service/CancelSearchJob` stops a running job.

The last two are best-effort. An instance that does not implement them returns an error that `Unsupported` in `api.go` recognizes, and the dashboard quietly does without: it falls back to a local cache of job names for the list, and it hides the cancel action. Check your instance's own reference at `$SRC_ENDPOINT/api-reference` for what it actually supports.

Four things needed more than the framework gives you.

**Animating a list row.** A `list.ItemDelegate` renders rows without access to the model, so it cannot read the spinner. The update loop pushes the current frame into the delegate with `SetDelegate` on every tick. `MiniDot` is the spinner because its frames are one cell wide: the marker sits in a fixed-width column, so a wider frame would shift every column to its right, and only while a job is running. A test walks every frame and checks the row width holds.

**Pasting a query.** A terminal sends a paste as one bracketed-paste event, not as a run of key presses, so a model that switches only on `tea.KeyPressMsg` drops it silently. `tea.PasteMsg` gets its own case, and a paste arriving while the list has focus opens the query box first. `ctrl+v` takes one more step: the input answers that key press with a command that reads the OS clipboard, and the reply is an unexported message type, so the update loop forwards anything it does not recognize to the input while the box is open instead of matching it by name.

**Polling many jobs.** A one-second tick drives elapsed timers. Every `--poll` interval that tick also fans out one command per unfinished job. Each command is independent, so one slow request cannot stall the rest, and a failed poll writes to the status line instead of ending the program. Jobs in `STATE_COMPLETED`, `STATE_FAILED`, or `STATE_CANCELED` drop out of the poll set.

**Streaming download progress.** A `tea.Cmd` returns exactly one message, so it cannot report progress as it goes. The download runs in a goroutine that writes samples onto a buffered channel; a command reads one value from that channel and turns it into a message; the update loop re-issues that command after every read. Progress sends are non-blocking and drop when the buffer is full, because a fresher byte count is always right behind. The final message is a blocking send, so completion is never dropped.

Results stream straight to disk and are never held in memory, so a multi-gigabyte result set is fine. Canceling a download deletes the partial file rather than leaving something that looks like a complete result set.

## Testing

The layout tests run without a token or a network connection:

```sh
go test ./...
```

They pin the things that break quietly: every frame fills the window exactly, no line exceeds the terminal width, and job rows stay aligned whether or not they are selected.

## Troubleshooting

**A 401 or 403 response.** The token is missing a scope. Creating and canceling jobs need `externalapi:write`; reading them needs `externalapi:read`. The failure shows up in the status line rather than closing the dashboard.

**`stdout is not a terminal`.** This is a dashboard, so it refuses to render into a pipe. Use the [search-jobs-api](../search-jobs-api/) recipe for scripting; it writes clean JSONL to stdout.

**The job list is empty on a fresh machine.** Either this instance does not implement `ListSearchJobs`, in which case only jobs created through this tool are remembered, or there are genuinely no jobs. Create one with `n`.

**A job fails immediately.** Some queries are not supported by Search Jobs, including catch-all regular expressions like `.*`, file predicates, multiple `rev` filters, and `index:` filters. The failed job's `logsUrl` is shown under it; fetch it with your token:

```bash
curl -s -H "Authorization: token $SRC_ACCESS_TOKEN" \
  "$SRC_ENDPOINT/api/users/-/searchJobs/<id>/logs.log"
```

`logsUrl` and `resultsUrl` are external API paths, so they authenticate with the `Authorization` header only. Pasting one into a browser answers `External API requires authentication.` even when you are signed in, because the browser sends a session cookie instead of the token. `o` opens `/search-jobs` in the web UI, where the same jobs have download links that do work off the session.
