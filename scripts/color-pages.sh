#!/usr/bin/env bash
# The 1Color pages (deploy a token; root's registered token topics),
# copied into www/color/ from the skein-1color v0.11.0 tag, as built there.
# They are outside that package's Zig `paths`, so they come from the tag's
# tarball by URL and sha256 (the same tag build.zig.zon names by URL and
# hash). Nothing is built here.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

TAG=v0.11.0
URL="https://github.com/shruggr/skein-1color/archive/refs/tags/${TAG}.tar.gz"
SHA256=865113ddf8c7f0537b3b9628305db15d83fb9322c929bfcef5efec9e52715e58

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -sSfL -o "$tmp/c.tar.gz" "$URL"
echo "${SHA256}  $tmp/c.tar.gz" | sha256sum -c --quiet -
tar -xzf "$tmp/c.tar.gz" -C "$tmp" "skein-1color-${TAG#v}/www"
rm -rf www/color
mkdir -p www/color
cp -R "$tmp/skein-1color-${TAG#v}/www/." www/color/
echo "www/color/ <- skein-1color ${TAG} www/"
