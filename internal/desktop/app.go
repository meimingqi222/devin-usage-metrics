// Package desktop owns application state and native MyGo views. Background
// workers deliver results through the main-thread event queue.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/analytics"
	"github.com/meimingqi222/devin-usage-metrics/internal/compaction"
	"github.com/meimingqi222/devin-usage-metrics/internal/data"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
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
	apiURL, deviceName                         string
	auth                                       syncclient.Authorization
	loginCancel                                context.CancelFunc
	confirm                                    bool
	pending                                    *compaction.Recommendation
	release                                    updater.Release
	payload                                    string
	quit                                       func()
	openFolder, openURL                        func(string)
	languageChanged                            func()
	updatePrefs                                updatePrefs
	localID                                    string
	pendingApply                               func() error
	settingsUI                                 settingsUIState
	trayUI                                     trayUIState
	otherAgents, agentsClosed, deviceMenu      bool
	expandedBuckets                            map[int64]bool
	sessionsUI                                 sessionsUIState
	loadedAt                                   string
	hasLoaded                                  bool
	keychainDismissed                          bool
	compactionDetails                          bool
	installed                                  map[model.Agent]bool
	hideEmptyBuckets                           bool
	syncLastAt, syncError                      string
	syncCompletedAt                            time.Time
	cachedData                                 *model.Data
	cachedKey                                  dataKey
	compactionUI                               compactionState
	chartMode                                  int
	sessionCosts                               map[sessionIdentity]sessionUsage
	usageCache                                 struct {
		period  analytics.Period
		page    int
		device  string
		start   int64
		buckets []analytics.Bucket
	}
}

func New(ctx context.Context) *App {
	// Like Rust, start on this device's data; All devices is one click away.
	id := data.DeviceID()
	return &App{ctx: ctx, events: make(chan func(), 64), agent: model.Devin, period: analytics.Day, tab: "usage", screen: "history", lang: language(), loaded: map[model.Agent]model.Data{}, busy: map[string]bool{}, apiURL: syncclient.ReadConfig().APIURL, deviceName: data.DeviceName(), localID: id, device: id, expandedBuckets: map[int64]bool{}, sessionsUI: loadSessionUI(), installed: installedAgents(), updatePrefs: loadUpdatePrefs()}
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
		ctx := syncdiag.WithFields(syncdiag.WithRun(a.ctx), "operation", "desktop_load", "agent", string(agent), "refresh", refresh)
		syncdiag.Log(ctx, "desktop_load_start", syncdiag.BuildFields()...)
		end := time.Now().Add(time.Hour).Unix()
		var d model.Data
		if refresh {
			d = data.ReloadContext(ctx, agent, 0, end)
		} else {
			d = data.LoadContext(ctx, agent, 0, end, false)
		}
		readyAt := time.Now()
		return func() {
			syncdiag.Log(ctx, "desktop_load_applied", "apply_wait_ms", time.Since(readyAt).Milliseconds())
			a.loaded[agent] = d
			a.loadedAt = time.Now().Format("15:04:05")
			a.hasLoaded = true
			if refresh && a.syncDue(time.Now()) {
				a.sync()
			}
		}
	})
}

func (a *App) syncDue(now time.Time) bool {
	return a.syncCompletedAt.IsZero() || now.Sub(a.syncCompletedAt) >= 5*time.Minute
}

// syncDeadline bounds a whole cycle. Individual requests already time out, but
// many devices on a flaky network can otherwise keep the tray in "Syncing" for
// minutes with no clear failure.
const syncDeadline = 2 * time.Minute

func (a *App) sync() {
	if !syncclient.ReadConfig().Enabled {
		return
	}
	a.work("sync", func() func() {
		runCtx := syncdiag.WithFields(syncdiag.WithRun(a.ctx), "operation", "sync")
		started := time.Now()
		syncdiag.Log(runCtx, "desktop_sync_start", append([]any{"network_deadline_ms", syncDeadline.Milliseconds()}, syncdiag.BuildFields()...)...)
		collectCtx, collected := syncdiag.Watch(runCtx, "local_collection")
		local := data.LoadAllContext(collectCtx, 0, time.Now().Add(time.Hour).Unix(), false)
		syncdiag.Log(collectCtx, "collection_result", "sessions", len(local.Sessions), "turns", len(local.Turns), "source_errors", len(local.Errors))
		collected(nil)
		ctx, cancel := context.WithTimeout(runCtx, syncDeadline)
		defer cancel()
		remote, err := syncclient.Cycle(ctx, local)
		syncdiag.Log(runCtx, "desktop_sync_result", "elapsed_ms", time.Since(started).Milliseconds(), "error", syncdiag.ErrorCode(err), "remote_sessions", len(remote.Sessions), "remote_turns", len(remote.Turns))
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("sync timed out after %s", syncDeadline)
		}
		// A partial cycle still returns every device it could read, with the
		// last good data for those it could not.
		usable := err == nil || errors.Is(err, syncclient.ErrPartial)
		readyAt := time.Now()
		return func() {
			syncdiag.Log(runCtx, "desktop_sync_applied", "remote_replaced", usable, "apply_wait_ms", time.Since(readyAt).Milliseconds())
			if usable {
				a.remote = remote
				a.syncCompletedAt = time.Now()
				a.syncLastAt = a.syncCompletedAt.Format("15:04:05")
			}
			a.syncError = ""
			if err != nil {
				a.syncError = err.Error()
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

// dataKey identifies the slices allData merges. Loads and syncs replace whole
// slices, so their identity changes whenever the merged view would.
type dataKey struct {
	agent                      model.Agent
	sessions, remoteSessions   *model.Session
	turns, remoteTurns         *model.Turn
	nSessions, nRemoteSessions int
	nTurns, nRemoteTurns       int
}

func first[T any](s []T) *T {
	if len(s) == 0 {
		return nil
	}
	return &s[0]
}

func (a *App) dataKey() dataKey {
	d := a.loaded[a.agent]
	return dataKey{a.agent, first(d.Sessions), first(a.remote.Sessions), first(d.Turns), first(a.remote.Turns), len(d.Sessions), len(a.remote.Sessions), len(d.Turns), len(a.remote.Turns)}
}

// invalidateDerived drops values computed from data, e.g. after prices change.
func (a *App) invalidateDerived() {
	a.cachedData = nil
}

func (a *App) allData() model.Data {
	key := a.dataKey()
	if a.cachedData != nil && a.cachedKey == key {
		return *a.cachedData
	}
	d := a.loaded[a.agent]
	d.Sessions = append(append([]model.Session(nil), d.Sessions...), a.remote.Sessions...)
	d.Turns = append(append([]model.Turn(nil), d.Turns...), a.remote.Turns...)
	a.cachedData, a.cachedKey = &d, key
	a.sessionCosts = nil
	a.usageCache.buckets = nil
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
