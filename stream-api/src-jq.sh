#!/bin/sh

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    printf 'Usage: %s <query> [jq-filter]\n' "$0" >&2
    exit 2
fi

if [ "$#" -eq 2 ]; then
    if ! command -v jq >/dev/null 2>&1; then
        printf 'jq is required when a filter is provided\n' >&2
        exit 127
    fi

    src search --get-curl -stream -json "$1" | jq "$2"
else
    src search --get-curl -stream -json "$1"
fi
