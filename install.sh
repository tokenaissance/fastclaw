#!/bin/bash
set -euo pipefail

# FastClaw installer
# Usage: curl -fsSL https://raw.githubusercontent.com/fastclaw-ai/fastclaw/main/install.sh | bash

REPO="fastclaw-ai/fastclaw"
BINARY="fastclaw"
INSTALL_DIR="/usr/local/bin"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

info()  { echo -e "${CYAN}▸${NC} $*"; }
ok()    { echo -e "${GREEN}✓${NC} $*"; }
warn()  { echo -e "${YELLOW}!${NC} $*"; }
error() { echo -e "${RED}✗${NC} $*" >&2; exit 1; }

# Detect OS and architecture
detect_platform() {
    local os arch

    case "$(uname -s)" in
        Linux*)   os="linux" ;;
        Darwin*)  os="darwin" ;;
        MINGW*|MSYS*|CYGWIN*) os="windows" ;;
        *) error "Unsupported OS: $(uname -s)" ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64)  arch="amd64" ;;
        arm64|aarch64) arch="arm64" ;;
        armv7*)        arch="armv7" ;;
        *) error "Unsupported architecture: $(uname -m)" ;;
    esac

    echo "${os}_${arch}"
}

# Get latest release tag
get_latest_version() {
    curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
        | grep '"tag_name"' \
        | sed -E 's/.*"v?([^"]+)".*/\1/'
}

main() {
    echo ""
    echo -e "${CYAN}⚡ FastClaw Installer${NC}"
    echo ""

    # Detect platform
    local platform
    platform=$(detect_platform)
    info "Platform: ${platform}"

    # Get version
    local version
    version=$(get_latest_version 2>/dev/null || echo "")

    if [ -z "$version" ]; then
        warn "Could not fetch latest release. Building from source..."
        install_from_source
        return
    fi

    info "Version: v${version}"

    # Download
    local ext="tar.gz"
    [[ "$platform" == windows_* ]] && ext="zip"

    local filename="${BINARY}_${version}_${platform}.${ext}"
    local url="https://github.com/${REPO}/releases/download/v${version}/${filename}"

    info "Downloading ${filename}..."
    local tmpdir
    tmpdir=$(mktemp -d)
    trap "rm -rf ${tmpdir}" EXIT

    if ! curl -fsSL -o "${tmpdir}/${filename}" "$url"; then
        warn "Download failed. Building from source..."
        install_from_source
        return
    fi

    # Extract
    info "Extracting..."
    cd "$tmpdir"
    if [ "$ext" = "tar.gz" ]; then
        tar xzf "$filename"
    else
        unzip -q "$filename"
    fi

    # Install
    local bin="${BINARY}"
    [[ "$platform" == windows_* ]] && bin="${BINARY}.exe"

    if [ -w "$INSTALL_DIR" ]; then
        mv "$bin" "${INSTALL_DIR}/${bin}"
    else
        info "Need sudo to install to ${INSTALL_DIR}"
        sudo mv "$bin" "${INSTALL_DIR}/${bin}"
    fi

    chmod +x "${INSTALL_DIR}/${bin}"

    ok "FastClaw installed to ${INSTALL_DIR}/${bin}"
    echo ""

    # Setup config
    setup_config

    echo ""
    echo -e "${GREEN}⚡ FastClaw is ready!${NC}"
    echo ""
    echo "  Run:  fastclaw gateway"
    echo "  Docs: https://fastclaw.ai/docs"
    echo ""
}

install_from_source() {
    if ! command -v go &>/dev/null; then
        error "Go is not installed. Install Go 1.25+ from https://go.dev/dl/"
    fi

    info "Installing from source..."
    go install "github.com/${REPO}/cmd/fastclaw@latest"
    ok "FastClaw installed via 'go install'"
}

setup_config() {
    local config_dir="$HOME/.fastclaw"
    local config_file="${config_dir}/fastclaw.json"

    if [ -f "$config_file" ]; then
        ok "Config already exists: ${config_file}"
        return
    fi

    info "Creating default config..."
    mkdir -p "$config_dir"

    cat > "$config_file" << 'CFGEOF'
{
  "providers": {
    "openai": {
      "apiKey": "YOUR_API_KEY_HERE",
      "apiBase": "https://api.openai.com/v1"
    }
  },
  "agents": {
    "defaults": {
      "model": "gpt-4o",
      "maxTokens": 8192,
      "temperature": 0.7,
      "maxToolIterations": 20
    },
    "list": [
      { "id": "main", "workspace": "~/.fastclaw/agents/main/agent" }
    ]
  },
  "channels": {
    "telegram": {
      "enabled": true,
      "accounts": {
        "default": {
          "botToken": "YOUR_TELEGRAM_BOT_TOKEN"
        }
      }
    }
  }
}
CFGEOF

    # Create workspace directories
    mkdir -p "${config_dir}/agents/main/agent/sessions"
    mkdir -p "${config_dir}/agents/main/agent/memory"
    mkdir -p "${config_dir}/skills"

    # Create default workspace files
    [ ! -f "${config_dir}/agents/main/agent/SOUL.md" ] && cat > "${config_dir}/agents/main/agent/SOUL.md" << 'EOF'
# Soul

I am a helpful AI assistant.

## Personality
- Concise and practical
- Friendly but professional
- Action-oriented: prefer doing over discussing

## Communication Style
- Default to the user's language
- Use markdown when helpful
- Ask clarifying questions when the request is ambiguous
EOF

    [ ! -f "${config_dir}/agents/main/agent/AGENTS.md" ] && cat > "${config_dir}/agents/main/agent/AGENTS.md" << 'EOF'
# Agent Instructions

- Write clean, readable responses
- Provide step-by-step guidance for complex tasks
- Use tools when actions are needed, don't just describe what to do
EOF

    ok "Config created: ${config_file}"
    warn "Edit the config to add your API key and Telegram bot token"
}

main "$@"
