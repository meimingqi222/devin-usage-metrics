package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"

	"github.com/meimingqi222/devin-usage-metrics/internal/cli"
	"github.com/meimingqi222/devin-usage-metrics/internal/desktop"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
	"github.com/meimingqi222/devin-usage-metrics/internal/updater"
)

// The tray icon follows each platform: a template of the app icon's glyph
// that macOS tints for the menu bar, and the color app icon elsewhere.
var (
	//go:embed assets/tray_icon.png
	trayTemplate []byte
	//go:embed assets/tray_icon_color.png
	trayColor []byte
)

// mygo.json's version is the app's only version: it names the package,
// the bundle and every place the app shows or compares its version.
//
//go:embed mygo.json
var appConfig []byte

func main() {
	var config struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(appConfig, &config) == nil && config.Version != "" {
		updater.CurrentVersion = config.Version
	}
	syncdiag.AppVersion = updater.CurrentVersion
	args := os.Args[1:]
	var err error
	if len(args) > 0 {
		err = cli.Run(args, os.Stdout, os.Stderr)
	} else {
		err = desktop.Run(trayTemplate, trayColor)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
