#!/bin/sh
# Run by `nc -lk -e` for each connection: read "ping N", wait 1 s, answer.
read -r _ n || exit 0
sleep 1
echo "pong $n v1"
