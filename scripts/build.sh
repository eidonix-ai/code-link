#!/bin/sh
# Builds code-link into bin/:
#   macOS:   bin/code-link.app (arm64, signed),
#            bin/code-link-<version>-macos-arm64.dmg (the download: the app and
#            an Applications shortcut to drag it onto) and
#            bin/code-link-<version>-macos-arm64.zip (the update file: a ditto
#            zip of the app, which the app unpacks itself)
#   Windows: bin/code-link.exe (amd64, portable) and the same file as
#            bin/code-link-<version>-windows-amd64.exe (the download and the
#            update file)
#   bin/VERSION, which scripts/publish.sh reads.
#
#   scripts/build.sh [all|mac|windows|frontend|icons]     (default all)
#
# Environment: VERSION (0.2.0), BUNDLE_ID (com.eidonix.code-link-go),
# SIGN_IDENTITY ("Apple Development", the team 342KG4SM2D certificate; "-"
# for ad-hoc), WAILS3 (wails3 on PATH, else ~/go/bin/wails3).
# The macOS app must be built on a Mac; the Windows exe cross-compiles
# anywhere (no cgo).
set -eu
cd "$(dirname "$0")/.."

VERSION=${VERSION:-0.2.0}
BUNDLE_ID=${BUNDLE_ID:-com.eidonix.code-link-go}
SIGN_IDENTITY=${SIGN_IDENTITY:-Apple Development}
WAILS3=${WAILS3:-$(command -v wails3 || echo "$HOME/go/bin/wails3")}
export GOWORK=off
target=${1:-all}
# The version the app reports and compares against the update feed.
LDFLAGS="-w -s -X main.version=$VERSION"

checkkey() {
	if [ -z "$(tr -d '[:space:]' <internal/selfupdate/update-public-key.txt)" ]; then
		echo "  note: internal/selfupdate/update-public-key.txt is empty, so this build won't update itself"
		echo "        (run: go run ./cmd/updatekeys keygen, then commit the public key)"
	fi
}

stamp() {
	mkdir -p bin
	printf '%s\n' "$VERSION" >bin/VERSION
}

frontend() {
	echo "› frontend"
	[ -d frontend/node_modules ] || (cd frontend && npm ci)
	"$WAILS3" generate bindings -ts -i -clean >/dev/null
	(cd frontend && npm run build >/dev/null)
	touch frontend/dist/.keep
}

icons() {
	echo "› icons"
	go run ./scripts/icons -o build/appicon.png
	"$WAILS3" generate icons -input build/appicon.png -macfilename build/darwin/icons.icns -windowsfilename build/windows/icon.ico >/dev/null
}

mac() {
	echo "› macOS arm64"
	mkdir -p bin
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 MACOSX_DEPLOYMENT_TARGET=13.0 \
		CGO_CFLAGS=-mmacosx-version-min=13.0 CGO_LDFLAGS=-mmacosx-version-min=13.0 \
		go build -tags production -trimpath -buildvcs=false -ldflags "$LDFLAGS" -o bin/code-link-darwin-arm64 .
	app=bin/code-link.app
	rm -rf "$app"
	mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
	mv bin/code-link-darwin-arm64 "$app/Contents/MacOS/code-link"
	cp build/darwin/icons.icns "$app/Contents/Resources/"
	sed -e "s/@VERSION@/$VERSION/g" -e "s/@BUNDLE_ID@/$BUNDLE_ID/g" build/darwin/Info.plist >"$app/Contents/Info.plist"
	xattr -cr "$app"
	identity=$SIGN_IDENTITY
	if [ "$identity" != "-" ] && ! security find-identity -v -p codesigning | grep -q "$identity"; then
		echo "  no \"$identity\" signing identity; signing ad hoc"
		identity=-
	fi
	codesign --force --sign "$identity" "$app"
	codesign --verify --strict "$app"
	rm -f "bin/code-link-$VERSION-macos-arm64.zip"
	ditto -c -k --sequesterRsrc --keepParent "$app" "bin/code-link-$VERSION-macos-arm64.zip"
	dmg "$app" "$identity"
	echo "  $app"
	echo "  bin/code-link-$VERSION-macos-arm64.dmg, bin/code-link-$VERSION-macos-arm64.zip"
}

# dmg makes the macOS download: a disk image whose window shows code-link
# and an Applications shortcut, to drag one onto the other.
dmg() {
	out="bin/code-link-$VERSION-macos-arm64.dmg"
	src=$(mktemp -d)
	ditto "$1" "$src/code-link.app"
	ln -s /Applications "$src/Applications"
	rm -f "$out"
	hdiutil create -quiet -volname code-link -srcfolder "$src" -fs HFS+ -format UDZO -ov "$out"
	rm -rf "$src"
	if [ "$2" != "-" ]; then
		codesign --force --sign "$2" "$out"
	fi
}

windows() {
	echo "› Windows amd64"
	mkdir -p bin
	sed "s/@VERSION@/$VERSION/g" build/windows/info.json >build/windows/info.gen.json
	"$WAILS3" generate syso -arch amd64 -icon build/windows/icon.ico -manifest build/windows/code-link.exe.manifest \
		-info build/windows/info.gen.json -out code-link_windows_amd64.syso >/dev/null
	rm -f build/windows/info.gen.json
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
		go build -tags production -trimpath -buildvcs=false -ldflags "$LDFLAGS -H windowsgui" -o bin/code-link.exe . ||
		{ rm -f code-link_windows_amd64.syso; exit 1; }
	rm -f code-link_windows_amd64.syso
	cp bin/code-link.exe "bin/code-link-$VERSION-windows-amd64.exe"
	echo "  bin/code-link.exe, bin/code-link-$VERSION-windows-amd64.exe"
}

case $target in
all) checkkey; stamp; frontend; mac; windows ;;
mac) checkkey; stamp; frontend; mac ;;
windows) checkkey; stamp; frontend; windows ;;
frontend) frontend ;;
icons) icons ;;
*) echo "usage: scripts/build.sh [all|mac|windows|frontend|icons]" >&2; exit 2 ;;
esac
