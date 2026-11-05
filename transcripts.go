// Per-session usage attribution. Anthropic exposes only the account-wide
// limit utilization, so a session's share is estimated from the token usage
// Claude Code records in its local transcripts: a session's share of a window
// is the utilization times the session's weighted tokens over all local
// weighted tokens in that window. Dividing by the local total keeps the
// estimate self-calibrating, so no absolute %-per-token constant can rot.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Token weights relative to one output token, fit against the recorded limit
// history (weekly and 5h series, Sep 22 to Oct 30 2026; held-out R² 0.96
// weekly, 0.74 for the noisier 5h series). Cache reads weigh far less than
// the list price ratio (0.02) suggests and long-context sessions are mostly
// cache reads, so the list-price ratios overweighted them. Input tokens are
// too few to pin down, so that weight is a plain guess.
//
// Opus tokens cost a bit more than sonnet tokens: opus-to-sonnet came out at
// 1.1-1.3 across both series and estimators (95% intervals about 1.0-1.4),
// far below the list-price gap, so a flat 1.2 is used. Other models weigh 1.
const (
	weightInput      = 0.2
	weightCacheWrite = 0.15
	weightCacheRead  = 0.005
	weightOpus       = 1.2
)

func modelFactor(model string) float64 {
	if strings.Contains(strings.ToLower(model), "opus") {
		return weightOpus
	}
	return 1
}

type transcriptUsage struct {
	Input      int `json:"input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	Output     int `json:"output_tokens"`
}

func (u transcriptUsage) weight() float64 {
	return float64(u.Output) +
		weightInput*float64(u.Input) +
		weightCacheWrite*float64(u.CacheWrite) +
		weightCacheRead*float64(u.CacheRead)
}

type transcriptLine struct {
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID    string           `json:"id"`
		Model string           `json:"model"`
		Usage *transcriptUsage `json:"usage"`
	} `json:"message"`
}

type usageRecord struct {
	at      time.Time
	weight  float64
	session string
}

type fileState struct {
	offset int64
}

// transcriptIndex incrementally indexes the usage in Claude Code's JSONL
// transcripts. Files are append-only, so each refresh only reads what was
// added since the previous one.
type transcriptIndex struct {
	root    string
	files   map[string]*fileState
	records map[string]usageRecord // by message id
}

func newTranscriptIndex(root string) *transcriptIndex {
	return &transcriptIndex{
		root:    root,
		files:   map[string]*fileState{},
		records: map[string]usageRecord{},
	}
}

// transcriptSession names the Claude session a transcript file belongs to:
// <project>/<uuid>.jsonl for the main transcript, <project>/<uuid>/... for
// its subagent transcripts.
func transcriptSession(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 {
		return "", false
	}
	return strings.TrimSuffix(parts[1], ".jsonl"), true
}

// refresh ingests new transcript content and drops records older than
// cutoff, which also bounds which files are worth reading.
func (ix *transcriptIndex) refresh(ctx context.Context, cutoff time.Time) error {
	seen := map[string]bool{}
	err := filepath.WalkDir(ix.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		session, ok := transcriptSession(ix.root, path)
		if !ok {
			return nil
		}
		seen[path] = true
		st := ix.files[path]
		if st == nil {
			st = &fileState{}
			ix.files[path] = st
		}
		if info.Size() < st.offset {
			st.offset = 0
		}
		if info.Size() == st.offset {
			return nil
		}
		return ix.ingest(path, session, st)
	})
	if err != nil {
		return err
	}
	for path := range ix.files {
		if !seen[path] {
			delete(ix.files, path)
		}
	}
	for id, rec := range ix.records {
		if rec.at.Before(cutoff) {
			delete(ix.records, id)
		}
	}
	return nil
}

func (ix *transcriptIndex) ingest(path, session string, st *fileState) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				// A trailing line without its newline is still being written:
				// leave it unconsumed so the next refresh re-reads it whole.
				return nil
			}
			return err
		}
		st.offset += int64(len(line))
		ix.addLine(line, session)
	}
}

var usageMarker = []byte(`"usage"`)

func (ix *transcriptIndex) addLine(line []byte, session string) {
	if !bytes.Contains(line, usageMarker) {
		return
	}
	var tl transcriptLine
	if err := json.Unmarshal(line, &tl); err != nil {
		return
	}
	if tl.Message.Usage == nil || tl.Message.ID == "" {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, tl.Timestamp)
	if err != nil {
		return
	}
	rec := usageRecord{
		at:      at,
		weight:  tl.Message.Usage.weight() * modelFactor(tl.Message.Model),
		session: session,
	}
	if old, ok := ix.records[tl.Message.ID]; ok {
		// A forked or resumed session copies its ancestor's history: the
		// first session seen keeps a message. Within one session a message
		// is written repeatedly while it streams, and the last write is the
		// one with the final (largest) token counts.
		if old.session != session || old.weight >= rec.weight {
			return
		}
	}
	ix.records[tl.Message.ID] = rec
}

// weights sums the weighted tokens per Claude session over [from, to], and
// across all of them.
func (ix *transcriptIndex) weights(from, to time.Time) (perSession map[string]float64, total float64) {
	perSession = map[string]float64{}
	for _, rec := range ix.records {
		if rec.at.Before(from) || rec.at.After(to) {
			continue
		}
		perSession[rec.session] += rec.weight
		total += rec.weight
	}
	return perSession, total
}
