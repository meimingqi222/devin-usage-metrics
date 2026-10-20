// Package desktop owns application state and native MyGo views. Background
// workers deliver results through the main-thread event queue.
package desktop

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/analytics"
	"github.com/meimingqi222/devin-usage-metrics/internal/compaction"
	"github.com/meimingqi222/devin-usage-metrics/internal/data"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
	"github.com/meimingqi222/devin-usage-metrics/internal/updater"
)

type accountResult struct {
	account quota.Account
	result  quota.Result
	err     error
}

type App struct {
	ctx                                        context.Context
	events                                     chan func()
	wake                                       func()
	agent                                      model.Agent
	period                                     analytics.Period
	page                                       int
	tab, screen, lang, device, search, message string
	loaded                                     map[model.Agent]model.Data
	busy                                       map[string]bool
	remote                                     model.Data
	accounts                                   []accountResult
	selected                                   *model.Session
	settings                                   bool
	apiURL, deviceName, cookie, accountLabel   string
	auth                                       syncclient.Authorization
	loginCancel                                context.CancelFunc
	confirm                                    bool
	pending                                    *compaction.Recommendation
	release                                    updater.Release
	payload                                    string
	quit                                       func()
	openFolder                                 func(string)
	localID                                    string
	pendingApply                               func() error
	settingsUI                                 settingsUIState
	trayUI                                     trayUIState
	otherAgents, agentsClosed, devicesClosed   bool
	expandedBuckets                            map[int64]bool
	sessionsUI                                 sessionsUIState
	loadedAt                                   string
	compactionDetails                          bool
	installed                                  map[model.Agent]bool
	hideEmptyBuckets                           bool
	syncLastAt, syncError                      string
}

func New(ctx context.Context) *App {
	return &App{ctx: ctx, events: make(chan func(), 64), agent: model.Devin, period: analytics.Day, tab: "usage", screen: "history", lang: language(), loaded: map[model.Agent]model.Data{}, busy: map[string]bool{}, apiURL: syncclient.ReadConfig().APIURL, deviceName: data.DeviceName(), localID: data.DeviceID(), expandedBuckets: map[int64]bool{}, sessionsUI: loadSessionUI(), installed: installedAgents()}
}

func (a *App) t(en, zh string) string {
	if a.lang == "zh" {
		return zh
	}
	return en
}

func (a *App) post(fn func()) {
	select {
	case a.events <- fn:
		if a.wake != nil {
			a.wake()
		}
	case <-a.ctx.Done():
	}
}

func (a *App) drain() {
	for {
		select {
		case fn := <-a.events:
			fn()
		default:
			return
		}
	}
}

func (a *App) work(key string, fn func() func()) {
	if a.busy[key] {
		return
	}
	a.busy[key] = true
	go func() {
		done := fn()
		a.post(func() {
			delete(a.busy, key)
			if done != nil {
				done()
			}
		})
	}()
}

func (a *App) load(agent model.Agent, refresh bool) {
	a.work(string(agent), func() func() {
		d := data.Load(agent, 0, time.Now().Add(time.Hour).Unix(), refresh)
		return func() { a.loaded[agent] = d; a.loadedAt = time.Now().Format("15:04:05") }
	})
}

func (a *App) sync() {
	if !syncclient.ReadConfig().Enabled {
		return
	}
	a.work("sync", func() func() {
		local := data.LoadAll(0, time.Now().Add(time.Hour).Unix(), false)
		remote, err := syncclient.Cycle(a.ctx, local)
		return func() {
			if err != nil {
				a.syncError = err.Error()
			} else {
				a.remote = remote
				a.syncError = ""
				a.syncLastAt = time.Now().Format("15:04:05")
			}
		}
	})
}

func (a *App) refreshQuota(force bool) {
	a.work("quota", func() func() {
		accounts := quota.Discover(quota.KeychainAllowed())
		out := make([]accountResult, len(accounts))
		var wg sync.WaitGroup
		limit := make(chan struct{}, 4)
		for i, account := range accounts {
			wg.Go(func() {
				limit <- struct{}{}
				defer func() { <-limit }()
				result, err := quota.Fetch(a.ctx, account, force)
				out[i] = accountResult{account, result, err}
			})
		}
		wg.Wait()
		return func() { a.accounts = out }
	})
}

func (a *App) allData() model.Data {
	d := a.loaded[a.agent]
	d.Sessions = append(append([]model.Session(nil), d.Sessions...), a.remote.Sessions...)
	d.Turns = append(append([]model.Turn(nil), d.Turns...), a.remote.Turns...)
	return d
}

func formatTokens(n float64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2fB", n/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", n/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fK", n/1e3)
	default:
		return fmt.Sprintf("%.0f", n)
	}
}
