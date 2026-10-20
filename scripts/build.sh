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
# SIGN_IDENTITY ("Developer ID Application" when that certificate is in the
# keychain, else "Apple Development"; "-" for ad-hoc), NOTARY_PROFILE
# (code-link: the notarytool keychain profile; with a Developer ID the app
# and the .dmg are notarized and stapled, so they open on any Mac without a
# warning), WAILS3 (wails3 on PATH, else ~/go/bin/wails3).
# The macOS app must be built on a Mac; the Windows exe cross-compiles
# anywhere (no cgo).
set -eu
cd "$(dirname "$0")/.."

VERSION=${VERSION:-0.2.0}
BUNDLE_ID=${BUNDLE_ID:-com.eidonix.code-link-go}
if [ -z "${SIGN_IDENTITY:-}" ]; then
	if security find-identity -v -p codesigning 2>/dev/null | grep -q "Developer ID Application"; then
		SIGN_IDENTITY="Developer ID Application"
	else
		SIGN_IDENTITY="Apple Development"
	fi
fi
NOTARY_PROFILE=${NOTARY_PROFILE:-code-link}
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
	notarize=
	case "$identity" in
	"Developer ID Application"*)
		# Notarization needs the hardened runtime and a secure timestamp.
		codesign --force --options runtime --timestamp --sign "$identity" "$app"
		if xcrun notarytool history --keychain-profile "$NOTARY_PROFILE" >/dev/null 2>&1; then
			notarize=1
		else
			echo "  no notarytool profile \"$NOTARY_PROFILE\": signed with Developer ID but not notarized"
		fi
		;;
	*)
		codesign --force --sign "$identity" "$app"
		;;
	esac
	codesign --verify --strict "$app"
	if [ -n "$notarize" ]; then
		echo "  notarizing the app (Apple usually answers within a few minutes)"
		sub=$(mktemp -d)
		ditto -c -k --keepParent "$app" "$sub/code-link.zip"
		xcrun notarytool submit "$sub/code-link.zip" --keychain-profile "$NOTARY_PROFILE" --wait >"$sub/out.txt" 2>&1 || true
		rm -rf "$sub/code-link.zip"
		if ! grep -q "status: Accepted" "$sub/out.txt"; then
			cat "$sub/out.txt" >&2
			echo "notarization of the app failed (xcrun notarytool log <id> --keychain-profile $NOTARY_PROFILE says why)" >&2
			exit 1
		fi
		rm -rf "$sub"
		xcrun stapler staple -q "$app"
	fi
	rm -f "bin/code-link-$VERSION-macos-arm64.zip"
	ditto -c -k --sequesterRsrc --keepParent "$app" "bin/code-link-$VERSION-macos-arm64.zip"
	dmg "$app" "$identity" "$notarize"
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
		codesign --force --timestamp --sign "$2" "$out"
	fi
	if [ -n "${3:-}" ]; then
		echo "  notarizing the disk image"
		log=$(mktemp)
		xcrun notarytool submit "$out" --keychain-profile "$NOTARY_PROFILE" --wait >"$log" 2>&1 || true
		if ! grep -q "status: Accepted" "$log"; then
			cat "$log" >&2
			echo "notarization of the disk image failed" >&2
			exit 1
		fi
		rm -f "$log"
		xcrun stapler staple -q "$out"
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
