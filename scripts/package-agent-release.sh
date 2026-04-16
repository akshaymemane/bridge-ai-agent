#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:-v0.1.0-beta.1}"
OUT_DIR="$ROOT_DIR/dist/bridge-agent_${VERSION}"

mkdir -p "$OUT_DIR"

build_target() {
  local goos="$1"
  local goarch="$2"
  local stage_dir="$OUT_DIR/bridge-agent_${VERSION}_${goos}_${goarch}"

  mkdir -p "$stage_dir"
  (
    cd "$ROOT_DIR"
    GOOS="$goos" GOARCH="$goarch" go build -o "$stage_dir/bridge-agent" ./cmd/bridge-agent
  )

  cp "$ROOT_DIR/config/agent.example.yaml" "$stage_dir/agent.yaml.example"
  cp "$ROOT_DIR/scripts/install-agent.sh" "$stage_dir/install-agent.sh"
  chmod +x "$stage_dir/install-agent.sh"

  cat > "$stage_dir/README.md" <<EOF
# bridge-agent $VERSION

This release contains the BridgeAIChat device agent.

## Quick Setup

Extract the archive and run the installer:

\`\`\`bash
tar -xzf bridge-agent_${VERSION}_${goos}_${goarch}.tar.gz
cd bridge-agent_${VERSION}_${goos}_${goarch}
bash install-agent.sh
\`\`\`

The installer creates \`agent.yaml\`, checks for tmux, and optionally
sets up bridge-agent as a startup service (launchd on macOS, systemd on Linux).

## Manual Start

\`\`\`bash
chmod +x ./bridge-agent
./bridge-agent -config ./agent.yaml
\`\`\`

## Requirements

- tmux
- An AI CLI on this device (claude, codex, ollama, etc.)
- Network access to the BridgeAIChat gateway (Tailscale recommended)
EOF

  (
    cd "$OUT_DIR"
    tar -czf "bridge-agent_${VERSION}_${goos}_${goarch}.tar.gz" "bridge-agent_${VERSION}_${goos}_${goarch}"
  )
}

build_target darwin arm64
build_target darwin amd64
build_target linux amd64
build_target linux arm64

(
  cd "$OUT_DIR"
  shasum -a 256 ./*.tar.gz > SHA256SUMS.txt
)

echo "Agent release ready:"
echo "  $OUT_DIR"
