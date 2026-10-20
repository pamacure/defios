// Command defios is the DeFiOS Node CLI.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"defios.com/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	home, _ := os.UserHomeDir()
	app := cli.App{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv,
		Home:   home,
	}
	code := app.Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
