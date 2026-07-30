# Search Jobs API

Create a Sourcegraph Search Job over the API, poll it to completion, and download the results as JSONL. Runs under bare `node` with no dependencies.

Video: [VIDEO_URL](VIDEO_URL)

## What it does

A Search Job runs one query exhaustively across every repository, branch, and revision. It runs asynchronously and returns a complete result set. Regular search is fast and ranked; this is the opposite trade. Use it for audits, security reviews, and large migrations.

Both scripts here wrap the three HTTP calls that make up that flow: create the job, poll it until it finishes, then stream the results file.

| File | Use it for |
| --- | --- |
| `search_job.ts` | The full version. Spinner, progress bar, byte and line counts, flag validation. |
| `search_job_minimal.ts` | The same three calls with no terminal UI. Read this one first if you want the API, not the polish. |

## Prerequisites

- Sourcegraph 7.0 or later.
- Node 22.18+ or 24.3+, where TypeScript type stripping is on by default. On Node 22.6 through 22.17, run `node --experimental-strip-types search_job.ts`. On anything older, run `npx tsx search_job.ts`.
- An access token with both the `externalapi:write` scope (to create a job) and the `externalapi:read` scope (to read one).

Set two environment variables:

| Variable | Required | Default |
| --- | --- | --- |
| `SRC_ENDPOINT` | No | `https://demo.sourcegraph.com` |
| `SRC_ACCESS_TOKEN` | Yes | none |

## Quickstart

```sh
export SRC_ENDPOINT="https://demo.sourcegraph.com"
export SRC_ACCESS_TOKEN="sgp_..."

node search_job.ts
```

Write the results somewhere specific:

```sh
node search_job.ts \
  --query 'context:global patterntype:keyword TODO count:all' \
  --out todos.jsonl
```

## Flags

| Flag | Default | What it does |
| --- | --- | --- |
| `--query <q>` | built-in default query | The search query to run exhaustively. |
| `--out <path>` | `results.jsonl` | Where to write JSONL. Use `-` for stdout. |
| `--poll <secs>` | `5` | How long to wait between status checks. |
| `--timeout <secs>` | `1800` | Give up after this long. |
| `--quiet` | off | Drop the spinner and progress bar. |

`search_job_minimal.ts` takes the same flags except `--quiet`, which it does not need. It also ignores flags it does not recognize instead of rejecting them.

## How it works

Three HTTP calls, all under `/api`. Every request carries the header `Authorization: token <token>`.

1. `POST /api/searchjobs.v1.Service/CreateSearchJob` with body `{"parent": "users/-", "searchJob": {"query": "..."}}`. The `users/-` parent means the authenticated user. The response is a `SearchJob` with a `name` like `users/{user}/searchJobs/{id}` and a `state`. Needs the `externalapi:write` scope.
2. `POST /api/searchjobs.v1.Service/GetSearchJob` with body `{"name": "..."}`. Poll this until `state` is `STATE_COMPLETED`. The other states are `STATE_UNSPECIFIED`, `STATE_QUEUED`, `STATE_PROCESSING`, `STATE_ERRORED`, `STATE_FAILED`, and `STATE_CANCELED`. Needs the `externalapi:read` scope.
3. `GET` the completed job's `resultsUrl`, which streams JSONL.

Failed and canceled jobs expose a `logsUrl`. The script prints it so you can go read what happened.

Full reference: [Sourcegraph API reference](https://sourcegraph.com/api-reference).

## Piping results

All status output goes to stderr and results go to stdout, so `--out -` gives you clean JSONL to pipe:

```sh
node search_job.ts --out - | jq -r .path | sort -u
```

Color and animation turn themselves off when stderr is not a TTY. `NO_COLOR` and `FORCE_COLOR` are both honored.

## Troubleshooting

**A 401 or 403 response.** The token is missing a scope. Creating a job needs `externalapi:write` and reading one needs `externalapi:read`. This script needs both, so a token with only one will fail partway through.

**A `SyntaxError` on startup.** Your Node version is too old to strip TypeScript types without a flag. Check `node --version` against the versions in [prerequisites](#prerequisites).

**Zero result lines.** The query matched nothing. It does not mean the job failed. A job that failed reports a non-completed state and a `logsUrl`.
