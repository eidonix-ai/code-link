// code-link runs Claude Code and Codex on your own models (docs/DESIGN.md).
//
//	code-link                      the tray app (menu bar on macOS)
//	code-link serve [config.json]  the engine alone, in the foreground
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
)

//go:embed all:frontend/dist
var assets embed.FS

// featuresFactory builds the feature hooks (documents, images, web search,
// auto mode). Integration: = features.Factory.
var featuresFactory hooks.Factory = features.Factory

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			os.Exit(serve(args[1:]))
		case "-h", "-help", "--help", "help":
			attachConsole()
			fmt.Println("usage:\n  code-link                      run the tray app\n  code-link serve [config.json]  run the engine alone (Ctrl-C stops it)")
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
	if err := gui.Run(app.New(path, featuresFactory), dist); err != nil {
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
