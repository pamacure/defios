// Command defios is the DeFiOS Node CLI.
package main

import (
	"context"
	"os"

	"defios.com/internal/cli"
)

func main() {
	home, _ := os.UserHomeDir()
	app := cli.App{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv,
		Home:   home,
	}
	os.Exit(app.Run(context.Background(), os.Args[1:]))
}
