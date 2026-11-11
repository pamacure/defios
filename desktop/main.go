//go:build windows || (darwin && (cgo || purego)) || (linux && (purego || desktop))

// Command desktop is the DeFiOS desktop app (ADR 0034): a Wails v3 window
// and tray icon that host the Node through internal/node, as `defios
// serve` does for the terminal. Closing the window hides it to the tray
// and the Node keeps serving; Lock or Quit stops it.
//
// On Linux it builds only when asked, so `go test ./...` needs no GTK:
// -tags desktop for Wails' cgo backend, which needs the GTK 4 and
// WebKitGTK 6.0 development packages, or CGO_ENABLED=0 -tags purego,
// which loads those libraries at run time. On macOS it needs cgo (or
// -tags purego); on Windows nothing.
package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"defios.com/desktop/internal/prefs"
	"defios.com/desktop/internal/shell"
	"defios.com/internal/node"
	"defios.com/internal/nodedir"
)

//go:embed all:frontend
var frontend embed.FS

//go:embed build/tray.png
var trayIcon []byte

//go:embed build/appicon.png
var appIcon []byte

// loginFlag is the argument start at login launches the app with.
const loginFlag = "--login"

func main() {
	dir := flag.String("dir", "", "the Node directory, as `defios --dir`")
	atLogin := flag.Bool("login", false, "the OS started the app at login")
	flag.Parse()
	if err := run(*dir, *atLogin); err != nil {
		log.Fatal(err)
	}
}

func run(dir string, atLogin bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	paths := nodedir.Resolve(nodedir.Lookup{Dir: dir, Getenv: os.Getenv, Home: home})
	userConfig, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	prefsFile, err := prefs.Path(userConfig, paths)
	if err != nil {
		return err
	}
	store := prefs.Store{File: prefsFile}
	saved, _ := store.Load() // a bad file shows in the app once Start loads it

	assets, err := fs.Sub(frontend, "frontend")
	if err != nil {
		return err
	}
	notifier := notifications.New()
	ui := &UI{}
	app := application.New(application.Options{
		Name:        "DeFiOS",
		Description: "A self-hosted multi-party computation wallet",
		Icon:        appIcon,
		Services:    []application.Service{application.NewService(ui), application.NewService(notifier)},
		Assets:      application.AssetOptions{Handler: application.BundledAssetFileServer(assets)},
		Mac:         application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false},
		Windows:     application.WindowsOptions{DisableQuitOnLastWindowClosed: true},
		Linux:       application.LinuxOptions{DisableQuitOnLastWindowClosed: true, ProgramName: "defios-desktop"},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID:               "com.defios.desktop",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) { ui.show() },
		},
	})

	width, height := 1200, 800
	if saved.Width > 0 && saved.Height > 0 {
		width, height = saved.Width, saved.Height
	}
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:      "main",
		Title:     "DeFiOS",
		Width:     width,
		Height:    height,
		MinWidth:  720,
		MinHeight: 520,
		// Started at login, the app waits hidden in the tray.
		Hidden: atLogin && saved.StartAtLogin,
		URL:    "/",
	})

	var autostartArgs []string
	if dir != "" {
		autostartArgs = append(autostartArgs, "--dir", dir)
	}
	sh := shell.New(shell.Deps{
		Open: func() (shell.Node, error) {
			h, err := node.Open(paths, node.Options{Logf: log.Printf})
			if err != nil {
				return nil, err
			}
			return h, nil
		},
		Prefs:     store,
		Keychain:  osKeychain{},
		Autostart: autostart{app: app, args: append(autostartArgs, loginFlag)},
		Notify: func(title, body string) {
			if err := notifier.SendNotification(notifications.NotificationOptions{ID: "defios-locked", Title: title, Body: body}); err != nil {
				log.Printf("notification: %v", err)
			}
		},
		OnChange: func(st shell.State) {
			app.Event.Emit(stateEvent, st)
			ui.trayFor(st)
		},
	})
	ui.shell, ui.win = sh, win

	// Closing the window hides it; the Node keeps serving.
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		w, h := win.Size()
		sh.SetWindowSize(w, h)
		win.Hide()
		e.Cancel()
	})
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) { ui.show() })

	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetTooltip("DeFiOS")
	menu := app.NewMenu()
	menu.Add("Show DeFiOS").OnClick(func(*application.Context) { ui.show() })
	ui.lockItem = menu.Add("Lock").OnClick(func(*application.Context) { sh.Lock() })
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(*application.Context) { app.Quit() })
	tray.SetMenu(menu)
	tray.OnClick(ui.show)
	ui.trayFor(sh.State())

	// Quit stops serving before the process exits.
	app.OnShutdown(sh.Close)
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		go sh.Start(atLogin)
	})
	if err := app.Run(); err != nil {
		return fmt.Errorf("desktop: %w", err)
	}
	return nil
}
