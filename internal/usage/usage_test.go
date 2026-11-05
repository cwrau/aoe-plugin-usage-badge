package usage

import (
	"strings"
	"testing"
	"time"
)

// sampleUsageOutput is a real `claude -p /usage` transcript, captured to
// pin the parser against the CLI's actual wording rather than a guess.
const sampleUsageOutput = `You are currently using your subscription to power your Claude Code usage

Current session: 51% used · resets Sep 23, 6:39pm (Europe/Berlin)
Current week (all models): 15% used · resets Sep 30, 12:59am (Europe/Berlin)
Current week (Fable): 0% used · resets Sep 30, 1am (Europe/Berlin)

What's contributing to your limits usage?
`

var testNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func TestParseUsageWindowSession(t *testing.T) {
	w, ok := parseUsageWindow(sampleUsageOutput, sessionLineRe, testNow)
	if !ok {
		t.Fatal("session line not found")
	}
	if w.UsedPercent != 51 {
		t.Errorf("usedPercent = %d, want 51", w.UsedPercent)
	}
	if !w.HasReset {
		t.Fatal("hasReset = false, want true")
	}
}

func TestParseUsageWindowWeekIgnoresPerModelLines(t *testing.T) {
	w, ok := parseUsageWindow(sampleUsageOutput, weekLineRe, testNow)
	if !ok {
		t.Fatal("week (all models) line not found")
	}
	// The output also has a "Current week (Fable): 0% used" line right
	// after it; weekLineRe must not match that one instead.
	if w.UsedPercent != 15 {
		t.Errorf("usedPercent = %d, want 15 (not the per-model 0%%)", w.UsedPercent)
	}
}

func TestParseUsageWindowNoMatch(t *testing.T) {
	if _, ok := parseUsageWindow("nothing usage-shaped here", sessionLineRe, testNow); ok {
		t.Error("got ok=true for output with no matching line")
	}
}

func TestParseResetTimeWithMinutes(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	got, ok := parseResetTime("Sep 23, 6:39pm (Europe/Berlin)", now)
	if !ok {
		t.Fatal("parse failed")
	}
	loc, _ := time.LoadLocation("Europe/Berlin")
	want := time.Date(2026, 9, 23, 18, 39, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseResetTimeOnTheHour(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	got, ok := parseResetTime("Sep 30, 1am (Europe/Berlin)", now)
	if !ok {
		t.Fatal("parse failed")
	}
	loc, _ := time.LoadLocation("Europe/Berlin")
	want := time.Date(2026, 9, 30, 1, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseResetTimeRollsYearForwardAcrossDecemberWrap(t *testing.T) {
	now := time.Date(2026, 12, 30, 12, 0, 0, 0, time.UTC)
	got, ok := parseResetTime("Jan 2, 3pm (Europe/Berlin)", now)
	if !ok {
		t.Fatal("parse failed")
	}
	if got.Year() != 2027 {
		t.Errorf("year = %d, want 2027 (rolled forward past the Dec->Jan wrap)", got.Year())
	}
}

func TestIdealPercentHalfwayThroughWindow(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	resetsAt := now.Add(SessionWindow / 2) // half of the 5h session window left
	ideal, ok := idealPercent(resetsAt, now, SessionWindow)
	if !ok {
		t.Fatal("idealPercent returned ok=false")
	}
	if ideal != 50 {
		t.Errorf("ideal = %v, want 50", ideal)
	}
}

func TestIdealPercentNoResetTime(t *testing.T) {
	if _, ok := idealPercent(time.Time{}, time.Now(), SessionWindow); ok {
		t.Error("got ok=true with a zero resetsAt")
	}
}

func TestIdealPercentClampsPastWindowEdges(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

	// resetsAt already passed: elapsed would exceed the window, must clamp to 100.
	ideal, ok := idealPercent(now.Add(-time.Hour), now, SessionWindow)
	if !ok || ideal != 100 {
		t.Errorf("ideal = %v, ok = %v, want 100, true", ideal, ok)
	}

	// resetsAt further out than a full window: elapsed would go negative, must clamp to 0.
	ideal, ok = idealPercent(now.Add(SessionWindow*2), now, SessionWindow)
	if !ok || ideal != 0 {
		t.Errorf("ideal = %v, ok = %v, want 0, true", ideal, ok)
	}
}

func TestPaceToneAgainstIdeal(t *testing.T) {
	cases := []struct {
		name        string
		usedPercent int
		ideal       float64
		want        tone
	}{
		{"at pace", 50, 50, toneSuccess},
		{"under pace", 30, 50, toneSuccess},
		{"moderately ahead", 60, 50, toneWarn},
		{"far ahead", 80, 50, toneDanger},
		// 100% used, 39m left in a 5h window (87% elapsed) is only 13pts
		// ahead of pace -- comfortably under paceDangerThreshold -- but the
		// quota is fully exhausted regardless, which must always be danger.
		{"exhausted despite being near pace", 100, 87, toneDanger},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := paceTone(c.usedPercent, c.ideal, true); got != c.want {
				t.Errorf("paceTone(%d, %v) = %v, want %v", c.usedPercent, c.ideal, got, c.want)
			}
		})
	}
}

func TestPaceToneFallsBackToFixedThresholdsWithoutIdeal(t *testing.T) {
	if got := paceTone(90, 0, false); got != toneDanger {
		t.Errorf("got %v, want danger", got)
	}
	if got := paceTone(10, 0, false); got != toneSuccess {
		t.Errorf("got %v, want success", got)
	}
}

func TestPaceLabelIncludesTimeEquivalent(t *testing.T) {
	// 13pts of a 5h session window = 39m.
	if got, want := paceLabel(100, 87, true, SessionWindow), "13pts (39m) ahead of pace"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// 7pts under pace on the same window = 21m.
	if got, want := paceLabel(13, 20, true, SessionWindow), "7pts (21m) under pace"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPaceShort(t *testing.T) {
	cases := []struct {
		name        string
		usedPercent int
		ideal       float64
		hasIdeal    bool
		want        string
	}{
		{"ahead", 100, 87, true, "+13pts/39m"},
		{"under", 13, 20, true, "-7pts/21m"},
		{"on pace", 50, 50, true, "on pace"},
		{"no ideal", 50, 0, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := paceShort(c.usedPercent, c.ideal, c.hasIdeal, SessionWindow); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestPaceLabelOnPaceOmitsTimeEquivalent(t *testing.T) {
	if got, want := paceLabel(50, 50, true, SessionWindow), "on pace"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPaceLabelEmptyWithoutIdeal(t *testing.T) {
	if got := paceLabel(50, 0, false, SessionWindow); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestWindowItemsUsageThenPace(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	// 39m left of a 5h window = 87% elapsed; 100% used is 13pts ahead.
	w := Window{UsedPercent: 100, ResetsAt: now.Add(39 * time.Minute), HasReset: true}

	items := windowItems("S", "Session", w, SessionWindow, now)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2 (usage, pace): %+v", len(items), items)
	}

	if got, want := items[0]["text"], "S 100% · 39m"; got != want {
		t.Errorf("usage text = %v, want %v", got, want)
	}
	if got, want := items[1]["text"], "S +13pts/39m"; got != want {
		t.Errorf("pace text = %v, want %v", got, want)
	}
	if tooltip, _ := items[1]["tooltip"].(string); !strings.Contains(tooltip, "13pts (39m) ahead of pace") {
		t.Errorf("pace tooltip = %q, want it to contain the pace label", tooltip)
	}

	// The host rejects unknown BadgeItem fields (deny_unknown_fields).
	allowed := map[string]bool{"text": true, "icon": true, "tone": true, "href": true, "tooltip": true, "group": true}
	for _, item := range items {
		if got, want := item["tone"], string(toneDanger); got != want {
			t.Errorf("tone = %v, want %v (exhausted)", got, want)
		}
		if got, want := item["group"], usageBadgeGroup; got != want {
			t.Errorf("group = %v, want %v", got, want)
		}
		for key := range item {
			if !allowed[key] {
				t.Errorf("item has key %q the host's BadgeItem does not accept", key)
			}
		}
	}
}

func TestWindowItemsWithoutResetHasNoPaceItem(t *testing.T) {
	items := windowItems("W", "Week (all models)", Window{UsedPercent: 12}, WeekWindow, testNow)
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 (no reset time to pace against): %+v", len(items), items)
	}
	if got, want := items[0]["text"], "W 12%"; got != want {
		t.Errorf("text = %v, want %v", got, want)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "now"},
		{-time.Minute, "now"},
		{45 * time.Minute, "45m"},
		{2*time.Hour + 34*time.Minute, "2h34m"},
		{6*24*time.Hour + 3*time.Hour, "6d3h"},
	}
	for _, c := range cases {
		if got := formatDuration(c.in); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
