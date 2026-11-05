// Status-bar rendering of the usage windows (pace, tone, cycling items) and
// the fallback usage source: a headless `claude -p /usage`, resumed against
// the session id the user configures as a plugin setting. Same technique as
// the systemd agent-limit.service scraper this plugin is modeled on.
package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	sessionIDSettingKey = "session_id"
	configFetchTimeout  = 1 * time.Second

	// usageBadgeGroup is the shared `group` that makes the host collapse the
	// per-window badge items into one chip that cycles on click/tap.
	usageBadgeGroup = "usage"

	// usageCommandTimeout bounds one `claude -p /usage` call. Measured
	// against a long-lived, heavily-used session this routinely takes
	// 12-19s (headless resume replays/loads the session first), so this
	// needs real headroom above that, not just above a "should be instant"
	// assumption -- a too-tight timeout here is what turns a merely slow
	// resume into a spurious "err" badge.
	usageCommandTimeout = 45 * time.Second

	// Claude Code's session and weekly limit windows are fixed durations set
	// by Anthropic -- /usage reports only the reset time, not the window
	// length, so these are hardcoded to derive a window's start (and thus
	// how far into it "now" is) from resetsAt.
	sessionWindow = 5 * time.Hour
	weekWindow    = 7 * 24 * time.Hour

	// paceDangerThreshold is how many percentage points of usage ahead of
	// ideal pace (see idealPercent) tips the badge from "warn" to "danger".
	paceDangerThreshold = 15.0

	// paceOnBand is the +/- points around ideal that read as "on pace"
	// rather than ahead/under.
	paceOnBand = 1.0
)

type tone string

const (
	toneNeutral tone = "neutral"
	toneSuccess tone = "success"
	toneWarn    tone = "warn"
	toneDanger  tone = "danger"
)

var (
	sessionLineRe = regexp.MustCompile(`(?i)^current session:\s*(\d+)% used(?:\s*·\s*resets\s+(.+))?$`)
	weekLineRe    = regexp.MustCompile(`(?i)^current week \(all models\):\s*(\d+)% used(?:\s*·\s*resets\s+(.+))?$`)
	// resetTimeRe splits "Sep 23, 6:39pm (Europe/Berlin)" into its date/time
	// part and IANA zone name; the CLI always appends the zone in parens.
	resetTimeRe = regexp.MustCompile(`^(.+?)\s*\(([^()]+)\)$`)
)

type usageWindow struct {
	usedPercent int
	resetsAt    time.Time
	hasReset    bool
}

func errorPayload(message string) map[string]any {
	return map[string]any{"text": "err", "tooltip": message, "tone": string(toneWarn)}
}

// usageResult is one fetch's outcome: the status-bar payload to show, plus
// the parsed windows (when there are any) that the per-session composer
// badges are derived from.
type usageResult struct {
	payload map[string]any
	// keepLast is true when there's nothing better to show than whatever was
	// displayed before: a rate-limited API with no fallback configured.
	keepLast    bool
	session     usageWindow
	week        usageWindow
	weekOK      bool
	haveWindows bool
}

// usageFromWindows renders the two windows as cycling badge items. The reset
// time is put inline in each item's "text" (not just "tooltip") since
// mobile has no hover to reveal a tooltip.
func usageFromWindows(session, week usageWindow, weekOK bool, now time.Time) usageResult {
	items := windowItems("S", "Session", session, sessionWindow, now)
	if weekOK {
		items = append(items, windowItems("W", "Week (all models)", week, weekWindow, now)...)
	}
	return usageResult{
		payload:     map[string]any{"items": items},
		session:     session,
		week:        week,
		weekOK:      weekOK,
		haveWindows: true,
	}
}

// getUsage fetches usage and renders the status-bar payload. The OAuth usage
// API is tried first; when it can't answer (no credentials, expired token,
// rate limit, any other failure) the optional session-id setting enables the
// slower `claude -p /usage` fallback.
func getUsage(ctx context.Context, rpc *hostRPC) usageResult {
	now := time.Now()
	session, week, weekOK, apiErr := usageFromAPI(ctx, now)
	if apiErr == nil {
		return usageFromWindows(session, week, weekOK, now)
	}

	settingCtx, cancel := context.WithTimeout(ctx, configFetchTimeout)
	sessionID := rpc.getStringSetting(settingCtx, sessionIDSettingKey, "")
	cancel()
	if sessionID == "" {
		switch {
		case errors.Is(apiErr, errNoCredentials):
			return usageResult{payload: map[string]any{
				"text":    "N/A",
				"tooltip": `Sign in to Claude Code on this machine, or set the "Claude Code session ID" plugin setting, to enable usage tracking.`,
				"tone":    string(toneNeutral),
			}}
		case errors.Is(apiErr, errUsageRateLimited):
			return usageResult{payload: errorPayload(apiErr.Error()), keepLast: true}
		default:
			return usageResult{payload: errorPayload(apiErr.Error())}
		}
	}

	return usageFromCLI(ctx, sessionID, now)
}

// usageFromCLI is the fallback path: a headless `claude -p /usage` resumed
// against the configured session.
func usageFromCLI(ctx context.Context, sessionID string, now time.Time) usageResult {
	cmdCtx, cancel := context.WithTimeout(ctx, usageCommandTimeout)
	defer cancel()
	output, err := runClaudeUsage(cmdCtx, sessionID)
	if err != nil {
		return usageResult{payload: errorPayload(err.Error())}
	}

	session, ok := parseUsageWindow(output, sessionLineRe, now)
	if !ok {
		return usageResult{payload: errorPayload("could not parse `claude -p /usage` output")}
	}
	week, weekOK := parseUsageWindow(output, weekLineRe, now)
	return usageFromWindows(session, week, weekOK, now)
}

// windowItems renders one usage window as badge items: the usage itself,
// then (when there's a reset time to pace against) its pace as a separate
// item. All items share usageBadgeGroup, so the host collapses them into a
// single chip that cycles on tap. Each item carries the window's pace tone:
// a chip's color must match the window it displays.
func windowItems(label, tooltipName string, w usageWindow, window time.Duration, now time.Time) []map[string]any {
	ideal, hasIdeal := idealPercent(w.resetsAt, now, window)
	itemTone := string(paceTone(w.usedPercent, ideal, hasIdeal))

	text := fmt.Sprintf("%s %d%%", label, w.usedPercent)
	tooltip := fmt.Sprintf("%s: %d%% used", tooltipName, w.usedPercent)
	if w.hasReset {
		remaining := formatDuration(w.resetsAt.Sub(now))
		text += " · " + remaining
		tooltip += fmt.Sprintf(", resets in %s (%s)", remaining, w.resetsAt.Format("Jan 2, 15:04 MST"))
	}
	items := []map[string]any{{
		"text":    text,
		"tooltip": tooltip,
		"tone":    itemTone,
		"group":   usageBadgeGroup,
	}}

	if pace := paceShort(w.usedPercent, ideal, hasIdeal, window); pace != "" {
		items = append(items, map[string]any{
			"text":    label + " " + pace,
			"tooltip": tooltipName + ": " + paceLabel(w.usedPercent, ideal, hasIdeal, window),
			"tone":    itemTone,
			"group":   usageBadgeGroup,
		})
	}
	return items
}

// idealPercent is where usage "should" be if perfectly paced to reach
// exactly 100% right as the window resets: the fraction of the window's
// fixed duration that has already elapsed, inferred from resetsAt since
// /usage never reports a window's start. false when there's no reset time
// to pace against (e.g. an unparsed reset, or genuinely no active window).
func idealPercent(resetsAt time.Time, now time.Time, window time.Duration) (float64, bool) {
	if resetsAt.IsZero() {
		return 0, false
	}
	elapsed := min(max(window-resetsAt.Sub(now), 0), window)
	return float64(elapsed) / float64(window) * 100, true
}

// paceTone buckets a window's usage against its time-paced ideal: at or
// under pace is "success", moderately ahead is "warn", and far enough ahead
// that the window will likely exhaust before it resets is "danger". Falls
// back to fixed thresholds when there's no reset time to pace against.
func paceTone(usedPercent int, ideal float64, hasIdeal bool) tone {
	// Fully exhausted is always danger, however close to "on pace" the diff
	// against ideal happens to land -- there's no quota left regardless of
	// how well-paced getting there was.
	if usedPercent >= 100 {
		return toneDanger
	}
	if !hasIdeal {
		switch {
		case usedPercent < 50:
			return toneSuccess
		case usedPercent < 80:
			return toneWarn
		default:
			return toneDanger
		}
	}
	switch diff := float64(usedPercent) - ideal; {
	case diff <= 0:
		return toneSuccess
	case diff <= paceDangerThreshold:
		return toneWarn
	default:
		return toneDanger
	}
}

// paceLabel renders the tooltip explanation behind a window's tone, e.g.
// "12pts (36m) ahead of pace" -- without this, the badge color alone doesn't
// say why it's red. The points figure is also translated into a time
// equivalent (points/100 * window) since "12pts" alone doesn't say whether
// that's 36 minutes of a 5h session or half a day of a 7d week. Empty when
// there's no ideal to compare against.
func paceLabel(usedPercent int, ideal float64, hasIdeal bool, window time.Duration) string {
	if !hasIdeal {
		return ""
	}
	diff := float64(usedPercent) - ideal
	pts, dur := paceAmount(diff, window)
	switch {
	case diff <= -paceOnBand:
		return fmt.Sprintf("%s (%s) under pace", pts, dur)
	case diff < paceOnBand:
		return "on pace"
	default:
		return fmt.Sprintf("%s (%s) ahead of pace", pts, dur)
	}
}

// paceShort is paceLabel squeezed for the badge text itself, e.g.
// "+13pts/39m" (ahead) or "-14pts/42m" (under) -- inline because mobile has
// no hover to reveal the tooltip, so it has to stay short enough not to
// truncate the chip. Empty when there's no ideal to compare against.
func paceShort(usedPercent int, ideal float64, hasIdeal bool, window time.Duration) string {
	if !hasIdeal {
		return ""
	}
	diff := float64(usedPercent) - ideal
	pts, dur := paceAmount(diff, window)
	switch {
	case diff <= -paceOnBand:
		return fmt.Sprintf("-%s/%s", pts, dur)
	case diff < paceOnBand:
		return "on pace"
	default:
		return fmt.Sprintf("+%s/%s", pts, dur)
	}
}

// paceAmount is the magnitude of a pace diff as points and as its time
// equivalent within the window (points/100 * window).
func paceAmount(diff float64, window time.Duration) (pts, dur string) {
	magnitude := math.Abs(diff)
	return fmt.Sprintf("%.0fpts", magnitude), formatDuration(time.Duration(magnitude / 100 * float64(window)))
}

func runClaudeUsage(ctx context.Context, sessionID string) (string, error) {
	cmd := exec.CommandContext(ctx, "claude", "--strict-mcp-config", "--resume", sessionID, "-p", "/usage")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("claude -p /usage: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("claude -p /usage: %w", err)
	}
	return string(out), nil
}

func parseUsageWindow(output string, re *regexp.Regexp, now time.Time) (usageWindow, bool) {
	for line := range strings.SplitSeq(output, "\n") {
		m := re.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		percent, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		w := usageWindow{usedPercent: percent}
		if resetsText := strings.TrimSpace(m[2]); resetsText != "" {
			if t, ok := parseResetTime(resetsText, now); ok {
				w.resetsAt = t
				w.hasReset = true
			}
		}
		return w, true
	}
	return usageWindow{}, false
}

// parseResetTime parses the CLI's "Sep 23, 6:39pm (Europe/Berlin)" format.
// The year is never printed, so it's inferred from now and rolled forward a
// year if that placement lands more than a day in the past (the Dec->Jan
// wrap, since every reset window is at most a week out).
func parseResetTime(s string, now time.Time) (time.Time, bool) {
	m := resetTimeRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	datePart, zoneName := m[1], m[2]
	loc, err := time.LoadLocation(zoneName)
	if err != nil {
		loc = time.UTC
	}
	// The CLI omits ":00" for on-the-hour times (e.g. "1am"), so both
	// layouts must be tried.
	for _, layout := range []string{"Jan 2, 3:04pm", "Jan 2, 3pm"} {
		t, err := time.ParseInLocation(layout, datePart, loc)
		if err != nil {
			continue
		}
		t = t.AddDate(now.Year(), 0, 0)
		if t.Before(now.Add(-24 * time.Hour)) {
			t = t.AddDate(1, 0, 0)
		}
		return t, true
	}
	return time.Time{}, false
}

// formatDuration renders a compact, mobile-friendly "time until" (e.g.
// "2h34m", "6d3h") rather than an absolute clock time, so the badge reads
// the same regardless of the viewer's timezone context.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	d = d.Round(time.Minute)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	hours := d / time.Hour
	d -= hours * time.Hour
	minutes := d / time.Minute

	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
