//go:build windows || (darwin && (cgo || purego)) || (linux && (purego || desktop))

package main

import (
	"errors"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/zalando/go-keyring"

	"defios.com/desktop/internal/shell"
)

// keychainService names the app's entries in the OS keychain; each is
// keyed by Node ID.
const keychainService = "DeFiOS"

// osKeychain is the OS keychain: the Secret Service on Linux, the login
// Keychain on macOS and the Credential Manager on Windows.
type osKeychain struct{}

func (osKeychain) Get(account string) (string, error) {
	s, err := keyring.Get(keychainService, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", shell.ErrNoSecret
	}
	return s, err
}

func (osKeychain) Set(account, secret string) error {
	return keyring.Set(keychainService, account, secret)
}

func (osKeychain) Delete(account string) error {
	err := keyring.Delete(keychainService, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return shell.ErrNoSecret
	}
	return err
}

// autostart registers the app with the OS to start at login, with args:
// an XDG autostart entry on Linux, a LaunchAgent or login item on macOS,
// the Run key on Windows.
type autostart struct {
	app  *application.App
	args []string
}

func (a autostart) Enable() error {
	return a.app.Autostart.EnableWithOptions(application.AutostartOptions{Identifier: "com.defios.desktop", Arguments: a.args})
}

func (a autostart) Disable() error { return a.app.Autostart.Disable() }
