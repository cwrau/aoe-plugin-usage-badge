package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// usageLine is one assistant message line as Claude Code writes it, reduced
// to the fields the index reads.
func usageLine(id string, at time.Time, in, cacheWrite, cacheRead, out int) string {
	return usageLineForModel("claude-sonnet-5-5", id, at, in, cacheWrite, cacheRead, out)
}

func usageLineForModel(model, id string, at time.Time, in, cacheWrite, cacheRead, out int) string {
	return fmt.Sprintf(
		`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d}}}`+"\n",
		at.UTC().Format(time.RFC3339Nano), id, model, in, cacheWrite, cacheRead, out)
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path string, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func refreshIndex(t *testing.T, ix *transcriptIndex, cutoff time.Time) {
	t.Helper()
	if err := ix.refresh(context.Background(), cutoff); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

func TestMessageWeight(t *testing.T) {
	u := transcriptUsage{Input: 100, CacheWrite: 100, CacheRead: 100, Output: 100}
	// out 100 + 0.2*100 + 0.15*100 + 0.005*100
	if got, want := u.weight(), 135.5; got != want {
		t.Errorf("weight = %v, want %v", got, want)
	}
}

func TestModelFactor(t *testing.T) {
	cases := map[string]float64{
		"claude-opus-5-5":           weightOpus,
		"claude-opus-4-7":           weightOpus,
		"Claude-OPUS-5":             weightOpus,
		"claude-sonnet-5-5":         1,
		"claude-haiku-4-5-20251001": 1,
		"<synthetic>":               1,
		"":                          1,
	}
	for model, want := range cases {
		if got := modelFactor(model); got != want {
			t.Errorf("modelFactor(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestIndexWeighsOpusMessagesByTheOpusFactor(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	at := now.Add(-10 * time.Minute)
	writeFile(t, filepath.Join(root, "proj", "s1.jsonl"),
		usageLineForModel("claude-opus-5-5", "m1", at, 0, 0, 0, 100)+usageLineForModel("claude-sonnet-5-5", "m2", at, 0, 0, 0, 100))

	ix := newTranscriptIndex(root)
	refreshIndex(t, ix, now.Add(-time.Hour))

	_, total := ix.weights(now.Add(-time.Hour), now)
	if want := 100*weightOpus + 100; total != want {
		t.Errorf("total = %v, want %v (opus message scaled, sonnet not)", total, want)
	}
}

func TestTranscriptSessionFromPath(t *testing.T) {
	root := "/r/projects"
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"/r/projects/-home-x/abc.jsonl", "abc", true},
		{"/r/projects/-home-x/abc/subagents/agent-1.jsonl", "abc", true},
		{"/r/projects/loose.jsonl", "", false},
	}
	for _, c := range cases {
		got, ok := transcriptSession(root, c.path)
		if got != c.want || ok != c.ok {
			t.Errorf("transcriptSession(%q) = %q, %v; want %q, %v", c.path, got, ok, c.want, c.ok)
		}
	}
}

func TestIndexWeightsPerSessionIncludingSubagents(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	recent := now.Add(-30 * time.Minute)
	writeFile(t, filepath.Join(root, "proj", "s1.jsonl"), usageLine("m1", recent, 0, 0, 0, 100)+usageLine("m2", recent, 0, 0, 0, 50))
	writeFile(t, filepath.Join(root, "proj", "s1", "subagents", "agent-a.jsonl"), usageLine("m3", recent, 0, 0, 0, 25))
	writeFile(t, filepath.Join(root, "proj", "s2.jsonl"), usageLine("m4", recent, 0, 0, 0, 25))

	ix := newTranscriptIndex(root)
	refreshIndex(t, ix, now.Add(-time.Hour))

	per, total := ix.weights(now.Add(-time.Hour), now)
	if per["s1"] != 175 || per["s2"] != 25 || total != 200 {
		t.Errorf("per = %v, total = %v; want s1=175 (incl. subagent), s2=25, total=200", per, total)
	}
}

func TestIndexWeightsRespectsWindow(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeFile(t, filepath.Join(root, "proj", "s1.jsonl"),
		usageLine("old", now.Add(-4*time.Hour), 0, 0, 0, 100)+usageLine("new", now.Add(-10*time.Minute), 0, 0, 0, 10))

	ix := newTranscriptIndex(root)
	refreshIndex(t, ix, now.Add(-24*time.Hour))

	_, total := ix.weights(now.Add(-time.Hour), now)
	if total != 10 {
		t.Errorf("total = %v, want 10 (the 4h-old message is outside a 1h window)", total)
	}
}

func TestIndexStreamedMessageKeepsFinalCounts(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	at := now.Add(-10 * time.Minute)
	writeFile(t, filepath.Join(root, "proj", "s1.jsonl"),
		usageLine("m1", at, 0, 0, 0, 10)+usageLine("m1", at, 0, 0, 0, 90)+usageLine("m1", at, 0, 0, 0, 40))

	ix := newTranscriptIndex(root)
	refreshIndex(t, ix, now.Add(-time.Hour))

	if per, _ := ix.weights(now.Add(-time.Hour), now); per["s1"] != 90 {
		t.Errorf("weight = %v, want 90 (the largest write of the streamed message)", per["s1"])
	}
}

func TestIndexForkedHistoryCountsOnce(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	at := now.Add(-10 * time.Minute)
	// A forked session's transcript starts with a copy of its ancestor's history.
	writeFile(t, filepath.Join(root, "proj", "aaa-original.jsonl"), usageLine("shared", at, 0, 0, 0, 100))
	writeFile(t, filepath.Join(root, "proj", "bbb-fork.jsonl"), usageLine("shared", at, 0, 0, 0, 100)+usageLine("own", at, 0, 0, 0, 7))

	ix := newTranscriptIndex(root)
	refreshIndex(t, ix, now.Add(-time.Hour))

	per, total := ix.weights(now.Add(-time.Hour), now)
	if total != 107 || per["aaa-original"] != 100 || per["bbb-fork"] != 7 {
		t.Errorf("per = %v, total = %v; want the shared message counted once for the first session", per, total)
	}
}

func TestIndexIncrementalRefreshReadsOnlyAppendedLines(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	path := filepath.Join(root, "proj", "s1.jsonl")
	writeFile(t, path, usageLine("m1", now.Add(-20*time.Minute), 0, 0, 0, 10))

	ix := newTranscriptIndex(root)
	refreshIndex(t, ix, now.Add(-time.Hour))
	if _, total := ix.weights(now.Add(-time.Hour), now); total != 10 {
		t.Fatalf("after first refresh total = %v, want 10", total)
	}

	// A line still being written has no newline yet and must not be consumed.
	partial := usageLine("m2", now.Add(-10*time.Minute), 0, 0, 0, 5)
	appendFile(t, path, partial[:len(partial)-1])
	refreshIndex(t, ix, now.Add(-time.Hour))
	if _, total := ix.weights(now.Add(-time.Hour), now); total != 10 {
		t.Errorf("with a partial line total = %v, want it still 10", total)
	}

	appendFile(t, path, "\n")
	refreshIndex(t, ix, now.Add(-time.Hour))
	if _, total := ix.weights(now.Add(-time.Hour), now); total != 15 {
		t.Errorf("after the line completed total = %v, want 15", total)
	}
}

func TestIndexPrunesRecordsOlderThanCutoff(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	writeFile(t, filepath.Join(root, "proj", "s1.jsonl"), usageLine("m1", now.Add(-3*time.Hour), 0, 0, 0, 10))

	ix := newTranscriptIndex(root)
	refreshIndex(t, ix, now.Add(-4*time.Hour))
	if len(ix.records) != 1 {
		t.Fatalf("records = %d, want 1", len(ix.records))
	}
	refreshIndex(t, ix, now.Add(-time.Hour))
	if len(ix.records) != 0 {
		t.Errorf("records = %d, want 0 after the cutoff moved past the message", len(ix.records))
	}
}

func TestIndexMissingRootIsEmpty(t *testing.T) {
	ix := newTranscriptIndex(filepath.Join(t.TempDir(), "does-not-exist"))
	refreshIndex(t, ix, time.Now().Add(-time.Hour))
	if _, total := ix.weights(time.Now().Add(-time.Hour), time.Now()); total != 0 {
		t.Errorf("total = %v, want 0", total)
	}
}
