package desktop

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/pricing"
	"github.com/meimingqi222/devin-usage-metrics/internal/updater"
)

// Run starts the native desktop host. The content views never own window handles.
func Run(icon []byte) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if version := mygo.App.Version(); version != "" {
		updater.CurrentVersion = version
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
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { quitting = true; cancel() })
	mygo.App.WhenReady(func() {
		main = mygo.NewWindow(mygo.WindowOptions{Title: "Agent Usage Metrics", Width: 1240, Height: 780, MinWidth: 1000, MinHeight: 640, TitleBarStyle: mygo.TitleBarHidden, TitleBarHeight: 48, TrafficLightPosition: &mygo.Point{X: 16, Y: 16}, Content: ui.View(a.View)})
		popup = mygo.NewWindow(mygo.WindowOptions{Title: "Agent Usage Metrics — Quotas", Width: 350, Height: 520, Hidden: true, Frameless: true, DisableResize: true, AlwaysOnTop: true, SkipTaskbar: true, Content: ui.View(a.trayView)})
		a.trayUI.openDashboard = func() { popup.Hide(); main.Show(); main.Focus() }
		a.trayUI.closePopup = popup.Hide
		a.trayUI.quit = mygo.App.Quit
		popup.OnBlur(popup.Hide)
		popup.OnClose(func(e *mygo.CloseEvent) {
			if !quitting {
				e.PreventDefault()
				popup.Hide()
			}
		})
		a.wake = func() { mygo.RunOnMain(func() { main.Invalidate(); popup.Invalidate() }) }
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
		var err error
		tray, err = mygo.NewTray(mygo.TrayOptions{Icon: icon, IconIsTemplate: runtime.GOOS == "darwin", ToolTip: "Agent Usage Metrics"})
		if err != nil {
			a.message = fmt.Sprintf("Tray unavailable: %v", err)
		} else {
			menu := mygo.NewMenu([]*mygo.MenuItem{{Label: "Quotas", Click: func(*mygo.MenuItem, *mygo.Window) { show() }}, {Label: "Open dashboard", Click: func(*mygo.MenuItem, *mygo.Window) { main.Show(); main.Focus() }}, mygo.Separator(), {Role: mygo.RoleQuit}})
			if runtime.GOOS == "linux" {
				tray.SetMenu(menu)
			} else {
				tray.OnClick(func() {
					if popup.IsVisible() {
						popup.Hide()
					} else {
						show()
					}
				})
				tray.OnRightClick(func() { popup.Hide(); tray.SetMenu(menu); defer tray.SetMenu(nil); tray.PopUpMenu() })
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
		go func() { pricing.Refresh(ctx, false); a.post(func() { main.Invalidate() }) }()
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
									a.post(func() {})
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
