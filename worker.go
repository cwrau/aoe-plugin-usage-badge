// Agent of Empires worker: JSON-RPC 2.0 over newline-delimited JSON on stdio.
//
// The host spawns this process, sends one JSON-RPC request per line on
// stdin, and reads one response per line on stdout. The worker exits
// cleanly on EOF.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

const runtimeName = "go"

// StatusBar is a global slot (no session_id). Must match aoe-plugin.toml's [[ui]].
const (
	uiSlot = "status-bar"
	uiID   = "usage-badge"
)

const pollInterval = 60 * time.Second

// handle maps a command id to a method; dispatch on the trailing segment so
// the worker is robust to the host's exact method prefix. A real handler
// reads params for the request arguments.
func handle(method string, _ json.RawMessage) (any, error) {
	parts := strings.Split(method, ".")
	command := parts[len(parts)-1]
	if command == "status" {
		return map[string]any{
			"ok":      true,
			"runtime": runtimeName,
			"message": "Usage Badge worker is running",
		}, nil
	}
	return nil, &methodNotFoundError{method: method}
}

func main() {
	ctx := context.Background()
	rpc := newHostRPC(os.Stdout)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	poller := newUsagePoller(rpc, &wg)
	wg.Go(func() {
		poller.run(ctx, stop)
	})

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		rpc.handleInboundLine([]byte(line), handle, poller.handleNotification)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "usage-badge-worker: reading stdin:", err)
	}

	close(stop)
	wg.Wait()
}
