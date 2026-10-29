#!/usr/bin/env bash
# The 1Color pages (deploy a token; root's registered token topics),
# copied into the config app's www/color/ (apps/config/www/color/, 0.17.0)
# from the skein-1color v0.12.0 tag, as built there.
# They are outside that package's Zig `paths`, so they come from the tag's
# tarball by URL and sha256 (the same tag build.zig.zon names by URL and
# hash). Nothing is built here.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

TAG=v0.12.0
URL="https://github.com/shruggr/skein-1color/archive/refs/tags/${TAG}.tar.gz"
SHA256=8b0bc80c0472014763b5a84e476815a9e2967b5cd3511a17b12bdd5e130c39ec

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -sSfL -o "$tmp/c.tar.gz" "$URL"
echo "${SHA256}  $tmp/c.tar.gz" | sha256sum -c --quiet -
tar -xzf "$tmp/c.tar.gz" -C "$tmp" "skein-1color-${TAG#v}/www"
rm -rf apps/config/www/color
mkdir -p apps/config/www/color
cp -R "$tmp/skein-1color-${TAG#v}/www/." apps/config/www/color/
echo "apps/config/www/color/ <- skein-1color ${TAG} www/"
