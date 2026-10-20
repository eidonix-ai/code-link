#!/bin/sh
# Builds a Release CodeLink.app and installs it in /Applications (a stable
# place for Open at login), replacing an older copy. Quits a running copy first.
set -eu
cd "$(dirname "$0")/.."
app=$(./scripts/build.sh Release | tail -1)
osascript -e 'tell application id "com.eidonix.code-link" to quit' >/dev/null 2>&1 || true
sleep 1
rm -rf /Applications/CodeLink.app
ditto "$app" /Applications/CodeLink.app
open /Applications/CodeLink.app
echo "Installed /Applications/CodeLink.app"
