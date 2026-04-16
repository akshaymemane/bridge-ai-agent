# bridge-agent

`bridge-agent` runs on each remote machine, connects back to the BridgeAIChat gateway, and executes local AI CLIs through tmux-backed sessions or direct subprocess mode.

## Requirements

- Go 1.22+
- `tmux` installed on devices using Codex or other tmux-backed tools
- One or more local AI CLIs such as `codex`, `claude`, or `openclaw`

## Build

```bash
go build ./cmd/bridge-agent
```

## Configure

```bash
cp config/agent.example.yaml agent.yaml
```

The config can stay minimal:

- `device.id`, `device.name`, and `device.tailnet_id` auto-derive from hostname and Tailscale when omitted
- `gateway.url` falls back to `BRIDGE_AGENT_GATEWAY_URL`, `BRIDGE_GATEWAY_URL`, or `wss://bridgeai.dev/agent`
- tool definitions auto-detect common CLIs on `PATH` when omitted

## Run

```bash
./bridge-agent -config ./agent.yaml
```

The `-config` flag defaults to `./agent.yaml` if omitted.

## Direct Mode

Use `direct: true` for one-shot CLIs that read from args and write a single response to stdout.

```yaml
tools:
  claude:
    cmd: claude
    args: ["-p", "--dangerously-skip-permissions"]
    continue_args: ["--continue", "-p", "--dangerously-skip-permissions"]
    direct: true
```

Direct mode bypasses tmux and avoids pane-capture timeouts for tools like Claude.

## Session Model

- Each `chat_id` maps to a persistent tmux session named `bridge-{chat_id}`
- Follow-up turns reuse the same session metadata
- `chat_id` must match `[a-z0-9_-]{1,64}`

## Release Packaging

```bash
bash scripts/package-agent-release.sh v0.1.0-beta.1
```

The release archive contains `bridge-agent`, `agent.yaml.example`, `install-agent.sh`, and `SHA256SUMS.txt`.

## Failure States

| Condition              | Result              |
|------------------------|---------------------|
| `tmux` missing         | startup abort       |
| Tool binary missing    | `tool_not_found`    |
| Invalid `chat_id`      | `session_error`     |
| tmux session failure   | `session_error`     |
| Response timeout (5m)  | `session_error`     |
