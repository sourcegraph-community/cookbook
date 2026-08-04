#!/bin/sh

output=$(mktemp "${TMPDIR:-/tmp}/sourcegraph-search.XXXXXX") || exit 1
trap 'rm -f "$output"' EXIT HUP INT TERM

curl --silent --show-error --fail-with-body \
     --header "Accept: text/event-stream" \
     --header "Authorization: token $SRC_ACCESS_TOKEN" \
     --get \
     --url "$SRC_ENDPOINT/.api/search/stream" \
     --data-urlencode "q=$1" \
     --data-urlencode "t=keyword" \
     --data-urlencode "cl=3" \
     --output "$output" || exit 1

if tr -d '\r' < "$output" | grep -q '^event: done$'; then
    cat "$output"
else
    printf '%s\n' 'Search stream ended without a done event.' >&2
    exit 1
fi
