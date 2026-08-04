#!/bin/sh

curl --header "Accept: text/event-stream" \
     --header "Authorization: token $SRC_ACCESS_TOKEN" \
     --get \
     --url "$SRC_ENDPOINT/.api/search/stream" \
     --data-urlencode "q=$1" \
     --data-urlencode "t=keyword" \
     --data-urlencode "cl=3"
