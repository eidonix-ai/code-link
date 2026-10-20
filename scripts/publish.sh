#!/bin/sh
# Publishes codelink.eidonix.com to the OVH host's /srv/codelink, which the
# edge Caddy serves: the site (site/), the downloads and the signed update
# feed, from the builds scripts/build.sh made.
#
#   files/code-link-macos-arm64.dmg            the newest macOS download
#   files/code-link-windows-amd64.exe          the newest Windows download
#   files/code-link-<version>-macos-arm64.dmg  versioned copies (kept)
#   files/code-link-<version>-windows-amd64.exe   (also the Windows update file)
#   files/code-link-<version>-macos-arm64.zip  the macOS update file
#   update/manifest.json, manifest.json.sig    the update feed, signed
#
# /download/macos and /download/windows (in the edge Caddyfile) point at the
# two stable names. The files go up first and the manifest last, so the feed
# never names a file that isn't there yet.
#
#   scripts/build.sh && scripts/publish.sh
#
# Environment: VERSION (default: bin/VERSION from the build), NOTES (what
# changed; default: the last commit's subject), UPDATE_SIGNING_KEY
# (~/.code-link/update-signing.key), PUBLISH_HOST (eidonix-vps).
set -eu
cd "$(dirname "$0")/.."
export GOWORK=off
host=${PUBLISH_HOST:-eidonix-vps}
version=${VERSION:-$(cat bin/VERSION 2>/dev/null || true)}
notes=${NOTES:-$(git log -1 --format=%s)}
key=${UPDATE_SIGNING_KEY:-$HOME/.code-link/update-signing.key}
base=https://codelink.eidonix.com/files/
die() { echo "publish: $*" >&2; exit 1; }

[ -n "$version" ] || die "no version — run scripts/build.sh first (or set VERSION)"
dmg="bin/code-link-$version-macos-arm64.dmg"
zip="bin/code-link-$version-macos-arm64.zip"
exe="bin/code-link-$version-windows-amd64.exe"
for f in "$dmg" "$zip" "$exe"; do [ -f "$f" ] || die "missing $f — run scripts/build.sh first"; done

# The feed must be signed, and the builds must trust the key that signs it.
[ -f "$key" ] || die "no update signing key at $key — refusing to publish an unsigned feed (it is made once with: go run ./cmd/updatekeys keygen)"
pub=$(tr -d '[:space:]' <internal/selfupdate/update-public-key.txt)
[ -n "$pub" ] || die "internal/selfupdate/update-public-key.txt is empty — run go run ./cmd/updatekeys keygen and commit it, then rebuild"
grep -qaF "$pub" "$exe" || die "$exe was built without the update key — rebuild"
unzip -p "$zip" code-link.app/Contents/MacOS/code-link | grep -qaF "$pub" || die "$zip was built without the update key — rebuild"

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/site" "$stage/files" "$stage/update"
cp site/*.html site/*.css "$stage/site/"
cp build/appicon.png "$stage/site/icon.png"
sed -i.bak "s/<span id=\"version\">VERSION<\/span>/<span id=\"version\">$version<\/span>/" "$stage/site/index.html" && rm "$stage/site/index.html.bak"
cp "$dmg" "$zip" "$exe" "$stage/files/"
cp "$dmg" "$stage/files/code-link-macos-arm64.dmg"
cp "$exe" "$stage/files/code-link-windows-amd64.exe"

go run ./cmd/updatekeys manifest -version "$version" -notes "$notes" -base "$base" -out "$stage/update/manifest.json" \
	"darwin-arm64=$zip" "windows-amd64=$exe"
go run ./cmd/updatekeys sign -key "$key" -in "$stage/update/manifest.json"
go run ./cmd/updatekeys verify -in "$stage/update/manifest.json"

ssh "$host" 'sudo mkdir -p /srv/codelink/files /srv/codelink/update && sudo chown -R "$(id -un)" /srv/codelink'
# No --delete on files/: older versions stay downloadable.
rsync -rt --chmod=Da+rx,Fa+r "$stage/files/" "$host:/srv/codelink/files/"
rsync -rt --chmod=Da+rx,Fa+r "$stage/site/" "$host:/srv/codelink/"
rsync -rt --chmod=Da+rx,Fa+r "$stage/update/" "$host:/srv/codelink/update/"
echo "published code-link $version to https://codelink.eidonix.com"
