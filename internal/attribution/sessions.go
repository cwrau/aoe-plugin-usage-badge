package attribution

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Session is a live AoE session and the Claude session its transcript is
// named after. The host's sessions.list RPC does not expose that Claude (ACP)
// session id, so the mapping is read from the host's session store. That file
// is a host internal: when it can't be read or parsed the composer badges
// simply don't update, and nothing else is affected.
type Session struct {
	ID         string
	Transcript string
}

type storedSession struct {
	ID         string          `json:"id"`
	ACPSession string          `json:"acp_session_id"`
	ArchivedAt json.RawMessage `json:"archived_at"`
	TrashedAt  json.RawMessage `json:"trashed_at"`
}

func isSet(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null" && string(raw) != `""`
}

// AoEConfigDir is the Agent of Empires config directory.
func AoEConfigDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent-of-empires"), nil
}

// LoadLiveSessions reads every profile's session store and returns the
// sessions that are neither archived nor trashed and have a Claude session id.
func LoadLiveSessions(configDir string) ([]Session, error) {
	stores, err := filepath.Glob(filepath.Join(configDir, "profiles", "*", "sessions.json"))
	if err != nil {
		return nil, err
	}
	var sessions []Session
	for _, store := range stores {
		data, err := os.ReadFile(store)
		if err != nil {
			return nil, err
		}
		var stored []storedSession
		if err := json.Unmarshal(data, &stored); err != nil {
			return nil, fmt.Errorf("parse %s: %w", store, err)
		}
		for _, s := range stored {
			if s.ID == "" || s.ACPSession == "" || isSet(s.ArchivedAt) || isSet(s.TrashedAt) {
				continue
			}
			sessions = append(sessions, Session{ID: s.ID, Transcript: s.ACPSession})
		}
	}
	return sessions, nil
}
