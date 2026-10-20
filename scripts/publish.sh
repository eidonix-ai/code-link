#!/bin/sh
# Publishes codelink.eidonix.com: the site (site/) and the current builds
# (bin/*.zip from scripts/build.sh) to the OVH host's /srv/codelink, which
# the edge Caddy serves. /download/macos and /download/windows always point
# at the newest build; versioned copies stay under /files/.
#
#   scripts/build.sh && scripts/publish.sh
set -eu
cd "$(dirname "$0")/.."
host=${PUBLISH_HOST:-eidonix-vps}
version=${VERSION:-0.1.0}
mac="bin/code-link-$version-macos-arm64.zip"
win="bin/code-link-$version-windows-amd64.zip"
for f in "$mac" "$win"; do [ -f "$f" ] || { echo "missing $f — run scripts/build.sh first" >&2; exit 1; }; done

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/files"
cp site/*.html site/*.css "$stage/"
cp build/appicon.png "$stage/icon.png"
sed -i.bak "s/<span id=\"version\">VERSION<\/span>/<span id=\"version\">$version<\/span>/" "$stage/index.html" && rm "$stage/index.html.bak"
cp "$mac" "$win" "$stage/files/"
cp "$mac" "$stage/files/code-link-macos-arm64.zip"
cp "$win" "$stage/files/code-link-windows-amd64.zip"

ssh "$host" 'sudo mkdir -p /srv/codelink/files && sudo chown -R "$(id -un)" /srv/codelink'
# No --delete on files/: older versions stay downloadable.
rsync -rt --chmod=Da+rx,Fa+r "$stage/files/" "$host:/srv/codelink/files/"
rsync -rt --chmod=Da+rx,Fa+r --exclude files "$stage/" "$host:/srv/codelink/"
echo "published code-link $version to https://codelink.eidonix.com"
