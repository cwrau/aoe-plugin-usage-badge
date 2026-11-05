// Primary usage source: the OAuth usage endpoint Claude Code itself reads
// for /usage, authenticated with the access token Claude Code already keeps
// in its credentials file. Undocumented, so every failure falls back to the
// slower `claude -p /usage` path (see getUsage). The token is only ever
// read, never refreshed: refresh tokens rotate, and refreshing here could
// invalidate the one the real CLI holds.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	usageAPIURL     = "https://api.anthropic.com/api/oauth/usage"
	usageAPITimeout = 10 * time.Second
	versionTimeout  = 5 * time.Second
)

var (
	errNoCredentials    = errors.New("no Claude Code credentials found")
	errTokenExpired     = errors.New("claude code access token expired")
	errUsageRateLimited = errors.New("usage API rate limited")
)

type credentialsFile struct {
	ClaudeAIOAuth struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   int64  `json:"expiresAt"` // unix milliseconds
	} `json:"claudeAiOauth"`
}

// claudeConfigDir honors CLAUDE_CONFIG_DIR like the CLI does.
func claudeConfigDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

func credentialsPath() (string, error) {
	dir, err := claudeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".credentials.json"), nil
}

// readAccessToken returns the stored access token, or errNoCredentials /
// errTokenExpired when there's nothing usable.
func readAccessToken(path string, now time.Time) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errNoCredentials
		}
		return "", fmt.Errorf("read credentials: %w", err)
	}
	var creds credentialsFile
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("parse credentials: %w", err)
	}
	oauth := creds.ClaudeAIOAuth
	if oauth.AccessToken == "" {
		return "", errNoCredentials
	}
	if oauth.ExpiresAt != 0 && now.UnixMilli() >= oauth.ExpiresAt {
		return "", errTokenExpired
	}
	return oauth.AccessToken, nil
}

var (
	claudeVersionOnce sync.Once
	claudeVersionStr  string
)

// claudeVersion is looked up once: the endpoint rejects requests that don't
// look like Claude Code, so the user agent carries the installed version.
func claudeVersion(ctx context.Context) string {
	claudeVersionOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, versionTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "claude", "--version").Output()
		if err != nil {
			return
		}
		if fields := strings.Fields(string(out)); len(fields) > 0 {
			claudeVersionStr = fields[0]
		}
	})
	return claudeVersionStr
}

type apiWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type apiUsage struct {
	FiveHour *apiWindow `json:"five_hour"`
	SevenDay *apiWindow `json:"seven_day"`
}

func (w *apiWindow) toUsageWindow() usageWindow {
	out := usageWindow{usedPercent: int(math.Round(w.Utilization))}
	if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
		out.resetsAt = t.Local()
		out.hasReset = true
	}
	return out
}

// fetchUsageAPI asks the usage endpoint for the session (five_hour) and
// week (seven_day) windows. weekOK is false when the response has no
// seven_day window.
func fetchUsageAPI(ctx context.Context, url, token, userAgent string) (session, week usageWindow, weekOK bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, usageAPITimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return usageWindow{}, usageWindow{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return usageWindow{}, usageWindow{}, false, fmt.Errorf("usage API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return usageWindow{}, usageWindow{}, false, errUsageRateLimited
	case resp.StatusCode != http.StatusOK:
		return usageWindow{}, usageWindow{}, false, fmt.Errorf("usage API: HTTP %d", resp.StatusCode)
	}

	var usage apiUsage
	if err := json.NewDecoder(resp.Body).Decode(&usage); err != nil {
		return usageWindow{}, usageWindow{}, false, fmt.Errorf("usage API: decode: %w", err)
	}
	if usage.FiveHour == nil {
		return usageWindow{}, usageWindow{}, false, errors.New("usage API: response has no five_hour window")
	}
	session = usage.FiveHour.toUsageWindow()
	if usage.SevenDay != nil {
		week, weekOK = usage.SevenDay.toUsageWindow(), true
	}
	return session, week, weekOK, nil
}

// usageFromAPI is the full primary path: credentials, user agent, request.
func usageFromAPI(ctx context.Context, now time.Time) (session, week usageWindow, weekOK bool, err error) {
	path, err := credentialsPath()
	if err != nil {
		return usageWindow{}, usageWindow{}, false, err
	}
	token, err := readAccessToken(path, now)
	if err != nil {
		return usageWindow{}, usageWindow{}, false, err
	}
	version := claudeVersion(ctx)
	if version == "" {
		return usageWindow{}, usageWindow{}, false, errors.New("could not determine the installed claude version")
	}
	return fetchUsageAPI(ctx, usageAPIURL, token, "claude-code/"+version)
}
