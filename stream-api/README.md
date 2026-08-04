# Streaming Search API

These shell scripts show ways to call Sourcegraph's [streaming search API](https://sourcegraph.com/docs/api/stream-api). The API returns search results and metadata as a stream of server-sent events (SSE).

## Prerequisites

- `curl`
- [`src`](https://github.com/sourcegraph/src-cli) for `src-jq.sh`; optionally `jq` to filter its output
- A Sourcegraph instance and access token

Set the instance URL and token:

```sh
export SRC_ENDPOINT="https://sourcegraph.com"
export SRC_ACCESS_TOKEN="sgp_..."
```

## Examples

### Stream results directly

`simple.sh` writes each event to stdout as Sourcegraph sends it:

```sh
sh simple.sh 'repo:sourcegraph/sourcegraph TODO count:10'
```

Use this version when you want results immediately or plan to pipe the event stream into another program.

### Filter JSON results

`src-jq.sh` uses `src` to stream JSON search results. Pass an optional second argument to filter them with `jq`:

```sh
sh src-jq.sh 'repo:sourcegraph/sourcegraph TODO count:10'
sh src-jq.sh 'repo:sourcegraph/sourcegraph TODO count:10' '.type'
```

The first argument is the Sourcegraph query. The second is an optional `jq` filter and requires `jq` to be installed.

### Verify the stream completed

`polling.sh` saves the response to a temporary file and prints it only when the stream contains the final `done` event:

```sh
sh polling.sh 'repo:sourcegraph/sourcegraph TODO count:10'
```

If the request fails or ends before a `done` event arrives, the script exits with a non-zero status. Despite its name, it makes one streaming request rather than repeatedly polling the API.

## Request details

Both scripts call `/.api/search/stream`, use keyword search, and pass the first command-line argument as the Sourcegraph query. Responses contain events such as `matches`, `progress`, `filters`, `alert`, and the final `done` event:

```text
event: matches
data: [{...}]

event: done
data: {}
```

The scripts also send `cl=3`, which requests three context lines when chunk matches are enabled. See the [API documentation](https://sourcegraph.com/docs/api/stream-api) for all request parameters and event types.
