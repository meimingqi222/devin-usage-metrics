// Package desktop owns application state and native MyGo views. Background
// workers deliver results through the main-thread event queue.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
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
	account   quota.Account
	result    quota.Result
	err       error
	profileID string
	isLive    bool
}

type App struct {
	ctx                                        context.Context
	events                                     chan func()
	wake                                       func()
	agent                                      model.Agent
	period                                     analytics.Period
	page                                       int
	tab, screen, lang, device, search, message string
	// loaded holds only the current agent's data; switching agents drops the
	// previous model.Data entirely instead of retaining one copy per agent.
	// counts carries the sidebar's session count for agents already seen.
	loaded      model.Data
	loadedAgent model.Agent
	counts      map[model.Agent]int
	trayEntries map[model.Agent][]trayEntry
	busy        map[string]bool
	remote      model.Data
	// remoteTurns holds remote devices' turns only while the device scope
	// shows them; remoteHeads names each remote device's last head object
	// so ensureRemote can rebuild the turns from the sync disk cache.
	remoteTurns                           []model.Turn
	remoteHeads                           map[string]string
	accounts                              []accountResult
	selected                              *model.Session
	settings                              bool
	apiURL, deviceName                    string
	auth                                  syncclient.Authorization
	loginCancel                           context.CancelFunc
	confirm                               bool
	pending                               *compaction.Recommendation
	release                               updater.Release
	payload                               string
	quit                                  func()
	openFolder, openURL                   func(string)
	writeClipboard                        func(string) error
	accountBackupFile                     func(save bool, title string) (string, error)
	languageChanged                       func()
	updatePrefs                           updatePrefs
	localID                               string
	pendingApply                          func() error
	settingsUI                            settingsUIState
	trayUI                                trayUIState
	otherAgents, agentsClosed, deviceMenu bool
	sessionsUI                            sessionsUIState
	loadedAt                              string
	hasLoaded                             bool
	compactionDetails                     bool
	installed                             map[model.Agent]bool
	cachedData                            *model.Data
	cachedKey                             dataKey
	compactionUI                          compactionState
	sessionCosts                          map[sessionIdentity]sessionUsage
	sessionList                           sessionListCache
	quotaUI                               quotaUIState
	accountStore                          *accounts.Store
	profiles                              []accounts.Profile
	accountUI                             accountUIState
	backupUI                              accountBackupUI
	accountSync                           accountSyncState
	accountPasswords                      accountPasswordStore
	accountPasswordMu                     sync.Mutex
	usageUI                               usageUIState
	syncUI                                syncState
	extUI                                 extUIState
}

// syncState is the last sync's outcome shown in the sidebar.
type syncState struct {
	err         string
	completedAt time.Time
}

func New(ctx context.Context) *App {
	// Like Rust, start on this device's data; All devices is one click away.
	id := data.DeviceID()
	store := accounts.Open()
	return &App{ctx: ctx, events: make(chan func(), 64), agent: model.Devin, loadedAgent: model.Agent(""), period: analytics.Day, tab: "usage", screen: "history", lang: language(), counts: map[model.Agent]int{}, trayEntries: map[model.Agent][]trayEntry{}, busy: map[string]bool{}, apiURL: syncclient.ReadConfig().APIURL, deviceName: data.DeviceName(), localID: id, device: id, usageUI: usageUIState{expandedBuckets: map[int64]bool{}}, sessionsUI: loadSessionUI(), installed: installedAgents(), quotaUI: quotaUIState{keychainLikely: quota.MacOSMayHaveKeychainAccounts()}, accountStore: store, profiles: store.List(), updatePrefs: loadUpdatePrefs()}
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
		// Collections peak several times the data they keep; without this the
		// GC keeps the freed pages and the tray's RSS stays at the high-water
		// mark for the life of the process.
		debug.FreeOSMemory()
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
		apply := func(d model.Data, phase string) func() {
			readyAt := time.Now()
			return func() {
				syncdiag.Log(ctx, "desktop_load_applied", "apply_wait_ms", time.Since(readyAt).Milliseconds(), "phase", phase)
				a.storeLoaded(agent, d)
			}
		}
		var d model.Data
		if refresh && agent == model.Devin {
			// A changed Devin database takes seconds to rebuild in full; show
			// the incremental result first while the rebuild stays busy.
			a.post(apply(data.CheckContext(ctx, agent, 0, end), "incremental"))
		}
		if refresh {
			d = data.ReloadContext(ctx, agent, 0, end)
		} else {
			d = data.LoadContext(ctx, agent, 0, end, false)
		}
		applied := apply(d, "final")
		return func() {
			applied()
			if refresh && a.syncDue(time.Now()) {
				a.sync()
			}
		}
	})
}

// storeLoaded records a finished load: sidebar/tray aggregates always land,
// but the model.Data itself is kept only for the agent still selected.
func (a *App) storeLoaded(agent model.Agent, d model.Data) {
	a.counts[agent] = len(d.Sessions)
	a.trayEntries[agent] = trayEntriesOf(d.Turns, a.localID, d.Sessions)
	if agent != a.agent {
		return
	}
	a.loaded = d
	a.loadedAgent = agent
	a.loadedAt = time.Now().Format("15:04:05")
	a.hasLoaded = true
}

func (a *App) syncDue(now time.Time) bool {
	return a.syncUI.completedAt.IsZero() || now.Sub(a.syncUI.completedAt) >= 5*time.Minute
}

// syncDeadline bounds a whole cycle. Individual requests already time out, but
// many devices on a flaky network can otherwise keep the tray in "Syncing" for
// minutes with no clear failure.
const syncDeadline = 2 * time.Minute

func (a *App) sync() {
	if !syncclient.ReadConfig().Enabled {
		return
	}
	a.syncAccounts()
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
				a.remoteHeads = remote.RemoteHeads
				if a.scopeIsLocal() {
					// This device is the scope: remote turns and their
					// last-good fallback copies stay out of memory until
					// the scope needs them.
					a.remoteTurns = nil
					remote.Turns = nil
					syncclient.DropRemoteCache()
				} else {
					a.remoteTurns = remote.Turns
				}
				a.remote = remote
				a.syncUI.completedAt = time.Now()
			}
			a.syncUI.err = ""
			if err != nil {
				a.syncUI.err = err.Error()
			}
		}
	})
}

func (a *App) refreshQuota(force bool) {
	// Saved credentials may refresh during quota reads. Do not replace the
	// same files concurrently while account changes are being synchronized.
	if a.busy["account-sync"] {
		a.accountSync.quotaPending = true
		a.accountSync.quotaForce = a.accountSync.quotaForce || force
		return
	}
	existing := make(map[string]accountResult, len(a.accounts))
	for _, ar := range a.accounts {
		existing[ar.account.Key] = ar
	}
	a.work("quota", func() func() {
		liveAccounts := quota.Discover(quota.KeychainAllowed())
		profiles, profileErr := a.accountStore.ReconcileContext(a.ctx, liveAccounts)

		type target struct {
			account   quota.Account
			profileID string
			isLive    bool
		}
		var targets []target
		seenProvider := map[string]bool{}

		for _, acc := range liveAccounts {
			seenProvider[acc.Provider] = true
			profID := ""
			for _, p := range profiles {
				if p.Provider == acc.Provider && p.Active {
					profID = p.ID
					break
				}
			}
			targets = append(targets, target{account: acc, profileID: profID, isLive: true})

			if a.accountStore.Enabled() {
				for _, p := range profiles {
					if p.Provider == acc.Provider && !p.Hidden && !p.Active {
						credPath := a.accountStore.CredentialPath(p.Provider, p.ID)
						if _, err := os.Stat(credPath); err == nil {
							key := "profile-" + p.ID
							label := a.profileName(p.Provider, p)
							storedAcc := quota.ProfileAccount(key, p.Provider, label, credPath)
							targets = append(targets, target{account: storedAcc, profileID: p.ID, isLive: false})
						}
					}
				}
			}
		}

		if a.accountStore.Enabled() {
			for _, p := range profiles {
				if !seenProvider[p.Provider] && !p.Hidden && !p.Active {
					credPath := a.accountStore.CredentialPath(p.Provider, p.ID)
					if _, err := os.Stat(credPath); err == nil {
						key := "profile-" + p.ID
						label := a.profileName(p.Provider, p)
						storedAcc := quota.ProfileAccount(key, p.Provider, label, credPath)
						targets = append(targets, target{account: storedAcc, profileID: p.ID, isLive: false})
					}
				}
			}
		}

		// Frame-0 snapshot: immediately surface discovered targets and their tabbed grouping
		// to the UI without waiting seconds for remote network queries to complete.
		initial := make([]accountResult, len(targets))
		for i, t := range targets {
			if prev, ok := existing[t.account.Key]; ok {
				initial[i] = prev
				initial[i].isLive = t.isLive
				initial[i].profileID = t.profileID
			} else {
				initial[i] = accountResult{
					account:   t.account,
					profileID: t.profileID,
					isLive:    t.isLive,
				}
			}
		}
		a.post(func() {
			a.accounts = initial
			a.profiles = profiles
		})

		out := make([]accountResult, len(targets))
		copy(out, initial)
		var wg sync.WaitGroup
		limit := make(chan struct{}, 4)
		for i, t := range targets {
			wg.Go(func() {
				limit <- struct{}{}
				defer func() { <-limit }()
				result, err := quota.Fetch(a.ctx, t.account, force)
				res := accountResult{
					account:   t.account,
					result:    result,
					err:       err,
					profileID: t.profileID,
					isLive:    t.isLive,
				}
				out[i] = res
				a.post(func() {
					if i < len(a.accounts) && a.accounts[i].account.Key == res.account.Key {
						a.accounts[i] = res
					}
				})
			})
		}
		wg.Wait()
		return func() {
			a.accounts, a.quotaUI.queriedAt = out, time.Now().Format("15:04:05")
			a.profiles = profiles
			if profileErr != nil {
				a.message = profileErr.Error()
			}
			if a.accountSync.dirty {
				a.syncAccounts()
			}
		}
	})
}

// dataKey identifies the slices allData merges. Loads and syncs replace whole
// slices, so their identity changes whenever the merged view would.
type dataKey struct {
	agent                      model.Agent
	device                     string
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

// switchAgent selects an agent, dropping the previous agent's data and
// reloading when the selection actually changes. Clicking the current
// agent still returns from the quota/extension screens to history.
func (a *App) switchAgent(agent model.Agent) {
	if agent == a.agent {
		a.screen = "history"
		return
	}
	a.agent = agent
	a.screen = "history"
	a.page = 0
	a.selected = nil
	if a.tab == "compaction" && agent != model.Claude && agent != model.Codex {
		a.tab = "usage"
	}
	a.loaded = model.Data{}
	a.loadedAgent = ""
	a.invalidateDerived()
	a.load(agent, false)
}

// switching reports that the selected agent's load is in flight: the
// content area shows a spinner instead of the previous agent's data.
func (a *App) switching() bool {
	return a.screen == "history" && a.loadedAgent != a.agent && a.busy[string(a.agent)]
}

// remoteView returns the remote records the current device scope needs:
// everything for "All devices" or a remote device, sessions only when the
// scope is this device. Remote turns are kept out of memory until then;
// a.remoteTurns carries the on-demand rebuild.

// scopeIsLocal is true only for the explicit "this device" scope; an empty
// device is the "All devices" scope, not a missing device.
func (a *App) scopeIsLocal() bool {
	return a.localID != "" && a.device == a.localID
}

func (a *App) remoteView() model.Data {
	var r model.Data
	if !a.scopeIsLocal() {
		r.Sessions, r.Turns = a.remote.Sessions, a.remoteTurns
		return r
	}
	r.Sessions = a.remote.Sessions
	return r
}

// ensureRemote rebuilds remote turns from the cached sync heads when the
// device scope needs them, and frees them when it does not.
func (a *App) ensureRemote() {
	if a.scopeIsLocal() {
		if len(a.remoteTurns) > 0 {
			a.remoteTurns = nil
			a.invalidateDerived()
			debug.FreeOSMemory()
		}
		return
	}
	if len(a.remoteTurns) > 0 || len(a.remoteHeads) == 0 || a.busy["remote"] {
		return
	}
	a.work("remote", func() func() {
		ctx := syncdiag.WithFields(syncdiag.WithRun(a.ctx), "operation", "remote_decode")
		remote, err := syncclient.DecodeRemoteDevices(ctx, a.remoteHeads)
		return func() {
			usable := err == nil || errors.Is(err, syncclient.ErrPartial)
			syncdiag.Log(ctx, "remote_decode_result", "usable", usable, "error", syncdiag.ErrorCode(err), "turns", len(remote.Turns))
			if usable {
				a.remoteTurns = remote.Turns
				a.invalidateDerived()
			}
		}
	})
}

func (a *App) dataKey() dataKey {
	d := a.loaded
	if a.loadedAgent != a.agent {
		d = model.Data{}
	}
	r := a.remoteView()
	return dataKey{a.agent, a.device, first(d.Sessions), first(r.Sessions), first(d.Turns), first(r.Turns), len(d.Sessions), len(r.Sessions), len(d.Turns), len(r.Turns)}
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
	d := a.loaded
	if a.loadedAgent != a.agent {
		d = model.Data{}
	}
	r := a.remoteView()
	d.Sessions = append(append([]model.Session(nil), d.Sessions...), r.Sessions...)
	d.Turns = append(append([]model.Turn(nil), d.Turns...), r.Turns...)
	a.cachedData, a.cachedKey = &d, key
	a.sessionCosts = nil
	a.sessionList.valid = false
	a.usageUI.cache.buckets = nil
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
