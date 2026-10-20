package main

import (
	_ "embed"
	"fmt"
	"os"

	"github.com/meimingqi222/devin-usage-metrics/internal/cli"
	"github.com/meimingqi222/devin-usage-metrics/internal/desktop"
)

//go:embed assets/tray_icon.png
var trayIcon []byte

func main() {
	args := os.Args[1:]
	var err error
	if len(args) > 0 {
		err = cli.Run(args, os.Stdout, os.Stderr)
	} else {
		err = desktop.Run(trayIcon)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
