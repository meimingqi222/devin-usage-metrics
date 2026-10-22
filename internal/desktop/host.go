package desktop

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/pricing"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
	"github.com/meimingqi222/devin-usage-metrics/internal/updater"
)

const (
	updateFirstCheck    = 10 * time.Second
	updateCheckInterval = 4 * time.Hour
)

// trayMemoryLimit is a soft heap limit for the long-running tray process.
// Loading every agent briefly needs several times the data it keeps, and
// without a limit the GC let that resident set grow far beyond Rust's. Above
// it the GC works harder; nothing fails. GOMEMLIMIT still takes precedence.
const trayMemoryLimit = 384 << 20

// Run starts the native desktop host. The content views never own window handles.
func Run(icon []byte) error {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(trayMemoryLimit)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A packaged app reports its bundle's version, which the build took
	// from the same mygo.json main.go embeds.
	if version := mygo.App.Version(); version != "" {
		updater.CurrentVersion = version
		syncdiag.AppVersion = version
	}
	a := New(ctx)
	mygo.App.SetName("Agent Usage Metrics")
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyRegular)
	if !mygo.App.RequestSingleInstanceLock() {
		return nil
	}
	var main, popup *mygo.Window
	var tray *mygo.Tray
	quitting := false
	a.quit = mygo.App.Quit
	a.openFolder = func(path string) {
		if err := mygo.Shell.OpenPath(path); err != nil {
			a.message = err.Error()
		}
	}
	a.openURL = func(url string) {
		if err := mygo.Shell.OpenExternal(url); err != nil {
			a.message = err.Error()
		}
	}
	if runtime.GOOS == "darwin" {
		// Rust binds only the app and window menus, with localized labels.
		// Keep the edit menu so text fields still get copy and paste.
		mygo.App.SetMenu(mygo.NewMenu([]*mygo.MenuItem{
			{Label: "Agent Usage Metrics", Submenu: []*mygo.MenuItem{
				{Role: mygo.RoleHide, Label: a.t("Hide Agent Usage Metrics", "隐藏 Agent Usage Metrics")},
				{Role: mygo.RoleHideOthers, Label: a.t("Hide Others", "隐藏其他应用")},
				mygo.Separator(),
				{Role: mygo.RoleQuit, Label: a.t("Quit", "退出")},
			}},
			{Role: mygo.RoleEditMenu},
			{Label: a.t("Window", "窗口"), Submenu: []*mygo.MenuItem{{Role: mygo.RoleClose, Label: a.t("Close Window", "关闭窗口")}}},
		}))
	}
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { quitting = true; cancel() })
	mygo.App.WhenReady(func() {
		main = mygo.NewWindow(mygo.WindowOptions{Title: "Agent Usage Metrics", Width: 1240, Height: 780, MinWidth: 1000, MinHeight: 640, TitleBarStyle: mygo.TitleBarHidden, TitleBarHeight: 48, TrafficLightPosition: &mygo.Point{X: 16, Y: 16}, Content: ui.View(a.View)})
		popup = mygo.NewWindow(mygo.WindowOptions{Title: "Agent Usage Metrics — Quotas", Width: trayWidth, Height: 520, Hidden: true, Frameless: true, DisableResize: true, AlwaysOnTop: true, SkipTaskbar: true, Content: ui.View(a.trayView)})
		openDashboard := func() { popup.Hide(); main.Show(); main.Focus() }
		a.trayUI.openDashboard = openDashboard
		a.trayUI.closePopup = popup.Hide
		a.trayUI.quit = mygo.App.Quit
		popup.OnBlur(popup.Hide)
		popup.OnClose(func(e *mygo.CloseEvent) {
			if !quitting {
				e.PreventDefault()
				popup.Hide()
			}
		})
		// Drain here too: hidden windows may not render, and an undrained queue
		// would block every worker once its buffer fills.
		a.wake = func() { mygo.RunOnMain(func() { a.drain(); main.Invalidate(); popup.Invalidate() }) }
		show := func() {
			if tray != nil {
				anchor := tray.Bounds()
				if anchor.Width > 0 && anchor.Height > 0 {
					area := mygo.Screen.DisplayNearestPoint(mygo.Point{X: anchor.X + anchor.Width/2, Y: anchor.Y + anchor.Height/2}).WorkArea
					b := popup.Bounds()
					x := anchor.X + (anchor.Width-b.Width)/2
					y := anchor.Y + anchor.Height + 6
					if y+b.Height > area.Y+area.Height {
						y = anchor.Y - b.Height - 6
					}
					popup.SetPosition(max(area.X, min(x, area.X+max(0, area.Width-b.Width))), max(area.Y, min(y, area.Y+max(0, area.Height-b.Height))))
				} else {
					popup.Center()
				}
			}
			popup.Show()
			popup.Focus()
			a.refreshQuota(false)
			for _, agent := range []model.Agent{model.Claude, model.Codex, model.Devin, model.Antigravity, model.OpenCode} {
				a.load(agent, false)
			}
		}
		// Like Rust's on_reopen: a Dock click brings back the hidden dashboard.
		mygo.App.OnActivate(func(bool) {
			if !main.IsVisible() {
				openDashboard()
			}
		})
		// Built on demand so labels follow the current language.
		trayMenu := func() *mygo.Menu {
			return mygo.NewMenu([]*mygo.MenuItem{
				{Label: a.t("Live quotas", "实时配额"), Click: func(*mygo.MenuItem, *mygo.Window) { show() }},
				{Label: a.t("Open Dashboard", "打开控制台"), Click: func(*mygo.MenuItem, *mygo.Window) { openDashboard() }},
				{Label: a.t("Refresh Quotas", "刷新配额"), Click: func(*mygo.MenuItem, *mygo.Window) { a.refreshQuota(true) }},
				mygo.Separator(),
				{Role: mygo.RoleQuit, Label: a.t("Quit", "退出")},
			})
		}
		var err error
		tray, err = mygo.NewTray(mygo.TrayOptions{Icon: icon, IconIsTemplate: runtime.GOOS == "darwin", ToolTip: "Agent Usage Metrics"})
		if err != nil {
			a.message = fmt.Sprintf("Tray unavailable: %v", err)
		} else {
			// The status text shows beside the icon on macOS. The Windows and
			// Linux trays have no title, so it joins the tooltip there.
			a.trayUI.setTitle = func(title string) {
				if runtime.GOOS == "darwin" {
					tray.SetTitle(title)
					return
				}
				tip := "Agent Usage Metrics"
				if title != "" {
					tip += " · " + title
				}
				tray.SetToolTip(tip)
			}
			a.updateTrayTitle()
			if runtime.GOOS == "linux" {
				tray.SetMenu(trayMenu())
				a.languageChanged = func() { tray.SetMenu(trayMenu()) }
			} else {
				tray.OnClick(func() {
					if popup.IsVisible() {
						popup.Hide()
					} else {
						show()
					}
				})
				tray.OnRightClick(func() { popup.Hide(); tray.SetMenu(trayMenu()); defer tray.SetMenu(nil); tray.PopUpMenu() })
			}
			main.OnClose(func(e *mygo.CloseEvent) {
				if !quitting {
					e.PreventDefault()
					main.Hide()
				}
			})
		}
		a.load(a.agent, false)
		a.refreshQuota(false)
		a.sync()
		if err := updater.RemoveBackups(); err != nil {
			a.message = err.Error()
		}
		go func() {
			// Match Rust: first check after 10s, then every four hours.
			delay := updateFirstCheck
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
					a.post(func() { a.checkUpdates(false) })
					delay = updateCheckInterval
				}
			}
		}()
		go func() { pricing.Refresh(ctx, false); a.post(a.invalidateDerived) }()
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			minutes := 0
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					minutes++
					hourly := minutes%60 == 0
					a.post(func() {
						a.refreshQuota(false)
						if hourly {
							a.sync()
							go func() {
								if pricing.Refresh(ctx, false) {
									a.post(a.invalidateDerived)
								}
							}()
						}
					})
				}
			}
		}()
	})
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		if tray != nil {
			tray.Destroy()
		}
	})
	return mygo.App.Run()
}
