package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var workerBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "usage-badge-worker-test")
	if err != nil {
		panic(err)
	}

	workerBinary = filepath.Join(dir, "usage-badge-worker")
	build := exec.Command("go", "build", "-o", workerBinary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		panic("build worker: " + err.Error() + "\n" + string(out))
	}

	// os.Exit below skips deferred calls, so clean up explicitly before it runs.
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		panic(err)
	}
	os.Exit(code)
}

// runWorker feeds each line as a JSON-RPC message on stdin and splits the
// worker's stdout into three buckets: responses to the host's requests (have
// "id" plus "result"/"error", no "method"), worker-initiated notifications
// (have "method", no "id" -- the status-bar push), and worker-initiated
// outbound calls to the host (have both "id" and "method" -- config.get).
// Callers that don't care about outbound calls (most of them: the test
// harness never answers config.get, so the worker's read on it just times
// out and falls back to its unconfigured state) can ignore the third
// return value.
func runWorker(t *testing.T, lines []string) (responses, notifications, hostCalls []map[string]any) {
	t.Helper()

	cmd := exec.Command(workerBinary)
	// Empty config dirs keep the worker off the developer's real Claude Code
	// credentials and AoE session store, so these tests never call the live
	// usage API or read real transcripts.
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir())
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker exited with error: %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("worker timed out")
	}

	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("bad json line %q: %v", line, err)
		}
		_, hasID := msg["id"]
		_, hasMethod := msg["method"]
		switch {
		case hasID && hasMethod:
			hostCalls = append(hostCalls, msg)
		case hasID:
			responses = append(responses, msg)
		default:
			notifications = append(notifications, msg)
		}
	}
	return responses, notifications, hostCalls
}

func TestStatusReturnsOK(t *testing.T) {
	responses, _, _ := runWorker(t, []string{`{"jsonrpc":"2.0","id":1,"method":"usage-badge.status","params":{}}`})
	if len(responses) != 1 {
		t.Fatalf("got %d responses, want 1: %+v", len(responses), responses)
	}
	result, ok := responses[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("result not an object: %+v", responses[0])
	}
	if result["ok"] != true {
		t.Errorf("ok = %v, want true", result["ok"])
	}
	if result["runtime"] != "go" {
		t.Errorf("runtime = %v, want go", result["runtime"])
	}
}

func TestUnknownMethodErrors(t *testing.T) {
	responses, _, _ := runWorker(t, []string{`{"jsonrpc":"2.0","id":2,"method":"usage-badge.nope"}`})
	if len(responses) != 1 {
		t.Fatalf("got %d responses, want 1: %+v", len(responses), responses)
	}
	errObj, ok := responses[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("error not an object: %+v", responses[0])
	}
	if int(errObj["code"].(float64)) != -32601 {
		t.Errorf("code = %v, want -32601", errObj["code"])
	}
}

func TestNotificationHasNoResponse(t *testing.T) {
	responses, _, _ := runWorker(t, []string{`{"jsonrpc":"2.0","method":"usage-badge.status"}`})
	if len(responses) != 0 {
		t.Errorf("got %d responses, want 0: %+v", len(responses), responses)
	}
}

// pushPayload extracts the status-bar payload from a ui.state.set
// notification, failing the test if the notification isn't shaped as
// expected.
func pushPayload(t *testing.T, push map[string]any) map[string]any {
	t.Helper()
	if push["method"] != "ui.state.set" {
		t.Fatalf("method = %v, want ui.state.set", push["method"])
	}
	params, ok := push["params"].(map[string]any)
	if !ok {
		t.Fatalf("params not an object: %+v", push)
	}
	if params["slot"] != "status-bar" {
		t.Errorf("slot = %v, want status-bar", params["slot"])
	}
	if params["id"] != "usage-badge" {
		t.Errorf("id = %v, want usage-badge", params["id"])
	}
	payload, ok := params["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload not an object: %+v", params)
	}
	return payload
}

// TestWorkerPushesStatusBarStateOnStart never answers the worker's
// config.get for session_id, so the async fetch's read times out (see
// configFetchTimeout) and falls back to the unconfigured "N/A" state --
// same outcome a real host gives when the setting was never set. Two
// pushes are expected: the immediate "pending" placeholder (there's no
// previous payload yet to keep showing), then the resolved fallback once
// the fetch completes.
func TestWorkerPushesStatusBarStateOnStart(t *testing.T) {
	_, notifications, hostCalls := runWorker(t, []string{`{"jsonrpc":"2.0","id":1,"method":"usage-badge.status","params":{}}`})
	if len(notifications) != 2 {
		t.Fatalf("got %d notifications, want 2: %+v", len(notifications), notifications)
	}

	pending := pushPayload(t, notifications[0])
	if pending["text"] != "…" {
		t.Errorf("first push text = %v, want the pending placeholder", pending["text"])
	}

	resolved := pushPayload(t, notifications[1])
	if resolved["text"] != "N/A" {
		t.Errorf("second push text = %v, want N/A (session_id unset)", resolved["text"])
	}

	if len(hostCalls) != 1 {
		t.Fatalf("got %d host calls, want 1: %+v", len(hostCalls), hostCalls)
	}
	if hostCalls[0]["method"] != "config.get" {
		t.Errorf("method = %v, want config.get", hostCalls[0]["method"])
	}
	callParams, ok := hostCalls[0]["params"].(map[string]any)
	if !ok || callParams["key"] != "session_id" {
		t.Errorf("params = %+v, want key=session_id", hostCalls[0]["params"])
	}
}
