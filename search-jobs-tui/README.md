# Search Jobs TUI

A terminal dashboard for Sourcegraph Search Jobs. Submit several queries, watch them all run at once, and download each result set when it finishes. Built with [Bubble Tea](https://github.com/charmbracelet/bubbletea).

<img width="2260" height="926" alt="CleanShot 2026-08-01 at 19 34 47@2x" src="https://github.com/user-attachments/assets/205593ba-b161-4b04-9efa-0f6b60493fc5" />


Video: TODO

## What it does

A Search Job runs one query exhaustively across every repository, branch, and revision. Jobs are asynchronous and can run for hours. The [search-jobs-api](../search-jobs-api/) recipe creates one job and blocks until it finishes, which is the right shape for a script and the wrong shape for a person running an audit. You end up with one terminal per query and no way to see them together.

This recipe watches many jobs at once. It polls each unfinished job independently, so a slow or failing job never holds up the others, and it streams result downloads to disk with live progress.

| File | Use it for |
| --- | --- |
| `api.go` | The whole HTTP surface. Read this one if you want the API, not the terminal UI. It imports nothing from Bubble Tea. |
| `model.go` | State and the update loop: polling fan-out, download progress, key handling. |
| `view.go` | Rendering and layout. |
| `help.go` | The help screen: every key as data, plus what this run is pointed at. |
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

Press `n`, type a query, press enter. Repeat for as many queries as you want to run. Press `d` on a completed job to download its results, or `l` for its log. Press `x` to delete a job you are done with, and `y` to confirm.

Press `?` for the help screen: every key with what it does, and what this run is pointed at: endpoint, where downloads land, the job cache, and whether this instance supports canceling and deleting.

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
| `h` `←` `pgup` `b` `u` | Previous page, when there are more jobs than rows for them |
| `pgdn` `→` `f` | Next page |
| `g` `home` / `G` `end` | First job / last job |
| `n` | New query |
| `cmd-v` `ctrl-v` | Paste a query. Works from the list too, which opens the query box |
| `enter` | Submit the query |
| `esc` | Leave the query box, the delete prompt, or the help screen. Does nothing in the list |
| `r` | Rerun the selected job's query as a new job, as the web UI's rerun does |
| `d` | Download the selected completed job's results |
| `l` | Download the selected job's log |
| `c` | Cancel the in-flight download, if there is one |
| `c` | With no download running, cancel the selected running job |
| `x` | Delete the selected job, after a `y`/`n` confirmation |
| `o` | Open the Search Jobs page in a browser |
| `?` | Help screen. Scrolls; `?`, `esc`, or `q` closes it |
| `q` `ctrl-c` | Quit |

## Flags

| Flag | Default | What it does |
| --- | --- | --- |
| `--query <q>` | none | Submit this query at startup. |
| `--poll <dur>` | `5s` | How long to wait between status checks. |
| `--out-dir <dir>` | `.` | Where downloads are written: `searchjob-<id>.jsonl` for results, `searchjob-<id>.log` for logs. |
| `--state <path>` | platform cache dir | File remembering jobs between runs. |

## How it works

The same three calls as the [search-jobs-api](../search-jobs-api/) recipe, plus three more. Every request carries the header `Authorization: token <token>`.

1. `POST /api/searchjobs.v1.Service/CreateSearchJob` with body `{"parent": "users/-", "searchJob": {"query": "..."}}`. The `users/-` parent means the authenticated user. Needs `externalapi:write`.
2. `POST /api/searchjobs.v1.Service/GetSearchJob` with body `{"name": "..."}`. Needs `externalapi:read`.
3. `GET` the completed job's `resultsUrl`, which streams JSONL.
4. `GET` the job's `logsUrl`, which streams the log as CSV.
5. `POST /api/searchjobs.v1.Service/ListSearchJobs` populates the dashboard at startup.
6. `POST /api/searchjobs.v1.Service/CancelSearchJob` stops a running job.
7. `POST /api/searchjobs.v1.Service/DeleteSearchJob` removes a job and its stored results. Needs `externalapi:write`.

The last three are best-effort. An instance that does not implement them returns an error that `Unsupported` in `api.go` recognizes, and the dashboard quietly does without: it falls back to a local cache of job names for the list, and pressing cancel or delete says the instance does not support it rather than failing. The help screen reports which of the two this instance has. Check your instance's own reference at `$SRC_ENDPOINT/api-reference` for what it actually supports.

Cancel and delete are not the same thing. Cancel stops the work and leaves the record, so a canceled job still sits in the list with whatever results it had reached. Delete takes the record and the stored results away and cannot be undone, which is why `x` asks before sending anything. Files already written to `--out-dir` are local copies and are left alone.

The log is a CSV, one row per repository and revision the job touched, with a status and a failure message for each. It is the only explanation a rejected query ever gets, and it is worth reading on a job that succeeded too: that is where partial coverage shows up. `l` fetches it for any job that has started. An instance that sends no `logsUrl` falls back to `/.api/search/export/<id>.log`, which is what the web UI's own "View logs" button requests, built from the numeric id already in the job record. That path is the internal API, so a token needs the broader `user:all` scope to use it; `logsUrl` needs only `externalapi:read`, which is why it is preferred.

Six things needed more than the framework gives you.

**Animating a list row.** A `list.ItemDelegate` renders rows without access to the model, so it cannot read the spinner. The update loop pushes the current frame into the delegate with `SetDelegate` on every tick. `MiniDot` is the spinner because its frames are one cell wide: the marker sits in a fixed-width column, so a wider frame would shift every column to its right, and only while a job is running. A test walks every frame and checks the row width holds.

**Pasting a query.** A terminal sends a paste as one bracketed-paste event, not as a run of key presses, so a model that switches only on `tea.KeyPressMsg` drops it silently. `tea.PasteMsg` gets its own case, and a paste arriving while the list has focus opens the query box first. `ctrl+v` takes one more step: the input answers that key press with a command that reads the OS clipboard, and the reply is an unexported message type, so the update loop forwards anything it does not recognize to the input while the box is open instead of matching it by name.

**Polling many jobs.** A one-second tick drives elapsed timers. Every `--poll` interval that tick also fans out one command per unfinished job. Each command is independent, so one slow request cannot stall the rest, and a failed poll writes to the status line instead of ending the program. Jobs in `STATE_COMPLETED`, `STATE_FAILED`, or `STATE_CANCELED` drop out of the poll set.

**Streaming download progress.** A `tea.Cmd` returns exactly one message, so it cannot report progress as it goes. The download runs in a goroutine that writes samples onto a buffered channel; a command reads one value from that channel and turns it into a message; the update loop re-issues that command after every read. Progress sends are non-blocking and drop when the buffer is full, because a fresher byte count is always right behind. The final message is a blocking send, so completion is never dropped.

**Confirming a destructive key.** Bubbles ships no confirm component. [huh](https://github.com/charmbracelet/huh) has one, but embedding a form and its theme into a dashboard that already owns its layout is more machinery than a yes/no needs, so the prompt is a fourth mode instead: `x` records the job name and switches to `modeConfirm`, which takes the next key press. Only `y` sends the request; every other key backs out, `q` included, so quit cannot fire out from under an unanswered question, and `enter` is deliberately not wired up because enter is submit everywhere else. The prompt takes the status row and the footer, both of which already exist, so asking does not change the height of the frame.

The row stays until the server confirms the job is gone. Two of those replies are HTTP 404: ConnectRPC answers an unknown procedure with one, and a missing job gets one too, so the status alone would read "no such method" as "no such job". The `code` field separates them, which is what `NotFound` in `api.go` checks. Get that wrong and one job deleted from another window turns the delete key off for the rest of the session.

Results stream straight to disk and are never held in memory, so a multi-gigabyte result set is fine. Canceling a download deletes the partial file rather than leaving something that looks like a complete result set.

**A help screen worth opening.** The bubbles `help` component renders a keymap, which is the easy half. It cannot say that `c` means two different things depending on whether a transfer is running, that the list contributes paging keys this file never wrote, or that `c` does nothing at all on an instance without `CancelSearchJob`. So the help text is a table of sections in `help.go`, each row holding the `key.Binding` it documents, so the label comes from the binding and a rebind moves the text with it, plus a `keys` override for the rows describing someone else's keymap. The list binds `l` to next page and `d` to half a page down, but neither ever reaches it, because the log and download keys are matched first; printing the component's own labels would document a shadow.

That is more text than a footer holds, so help is a full screen with a `viewport` for a body. Not an overlay: lipgloss v2 ships a `Compositor`, but it sizes itself from the longest line of each layer, and this layout's whole contract is that a frame is exactly as tall and wide as the window. The viewport keeps that contract for free, padding to exactly the height it is given, which is stronger than the main view's fixed `chromeHeight` budget. That budget is what let the old help pane render three rows too many for as long as no test looked. Now every mode is checked. Since the screen scrolls, only `?`, `esc`, and `q` close it and everything else moves the text; `ctrl+c` still quits, and still cancels downloads on the way out.

## Testing

The layout tests run without a token or a network connection:

```sh
go test ./...
```

They pin the things that break quietly: every frame fills the window exactly in every mode, no line exceeds the terminal width, job rows stay aligned whether or not they are selected, `x` never deletes anything without an answer, and `esc` does not quit.

## Troubleshooting

**A 401 or 403 response.** The token is missing a scope. Creating and canceling jobs need `externalapi:write`; reading them needs `externalapi:read`. The failure shows up in the status line rather than closing the dashboard.

**`stdout is not a terminal`.** This is a dashboard, so it refuses to render into a pipe. Use the [search-jobs-api](../search-jobs-api/) recipe for scripting; it writes clean JSONL to stdout.

**The job list is empty on a fresh machine.** Either this instance does not implement `ListSearchJobs`, in which case only jobs created through this tool are remembered, or there are genuinely no jobs. Create one with `n`.

**A job fails immediately.** Some queries are not supported by Search Jobs, including catch-all regular expressions like `.*`, file predicates, multiple `rev` filters, and `index:` filters. Press `l` on the failed job to write its log to `--out-dir` as `searchjob-<id>.log`, or fetch it yourself:

```bash
curl -s -H "Authorization: token $SRC_ACCESS_TOKEN" \
  "$SRC_ENDPOINT/api/users/-/searchJobs/<id>/logs.log"
```

`logsUrl` and `resultsUrl` are external API paths, so they authenticate with the `Authorization` header only. Pasting one into a browser answers `External API requires authentication.` even when you are signed in, because the browser sends a session cookie instead of the token. `o` opens `/search-jobs` in the web UI, where the same jobs have download links that do work off the session.
