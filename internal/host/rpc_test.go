package host

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestResultResponseShape(t *testing.T) {
	got := resultResponse(json.RawMessage("1"), map[string]any{"ok": true})
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	if string(data) != want {
		t.Errorf("got %s, want %s", data, want)
	}
}

func TestMethodNotFoundMapsTo32601(t *testing.T) {
	resp := errorResponse(json.RawMessage("5"), &MethodNotFoundError{Method: "x.y"})
	if resp.Error.Code != errMethodNotFound {
		t.Errorf("got code %d, want %d", resp.Error.Code, errMethodNotFound)
	}
}

func TestInternalErrorMapsTo32603(t *testing.T) {
	resp := errorResponse(json.RawMessage("5"), errors.New("boom"))
	if resp.Error.Code != errInternal {
		t.Errorf("got code %d, want %d", resp.Error.Code, errInternal)
	}
}
