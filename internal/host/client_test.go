package host

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestHandleInboundLineRoutesRequestsNotificationsAndReplies(t *testing.T) {
	var out bytes.Buffer
	rpc := NewClient(&out)

	var dispatched, notified []string
	dispatch := func(method string, _ json.RawMessage) (any, error) {
		dispatched = append(dispatched, method)
		return map[string]any{"ok": true}, nil
	}
	onNotification := func(method string, _ json.RawMessage) {
		notified = append(notified, method)
	}

	rpc.HandleInboundLine([]byte(`{"jsonrpc":"2.0","id":7,"method":"x.status"}`), dispatch, onNotification)
	rpc.HandleInboundLine([]byte(`{"jsonrpc":"2.0","method":"usage.cycle","params":{"session_id":"s"}}`), dispatch, onNotification)

	if len(dispatched) != 1 || dispatched[0] != "x.status" {
		t.Errorf("dispatched = %v, want [x.status]", dispatched)
	}
	if len(notified) != 1 || notified[0] != "usage.cycle" {
		t.Errorf("notified = %v, want [usage.cycle]", notified)
	}
	if got, want := out.String(), `{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`+"\n"; got != want {
		t.Errorf("wrote %q, want only the request's response %q (a notification gets none)", got, want)
	}
}

func TestCallReceivesItsReplyThroughHandleInboundLine(t *testing.T) {
	var out bytes.Buffer
	rpc := NewClient(&out)
	noop := func(string, json.RawMessage) {}

	type result struct {
		raw json.RawMessage
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := rpc.Call(context.Background(), "config.get", map[string]any{"key": "k"})
		done <- result{raw, err}
	}()

	// The call registers its id before writing the request; wait for the write.
	deadline := time.After(2 * time.Second)
	for {
		rpc.mu.Lock()
		written := out.Len() > 0
		rpc.mu.Unlock()
		if written {
			break
		}
		select {
		case <-deadline:
			t.Fatal("call never wrote its request")
		case <-time.After(time.Millisecond):
		}
	}

	var req struct {
		ID int `json:"id"`
	}
	rpc.mu.Lock()
	err := json.Unmarshal(out.Bytes(), &req)
	rpc.mu.Unlock()
	if err != nil {
		t.Fatalf("request not JSON: %v", err)
	}
	reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"value": "v"}})
	rpc.HandleInboundLine(reply, nil, noop)

	select {
	case r := <-done:
		if r.err != nil || string(r.raw) != `{"value":"v"}` {
			t.Errorf("call = %s, %v; want the reply's result", r.raw, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("call never returned")
	}
}
