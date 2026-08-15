#!/usr/bin/env bash
# Run the bot in the foreground. Press Ctrl+C to stop.
set -e

cd "$(dirname "$0")"

go build -o clonebot ./cmd/clonebot
exec ./clonebot
