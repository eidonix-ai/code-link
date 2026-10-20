// Package gui is code-link's Wails shell: the tray (menu bar) item, the
// Settings window and the dialogs, over internal/app.
package gui

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"codelink/internal/app"
	"codelink/internal/gui/trayicon"
	"codelink/internal/launch"
)

// Event names the Settings page listens to.
const (
	EventStatus       = "status"
	EventActivity     = "activity"
	EventSettingsOpen = "settings:open"
)

func init() {
	application.RegisterEvent[app.Status](EventStatus)
	application.RegisterEvent[app.Entry](EventActivity)
	application.RegisterEvent[string](EventSettingsOpen)
}

// GUI is the running shell.
type GUI struct {
	app      *app.App
	wails    *application.App
	tray     *application.SystemTray
	menu     *application.Menu
	settings *application.WebviewWindow

	mu       sync.Mutex
	menuSig  string
	iconKind app.StatusKind
}

// UniqueID keeps one code-link running per user.
const UniqueID = "com.eidonix.code-link"

// Run shows the tray and runs until Quit. assets is the built frontend.
func Run(a *app.App, assets fs.FS) error {
	g := &GUI{app: a}
	g.wails = application.New(application.Options{
		Name:        "code-link",
		Description: "Claude Code and Codex on your own models",
		Services:    []application.Service{application.NewService(&Settings{app: a, gui: g})},
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(assets)},
		Mac: application.MacOptions{
			// A menu bar app: no Dock icon (LSUIElement in Info.plist too).
			ActivationPolicy: application.ActivationPolicyAccessory,
		},
		Windows: application.WindowsOptions{DisableQuitOnLastWindowClosed: true},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: UniqueID,
			// A second launch opens Settings of the running one.
			OnSecondInstanceLaunch: func(application.SecondInstanceData) { g.OpenSettings() },
		},
	})
	if runtime.GOOS == "darwin" {
		// Cmd-C/V/X/A in the text fields, Cmd-W, Cmd-Q.
		m := application.NewMenu()
		m.AddRole(application.AppMenu)
		m.AddRole(application.EditMenu)
		m.AddRole(application.WindowMenu)
		g.wails.Menu.Set(m)
	}

	g.settings = g.wails.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:      "settings",
		Title:     "code-link Settings",
		Width:     640,
		Height:    780,
		MinWidth:  520,
		MinHeight: 420,
		Hidden:    true,
		URL:       "/",
		Mac:       application.MacWindow{Backdrop: application.MacBackdropNormal},
	})
	g.settings.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		g.settings.Hide()
		e.Cancel()
	})

	g.menu = application.NewMenu()
	g.tray = g.wails.SystemTray.New()
	g.tray.SetMenu(g.menu)
	g.tray.OnClick(func() { g.tray.OpenMenu() })
	g.updateTray(true)

	// Opening code-link again while it runs (Finder, Spotlight) shows Settings.
	g.wails.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) { g.OpenSettings() })

	a.OnChange = func() {
		g.updateTray(false)
		g.wails.Event.Emit(EventStatus, a.Status())
	}
	a.Activity.OnAppend = func(e app.Entry) { g.wails.Event.Emit(EventActivity, e) }

	g.wails.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		if err := a.SyncLoginItem(); err != nil {
			a.Activity.Record(hookError("Open at login: " + err.Error()))
		}
		go func() {
			a.Start()
			if !a.Configured() {
				time.Sleep(400 * time.Millisecond)
				g.OpenSettings()
			}
		}()
		go a.Watch(g.wails.Context(), 20*time.Second)
	})
	g.wails.OnShutdown(a.Stop)
	return g.wails.Run()
}

// OpenSettings shows the Settings window in front.
func (g *GUI) OpenSettings() {
	g.wails.Event.Emit(EventSettingsOpen, "")
	g.settings.Show()
	g.settings.Focus()
}

// title is a menu label in the platform's case: Title Case on macOS,
// sentence case on Windows.
func title(mac, win string) string {
	if runtime.GOOS == "windows" {
		return win
	}
	return mac
}

// updateTray rebuilds the menu when what it shows changed, and sets the
// icon for the status.
func (g *GUI) updateTray(force bool) {
	st := g.app.Status()
	cfg := g.app.Config()
	recents := g.app.RecentFolders()
	sig := st.Line + "|" + strings.Join(recents, "|")
	if cfg.Configured() {
		sig += "|configured"
	}
	g.mu.Lock()
	rebuild := force || sig != g.menuSig
	g.menuSig = sig
	setIcon := force || st.Kind != g.iconKind
	g.iconKind = st.Kind
	g.mu.Unlock()

	if setIcon {
		ic := trayicon.For(st.Kind)
		if runtime.GOOS == "darwin" {
			g.tray.SetTemplateIcon(ic.Template)
		} else {
			g.tray.SetIcon(ic.Light)
			g.tray.SetDarkModeIcon(ic.Dark)
		}
	}
	g.tray.SetTooltip("code-link — " + st.Line)
	if !rebuild {
		return
	}
	m := g.menu
	m.Clear()
	m.Add(st.Line).SetEnabled(false)
	m.AddSeparator()
	home, _ := os.UserHomeDir()
	for _, t := range launch.Tools {
		label := "Launch " + t.DisplayName()
		if len(recents) == 0 || !cfg.Configured() {
			m.Add(label + "…").SetEnabled(cfg.Configured()).OnClick(func(*application.Context) { g.chooseAndLaunch(t) })
			continue
		}
		sub := m.AddSubmenu(label)
		sub.Add(title("Choose Folder…", "Choose folder…")).OnClick(func(*application.Context) { g.chooseAndLaunch(t) })
		sub.AddSeparator()
		for _, p := range recents {
			sub.Add(app.FolderTitle(p, home)).OnClick(func(*application.Context) { go g.launch(t, p) })
		}
		sub.AddSeparator()
		sub.Add(title("Clear Recent Folders", "Clear recent folders")).OnClick(func(*application.Context) { g.app.ClearRecentFolders() })
	}
	m.AddSeparator()
	for _, t := range launch.Tools {
		m.Add(title("Copy "+t.DisplayName()+" Command", "Copy "+t.DisplayName()+" command")).SetEnabled(cfg.Configured()).
			OnClick(func(*application.Context) { g.copyCommand(t) })
	}
	m.AddSeparator()
	m.Add(title("Settings…", "Settings…")).OnClick(func(*application.Context) { g.OpenSettings() })
	m.Add(title("Quit code-link", "Quit")).OnClick(func(*application.Context) { g.wails.Quit() })
	m.Update()
}

func (g *GUI) copyCommand(t launch.Tool) {
	cmd, err := g.app.Command(t)
	if err != nil {
		g.alert("Can't copy the "+t.DisplayName()+" command", err.Error())
		return
	}
	g.wails.Clipboard.SetText(cmd)
}

// chooseAndLaunch asks for a folder (starting at the last one) and launches.
func (g *GUI) chooseAndLaunch(t launch.Tool) {
	go func() {
		if !g.app.Configured() {
			g.launch(t, "")
			return
		}
		activate()
		d := g.wails.Dialog.OpenFile().
			CanChooseFiles(false).
			CanChooseDirectories(true).
			CanCreateDirectories(true).
			SetTitle("Choose the folder to start " + t.DisplayName() + " in").
			SetMessage("Choose the folder to start " + t.DisplayName() + " in").
			SetButtonText("Launch")
		if r := g.app.RecentFolders(); len(r) > 0 {
			d.SetDirectory(r[0])
		}
		folder, err := d.PromptForSingleSelection()
		if err != nil || folder == "" {
			return
		}
		g.launch(t, folder)
	}()
}

// launch runs a tool in a folder and explains anything that stops it.
func (g *GUI) launch(t launch.Tool, folder string) {
	err := g.app.Launch(t, folder)
	var missing *app.MissingCLIError
	switch {
	case err == nil, errors.Is(err, app.ErrBusy):
	case errors.Is(err, app.ErrNotConfigured):
		g.alert("Nothing to launch yet", "Add a provider and pick a main model in Settings first.")
		g.OpenSettings()
	case errors.As(err, &missing):
		activate()
		q := g.wails.Dialog.Question().SetTitle(t.DisplayName() + " isn't installed").SetMessage(err.Error())
		open := q.AddButton("Open install page")
		open.OnClick(func() { g.wails.Browser.OpenURL(t.InstallURL()) })
		cancel := q.AddButton("Cancel")
		q.SetDefaultButton(open)
		q.SetCancelButton(cancel)
		q.Show()
	default:
		g.alert("Couldn't launch "+t.DisplayName(), upperFirst(err.Error()))
	}
}

func (g *GUI) alert(title, detail string) {
	activate()
	g.wails.Dialog.Info().SetTitle(title).SetMessage(detail).Show()
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
