#!/usr/bin/env bash
# Run the bot in the foreground. Press Ctrl+C to stop.
set -e

cd "$(dirname "$0")"

# Binaries go under bin/ so the build cannot collide with a directory of the
# same name, such as the clonebot/ package left behind by the Python version.
go build -o bin/clonebot ./cmd/clonebot
exec ./bin/clonebot
