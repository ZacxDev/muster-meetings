#!/usr/bin/env bash
# update-goldens.sh remakes the visual goldens (e2e/goldens/) in the pinned
# Playwright image CI's `visual` job uses, so a golden made here compares
# equal there: the same fonts, the same rasteriser. Needs docker.
#
#   scripts/update-goldens.sh            # remake them
#   scripts/update-goldens.sh --check    # compare only (what CI does)
set -euo pipefail
here=$(realpath "$(dirname "$0")/..")
image=mcr.microsoft.com/playwright:v1.64.1-noble
gover=$(awk '/^go /{print $2; exit}' "$here/go.mod")
update=--update-snapshots=all
[ "${1:-}" = --check ] && update=
docker run --rm --ipc=host --user "$(id -u):$(id -g)" -v "$here:/src" -w /src -e HOME=/tmp -e GOFLAGS=-buildvcs=false "$image" bash -euc "
  curl -sSfL https://go.dev/dl/go${gover}.linux-amd64.tar.gz | tar -xz -C /tmp
  export PATH=/tmp/go/bin:\$PATH GOCACHE=/tmp/gocache GOMODCACHE=/tmp/gomod
  cd e2e && npm ci --no-audit --no-fund >/dev/null
  npx playwright test --project=visual $update
"
