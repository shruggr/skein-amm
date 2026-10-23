#!/usr/bin/env bash
# The 1Color pages (deploy a token; root's registered token topics),
# copied into www/color/ from the skein-1color v0.11.1 tag, as built there.
# They are outside that package's Zig `paths`, so they come from the tag's
# tarball by URL and sha256 (the same tag build.zig.zon names by URL and
# hash). Nothing is built here.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

TAG=v0.11.2
URL="https://github.com/shruggr/skein-1color/archive/refs/tags/${TAG}.tar.gz"
SHA256=5be817d2375d28882635734a5f77f2f6dacd8fb2a5317e447cd6126deea040a1

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -sSfL -o "$tmp/c.tar.gz" "$URL"
echo "${SHA256}  $tmp/c.tar.gz" | sha256sum -c --quiet -
tar -xzf "$tmp/c.tar.gz" -C "$tmp" "skein-1color-${TAG#v}/www"
rm -rf www/color
mkdir -p www/color
cp -R "$tmp/skein-1color-${TAG#v}/www/." www/color/
echo "www/color/ <- skein-1color ${TAG} www/"
