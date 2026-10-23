#!/usr/bin/env bash
# The 1Color pages (deploy a token; root's registered token topics),
# copied into www/color/ from the skein-1color v0.11.1 tag, as built there.
# They are outside that package's Zig `paths`, so they come from the tag's
# tarball by URL and sha256 (the same tag build.zig.zon names by URL and
# hash). Nothing is built here.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

TAG=v0.11.1
URL="https://github.com/shruggr/skein-1color/archive/refs/tags/${TAG}.tar.gz"
SHA256=0b49e277021f68482887fa962437184e31252f7c7fe0adba36b44b01e9614af4

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -sSfL -o "$tmp/c.tar.gz" "$URL"
echo "${SHA256}  $tmp/c.tar.gz" | sha256sum -c --quiet -
tar -xzf "$tmp/c.tar.gz" -C "$tmp" "skein-1color-${TAG#v}/www"
rm -rf www/color
mkdir -p www/color
cp -R "$tmp/skein-1color-${TAG#v}/www/." www/color/
echo "www/color/ <- skein-1color ${TAG} www/"
