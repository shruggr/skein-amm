#!/usr/bin/env bash
# The 1Color pages (deploy a token; root's registered token topics),
# copied into the config app's www/color/ (apps/config/www/color/, 0.17.0)
# from the skein-1color v0.12.1 tag, as built there.
# They are outside that package's Zig `paths`, so they come from the tag's
# tarball by URL and sha256 (the same tag build.zig.zon names by URL and
# hash). Nothing is built here.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

TAG=v0.12.1
URL="https://github.com/shruggr/skein-1color/archive/refs/tags/${TAG}.tar.gz"
SHA256=7d55df856a4c960d6f9d1d48658d39157f6da8c837c3805037569f85f8be3674

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -sSfL -o "$tmp/c.tar.gz" "$URL"
echo "${SHA256}  $tmp/c.tar.gz" | sha256sum -c --quiet -
tar -xzf "$tmp/c.tar.gz" -C "$tmp" "skein-1color-${TAG#v}/www"
rm -rf apps/config/www/color
mkdir -p apps/config/www/color
cp -R "$tmp/skein-1color-${TAG#v}/www/." apps/config/www/color/
echo "apps/config/www/color/ <- skein-1color ${TAG} www/"
