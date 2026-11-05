package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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
// payload yet to keep displaying -- see usagePoller.
func pendingPayload() map[string]any {
	return map[string]any{"text": "…", "tooltip": "Fetching Claude usage...", "tone": string(toneNeutral)}
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

// usagePoller refreshes the status-bar payload and the per-session composer
// badges in the background so a slow fetch (`claude -p /usage` measured
// 12-19s against a long-lived session, bounded at usageCommandTimeout) never
// blocks the badge: whatever was last pushed stays displayed until a fetch
// actually completes -- successfully or as a timed-out "err". Only the very
// first fetch, before any payload exists to keep showing, pushes the
// "pending" placeholder.
type usagePoller struct {
	rpc *hostRPC
	wg  *sync.WaitGroup

	fetch        func(context.Context) usageResult
	loadSessions func() ([]aoeSession, error)
	index        *transcriptIndex

	mu       sync.Mutex
	fetching bool
	hasReal  bool // a real (non-pending) payload has been pushed, so keepLast can skip a push
	snapshot shareSnapshot
	sessions []aoeSession
	showWeek map[string]bool   // AoE session id -> its composer badge shows the weekly share
	pushed   map[string]string // AoE session id -> last composer payload pushed (JSON)
}

func newUsagePoller(rpc *hostRPC, wg *sync.WaitGroup) *usagePoller {
	p := &usagePoller{
		rpc:      rpc,
		wg:       wg,
		showWeek: map[string]bool{},
		pushed:   map[string]string{},
	}
	p.fetch = func(ctx context.Context) usageResult { return getUsage(ctx, rpc) }
	p.loadSessions = func() ([]aoeSession, error) {
		dir, err := aoeConfigDir()
		if err != nil {
			return nil, err
		}
		return loadLiveSessions(dir)
	}
	root := ""
	if dir, err := claudeConfigDir(); err == nil {
		root = filepath.Join(dir, "projects")
	}
	p.index = newTranscriptIndex(root)
	return p
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "usage-badge-worker: "+format+"\n", args...)
}

// run pushes the pending placeholder, starts the first fetch, then triggers
// a refresh every pollInterval until stop is closed. It uses the caller's
// wg for its own fetch goroutines too, so main's shutdown wait also covers
// whichever fetch is still in flight, not just this loop.
func (p *usagePoller) run(ctx context.Context, stop <-chan struct{}) {
	p.pushSlot(uiSlot, uiID, "", pendingPayload())
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
func (p *usagePoller) refreshAsync(ctx context.Context) {
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
func (p *usagePoller) refresh(ctx context.Context) {
	res := p.fetch(ctx)

	p.mu.Lock()
	skip := res.keepLast && p.hasReal
	p.hasReal = p.hasReal || !res.keepLast
	p.mu.Unlock()
	if !skip {
		p.pushSlot(uiSlot, uiID, "", res.payload)
	}

	if res.haveWindows {
		p.updateShares(ctx, res)
	}
}

func (p *usagePoller) updateShares(ctx context.Context, res usageResult) {
	now := time.Now()
	sessions, err := p.loadSessions()
	if err != nil {
		logf("loading AoE sessions: %v", err)
		return
	}
	if err := p.index.refresh(ctx, now.Add(-weekWindow-time.Hour)); err != nil {
		logf("indexing transcripts: %v", err)
		return
	}
	snap := shareSnapshot{session: p.windowShares(res.session, sessionWindow, now)}
	if res.weekOK {
		snap.week = p.windowShares(res.week, weekWindow, now)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot = snap
	p.sessions = sessions
	p.syncComposerLocked()
}

// windowShares attributes a window's utilization to Claude sessions by their
// weighted tokens since the window opened (its reset time minus its length).
func (p *usagePoller) windowShares(w usageWindow, window time.Duration, now time.Time) windowShares {
	if !w.hasReset {
		return windowShares{}
	}
	perSession, total := p.index.weights(w.resetsAt.Add(-window), now)
	shares := make(map[string]float64, len(perSession))
	if total > 0 {
		for id, weight := range perSession {
			shares[id] = float64(w.usedPercent) * weight / total
		}
	}
	return windowShares{ok: true, usedPercent: w.usedPercent, perSession: shares}
}

// syncComposerLocked pushes a badge for every live session whose payload
// changed and removes the badge of sessions that are gone.
func (p *usagePoller) syncComposerLocked() {
	live := make(map[string]bool, len(p.sessions))
	for _, s := range p.sessions {
		live[s.id] = true
		p.pushComposerLocked(s)
	}
	for id := range p.pushed {
		if live[id] {
			continue
		}
		p.rpc.writeMessage(newNotification("ui.state.remove", map[string]any{
			"slot":       composerSlot,
			"id":         composerID,
			"session_id": id,
		}))
		delete(p.pushed, id)
		delete(p.showWeek, id)
	}
}

func (p *usagePoller) pushComposerLocked(s aoeSession) {
	payload := composerPayload(p.snapshot, s.transcript, p.showWeek[s.id])
	data, err := json.Marshal(payload)
	if err != nil || p.pushed[s.id] == string(data) {
		return
	}
	p.pushed[s.id] = string(data)
	p.pushSlot(composerSlot, composerID, s.id, payload)
}

// handleNotification reacts to a composer badge click: the host forwards it
// as a notification carrying the clicked session's id.
func (p *usagePoller) handleNotification(method string, params json.RawMessage) {
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
		if s.id == body.SessionID {
			p.showWeek[s.id] = !p.showWeek[s.id]
			p.pushComposerLocked(s)
			return
		}
	}
}

func (p *usagePoller) pushSlot(slot, id, sessionID string, payload map[string]any) {
	params := map[string]any{"slot": slot, "id": id, "payload": payload}
	if sessionID != "" {
		params["session_id"] = sessionID
	}
	p.rpc.writeMessage(newNotification("ui.state.set", params))
}

// composerPayload is one session's composer button: its estimated share of
// the 5h limit, or of the weekly limit once clicked.
func composerPayload(snap shareSnapshot, transcript string, showWeek bool) map[string]any {
	label, windowName, other, shares := "S", "5h limit", "weekly", snap.session
	if showWeek {
		label, windowName, other, shares = "W", "weekly limit", "5h", snap.week
	}
	payload := map[string]any{"method": composerCycleMethod, "tone": string(toneNeutral)}
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
