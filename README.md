# Usage Badge

An [Agent of Empires](https://github.com/agent-of-empires/agent-of-empires)
plugin. It contributes a `status` command, two settings, and a `status-bar` UI
slot, backed by a worker that speaks JSON-RPC 2.0 over newline-delimited JSON
on stdio. The worker polls usage every 60s and pushes it to the status bar via
`ui.state.set`. Usage comes from Anthropic's OAuth usage endpoint, using the
access token Claude Code already stores in `~/.claude/.credentials.json`
(`CLAUDE_CONFIG_DIR` is honored; the token is only read, never refreshed). If
that fails, the optional "Claude Code session ID" setting enables a slower
fallback that runs a headless `claude -p /usage` in that session. With neither
a stored login nor a session ID the badge shows N/A. See `getUsage` in
`usage.go` and `api.go`. The endpoint is undocumented and may change.

## Per-session estimate

Every live session also gets a button beside its ACP composer (`composer-action`,
dashboard only) showing that session's estimated share of the 5h limit; a click
cycles it to the weekly share. Anthropic exposes no per-session numbers, so this
is an estimate: a session's share of a window is the window's utilization times
the session's weighted tokens over all local weighted tokens in that window,
read from Claude Code's transcripts (`~/.claude/projects`, subagents included).
Token weights (output 1, input 0.2, cache write 0.15, cache read 0.005, opus
tokens times 1.2) were fit against the recorded limit history, Sep 22 to Oct 30:
held-out R² 0.96 for the weekly series, 0.74 for the noisier 5h series. Limits:
usage from other devices or claude.ai has no local transcript, so local sessions
are credited with all of it, and the AoE-to-Claude session mapping is read from
AoE's session store (`profiles/*/sessions.json`), a host internal.

## Install into Agent of Empires

```bash
# from a local checkout
aoe plugin install ./aoe-plugin-usage-badge

# or from GitHub once published
aoe plugin install gh:cwrau/aoe-plugin-usage-badge
```

The host runs the manifest's `[[runtime.build]]` steps at install time and then
launches the plugin-relative worker entrypoint under `.aoe-build/`. Manage the
plugin from the TUI, the web dashboard, or `aoe plugin {enable,disable,update}`.

## Layout

```
aoe-plugin.toml         the manifest (commands, settings, ui slots, runtime)
go.mod                  module definition
go.work                 links this module and magefiles/ for local tooling
worker.go               the JSON-RPC worker loop
poller.go               background refresh: status bar + per-session composer badges
rpc.go                  message shapes + response builders
worker_contract_test.go worker-contract and framing tests
rpc_test.go             response-builder tests
hostrpc.go              outbound host RPC calls (config.get) + inbound dispatch
hostrpc_test.go         request/notification/reply routing tests
api.go                  primary usage source: OAuth usage API + stored token
api_test.go             usage-API client tests
usage.go                badge rendering, pace/tone, `claude -p /usage` fallback
usage_test.go           usage-parser and rendering tests
transcripts.go          incremental transcript index + token weights
transcripts_test.go     transcript index tests
sessions.go             live AoE sessions and their Claude session ids
sessions_test.go        session store tests
poller_test.go          poller and composer-badge tests
magefiles/              zero-install Mage build (see below)
```

## Develop

Build tooling runs through [Mage](https://magefile.org/zeroinstall/) --
zero-install, no global `mage`/`golangci-lint` binary needed. From the repo
root:

```bash
go run -C magefiles mage.go -w .. -l       # list targets
go run -C magefiles mage.go -w .. check    # fmt, vet, lint, test (default)
go run -C magefiles mage.go -w .. build    # compile the worker binary
go run -C magefiles mage.go -w .. testRace # test suite with the race detector
```

`golangci-lint` is pulled in as a `go.sum`-pinned tool dependency of
`magefiles/go.mod` (via `go get -tool`), not an ambient binary -- see
`magefiles/magefile.go`.

Drive the worker by hand (it reads one JSON-RPC request per line on stdin):

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"usage-badge.status","params":{}}' \
  | go run .
```
## Worker protocol

The host spawns the worker and exchanges newline-delimited JSON-RPC 2.0 messages
over stdio. The worker answers a request whose method ends in `.status` and exits
cleanly when stdin reaches EOF. A request:

```json
{"jsonrpc": "2.0", "id": 1, "method": "usage-badge.status", "params": {}}
```

A response:

```json
{"jsonrpc": "2.0", "id": 1, "result": {"ok": true, "runtime": "go", "message": "..."}}
```

Unknown methods return a JSON-RPC error with code `-32601`. See the
[plugin authoring guide](https://agent-of-empires.com/docs/development/writing-plugins/)
and the [plugin API reference](https://agent-of-empires.com/docs/api/plugin-api/).

## Release

Push a `v0.1.0`-style tag to run the release workflow: it
runs checks, generates the changelog from conventional commits, and publishes a
GitHub Release with a source archive and `sha256`. To get listed in the Agent of
Empires featured index, see the authoring guide's publishing section.

## License

MIT
