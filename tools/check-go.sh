#!/bin/sh
set -eu
expected=$(awk '$1 == "go" && $2 == "=" {gsub(/"/, "", $3); print $3}' mise.toml)
locked=$(awk '/^\[\[tools.go\]\]$/ {found=1; next} found && /^version = / {gsub(/"/, "", $3); print $3; exit}' mise.lock)
module=$(awk '$1 == "go" {print $2}' go.mod)
if [ -z "$expected" ] || [ "$expected" != "$locked" ] || [ "$expected" != "$module" ] || [ "$(go env GOVERSION)" != "go$expected" ]; then
  echo 'Go compiler must match mise.toml, mise.lock and go.mod exactly' >&2
  exit 1
fi
