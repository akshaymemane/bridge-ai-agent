#!/usr/bin/env bash
# install-agent.sh — set up bridge-agent on this device.
#
# Usage (from the extracted release tarball directory):
#   bash install-agent.sh
#
# What it does:
#   1. Creates agent.yaml from agent.yaml.example if not already present.
#   2. Interactively fills in device.id, device.name, and gateway.url.
#   3. Verifies tmux is installed.
#   4. Optionally installs bridge-agent as a startup service
#      (launchd on macOS, systemd on Linux).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AGENT_BIN="$SCRIPT_DIR/bridge-agent"
EXAMPLE_YAML="$SCRIPT_DIR/agent.yaml.example"
AGENT_YAML="$SCRIPT_DIR/agent.yaml"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

info()    { printf '\033[0;34m[bridge-agent]\033[0m %s\n' "$*"; }
success() { printf '\033[0;32m[bridge-agent]\033[0m %s\n' "$*"; }
warn()    { printf '\033[0;33m[bridge-agent]\033[0m %s\n' "$*"; }
err()     { printf '\033[0;31m[bridge-agent]\033[0m %s\n' "$*" >&2; }

prompt() {
  local label="$1"
  local default="${2:-}"
  local value
  if [[ -n "$default" ]]; then
    read -r -p "  $label [$default]: " value
    echo "${value:-$default}"
  else
    read -r -p "  $label: " value
    echo "$value"
  fi
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

if [[ ! -f "$AGENT_BIN" ]]; then
  err "bridge-agent binary not found at $AGENT_BIN"
  err "Make sure you are running this script from the extracted release directory."
  exit 1
fi

chmod +x "$AGENT_BIN"

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

  info "Creating agent.yaml..."
  echo

  HOSTNAME_DEFAULT="$(hostname -s 2>/dev/null | tr '[:upper:]' '[:lower:]' | tr ' ' '-' || echo "my-device")"
  DEVICE_ID="$(prompt "device.id  (unique, lowercase, no spaces)" "$HOSTNAME_DEFAULT")"
  DEVICE_NAME="$(prompt "device.name  (display name shown in UI)" "$(hostname -s 2>/dev/null || echo 'My Device')")"
  GATEWAY_URL="$(prompt "gateway.url  (e.g. ws://100.x.x.x:8080/agent)")"

  if [[ -z "$GATEWAY_URL" ]]; then
    err "gateway.url is required."
    exit 1
  fi

  cp "$EXAMPLE_YAML" "$AGENT_YAML"
  sed -i.bak "s|^  id: .*|  id: $DEVICE_ID|" "$AGENT_YAML"
  sed -i.bak "s|^  name: .*|  name: $DEVICE_NAME|" "$AGENT_YAML"
  sed -i.bak "s|^  url: .*|  url: $GATEWAY_URL|" "$AGENT_YAML"
  rm -f "$AGENT_YAML.bak"

  success "Created $AGENT_YAML"
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
  info "Review agent.yaml before starting — set the correct 'cmd' and 'args'"
  info "for your installed AI CLI (claude, codex, ollama, etc.)."
  echo "    $AGENT_YAML"
fi
