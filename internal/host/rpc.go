// Package host is the worker's side of the Agent of Empires plugin protocol:
// JSON-RPC 2.0 over newline-delimited JSON on stdio. rpc.go has the message
// shapes and response builders (a MethodNotFoundError becomes -32601, anything
// else the internal-error code -32603); client.go the connection itself.
package host

import "encoding/json"

const (
	errMethodNotFound = -32601
	errInternal       = -32603
)

type rpcErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErrorBody   `json:"error,omitempty"`
}

// notification is a worker-initiated JSON-RPC call to the host with no reply
// expected (e.g. ui.state.set).
type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// MethodNotFoundError is the error a dispatch func returns for a method it
// doesn't implement.
type MethodNotFoundError struct {
	Method string
}

func (e *MethodNotFoundError) Error() string {
	return "unknown method " + e.Method
}

func resultResponse(id json.RawMessage, result any) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResponse(id json.RawMessage, err error) response {
	if mnf, ok := err.(*MethodNotFoundError); ok {
		return response{
			JSONRPC: "2.0",
			ID:      id,
			Error:   &rpcErrorBody{Code: errMethodNotFound, Message: "unknown method " + mnf.Method},
		}
	}
	return response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcErrorBody{Code: errInternal, Message: err.Error()},
	}
}

func newNotification(method string, params any) notification {
	return notification{JSONRPC: "2.0", Method: method, Params: params}
}
