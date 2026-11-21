#!/usr/bin/env bash
# Re-fetches the vendored LiveKit browser client into
# internal/server/web/static/vendor/: the npm tarball of the pinned version,
# checked against its pinned SHA-256 before anything is unpacked. The pages
# load it from the plugin's own origin (CSP script-src 'self'); no page ever
# loads a script from a CDN.
#
# To move to another version: change VERSION and SHA256 (sha256sum of the
# tarball from registry.npmjs.org), run this, and update the file name in
# internal/server/web/*.html and the pinned hash in the server test
# (TestTheVendoredLiveKitClientIsThePinnedOne).
set -euo pipefail
VERSION=2.25.0
SHA256=82b45694ef402f1ad02483e101b183e728d5b0d82cf3ff33bd1dc841a2c3798f
root=$(cd "$(dirname "$0")/.." && pwd)
dest="$root/internal/server/web/static/vendor"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -sSfL -o "$tmp/lkc.tgz" "https://registry.npmjs.org/livekit-client/-/livekit-client-${VERSION}.tgz"
echo "${SHA256}  $tmp/lkc.tgz" | sha256sum -c -
tar -xzf "$tmp/lkc.tgz" -C "$tmp" package/dist/livekit-client.umd.js package/LICENSE
mkdir -p "$dest"
# The source-map comment would make browsers fetch a .map this repository
# does not ship; drop it.
sed '/^\/\/# sourceMappingURL=/d' "$tmp/package/dist/livekit-client.umd.js" > "$dest/livekit-client-${VERSION}.umd.js"
cp "$tmp/package/LICENSE" "$dest/LICENSE-livekit-client"
sha256sum "$dest/livekit-client-${VERSION}.umd.js"
