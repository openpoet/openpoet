#!/bin/sh
# OpenPoet installer
# Usage: curl -fsSL https://raw.githubusercontent.com/openpoet/openpoet/main/install.sh | sh
#
# Environment variables:
#   OPENPOET_VERSION  - specific version to install (default: latest)
#   OPENPOET_INSTALL  - install directory (default: /usr/local/bin)

set -e

REPO="openpoet/openpoet"
INSTALL_DIR="${OPENPOET_INSTALL:-/usr/local/bin}"
VERSION="${OPENPOET_VERSION:-}"

# Colors (only if terminal supports them)
RED=""
GREEN=""
YELLOW=""
RESET=""
if [ -t 1 ]; then
    RED="\033[0;31m"
    GREEN="\033[0;32m"
    YELLOW="\033[0;33m"
    RESET="\033[0m"
fi

info()  { printf "${GREEN}[openpoet]${RESET} %s\n" "$1"; }
warn()  { printf "${YELLOW}[openpoet]${RESET} %s\n" "$1"; }
error() { printf "${RED}[openpoet]${RESET} %s\n" "$1" >&2; exit 1; }

# Detect OS
detect_os() {
    case "$(uname -s)" in
        Linux*)  echo "linux" ;;
        Darwin*) echo "darwin" ;;
        MINGW*|MSYS*|CYGWIN*) error "On Windows, use: winget install openpoet" ;;
        *)       error "Unsupported operating system: $(uname -s)" ;;
    esac
}

# Detect architecture
detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)   echo "amd64" ;;
        aarch64|arm64)  echo "arm64" ;;
        *)              error "Unsupported architecture: $(uname -m)" ;;
    esac
}

# Get latest version from GitHub API
get_latest_version() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | \
            grep '"tag_name"' | sed -E 's/.*"v([^"]+)".*/\1/'
    elif command -v wget >/dev/null 2>&1; then
        wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" | \
            grep '"tag_name"' | sed -E 's/.*"v([^"]+)".*/\1/'
    else
        error "curl or wget is required"
    fi
}

# Download file
download() {
    url="$1"
    dest="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$dest"
    elif command -v wget >/dev/null 2>&1; then
        wget -q "$url" -O "$dest"
    fi
}

main() {
    OS=$(detect_os)
    ARCH=$(detect_arch)

    if [ -z "$VERSION" ]; then
        info "Fetching latest version..."
        VERSION=$(get_latest_version)
    fi

    if [ -z "$VERSION" ]; then
        error "Could not determine latest version. Set OPENPOET_VERSION manually."
    fi

    info "Installing openpoet v${VERSION} (${OS}/${ARCH})..."

    ARCHIVE="openpoet_${VERSION}_${OS}_${ARCH}.tar.gz"
    URL="https://github.com/${REPO}/releases/download/v${VERSION}/${ARCHIVE}"

    # Download to temp directory
    TMPDIR=$(mktemp -d)
    trap 'rm -rf "$TMPDIR"' EXIT

    info "Downloading ${URL}..."
    download "$URL" "${TMPDIR}/${ARCHIVE}"

    # Verify checksum
    CHECKSUM_URL="https://github.com/${REPO}/releases/download/v${VERSION}/checksums.txt"
    if download "$CHECKSUM_URL" "${TMPDIR}/checksums.txt" 2>/dev/null; then
        EXPECTED=$(grep "${ARCHIVE}" "${TMPDIR}/checksums.txt" | awk '{print $1}')
        if [ -n "$EXPECTED" ]; then
            if command -v sha256sum >/dev/null 2>&1; then
                ACTUAL=$(sha256sum "${TMPDIR}/${ARCHIVE}" | awk '{print $1}')
            elif command -v shasum >/dev/null 2>&1; then
                ACTUAL=$(shasum -a 256 "${TMPDIR}/${ARCHIVE}" | awk '{print $1}')
            fi
            if [ -n "$ACTUAL" ] && [ "$EXPECTED" != "$ACTUAL" ]; then
                error "Checksum mismatch! Expected: ${EXPECTED}, Got: ${ACTUAL}"
            fi
            info "Checksum verified."
        fi
    fi

    # Extract
    tar -xzf "${TMPDIR}/${ARCHIVE}" -C "${TMPDIR}"

    # Install binary
    if [ -w "$INSTALL_DIR" ]; then
        mv "${TMPDIR}/openpoet" "${INSTALL_DIR}/openpoet"
    else
        info "Elevated permissions required to install to ${INSTALL_DIR}"
        sudo mv "${TMPDIR}/openpoet" "${INSTALL_DIR}/openpoet"
    fi
    chmod +x "${INSTALL_DIR}/openpoet"

    info "Installed openpoet to ${INSTALL_DIR}/openpoet"

    # Verify installation
    if command -v openpoet >/dev/null 2>&1; then
        info "$(openpoet version 2>/dev/null || echo "openpoet v${VERSION}")"
    else
        warn "${INSTALL_DIR} is not in your PATH. Add it with:"
        warn "  export PATH=\"${INSTALL_DIR}:\$PATH\""
    fi

    # Check for Node.js
    if ! command -v node >/dev/null 2>&1; then
        warn ""
        warn "Node.js not found. The Claude Agent SDK provider requires Node.js 18+."
        warn "Install from: https://nodejs.org"
    fi

    info ""
    info "Run 'openpoet' to start. Web UI: http://localhost:8080"
}

main
