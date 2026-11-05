package poller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cwrau/aoe-plugin-usage-badge/internal/attribution"
	"github.com/cwrau/aoe-plugin-usage-badge/internal/host"
	"github.com/cwrau/aoe-plugin-usage-badge/internal/usage"
)

// errorResult is what a failed fetch looks like to the poller: an "err"
// payload and no usage windows.
func errorResult(message string, keepLast bool) usage.Result {
	return usage.Result{
		Payload:  map[string]any{"text": "err", "tooltip": message},
		KeepLast: keepLast,
	}
}

// writeTranscript writes one assistant message (output tokens only, so its
// weight is exactly out) as a Claude Code transcript line.
func writeTranscript(t *testing.T, path, messageID string, at time.Time, out int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(
		`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":"claude-sonnet-5-5","usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":%d}}}`+"\n",
		at.UTC().Format(time.RFC3339Nano), messageID, out)
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

type pushed struct {
	method    string
	slot      string
	id        string
	sessionID string
	payload   map[string]any
}

func parsePushes(t *testing.T, out *bytes.Buffer) []pushed {
	t.Helper()
	var all []pushed
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Slot      string         `json:"slot"`
				ID        string         `json:"id"`
				SessionID string         `json:"session_id"`
				Payload   map[string]any `json:"payload"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("bad line %q: %v", line, err)
		}
		all = append(all, pushed{msg.Method, msg.Params.Slot, msg.Params.ID, msg.Params.SessionID, msg.Params.Payload})
	}
	return all
}

func composerPushes(all []pushed) []pushed {
	var out []pushed
	for _, p := range all {
		if p.slot == composerSlot {
			out = append(out, p)
		}
	}
	return out
}

// testPoller wires a poller to a buffer, a fake fetch and a temp transcript
// root: 20% of the 5h session used, 40% of the week, both opened an hour or
// more ago.
func testPoller(t *testing.T) (*Poller, *bytes.Buffer, string) {
	t.Helper()
	var out bytes.Buffer
	var wg sync.WaitGroup
	rpc := host.NewClient(&out)
	p := New(rpc, &wg)

	now := time.Now()
	res := usage.Result{
		Payload:     map[string]any{"text": "S 20%"},
		Session:     usage.Window{UsedPercent: 20, ResetsAt: now.Add(2 * time.Hour), HasReset: true},
		Week:        usage.Window{UsedPercent: 40, ResetsAt: now.Add(3 * 24 * time.Hour), HasReset: true},
		WeekOK:      true,
		HaveWindows: true,
	}
	p.fetch = func(context.Context) usage.Result { return res }

	root := t.TempDir()
	p.index = attribution.NewIndex(root)
	p.loadSessions = func() ([]attribution.Session, error) {
		return []attribution.Session{{ID: "a1", Transcript: "t1"}, {ID: "a2", Transcript: "t2"}}, nil
	}
	recent := now.Add(-30 * time.Minute)
	writeTranscript(t, filepath.Join(root, "proj", "t1.jsonl"), "m1", recent, 300)
	writeTranscript(t, filepath.Join(root, "proj", "t2.jsonl"), "m2", recent, 100)
	return p, &out, root
}

func TestRefreshPushesStatusAndPerSessionComposerBadges(t *testing.T) {
	p, out, _ := testPoller(t)

	p.refresh(context.Background())

	pushes := parsePushes(t, out)
	var status []pushed
	for _, push := range pushes {
		if push.slot == statusSlot {
			status = append(status, push)
		}
	}
	if len(status) != 1 {
		t.Fatalf("got %d status-bar pushes, want 1: %+v", len(status), pushes)
	}

	composer := composerPushes(pushes)
	if len(composer) != 2 {
		t.Fatalf("got %d composer pushes, want 2: %+v", len(composer), composer)
	}
	labels := map[string]string{}
	for _, push := range composer {
		if push.id != composerID || push.method != "ui.state.set" {
			t.Errorf("push = %+v, want ui.state.set on %s", push, composerID)
		}
		if push.payload["method"] != composerCycleMethod {
			t.Errorf("method = %v, want %s", push.payload["method"], composerCycleMethod)
		}
		labels[push.sessionID], _ = push.payload["label"].(string)
	}
	// 20% used, weights 300:100 -> 15% and 5%.
	if labels["a1"] != "S ~15%" || labels["a2"] != "S ~5.0%" {
		t.Errorf("labels = %v, want a1 'S ~15%%' and a2 'S ~5.0%%'", labels)
	}
}

func TestClickCyclesBetweenSessionAndWeeklyShare(t *testing.T) {
	p, out, _ := testPoller(t)
	p.refresh(context.Background())
	out.Reset()

	click := json.RawMessage(`{"session_id":"a1"}`)
	p.HandleNotification(composerCycleMethod, click)
	got := composerPushes(parsePushes(t, out))
	if len(got) != 1 || got[0].sessionID != "a1" {
		t.Fatalf("pushes = %+v, want exactly one for a1", got)
	}
	// 40% of the week, weights 300:100 -> 30%.
	if got[0].payload["label"] != "W ~30%" {
		t.Errorf("label = %v, want 'W ~30%%'", got[0].payload["label"])
	}

	out.Reset()
	p.HandleNotification(composerCycleMethod, click)
	got = composerPushes(parsePushes(t, out))
	if len(got) != 1 || got[0].payload["label"] != "S ~15%" {
		t.Errorf("pushes = %+v, want a second click to wrap back to 'S ~15%%'", got)
	}
}

func TestClickIgnoresUnknownSessionsAndMethods(t *testing.T) {
	p, out, _ := testPoller(t)
	p.refresh(context.Background())
	out.Reset()

	p.HandleNotification(composerCycleMethod, json.RawMessage(`{"session_id":"nope"}`))
	p.HandleNotification("something.else", json.RawMessage(`{"session_id":"a1"}`))
	p.HandleNotification(composerCycleMethod, json.RawMessage(`not json`))

	if out.Len() != 0 {
		t.Errorf("pushed %q, want nothing", out.String())
	}
}

func TestRefreshOnlyPushesChangedComposerBadges(t *testing.T) {
	p, out, _ := testPoller(t)
	p.refresh(context.Background())
	out.Reset()

	p.refresh(context.Background())

	if got := composerPushes(parsePushes(t, out)); len(got) != 0 {
		t.Errorf("got %d composer pushes on an unchanged refresh, want 0: %+v", len(got), got)
	}
}

func TestRefreshRemovesBadgesOfSessionsThatAreGone(t *testing.T) {
	p, out, _ := testPoller(t)
	p.refresh(context.Background())
	out.Reset()

	p.loadSessions = func() ([]attribution.Session, error) {
		return []attribution.Session{{ID: "a1", Transcript: "t1"}}, nil
	}
	p.refresh(context.Background())

	var removed []string
	for _, push := range parsePushes(t, out) {
		if push.method == "ui.state.remove" {
			removed = append(removed, push.sessionID)
		}
	}
	if len(removed) != 1 || removed[0] != "a2" {
		t.Errorf("removed = %v, want [a2]", removed)
	}
}

func TestRefreshWithoutWindowsLeavesComposerBadgesAlone(t *testing.T) {
	p, out, _ := testPoller(t)
	p.fetch = func(context.Context) usage.Result { return errorResult("boom", false) }

	p.refresh(context.Background())

	if got := composerPushes(parsePushes(t, out)); len(got) != 0 {
		t.Errorf("got composer pushes %+v without any usage windows, want none", got)
	}
}

func TestRateLimitedFetchKeepsLastStatusOnceOneWasShown(t *testing.T) {
	p, out, _ := testPoller(t)
	p.refresh(context.Background()) // a real payload is now displayed
	out.Reset()

	p.fetch = func(context.Context) usage.Result { return errorResult("rate limited", true) }
	p.refresh(context.Background())

	if out.Len() != 0 {
		t.Errorf("pushed %q after a rate-limited fetch, want the last state kept", out.String())
	}
}

func TestRateLimitedFirstFetchStillShowsTheError(t *testing.T) {
	p, out, _ := testPoller(t)
	p.fetch = func(context.Context) usage.Result { return errorResult("rate limited", true) }

	p.refresh(context.Background())

	if got := parsePushes(t, out); len(got) != 1 || got[0].payload["text"] != "err" {
		t.Errorf("pushes = %+v, want the err payload when nothing real was shown yet", got)
	}
}

func TestComposerPayloadWithoutEstimate(t *testing.T) {
	got := composerPayload(shareSnapshot{}, "t1", true)
	if got["label"] != "W n/a" {
		t.Errorf("label = %v, want 'W n/a'", got["label"])
	}
}

func TestFormatShare(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0%"},
		{0.04, "0%"},
		{0.4, "0.4%"},
		{9.94, "9.9%"},
		{12.4, "12%"},
		{100, "100%"},
	}
	for _, c := range cases {
		if got := formatShare(c.in); got != c.want {
			t.Errorf("formatShare(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
