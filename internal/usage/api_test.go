package usage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// sampleAPIResponse mirrors the fields of a real usage response the worker
// reads (trimmed: the live payload has many more, all ignored).
const sampleAPIResponse = `{
  "five_hour": {"utilization": 18.4, "resets_at": "2026-10-20T13:19:59.863457+00:00"},
  "seven_day": {"utilization": 2.0, "resets_at": "2026-10-20T22:59:59.863474+00:00"},
  "seven_day_opus": null
}`

func usageServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer tok"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("User-Agent"), "claude-code/9.9.9"; got != want {
			t.Errorf("User-Agent = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("anthropic-beta"), "oauth-2025-04-20"; got != want {
			t.Errorf("anthropic-beta = %q, want %q", got, want)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchUsageAPIParsesWindows(t *testing.T) {
	srv := usageServer(t, http.StatusOK, sampleAPIResponse)

	session, week, weekOK, err := fetchUsageAPI(context.Background(), srv.URL, "tok", "claude-code/9.9.9")
	if err != nil {
		t.Fatalf("fetchUsageAPI: %v", err)
	}
	if session.UsedPercent != 18 {
		t.Errorf("session usedPercent = %d, want 18 (18.4 rounded)", session.UsedPercent)
	}
	wantReset := time.Date(2026, 10, 20, 13, 19, 59, 863457000, time.UTC)
	if !session.HasReset || !session.ResetsAt.Equal(wantReset) {
		t.Errorf("session resetsAt = %v (hasReset %v), want %v", session.ResetsAt, session.HasReset, wantReset)
	}
	if !weekOK || week.UsedPercent != 2 {
		t.Errorf("week = %+v (ok %v), want usedPercent 2", week, weekOK)
	}
}

func TestFetchUsageAPIWithoutSevenDay(t *testing.T) {
	srv := usageServer(t, http.StatusOK, `{"five_hour": {"utilization": 5, "resets_at": "2026-10-20T13:00:00+00:00"}}`)

	_, _, weekOK, err := fetchUsageAPI(context.Background(), srv.URL, "tok", "claude-code/9.9.9")
	if err != nil {
		t.Fatalf("fetchUsageAPI: %v", err)
	}
	if weekOK {
		t.Error("weekOK = true, want false when seven_day is absent")
	}
}

func TestFetchUsageAPIRateLimited(t *testing.T) {
	srv := usageServer(t, http.StatusTooManyRequests, `{"error": {"type": "rate_limit_error"}}`)

	_, _, _, err := fetchUsageAPI(context.Background(), srv.URL, "tok", "claude-code/9.9.9")
	if !errors.Is(err, errUsageRateLimited) {
		t.Errorf("err = %v, want errUsageRateLimited", err)
	}
}

func TestFetchUsageAPIOtherStatusIsError(t *testing.T) {
	srv := usageServer(t, http.StatusUnauthorized, `{}`)

	_, _, _, err := fetchUsageAPI(context.Background(), srv.URL, "tok", "claude-code/9.9.9")
	if err == nil || errors.Is(err, errUsageRateLimited) {
		t.Errorf("err = %v, want a non-rate-limit error", err)
	}
}

func TestFetchUsageAPIMissingFiveHourIsError(t *testing.T) {
	srv := usageServer(t, http.StatusOK, `{"seven_day": {"utilization": 2, "resets_at": "2026-10-20T22:59:59+00:00"}}`)

	if _, _, _, err := fetchUsageAPI(context.Background(), srv.URL, "tok", "claude-code/9.9.9"); err == nil {
		t.Error("err = nil, want an error when five_hour is absent")
	}
}

func writeCredentials(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadAccessToken(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	cases := []struct {
		name    string
		content string
		want    string
		wantErr error
	}{
		{"valid", `{"claudeAiOauth": {"accessToken": "tok", "expiresAt": 2000000}}`, "tok", nil},
		{"expired", `{"claudeAiOauth": {"accessToken": "tok", "expiresAt": 500000}}`, "", errTokenExpired},
		{"no oauth section", `{"mcpOAuth": {}}`, "", errNoCredentials},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readAccessToken(writeCredentials(t, c.content), now)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("token = %q, want %q", got, c.want)
			}
		})
	}
}

func TestReadAccessTokenMissingFile(t *testing.T) {
	_, err := readAccessToken(filepath.Join(t.TempDir(), "nope.json"), time.Now())
	if !errors.Is(err, errNoCredentials) {
		t.Errorf("err = %v, want errNoCredentials", err)
	}
}

func TestCredentialsPathHonorsConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/dir")
	got, err := credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/custom/dir/.credentials.json"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}
