// code-link runs Claude Code and Codex on your own models (docs/DESIGN.md).
//
//	code-link                      the tray app (menu bar on macOS)
//	code-link serve [config.json]  the engine alone, in the foreground
//	code-link version              the version
package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"

	"codelink/internal/app"
	"codelink/internal/config"
	"codelink/internal/engine"
	"codelink/internal/features"
	"codelink/internal/gui"
	"codelink/internal/hooks"
	"codelink/internal/selfupdate"
)

//go:embed all:frontend/dist
var assets embed.FS

// featuresFactory builds the feature hooks (documents, images, web search,
// auto mode).
var featuresFactory hooks.Factory = features.Factory

// version is set by scripts/build.sh (-ldflags "-X main.version=0.2.0"); a
// plain `go build` is a development build that doesn't update itself.
var version = "dev"

func main() {
	// Started by an update: wait for the old copy to exit and remove it.
	args := selfupdate.AfterRelaunch(os.Args[1:])
	os.Args = append([]string{os.Args[0]}, args...)
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			os.Exit(serve(args[1:]))
		case "-v", "-version", "--version", "version":
			attachConsole()
			fmt.Println("code-link", version)
			return
		case "-h", "-help", "--help", "help":
			attachConsole()
			fmt.Println("usage:\n  code-link                      run the tray app\n  code-link serve [config.json]  run the engine alone (Ctrl-C stops it)\n  code-link version              print the version")
			return
		}
	}
	path, err := config.Path()
	if err != nil {
		fatal(err)
	}
	dist, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		fatal(err)
	}
	if err := gui.Run(app.New(path, featuresFactory), dist, version); err != nil {
		fatal(err)
	}
}

// serve runs the engine headless with a settings file (default: the app's)
// until interrupted, printing the port and the activity. CODE_LINK_TOKEN
// overrides the access token.
func serve(args []string) int {
	attachConsole()
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "usage: code-link serve [config.json]")
		return 2
	}
	path := ""
	if len(args) == 1 {
		path = args[0]
	} else {
		p, err := config.Path()
		if err != nil {
			fmt.Fprintln(os.Stderr, "code-link:", err)
			return 1
		}
		path = p
	}
	cfg, warning := app.LoadSettings(path)
	if warning != "" {
		fmt.Fprintln(os.Stderr, "code-link:", warning)
	}
	if t := os.Getenv("CODE_LINK_TOKEN"); t != "" {
		cfg.AccessToken = t
	}
	e := engine.New(cfg, printLog{}, featuresFactory)
	port, err := app.StartOnFreePort(e, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "code-link:", err)
		return 1
	}
	if port != cfg.Port {
		fmt.Printf("code-link: port %d was busy; using %d\n", cfg.Port, port)
	}
	fmt.Printf("code-link engine on http://127.0.0.1:%d (settings %s)\n", port, path)
	if !cfg.Configured() {
		fmt.Println("code-link: no main model is configured; requests will fail until one is")
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	e.Stop()
	return 0
}

type printLog struct{}

func (printLog) Record(ev hooks.Event) {
	kind, text, _ := app.Describe(ev)
	fmt.Printf("[%s] %s\n", kind, text)
}

func fatal(err error) {
	attachConsole()
	fmt.Fprintln(os.Stderr, "code-link:", err)
	os.Exit(1)
}
