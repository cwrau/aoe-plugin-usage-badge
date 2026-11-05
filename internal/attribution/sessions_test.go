package attribution

import (
	"path/filepath"
	"sort"
	"testing"
)

func TestLoadLiveSessionsFiltersAndMergesProfiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "profiles", "main", "sessions.json"), `[
  {"id": "live1", "acp_session_id": "t-live1", "archived_at": null},
  {"id": "archived", "acp_session_id": "t-arch", "archived_at": "2026-10-01T00:00:00Z"},
  {"id": "trashed", "acp_session_id": "t-trash", "trashed_at": "2026-10-01T00:00:00Z"},
  {"id": "no-claude-id", "acp_session_id": ""},
  {"id": "no-claude-key"}
]`)
	writeFile(t, filepath.Join(dir, "profiles", "other", "sessions.json"), `[
  {"id": "live2", "acp_session_id": "t-live2"}
]`)

	sessions, err := LoadLiveSessions(dir)
	if err != nil {
		t.Fatalf("loadLiveSessions: %v", err)
	}
	var got []string
	for _, s := range sessions {
		got = append(got, s.ID+"="+s.Transcript)
	}
	sort.Strings(got)
	want := []string{"live1=t-live1", "live2=t-live2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("sessions = %v, want %v", got, want)
	}
}

func TestLoadLiveSessionsNoProfiles(t *testing.T) {
	sessions, err := LoadLiveSessions(t.TempDir())
	if err != nil || len(sessions) != 0 {
		t.Errorf("sessions = %v, err = %v; want none and no error", sessions, err)
	}
}

func TestLoadLiveSessionsMalformedStoreIsError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "profiles", "main", "sessions.json"), `{not json`)
	if _, err := LoadLiveSessions(dir); err == nil {
		t.Error("err = nil, want a parse error")
	}
}
