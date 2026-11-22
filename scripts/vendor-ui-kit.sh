#!/usr/bin/env bash
# vendor-ui-kit.sh renders muster's plugin UI kit (docs/ui-kit.md in
# ZacxDev/muster) at one pinned commit into e2e/fixtures/ui-kit/, for the
# e2e harness's fake muster to serve. The plugin's pages link the kit LIVE
# from muster in production; this copy exists only so CI never depends on a
# live muster.
#
#   scripts/vendor-ui-kit.sh <a muster checkout> [<commit>]
#
# It renders the files with muster's OWN code (internal/uikit), not a copy of
# it: kit.css and theme.css for the default preset (brass) and for the harbor
# preset (a second accent, so a test can see the theme flow through), kit.js
# with its sprite, and icons.svg. SHA256SUMS pins every file, and the harness
# refuses to serve a fixture whose bytes differ (scripts/e2e-harness).
set -euo pipefail

src=${1:?usage: vendor-ui-kit.sh <muster checkout> [commit]}
commit=${2:-ff54356f0}
here=$(realpath "$(dirname "$0")/..")
out="$here/e2e/fixtures/ui-kit"
wt=$(mktemp -d)
trap 'git -C "$src" worktree remove --force "$wt" >/dev/null 2>&1 || true; rm -rf "$wt"' EXIT

git -C "$src" fetch -q origin
git -C "$src" worktree add -q --detach "$wt" "$commit"
full=$(git -C "$wt" rev-parse HEAD)
mkdir -p "$wt/cmd/kitdump"
cat >"$wt/cmd/kitdump/main.go" <<'EOF'
package main

import (
	"os"
	"path/filepath"

	"github.com/ZacxDev/muster/internal/theme"
	"github.com/ZacxDev/muster/internal/uikit"
)

func main() {
	out := os.Args[1]
	for _, p := range []string{"brass", "harbor"} {
		t, err := theme.Choice{Preset: p}.Resolve()
		if err != nil {
			panic(err)
		}
		dir := filepath.Join(out, p, "v1")
		must(os.MkdirAll(dir, 0o755))
		must(os.WriteFile(filepath.Join(dir, "kit.css"), uikit.KitCSS(t).Body, 0o644))
		must(os.WriteFile(filepath.Join(dir, "theme.css"), uikit.ThemeCSS(t).Body, 0o644))
		must(os.WriteFile(filepath.Join(dir, "kit.js"), uikit.KitJS().Body, 0o644))
		must(os.WriteFile(filepath.Join(dir, "icons.svg"), uikit.IconsFile().Body, 0o644))
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
EOF
rm -rf "$out"
mkdir -p "$out"
(cd "$wt" && go run ./cmd/kitdump "$out")
printf '%s\n' "$full" >"$out/COMMIT"
# The plugin's built-in fallback, used when the live kit cannot be loaded:
# kit.js byte for byte (it carries the icon sprite, and installs itself only
# when the live one did not), and kit.css in the default theme, both from
# this same render. scripts/e2e-harness TestTheFallbackIsTheVendoredKit
# holds them to the pinned fixtures.
static="$here/internal/server/web/static"
cp "$out/brass/v1/kit.js" "$static/vendor/muster-ui-kit-v1.js"
{
	printf '/* The built-in FALLBACK for muster'"'"'s UI kit, used only when the live kit\n   (<muster>/ui-kit/v1/kit.css) does not load (static/ui.js). Rendered from\n   muster %s, default theme, by scripts/vendor-ui-kit.sh: never edit by hand. */\n' "$full"
	tail -n +2 "$out/brass/v1/kit.css"
} >"$static/kit-fallback.css"
(cd "$out" && find . -type f ! -name SHA256SUMS ! -name COMMIT | LC_ALL=C sort | xargs sha256sum >SHA256SUMS)
echo "vendored the UI kit at $full into $out"
