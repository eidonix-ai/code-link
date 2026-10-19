#!/bin/sh
# Builds CodeLink.app into build/ (ad-hoc signed). Usage: scripts/build.sh [Debug|Release]
set -eu
cd "$(dirname "$0")/.."
config="${1:-Debug}"
xcodegen generate --quiet
xcodebuild -project CodeLink.xcodeproj -scheme CodeLink -configuration "$config" -derivedDataPath build/DerivedData \
  -destination 'platform=macOS' build -quiet
app="build/DerivedData/Build/Products/$config/CodeLink.app"
echo "$app"
