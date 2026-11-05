// Package poller keeps the status-bar badge and the per-session composer
// buttons up to date: it fetches usage in the background, attributes it to
// sessions from their transcripts, and pushes the resulting UI state to the
// host.
package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cwrau/aoe-plugin-usage-badge/internal/attribution"
	"github.com/cwrau/aoe-plugin-usage-badge/internal/host"
	"github.com/cwrau/aoe-plugin-usage-badge/internal/usage"
)

// statusSlot is the global status-bar segment (no session_id). Must match
// aoe-plugin.toml's [[ui]].
const (
	statusSlot = "status-bar"
	statusID   = "usage-badge"

	pollInterval = 60 * time.Second

	// sessionIDSettingKey names the optional plugin setting holding the
	// fallback Claude session; configFetchTimeout bounds reading it.
	sessionIDSettingKey = "session_id"
	configFetchTimeout  = 1 * time.Second
)

// composerSlot is the per-session button beside the ACP composer. Must match
// aoe-plugin.toml's [[ui]]. A click reaches the worker as a notification
// carrying composerCycleMethod, which flips that session between its 5h and
// weekly share.
const (
	composerSlot        = "composer-action"
	composerID          = "usage-share"
	composerCycleMethod = "usage.cycle"
)

// pendingPayload is pushed once, immediately, before there's any real
// payload yet to keep displaying -- see Poller.
func pendingPayload() map[string]any {
	return map[string]any{"text": "…", "tooltip": "Fetching Claude usage...", "tone": string(usage.ToneNeutral)}
}

// windowShares is each Claude session's estimated share, in percentage
// points, of one limit window's utilization.
type windowShares struct {
	ok          bool
	usedPercent int
	perSession  map[string]float64 // Claude session id -> percentage points
}

type shareSnapshot struct {
	session windowShares
	week    windowShares
}

// Poller refreshes the status-bar payload and the per-session composer
// badges in the background so a slow fetch (`claude -p /usage` measured
// 12-19s against a long-lived session) never blocks the badge: whatever was
// last pushed stays displayed until a fetch actually completes --
// successfully or as a timed-out "err". Only the very first fetch, before any
// payload exists to keep showing, pushes the "pending" placeholder.
type Poller struct {
	rpc *host.Client
	wg  *sync.WaitGroup

	fetch        func(context.Context) usage.Result
	loadSessions func() ([]attribution.Session, error)
	index        *attribution.Index

	mu       sync.Mutex
	fetching bool
	hasReal  bool // a real (non-pending) payload has been pushed, so KeepLast can skip a push
	snapshot shareSnapshot
	sessions []attribution.Session
	showWeek map[string]bool   // AoE session id -> its composer badge shows the weekly share
	pushed   map[string]string // AoE session id -> last composer payload pushed (JSON)
}

// New returns a Poller pushing through rpc. Its fetch goroutines run on wg,
// so the caller's shutdown wait also covers a fetch still in flight.
func New(rpc *host.Client, wg *sync.WaitGroup) *Poller {
	p := &Poller{
		rpc:      rpc,
		wg:       wg,
		showWeek: map[string]bool{},
		pushed:   map[string]string{},
	}
	p.fetch = func(ctx context.Context) usage.Result {
		return usage.Fetch(ctx, func(ctx context.Context) string {
			ctx, cancel := context.WithTimeout(ctx, configFetchTimeout)
			defer cancel()
			return rpc.GetStringSetting(ctx, sessionIDSettingKey, "")
		})
	}
	p.loadSessions = func() ([]attribution.Session, error) {
		dir, err := attribution.AoEConfigDir()
		if err != nil {
			return nil, err
		}
		return attribution.LoadLiveSessions(dir)
	}
	root := ""
	if dir, err := usage.ClaudeConfigDir(); err == nil {
		root = filepath.Join(dir, "projects")
	}
	p.index = attribution.NewIndex(root)
	return p
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "usage-badge-worker: "+format+"\n", args...)
}

// Run pushes the pending placeholder, starts the first fetch, then triggers
// a refresh every pollInterval until stop is closed. It uses the caller's
// wg for its own fetch goroutines too, so main's shutdown wait also covers
// whichever fetch is still in flight, not just this loop.
func (p *Poller) Run(ctx context.Context, stop <-chan struct{}) {
	p.pushSlot(statusSlot, statusID, "", pendingPayload())
	p.refreshAsync(ctx)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			p.refreshAsync(ctx)
		}
	}
}

// refreshAsync starts one fetch in the background, unless one is already in
// flight -- a fetch can run longer than pollInterval, and an overlapping
// one would just pile up subprocesses for no benefit.
func (p *Poller) refreshAsync(ctx context.Context) {
	p.mu.Lock()
	if p.fetching {
		p.mu.Unlock()
		return
	}
	p.fetching = true
	p.mu.Unlock()

	p.wg.Go(func() {
		defer func() {
			p.mu.Lock()
			p.fetching = false
			p.mu.Unlock()
		}()
		p.refresh(ctx)
	})
}

// refresh runs one fetch, pushes the status-bar payload, then updates the
// per-session composer badges from the fetched windows.
func (p *Poller) refresh(ctx context.Context) {
	res := p.fetch(ctx)

	p.mu.Lock()
	skip := res.KeepLast && p.hasReal
	p.hasReal = p.hasReal || !res.KeepLast
	p.mu.Unlock()
	if !skip {
		p.pushSlot(statusSlot, statusID, "", res.Payload)
	}

	if res.HaveWindows {
		p.updateShares(ctx, res)
	}
}

func (p *Poller) updateShares(ctx context.Context, res usage.Result) {
	now := time.Now()
	sessions, err := p.loadSessions()
	if err != nil {
		logf("loading AoE sessions: %v", err)
		return
	}
	if err := p.index.Refresh(ctx, now.Add(-usage.WeekWindow-time.Hour)); err != nil {
		logf("indexing transcripts: %v", err)
		return
	}
	snap := shareSnapshot{session: p.windowShares(res.Session, usage.SessionWindow, now)}
	if res.WeekOK {
		snap.week = p.windowShares(res.Week, usage.WeekWindow, now)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot = snap
	p.sessions = sessions
	p.syncComposerLocked()
}

// windowShares attributes a window's utilization to Claude sessions by their
// weighted tokens since the window opened (its reset time minus its length).
func (p *Poller) windowShares(w usage.Window, window time.Duration, now time.Time) windowShares {
	if !w.HasReset {
		return windowShares{}
	}
	perSession, total := p.index.Weights(w.ResetsAt.Add(-window), now)
	shares := make(map[string]float64, len(perSession))
	if total > 0 {
		for id, weight := range perSession {
			shares[id] = float64(w.UsedPercent) * weight / total
		}
	}
	return windowShares{ok: true, usedPercent: w.UsedPercent, perSession: shares}
}

// syncComposerLocked pushes a badge for every live session whose payload
// changed and removes the badge of sessions that are gone.
func (p *Poller) syncComposerLocked() {
	live := make(map[string]bool, len(p.sessions))
	for _, s := range p.sessions {
		live[s.ID] = true
		p.pushComposerLocked(s)
	}
	for id := range p.pushed {
		if live[id] {
			continue
		}
		p.rpc.Notify("ui.state.remove", map[string]any{
			"slot":       composerSlot,
			"id":         composerID,
			"session_id": id,
		})
		delete(p.pushed, id)
		delete(p.showWeek, id)
	}
}

func (p *Poller) pushComposerLocked(s attribution.Session) {
	payload := composerPayload(p.snapshot, s.Transcript, p.showWeek[s.ID])
	data, err := json.Marshal(payload)
	if err != nil || p.pushed[s.ID] == string(data) {
		return
	}
	p.pushed[s.ID] = string(data)
	p.pushSlot(composerSlot, composerID, s.ID, payload)
}

// HandleNotification reacts to a composer badge click: the host forwards it
// as a notification carrying the clicked session's id.
func (p *Poller) HandleNotification(method string, params json.RawMessage) {
	if method != composerCycleMethod {
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(params, &body); err != nil || body.SessionID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sessions {
		if s.ID == body.SessionID {
			p.showWeek[s.ID] = !p.showWeek[s.ID]
			p.pushComposerLocked(s)
			return
		}
	}
}

func (p *Poller) pushSlot(slot, id, sessionID string, payload map[string]any) {
	params := map[string]any{"slot": slot, "id": id, "payload": payload}
	if sessionID != "" {
		params["session_id"] = sessionID
	}
	p.rpc.Notify("ui.state.set", params)
}

// composerPayload is one session's composer button: its estimated share of
// the 5h limit, or of the weekly limit once clicked.
func composerPayload(snap shareSnapshot, transcript string, showWeek bool) map[string]any {
	label, windowName, other, shares := "S", "5h limit", "weekly", snap.session
	if showWeek {
		label, windowName, other, shares = "W", "weekly limit", "5h", snap.week
	}
	payload := map[string]any{"method": composerCycleMethod, "tone": string(usage.ToneNeutral)}
	if !shares.ok {
		payload["label"] = label + " n/a"
		payload["tooltip"] = fmt.Sprintf("No %s estimate yet. Tap for the %s share.", windowName, other)
		return payload
	}
	payload["label"] = label + " ~" + formatShare(shares.perSession[transcript])
	payload["tooltip"] = fmt.Sprintf(
		"This session used an estimated ~%s of the %s (%d%% used overall), attributed from local transcripts. Tap for the %s share.",
		formatShare(shares.perSession[transcript]), windowName, shares.usedPercent, other)
	return payload
}

func formatShare(percent float64) string {
	switch {
	case percent < 0.05:
		return "0%"
	case percent < 10:
		return fmt.Sprintf("%.1f%%", percent)
	default:
		return fmt.Sprintf("%.0f%%", percent)
	}
}
