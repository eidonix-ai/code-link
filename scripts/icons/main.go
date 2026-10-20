// Command icons writes build/appicon.png (the source of the .icns and .ico,
// see scripts/build.sh) and, with -preview DIR, the tray icons for a look.
//
//	go run ./scripts/icons
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"codelink/internal/gui/trayicon"
	"codelink/internal/icon"
)

func main() {
	out := flag.String("o", "build/appicon.png", "app icon output")
	preview := flag.String("preview", "", "also write the tray icons into this folder")
	flag.Parse()
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, icon.App(1024), 0o644); err != nil {
		log.Fatal(err)
	}
	if *preview != "" {
		os.MkdirAll(*preview, 0o755)
		for name, b := range trayicon.All() {
			if err := os.WriteFile(filepath.Join(*preview, name+".png"), b, 0o644); err != nil {
				log.Fatal(err)
			}
		}
		os.WriteFile(filepath.Join(*preview, "app-128.png"), icon.App(128), 0o644)
	}
}
