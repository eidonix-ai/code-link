// Package gui is code-link's Wails shell: the tray (menu bar) item, the
// Settings, launch and install windows, and the dialogs, over internal/app.
package gui

import (
	"context"
	"io/fs"
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

// Event names the pages listen to.
const (
	EventStatus       = "status"
	EventActivity     = "activity"
	EventSettingsOpen = "settings:open"
	// EventLaunchOpen (the tool) asks a launch window to reload its view.
	EventLaunchOpen = "launch:open"
	// EventJob is an install or update's progress.
	EventJob = "job"
	// EventTools: the tools' installs or the settings changed.
	EventTools = "tools"
)

func init() {
	application.RegisterEvent[app.Status](EventStatus)
	application.RegisterEvent[app.Entry](EventActivity)
	application.RegisterEvent[string](EventSettingsOpen)
	application.RegisterEvent[string](EventLaunchOpen)
	application.RegisterEvent[app.JobView](EventJob)
	application.RegisterEvent[string](EventTools)
}

// GUI is the running shell.
type GUI struct {
	app      *app.App
	wails    *application.App
	tray     *application.SystemTray
	menu     *application.Menu
	settings *application.WebviewWindow
	launch   map[launch.Tool]*application.WebviewWindow
	install  map[launch.Tool]*application.WebviewWindow

	mu       sync.Mutex
	menuSig  string
	iconKind app.StatusKind
}

// UniqueID keeps one code-link running per user.
const UniqueID = "com.eidonix.code-link"

// ToolCheckInterval is how often the Claude Code and Codex installs are
// re-checked for updates.
const ToolCheckInterval = 6 * time.Hour

// Run shows the tray and runs until Quit. assets is the built frontend.
func Run(a *app.App, assets fs.FS) error {
	g := &GUI{app: a, launch: map[launch.Tool]*application.WebviewWindow{}, install: map[launch.Tool]*application.WebviewWindow{}}
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

	g.settings = g.window("settings", "code-link Settings", "/", 640, 780)
	for _, t := range launch.Tools {
		h := 480 // Claude Code's window has More options
		if t == launch.Codex {
			h = 340
		}
		g.launch[t] = g.window("launch-"+string(t), "Launch "+t.DisplayName(), "/#/launch/"+string(t), 480, h)
		g.install[t] = g.window("install-"+string(t), t.DisplayName(), "/#/install/"+string(t), 480, 380)
	}

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
		g.wails.Event.Emit(EventTools, "")
	}
	a.OnJob = func(v app.JobView) { g.wails.Event.Emit(EventJob, v) }
	a.Activity.OnAppend = func(e app.Entry) { g.wails.Event.Emit(EventActivity, e) }

	g.wails.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		if err := a.SyncLoginItem(); err != nil {
			a.Activity.Record(hookError("Open at login: " + err.Error()))
		}
		ctx := g.wails.Context()
		go func() {
			a.Start()
			if !a.Configured() {
				time.Sleep(400 * time.Millisecond)
				g.OpenSettings()
			}
			a.SyncAll(ctx)
		}()
		go func() {
			a.RefreshTools(ctx)
			a.WatchTools(ctx, ToolCheckInterval)
		}()
		go a.Watch(ctx, 20*time.Second)
	})
	g.wails.OnShutdown(a.Stop)
	return g.wails.Run()
}

// window is a hidden window that hides again when closed.
func (g *GUI) window(name, title, url string, w, h int) *application.WebviewWindow {
	win := g.wails.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: name, Title: title, URL: url, Width: w, Height: h, MinWidth: 420, MinHeight: 320,
		Hidden: true,
		Mac:    application.MacWindow{Backdrop: application.MacBackdropNormal},
	})
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		win.Hide()
		e.Cancel()
	})
	return win
}

// OpenSettings shows the Settings window in front.
func (g *GUI) OpenSettings() {
	g.wails.Event.Emit(EventSettingsOpen, "")
	g.settings.Show()
	g.settings.Focus()
}

// OpenLaunch shows a tool's launch window and brings the providers'
// models up to date for it.
func (g *GUI) OpenLaunch(t launch.Tool) {
	w := g.launch[t]
	if w == nil {
		return
	}
	g.wails.Event.Emit(EventLaunchOpen, string(t))
	w.Show()
	w.Focus()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		g.app.SyncAll(ctx)
		g.wails.Event.Emit(EventLaunchOpen, string(t))
	}()
}

// OpenInstall starts installing or updating a tool and shows its progress.
func (g *GUI) OpenInstall(t launch.Tool, verb app.MenuAction) app.JobView {
	v := g.app.StartJob(t, verb)
	if w := g.install[t]; w != nil {
		g.wails.Event.Emit(EventJob, v)
		w.Show()
		w.Focus()
	}
	return v
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
	var items []app.MenuItem
	for _, t := range launch.Tools {
		info, checked := g.app.ToolInfo(t)
		items = append(items, app.ToolMenu(t, info, checked)...)
	}
	sig := st.Line
	for _, i := range items {
		sig += "|" + i.Label
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
	for _, it := range items {
		if it.Action == app.ActLaunch {
			m.Add(it.Label).OnClick(func(*application.Context) { g.OpenLaunch(it.Tool) })
		} else {
			m.Add(it.Label).OnClick(func(*application.Context) { g.OpenInstall(it.Tool, it.Action) })
		}
	}
	m.AddSeparator()
	m.Add(title("Settings…", "Settings…")).OnClick(func(*application.Context) { g.OpenSettings() })
	m.Add(title("Quit code-link", "Quit")).OnClick(func(*application.Context) { g.wails.Quit() })
	m.Update()
}

// chooseFolder asks for a folder, starting at current, over the tool's
// launch window; "" when cancelled.
func (g *GUI) chooseFolder(t launch.Tool, current string) string {
	activate()
	d := g.wails.Dialog.OpenFile().
		CanChooseFiles(false).
		CanChooseDirectories(true).
		CanCreateDirectories(true).
		SetTitle("Choose the folder to start " + t.DisplayName() + " in").
		SetMessage("Choose the folder to start " + t.DisplayName() + " in").
		SetButtonText("Choose")
	if w := g.launch[t]; w != nil {
		d.AttachToWindow(w)
	}
	if current != "" {
		d.SetDirectory(current)
	}
	folder, err := d.PromptForSingleSelection()
	if err != nil {
		return ""
	}
	return folder
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
