#!/usr/bin/env bash
# install-agent.sh — set up bridge-agent on this device.
#
# Usage:
#   bash install-agent.sh
#
# What it does:
#   1. Creates agent.yaml from agent.yaml.example if not already present.
#   2. Uses zero-config defaults for device identity and gateway routing.
#   3. Verifies tmux is installed.
#   4. Optionally installs bridge-agent as a startup service
#      (launchd on macOS, systemd on Linux).
#
# This works both from:
#   - an extracted release directory, where bridge-agent sits beside this script
#   - a package-manager install, where bridge-agent is on PATH and config lives in ~/.bridge-agent

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_AGENT_HOME="$SCRIPT_DIR"

if [[ -f "$SCRIPT_DIR/bridge-agent" ]]; then
  DEFAULT_AGENT_BIN="$SCRIPT_DIR/bridge-agent"
else
  DEFAULT_AGENT_BIN="$(command -v bridge-agent || true)"
  DEFAULT_AGENT_HOME="$HOME/.bridge-agent"
fi

AGENT_HOME="${BRIDGE_AGENT_HOME:-$DEFAULT_AGENT_HOME}"
AGENT_BIN="${BRIDGE_AGENT_BIN:-$DEFAULT_AGENT_BIN}"
EXAMPLE_YAML="${BRIDGE_AGENT_EXAMPLE:-$SCRIPT_DIR/agent.yaml.example}"
AGENT_YAML="${BRIDGE_AGENT_CONFIG:-$AGENT_HOME/agent.yaml}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

info()    { printf '\033[0;34m[bridge-agent]\033[0m %s\n' "$*"; }
success() { printf '\033[0;32m[bridge-agent]\033[0m %s\n' "$*"; }
warn()    { printf '\033[0;33m[bridge-agent]\033[0m %s\n' "$*"; }
err()     { printf '\033[0;31m[bridge-agent]\033[0m %s\n' "$*" >&2; }

detect_default_tool() {
  for tool in codex claude openclaw; do
    if command -v "$tool" >/dev/null 2>&1; then
      echo "$tool"
      return
    fi
  done
  echo "codex"
}

# ---------------------------------------------------------------------------
# Service install helpers
# ---------------------------------------------------------------------------

install_launchd() {
  local plist_dir="$HOME/Library/LaunchAgents"
  local plist_path="$plist_dir/com.bridge-agent.plist"
  mkdir -p "$plist_dir"

  info "Installing launchd plist: $plist_path"
  cat > "$plist_path" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.bridge-agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>$AGENT_BIN</string>
    <string>-config</string>
    <string>$AGENT_YAML</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>$HOME/.bridge-agent.log</string>
  <key>StandardErrorPath</key>
  <string>$HOME/.bridge-agent.log</string>
</dict>
</plist>
EOF

  launchctl load "$plist_path" 2>/dev/null || true
  success "launchd service installed. bridge-agent will start on login."
  info "Log file: $HOME/.bridge-agent.log"
  info "To stop:  launchctl unload $plist_path"
}

install_systemd() {
  local service_path="/etc/systemd/system/bridge-agent.service"
  local current_user
  current_user="$(id -un)"

  info "Installing systemd service: $service_path"
  if ! sudo tee "$service_path" > /dev/null <<EOF
[Unit]
Description=bridge-agent — BridgeAIChat device agent
After=network.target

[Service]
Type=simple
User=$current_user
ExecStart=$AGENT_BIN -config $AGENT_YAML
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
  then
    err "Failed to write service file (sudo required). Install manually."
    return
  fi

  sudo systemctl daemon-reload
  sudo systemctl enable bridge-agent
  sudo systemctl start bridge-agent
  success "systemd service installed and started."
  info "Check status: sudo systemctl status bridge-agent"
  info "View logs:    sudo journalctl -u bridge-agent -f"
  info "To stop:      sudo systemctl stop bridge-agent"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "Starting bridge-agent installer"
echo

if [[ -z "$AGENT_BIN" || ! -f "$AGENT_BIN" ]]; then
  err "bridge-agent binary not found."
  err "Expected either a local ./bridge-agent beside this installer or bridge-agent on PATH."
  exit 1
fi

chmod +x "$AGENT_BIN"
mkdir -p "$AGENT_HOME"

# Check tmux.
if ! command -v tmux &>/dev/null; then
  warn "tmux is not installed. bridge-agent requires tmux to run AI CLIs."
  echo
  if [[ "$(uname)" == "Darwin" ]]; then
    warn "Install with: brew install tmux"
  else
    warn "Install with: sudo apt-get install tmux   (Debian/Ubuntu)"
    warn "          or: sudo dnf install tmux        (Fedora/RHEL)"
  fi
  echo
  read -r -p "  Continue installer anyway? [y/N] " continue_anyway
  if [[ "${continue_anyway,,}" != "y" ]]; then
    info "Install tmux first, then re-run this script."
    exit 1
  fi
else
  success "tmux is installed: $(tmux -V)"
fi
echo

# Create agent.yaml if it doesn't exist.
CREATED_CONFIG=false
if [[ -f "$AGENT_YAML" ]]; then
  warn "agent.yaml already exists — skipping config creation."
  warn "Edit $AGENT_YAML manually if you need to change settings."
else
  if [[ ! -f "$EXAMPLE_YAML" ]]; then
    err "agent.yaml.example not found. Cannot create config."
    exit 1
  fi

  info "Creating agent.yaml with zero-config defaults..."
  DEFAULT_TOOL="$(detect_default_tool)"
  DEFAULT_GATEWAY_URL="${BRIDGE_AGENT_GATEWAY_URL:-${BRIDGE_GATEWAY_URL:-wss://bridgeai.dev/agent}}"

  cp "$EXAMPLE_YAML" "$AGENT_YAML"
  sed -i.bak "s|^default_tool: .*|default_tool: $DEFAULT_TOOL|" "$AGENT_YAML"
  rm -f "$AGENT_YAML.bak"

  if ! grep -q '^gateway:$' "$AGENT_YAML"; then
    printf '\n# Optional explicit gateway override.\ngateway:\n  url: %s\n' "$DEFAULT_GATEWAY_URL" >> "$AGENT_YAML"
  fi

  success "Created $AGENT_YAML"
  info "Default tool: $DEFAULT_TOOL"
  info "Gateway URL:  $DEFAULT_GATEWAY_URL"
  info "Device identity will be derived automatically from hostname and tailnet."
  CREATED_CONFIG=true
fi
echo

# Optional: install as a startup service.
read -r -p "  Install bridge-agent as a startup service? [y/N] " install_service
echo

if [[ "${install_service,,}" == "y" ]]; then
  OS="$(uname)"
  if [[ "$OS" == "Darwin" ]]; then
    install_launchd
  elif [[ "$OS" == "Linux" ]]; then
    install_systemd
  else
    warn "Unsupported OS for automatic service install: $OS"
    warn "Start the agent manually: $AGENT_BIN -config $AGENT_YAML"
  fi
fi

# Done.
echo
success "Installation complete."
echo
info "To start the agent now:"
echo "    $AGENT_BIN -config $AGENT_YAML"
echo
if [[ "$CREATED_CONFIG" == "true" ]]; then
  info "Review agent.yaml if you want to override the default tool, working directory,"
  info "or gateway URL. Device identity and tool badges will auto-register on startup."
  echo "    $AGENT_YAML"
fi
