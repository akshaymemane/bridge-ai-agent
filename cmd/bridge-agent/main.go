package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Session state — persisted to disk so agent restarts preserve conversation
// continuity (e.g. Claude --continue picks up the right session).
// ---------------------------------------------------------------------------

// sessionState is written to <configDir>/.bridge-sessions.json.
type sessionState struct {
	Sessions map[string]bool `json:"sessions"` // chat_id → true once first message sent
}

func loadSessionState(path string) sessionState {
	data, err := os.ReadFile(path)
	if err != nil {
		return sessionState{Sessions: make(map[string]bool)}
	}
	var s sessionState
	if err := json.Unmarshal(data, &s); err != nil || s.Sessions == nil {
		return sessionState{Sessions: make(map[string]bool)}
	}
	return s
}

func saveSessionState(path string, s sessionState) {
	data, err := json.Marshal(s)
	if err != nil {
		slog.Warn("failed to marshal session state", "err", err)
		return
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		slog.Warn("failed to write session state", "path", path, "err", err)
	}
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type ToolConfig struct {
	Cmd          string   `yaml:"cmd"`
	Args         []string `yaml:"args"`          // args for the first message in a session, e.g. ["-p"]
	ContinueArgs []string `yaml:"continue_args"` // args for follow-up messages, e.g. ["--continue", "-p"]
	WorkingDir   string   `yaml:"working_dir"`   // optional cwd for the tool; relative paths resolve from the config file directory
	FallbackTool string   `yaml:"fallback_tool"` // tool to try if this tool's binary is missing from PATH
	Direct       bool     `yaml:"direct"`        // run the tool directly (capture stdout), skip tmux pane scanning
}

type Config struct {
	Device struct {
		ID        string `yaml:"id"`
		Name      string `yaml:"name"`
		Hostname  string `yaml:"hostname"`
		TailnetID string `yaml:"tailnet_id"`
	} `yaml:"device"`
	Tools       map[string]ToolConfig `yaml:"tools"`
	DefaultTool string                `yaml:"default_tool"`
	WorkingDir  string                `yaml:"working_dir"`
	Gateway     struct {
		URL string `yaml:"url"`
	} `yaml:"gateway"`
	BaseDir string `yaml:"-"`
}

func loadConfig(path string) (*Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path %s: %w", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()
	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	cfg.BaseDir = filepath.Dir(absPath)
	applyConfigDefaults(&cfg)
	return &cfg, nil
}

func applyConfigDefaults(cfg *Config) {
	detectedHostname, detectedTailnet := detectLocalIdentity()

	cfg.Device.Hostname = firstNonEmpty(cfg.Device.Hostname, cfg.Device.Name, detectedHostname)
	cfg.Device.Name = firstNonEmpty(cfg.Device.Name, cfg.Device.Hostname, "Device")
	cfg.Device.TailnetID = firstNonEmpty(
		cfg.Device.TailnetID,
		os.Getenv("TAILSCALE_TAILNET"),
		detectedTailnet,
		"local",
	)
	if cfg.Device.ID == "" {
		cfg.Device.ID = deriveDeviceID(cfg.Device.Hostname, cfg.Device.TailnetID)
	}

	cfg.Gateway.URL = firstNonEmpty(
		cfg.Gateway.URL,
		os.Getenv("BRIDGE_AGENT_GATEWAY_URL"),
		os.Getenv("BRIDGE_GATEWAY_URL"),
		"wss://bridgeai.dev/agent",
	)

	if cfg.Tools == nil {
		cfg.Tools = autoToolConfigs(cfg.WorkingDir)
	}
	if cfg.WorkingDir != "" {
		for name, tool := range cfg.Tools {
			if tool.WorkingDir == "" {
				tool.WorkingDir = cfg.WorkingDir
				cfg.Tools[name] = tool
			}
		}
	}
	if cfg.DefaultTool == "" {
		cfg.DefaultTool = preferredDefaultTool(cfg.Tools)
	}
}

func detectLocalIdentity() (hostname string, tailnetID string) {
	hostname, _ = os.Hostname()
	hostname = strings.TrimSpace(hostname)

	if envTailnet := os.Getenv("TAILSCALE_TAILNET"); envTailnet != "" {
		return trimHostname(hostname), envTailnet
	}

	path, err := exec.LookPath("tailscale")
	if err != nil {
		return trimHostname(hostname), ""
	}
	output, err := exec.Command(path, "status", "--json").Output()
	if err != nil {
		return trimHostname(hostname), ""
	}

	var payload map[string]any
	if err := json.Unmarshal(output, &payload); err != nil {
		return trimHostname(hostname), ""
	}
	self, _ := payload["Self"].(map[string]any)
	dnsName := trimHostname(stringValue(self["DNSName"]))
	selfHostname := trimHostname(firstNonEmpty(
		stringValue(self["HostName"]),
		stringValue(self["Hostname"]),
		hostname,
	))
	tailnetID = tailnetFromDNSName(stringValue(self["DNSName"]))
	return firstNonEmpty(selfHostname, dnsName, trimHostname(hostname), "device"), tailnetID
}

func tailnetFromDNSName(dnsName string) string {
	dnsName = strings.TrimSuffix(strings.TrimSpace(dnsName), ".")
	if dnsName == "" {
		return ""
	}
	parts := strings.Split(dnsName, ".")
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[1:], ".")
}

func autoToolConfigs(workingDir string) map[string]ToolConfig {
	tools := map[string]ToolConfig{}
	if _, err := exec.LookPath("codex"); err == nil {
		tools["codex"] = ToolConfig{
			Cmd:        "codex",
			Args:       []string{"exec", "--full-auto"},
			WorkingDir: workingDir,
		}
	}
	if _, err := exec.LookPath("claude"); err == nil {
		tools["claude"] = ToolConfig{
			Cmd:          "claude",
			Args:         []string{"-p", "--dangerously-skip-permissions"},
			ContinueArgs: []string{"--continue", "-p", "--dangerously-skip-permissions"},
			WorkingDir:   workingDir,
			Direct:       true,
		}
	}
	if _, err := exec.LookPath("openclaw"); err == nil {
		tools["openclaw"] = ToolConfig{
			Cmd:        "openclaw",
			WorkingDir: workingDir,
		}
	}
	return tools
}

func preferredDefaultTool(tools map[string]ToolConfig) string {
	for _, candidate := range []string{"codex", "claude", "openclaw"} {
		if _, ok := tools[candidate]; ok {
			return candidate
		}
	}
	for name := range tools {
		return name
	}
	return ""
}

// ---------------------------------------------------------------------------
// Wire message types
// ---------------------------------------------------------------------------

type InboundMsg struct {
	Type     string `json:"type"`
	ChatID   string `json:"chat_id"`
	DeviceID string `json:"device_id,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	Tool     string `json:"tool"`
	Text     string `json:"text"`
}

type OutboundMsg struct {
	Type      string   `json:"type"`
	ChatID    string   `json:"chat_id,omitempty"`
	DeviceID  string   `json:"device_id,omitempty"`
	UserID    string   `json:"user_id,omitempty"`
	Name      string   `json:"name,omitempty"`
	Hostname  string   `json:"hostname,omitempty"`
	TailnetID string   `json:"tailnet_id,omitempty"`
	Status    string   `json:"status,omitempty"`
	Text      string   `json:"text,omitempty"`
	Code      string   `json:"code,omitempty"`
	Message   string   `json:"message,omitempty"`
	Tools     []string `json:"tools,omitempty"`
}

// ---------------------------------------------------------------------------
// chat_id validation
// ---------------------------------------------------------------------------

var chatIDRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

func validChatID(id string) bool {
	return chatIDRe.MatchString(id)
}

// ---------------------------------------------------------------------------
// ANSI strip
// ---------------------------------------------------------------------------

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b[^[\]a-zA-Z]`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

func isCodexExecTool(cmd string, args []string) bool {
	return filepath.Base(cmd) == "codex" && len(args) > 0 && args[0] == "exec"
}

func resolveWorkingDir(baseDir, workingDir string) string {
	if workingDir == "" {
		return ""
	}
	if filepath.IsAbs(workingDir) {
		return workingDir
	}
	return filepath.Clean(filepath.Join(baseDir, workingDir))
}

func runDirectTool(ctx context.Context, cmdPath string, args []string, prompt string, workingDir string) (string, string, error) {
	cmd := exec.CommandContext(ctx, cmdPath, append(args, prompt)...)
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// ---------------------------------------------------------------------------
// tmux helpers — all use exec.Command, never shell interpolation
// ---------------------------------------------------------------------------

// sentinelText is what we search for in pane output.
// sentinelCmd produces that output but does NOT contain the sentinel string
// literally — this prevents a false-positive when the echoed keystroke of the
// command itself appears in the pane before it executes.
const sentinelText = "__BRIDGE_DONE__"
const sentinelCmd = `printf '%s%s\n' '__BRIDGE' '_DONE__'`

const pollInterval = 200 * time.Millisecond
const sessionTimeout = 5 * time.Minute

// tmuxSessionName returns the tmux session name for a chat_id.
func tmuxSessionName(chatID string) string {
	return "bridge-" + chatID
}

// tmuxHasSession returns true if the named tmux session exists.
func tmuxHasSession(session string) bool {
	cmd := exec.Command("tmux", "has-session", "-t", session)
	return cmd.Run() == nil
}

// tmuxNewSession creates a new detached tmux session.
func tmuxNewSession(session, workingDir string) error {
	args := []string{"new-session", "-d", "-s", session}
	if workingDir != "" {
		args = append(args, "-c", workingDir)
	}
	cmd := exec.Command("tmux", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux new-session %s: %w: %s", session, err, out)
	}
	return nil
}

// tmuxSendKeys sends text to a tmux session followed by Enter.
func tmuxSendKeys(session, text string) error {
	cmd := exec.Command("tmux", "send-keys", "-t", session, text, "Enter")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux send-keys to %s: %w: %s", session, err, out)
	}
	return nil
}

func tmuxSendSentinel(session string) error {
	cmd := exec.Command("tmux", "send-keys", "-t", session, sentinelCmd, "Enter")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux send sentinel to %s: %w: %s", session, err, out)
	}
	return nil
}

// tmuxCapturePane captures the current pane contents.
func tmuxCapturePane(session string) (string, error) {
	cmd := exec.Command("tmux", "capture-pane", "-pt", session, "-S", "-")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane %s: %w", session, err)
	}
	return string(out), nil
}

// ---------------------------------------------------------------------------
// Tool resolution with fallback chain
// ---------------------------------------------------------------------------

// resolveToolCfg walks the fallback chain for the requested tool name and
// returns the first tool whose binary is reachable on PATH.
//
// Resolution order for each candidate:
//  1. Look up in cfg.Tools. If missing, jump to cfg.DefaultTool.
//  2. Check binary with exec.LookPath. If missing:
//     a. Try ToolConfig.FallbackTool (if set and not yet visited).
//     b. Try cfg.DefaultTool (if different and not yet visited).
//
// Cycle detection prevents infinite loops in misconfigured fallback chains.
func resolveToolCfg(cfg *Config, requestedTool string) (name string, tc ToolConfig, err error) {
	visited := make(map[string]bool)

	current := requestedTool
	if current == "" {
		current = cfg.DefaultTool
	}

	for {
		if visited[current] {
			return "", ToolConfig{}, fmt.Errorf("tool fallback cycle detected at %q", current)
		}
		visited[current] = true

		candidate, ok := cfg.Tools[current]
		if !ok {
			// Tool not configured — fall back to default if we haven't tried it.
			if current != cfg.DefaultTool && cfg.DefaultTool != "" && !visited[cfg.DefaultTool] {
				slog.Warn("tool not configured, trying default", "tool", current, "default", cfg.DefaultTool)
				current = cfg.DefaultTool
				continue
			}
			return "", ToolConfig{}, fmt.Errorf("tool not configured: %s", current)
		}

		if _, lookErr := exec.LookPath(candidate.Cmd); lookErr == nil {
			if current != requestedTool && requestedTool != "" {
				slog.Info("using fallback tool", "requested", requestedTool, "resolved", current)
			}
			return current, candidate, nil
		}

		// Binary missing — try per-tool fallback, then default.
		if candidate.FallbackTool != "" && !visited[candidate.FallbackTool] {
			slog.Warn("tool binary missing, trying fallback_tool",
				"tool", current, "cmd", candidate.Cmd, "fallback", candidate.FallbackTool)
			current = candidate.FallbackTool
			continue
		}
		if current != cfg.DefaultTool && cfg.DefaultTool != "" && !visited[cfg.DefaultTool] {
			slog.Warn("tool binary missing, trying default_tool",
				"tool", current, "cmd", candidate.Cmd, "default", cfg.DefaultTool)
			current = cfg.DefaultTool
			continue
		}

		return "", ToolConfig{}, fmt.Errorf("tool binary not found: %s (cmd: %s)", current, candidate.Cmd)
	}
}

// ---------------------------------------------------------------------------
// Session processor — one goroutine per active send_message
// ---------------------------------------------------------------------------

// processMessage handles a single send_message.
//
// Each message invokes the tool as a one-shot subprocess inside the tmux shell
// session. The tmux session is a persistent shell — the tool is NOT kept running
// as an interactive REPL. This ensures the sentinel `echo __BRIDGE_DONE__` runs
// as a shell command after the tool exits, not as input to the tool.
//
// Message text is passed via a temp file to avoid shell quoting issues.
func processMessage(a *Agent, sendMsg func([]byte), msg InboundMsg) {
	cfg := a.cfg
	if !validChatID(msg.ChatID) {
		sendErr(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, "session_error", "invalid chat_id: must match [a-z0-9_-]{1,64}")
		return
	}

	// Resolve tool — walks fallback chain if requested tool is missing or binary not found.
	resolvedTool, toolCfg, toolErr := resolveToolCfg(cfg, msg.Tool)
	if toolErr != nil {
		slog.Error("tool resolution failed", "requested", msg.Tool, "err", toolErr)
		sendErr(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, "tool_not_found", toolErr.Error())
		return
	}
	slog.Info("tool resolved", "chat_id", msg.ChatID, "requested", msg.Tool, "resolved", resolvedTool)
	workingDir := resolveWorkingDir(cfg.BaseDir, toolCfg.WorkingDir)

	session := tmuxSessionName(msg.ChatID)

	// Ensure a shell session exists. We do NOT start the tool here — the tool
	// is invoked fresh for each message via a script below.
	if !tmuxHasSession(session) {
		if err := tmuxNewSession(session, workingDir); err != nil {
			slog.Error("failed to create tmux session", "session", session, "err", err)
			sendErr(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, "session_error", "failed to create tmux session: "+err.Error())
			return
		}
		slog.Info("created tmux session", "session", session)
	}

	// Write the message to a temp file so we can pass it to the tool without
	// any shell quoting concerns.
	msgFile := fmt.Sprintf("/tmp/bridge_msg_%s", msg.ChatID)
	if err := os.WriteFile(msgFile, []byte(msg.Text), 0600); err != nil {
		slog.Error("failed to write message temp file", "err", err)
		sendErr(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, "session_error", "failed to write message file: "+err.Error())
		return
	}

	// Pick args: use ContinueArgs for follow-up messages if configured.
	isFirst := a.claimSession(msg.ChatID)
	invocationArgs := toolCfg.Args
	if !isFirst && len(toolCfg.ContinueArgs) > 0 {
		invocationArgs = toolCfg.ContinueArgs
	}

	// Direct mode: run the tool as a subprocess and capture stdout.
	// Use this for one-shot CLIs (e.g. claude -p) that don't need a persistent
	// shell session. Avoids the tmux pane-scanning approach, which breaks when
	// the response fits within the pane's fixed row height (no scrollback growth).
	if toolCfg.Direct {
		ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
		defer cancel()

		stdout, stderr, err := runDirectTool(ctx, toolCfg.Cmd, invocationArgs, msg.Text, workingDir)
		if err != nil {
			errMsg := strings.TrimSpace(stderr)
			if errMsg == "" {
				errMsg = strings.TrimSpace(stdout)
			}
			if errMsg == "" {
				errMsg = err.Error()
			}
			slog.Error("direct tool run failed", "tool", resolvedTool, "chat_id", msg.ChatID, "err", errMsg)
			sendErr(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, "session_error", errMsg)
			sendStreamEnd(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID)
			return
		}
		text := strings.TrimSpace(stdout)
		if text == "" {
			text = strings.TrimSpace(stderr)
		}
		if text != "" {
			sendChunk(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, text)
		}
		sendStreamEnd(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID)
		slog.Info("direct tool run complete", "tool", resolvedTool, "chat_id", msg.ChatID)
		return
	}

	outputFile := ""
	if isCodexExecTool(toolCfg.Cmd, invocationArgs) {
		outputFile = fmt.Sprintf("/tmp/bridge_out_%s", msg.ChatID)
		invocationArgs = append(append([]string{}, invocationArgs...), "--output-last-message", outputFile)

		ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
		defer cancel()

		stdout, stderr, err := runDirectTool(ctx, toolCfg.Cmd, invocationArgs, msg.Text, workingDir)
		content, readErr := os.ReadFile(outputFile)
		_ = os.Remove(outputFile)

		if readErr == nil {
			text := strings.TrimSpace(string(content))
			if text != "" {
				sendChunk(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, text)
			} else if strings.TrimSpace(stdout) != "" {
				sendChunk(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, strings.TrimSpace(stdout))
			}
			sendStreamEnd(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID)
			slog.Info("direct codex run complete", "chat_id", msg.ChatID)
			return
		}

		if err != nil {
			msgText := strings.TrimSpace(stderr)
			if msgText == "" {
				msgText = strings.TrimSpace(stdout)
			}
			if msgText == "" {
				msgText = err.Error()
			}
			sendErr(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, "session_error", msgText)
			sendStreamEnd(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID)
			return
		}

		fallback := strings.TrimSpace(stdout)
		if fallback == "" {
			fallback = strings.TrimSpace(stderr)
		}
		if fallback != "" {
			sendChunk(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, fallback)
		}
		sendStreamEnd(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID)
		slog.Warn("codex output file missing, used stdout fallback", "chat_id", msg.ChatID, "path", outputFile)
		return
	}

	// Build the shell command. Message is passed via $(cat msgFile) to avoid
	// quoting issues with arbitrary text content.
	//
	// Example: claude -p "$(cat /tmp/bridge_msg_xyz)"
	// Follow-up: claude --continue -p "$(cat /tmp/bridge_msg_xyz)"
	cmdArgs := append(append([]string{}, invocationArgs...), fmt.Sprintf("\"$(cat %s)\"", msgFile))
	scriptLine := fmt.Sprintf("%s %s; rm -f %s", toolCfg.Cmd, strings.Join(cmdArgs, " "), msgFile)

	// Snapshot pane before sending the command.
	pre, _ := tmuxCapturePane(session)
	baseline := stripANSI(pre)

	// Send the tool invocation command to the shell.
	if err := tmuxSendKeys(session, scriptLine); err != nil {
		slog.Error("failed to send tool command to tmux", "session", session, "err", err)
		sendErr(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, "session_error", "failed to send command: "+err.Error())
		return
	}

	// Send sentinel — runs in the shell AFTER the tool exits.
	if err := tmuxSendSentinel(session); err != nil {
		slog.Error("failed to send sentinel", "session", session, "err", err)
		sendErr(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, "session_error", "failed to send sentinel: "+err.Error())
		return
	}

	// Wait briefly for terminal echo of the command line to appear, then
	// re-snapshot so the echoed command line is already counted in the baseline.
	time.Sleep(150 * time.Millisecond)
	post, _ := tmuxCapturePane(session)
	baseline = stripANSI(post)

	// Use line-count-based tracking instead of string prefix matching.
	// tmux pads every line to the terminal width with trailing spaces, which
	// makes HasPrefix unreliable. Line counts are not affected by padding.
	baselineLines := strings.Count(baseline, "\n")
	lastLineCount := baselineLines

	// Poll capture-pane until sentinel is found or timeout.
	ctx, cancel := context.WithTimeout(context.Background(), sessionTimeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Warn("session timed out waiting for sentinel", "session", session)
			sendErr(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, "session_error", "response timed out")
			return
		case <-ticker.C:
			raw, err := tmuxCapturePane(session)
			if err != nil {
				slog.Warn("capture-pane error", "session", session, "err", err)
				sendErr(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, "session_error", "capture-pane failed: "+err.Error())
				return
			}

			clean := stripANSI(raw)
			lines := strings.Split(clean, "\n")
			if len(lines) <= lastLineCount {
				continue
			}

			// Extract only new lines since last check.
			newLines := lines[lastLineCount:]
			// Trim trailing spaces from each line (tmux pads lines to terminal width).
			for i, l := range newLines {
				newLines[i] = strings.TrimRight(l, " \t")
			}
			newContent := strings.Join(newLines, "\n")

			sentinelIdx := strings.Index(newContent, sentinelText)
			if sentinelIdx == -1 {
				// No sentinel yet — stream new content, preserving newlines so
				// the frontend can reconstruct line boundaries correctly.
				if newContent != "" {
					sendChunk(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, newContent)
				}
				lastLineCount = len(lines)
				continue
			}

			// Sentinel found — emit everything before it, then end stream.
			before := strings.TrimRight(newContent[:sentinelIdx], "\n ")
			if before != "" {
				sendChunk(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID, before)
			}
			sendStreamEnd(sendMsg, a.cfg.Device.ID, msg.UserID, msg.ChatID)
			slog.Info("stream complete", "chat_id", msg.ChatID, "session", session)
			return
		}
	}
}

func sendChunk(send func([]byte), deviceID, userID, chatID, text string) {
	msg := OutboundMsg{Type: "stream_chunk", DeviceID: deviceID, UserID: userID, ChatID: chatID, Text: text}
	data, _ := json.Marshal(msg)
	send(data)
}

func sendStreamEnd(send func([]byte), deviceID, userID, chatID string) {
	msg := OutboundMsg{Type: "stream_end", DeviceID: deviceID, UserID: userID, ChatID: chatID}
	data, _ := json.Marshal(msg)
	send(data)
}

func sendErr(send func([]byte), deviceID, userID, chatID, code, message string) {
	msg := OutboundMsg{Type: "error", DeviceID: deviceID, UserID: userID, ChatID: chatID, Code: code, Message: message}
	data, _ := json.Marshal(msg)
	send(data)
	slog.Warn("sending error to gateway", "chat_id", chatID, "code", code, "message", message)
}

// ---------------------------------------------------------------------------
// Gateway connection with exponential backoff
// ---------------------------------------------------------------------------

type Agent struct {
	cfg          *Config
	sendCh       chan []byte
	capabilities []string

	// sessionsMu guards sessions and statePath.
	sessionsMu sync.Mutex
	// sessions tracks chat_ids that have sent at least one message, so we know
	// whether to use Args (first message) or ContinueArgs (follow-up).
	// Backed by statePath on disk so agent restarts preserve continuity.
	sessions  map[string]bool
	statePath string
}

func newAgent(cfg *Config) *Agent {
	statePath := filepath.Join(cfg.BaseDir, ".bridge-sessions.json")
	state := loadSessionState(statePath)
	loaded := len(state.Sessions)
	if loaded > 0 {
		slog.Info("loaded persisted session state", "sessions", loaded, "path", statePath)
	}
	return &Agent{
		cfg:          cfg,
		sendCh:       make(chan []byte, 256),
		capabilities: detectToolCapabilities(cfg),
		sessions:     state.Sessions,
		statePath:    statePath,
	}
}

// claimSession marks chatID as having received its first message.
// Returns true on the very first call for a given chatID (use Args),
// false on all subsequent calls (use ContinueArgs).
// State is persisted to disk immediately so restarts preserve the distinction.
func (a *Agent) claimSession(chatID string) bool {
	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()
	if a.sessions[chatID] {
		return false
	}
	a.sessions[chatID] = true
	saveSessionState(a.statePath, sessionState{Sessions: a.sessions})
	return true
}

// send queues a message to be written to the gateway connection.
// Called from processMessage goroutines.
func (a *Agent) send(data []byte) {
	select {
	case a.sendCh <- data:
	default:
		slog.Warn("agent send channel full, dropping message")
	}
}

// run connects to the gateway and maintains the connection indefinitely.
func (a *Agent) run() {
	attempt := 0
	for {
		slog.Info("connecting to gateway", "url", a.cfg.Gateway.URL, "attempt", attempt)
		err := a.connect()
		if err != nil {
			slog.Warn("gateway connection lost", "err", err)
		}
		attempt++
		backoff := backoffDuration(attempt)
		slog.Info("reconnecting after backoff", "backoff", backoff)
		time.Sleep(backoff)
	}
}

// backoffDuration returns an exponential backoff with jitter, capped at 30s.
func backoffDuration(attempt int) time.Duration {
	base := math.Pow(2, float64(attempt)) * float64(time.Second)
	jitter := rand.Float64() * float64(time.Second)
	d := time.Duration(base + jitter)
	if d > 30*time.Second {
		d = 30*time.Second + time.Duration(rand.Float64()*float64(time.Second))
	}
	return d
}

// connect establishes a single WebSocket connection to the gateway,
// registers the device, and processes messages until the connection closes.
func (a *Agent) connect() error {
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.Dial(a.cfg.Gateway.URL, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", a.cfg.Gateway.URL, err)
	}
	defer conn.Close()

	slog.Info("connected to gateway", "url", a.cfg.Gateway.URL)

	// Send device_status online immediately.
	regMsg := OutboundMsg{
		Type:      "device_status",
		DeviceID:  a.cfg.Device.ID,
		Name:      a.cfg.Device.Name,
		Hostname:  a.cfg.Device.Hostname,
		TailnetID: a.cfg.Device.TailnetID,
		Status:    "online",
		Tools:     a.capabilities,
	}
	regData, _ := json.Marshal(regMsg)
	if err := conn.WriteMessage(websocket.TextMessage, regData); err != nil {
		return fmt.Errorf("send registration: %w", err)
	}
	slog.Info("sent device_status online", "device_id", a.cfg.Device.ID)

	// Drain any buffered messages that accumulated while disconnected.
	go a.writePump(conn)

	conn.SetReadLimit(64 * 1024)
	conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		return nil
	})

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		conn.SetReadDeadline(time.Now().Add(120 * time.Second))

		var msg InboundMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			slog.Warn("received invalid JSON from gateway", "err", err)
			continue
		}

		switch msg.Type {
		case "send_message":
			slog.Info("received send_message", "chat_id", msg.ChatID, "tool", msg.Tool)
			// Process each message in its own goroutine so the read loop is not blocked.
			go processMessage(a, a.send, msg)
		default:
			slog.Warn("received unknown message type", "type", msg.Type)
		}
	}
}

// writePump reads from sendCh and writes to conn until conn is closed.
func (a *Agent) writePump(conn *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case msg, ok := <-a.sendCh:
			if !ok {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				slog.Warn("write to gateway failed", "err", err)
				return
			}
		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Startup checks
// ---------------------------------------------------------------------------

// checkTmux verifies tmux is installed. Returns an error if not.
func checkTmux() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("tmux not found in PATH — install tmux before running bridge-agent")
	}
	return nil
}

// checkTools verifies all configured tool binaries exist.
// Returns a map of tool name → error for missing tools.
func checkTools(cfg *Config) map[string]error {
	missing := make(map[string]error)
	for name, t := range cfg.Tools {
		if _, err := exec.LookPath(t.Cmd); err != nil {
			missing[name] = fmt.Errorf("binary %q not found in PATH", t.Cmd)
		}
	}
	return missing
}

func detectToolCapabilities(cfg *Config) []string {
	seen := map[string]struct{}{}
	out := []string{}

	addIfPresent := func(name, cmd string) {
		if _, ok := seen[name]; ok {
			return
		}
		if _, err := exec.LookPath(cmd); err != nil {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}

	for name, tool := range cfg.Tools {
		addIfPresent(name, firstNonEmpty(tool.Cmd, name))
	}
	for _, name := range []string{"claude", "codex", "ollama", "openclaw"} {
		addIfPresent(name, name)
	}
	sort.Strings(out)
	return out
}

func stringValue(value any) string {
	if raw, ok := value.(string); ok {
		return strings.TrimSpace(raw)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func trimHostname(value string) string {
	value = strings.TrimSuffix(strings.TrimSpace(value), ".")
	if idx := strings.Index(value, "."); idx > 0 {
		return value[:idx]
	}
	return value
}

func deriveDeviceID(hostname, tailnetID string) string {
	host := trimHostname(hostname)
	if host == "" {
		host = "device"
	}
	slug := slugify(host)
	sum := sha1.Sum([]byte(strings.ToLower(host) + "|" + strings.ToLower(tailnetID)))
	return slug + "-" + hex.EncodeToString(sum[:4])
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	prevDash := false
	for _, r := range value {
		isAlphaNum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlphaNum {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "device"
	}
	return out
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	configPath := flag.String("config", "./agent.yaml", "path to agent.yaml")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	slog.Info("bridge-agent starting",
		"device_id", cfg.Device.ID,
		"name", cfg.Device.Name,
		"gateway", cfg.Gateway.URL,
	)

	// Check tmux — hard requirement.
	if err := checkTmux(); err != nil {
		slog.Error("startup check failed", "err", err)
		fmt.Fprintf(os.Stderr, "\nERROR: %s\n\nInstall tmux and try again.\n", err)
		os.Exit(1)
	}
	slog.Info("tmux check passed")

	// Check tools — log warnings but don't abort; tool might be installed later.
	for name, terr := range checkTools(cfg) {
		slog.Warn("tool binary not found at startup", "tool", name, "err", terr)
	}

	agent := newAgent(cfg)
	agent.run() // blocks forever, reconnects on failure
}
