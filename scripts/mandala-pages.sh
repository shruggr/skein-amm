#!/usr/bin/env bash
# The Mandala pages (deploy a token; the owner's registered token topics),
# copied into www/mandala/ from the skein-mandala v0.7.1 tag, as built there.
# They are outside that package's Zig `paths`, so they come from the tag's
# tarball by URL and sha256 (the same tag build.zig.zon names by URL and
# hash). Nothing is built here.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

TAG=v0.7.1
URL="https://github.com/shruggr/skein-mandala/archive/refs/tags/${TAG}.tar.gz"
SHA256=8c2da29fa3e109e511d81d0a466706c6f60d9be26d5fd1fe15a9131b517601e9

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -sSfL -o "$tmp/m.tar.gz" "$URL"
echo "${SHA256}  $tmp/m.tar.gz" | sha256sum -c --quiet -
tar -xzf "$tmp/m.tar.gz" -C "$tmp" "skein-mandala-${TAG#v}/www"
rm -rf www/mandala
mkdir -p www/mandala
cp -R "$tmp/skein-mandala-${TAG#v}/www/." www/mandala/
echo "www/mandala/ <- skein-mandala ${TAG} www/"
