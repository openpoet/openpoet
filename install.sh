#!/bin/sh
# OpenPoet installer
# Usage: curl -fsSL https://raw.githubusercontent.com/openpoet/openpoet/main/install.sh | sh
#
# Uninstall:
#   curl -fsSL https://raw.githubusercontent.com/openpoet/openpoet/main/install.sh | sh -s -- --uninstall
#
# Custom install directory:
#   curl -fsSL https://... | OPENPOET_INSTALL=/custom/path sh
#
# Environment variables:
#   OPENPOET_VERSION  - specific version to install (default: latest)
#   OPENPOET_INSTALL  - install directory (default: ~/.local/bin)

set -e

REPO="openpoet/openpoet"
INSTALL_DIR="${OPENPOET_INSTALL:-$HOME/.local/bin}"
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
        MINGW*|MSYS*|CYGWIN*) error "On Windows, download the .zip from https://github.com/openpoet/openpoet/releases" ;;
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

# Detect shell profile and offer to add INSTALL_DIR to PATH
ensure_path_setup() {
    # Determine the user's shell and profile file
    SHELL_NAME="$(basename "${SHELL:-/bin/sh}")"
    case "$SHELL_NAME" in
        zsh)
            PROFILE="$HOME/.zshrc"
            ;;
        bash)
            # macOS uses .bash_profile for login shells
            if [ "$(uname -s)" = "Darwin" ]; then
                PROFILE="$HOME/.bash_profile"
            else
                PROFILE="$HOME/.bashrc"
            fi
            ;;
        *)
            PROFILE="$HOME/.profile"
            ;;
    esac

    PATH_LINE="export PATH=\"${INSTALL_DIR}:\$PATH\""

    # Check if the profile already contains a PATH entry for this directory
    if [ -f "$PROFILE" ] && grep -q "${INSTALL_DIR}" "$PROFILE" 2>/dev/null; then
        warn "${INSTALL_DIR} appears to be in ${PROFILE} but not in your current PATH."
        warn "Restart your shell or run: source ${PROFILE}"
        return
    fi

    # Try to prompt the user interactively
    if [ -e /dev/tty ]; then
        printf "${YELLOW}[openpoet]${RESET} %s" "${INSTALL_DIR} is not in your PATH. Add it to ${PROFILE}? [y/N] "
        read -r REPLY < /dev/tty
        case "$REPLY" in
            [yY]|[yY][eE][sS])
                printf '\n# OpenPoet\n%s\n' "$PATH_LINE" >> "$PROFILE"
                info "Added to ${PROFILE}. Restart your shell or run:"
                info "  source ${PROFILE}"
                return
                ;;
        esac
    fi

    # Non-interactive or user declined — print manual instructions
    warn "${INSTALL_DIR} is not in your PATH. Add it with:"
    warn "  ${PATH_LINE}"
}

uninstall() {
    BINARY="${INSTALL_DIR}/openpoet"
    DATA_DIR="$HOME/.openpoet"

    # Remove binary
    if [ -f "$BINARY" ]; then
        rm "$BINARY"
        info "Removed ${BINARY}"
    else
        warn "Binary not found at ${BINARY}"
    fi

    # Ask about data directory
    if [ -d "$DATA_DIR" ]; then
        if [ -e /dev/tty ]; then
            printf "${YELLOW}[openpoet]${RESET} %s" "Remove data directory ${DATA_DIR}? This deletes your database and settings. [y/N] "
            read -r REPLY < /dev/tty
            case "$REPLY" in
                [yY]|[yY][eE][sS])
                    rm -rf "$DATA_DIR"
                    info "Removed ${DATA_DIR}"
                    ;;
                *)
                    info "Kept ${DATA_DIR}"
                    ;;
            esac
        else
            warn "Data directory ${DATA_DIR} was not removed (non-interactive mode)."
            warn "Remove it manually with: rm -rf ${DATA_DIR}"
        fi
    fi

    info "OpenPoet has been uninstalled."
}

main() {
    OS=$(detect_os)
    ARCH=$(detect_arch)

    # Check for existing installation
    EXISTING=$(command -v openpoet 2>/dev/null || true)
    if [ -n "$EXISTING" ]; then
        EXISTING_VER=$(openpoet version 2>/dev/null || echo "unknown")
        case "$EXISTING" in
            */Cellar/*|*/homebrew/*)
                warn "OpenPoet is already installed via Homebrew at ${EXISTING} (${EXISTING_VER})."
                warn "Use 'brew upgrade openpoet' to update, or 'brew uninstall openpoet' first."
                error "Aborting to avoid conflicting installations."
                ;;
            *)
                warn "OpenPoet is already installed at ${EXISTING} (${EXISTING_VER})."
                if [ -e /dev/tty ]; then
                    printf "${YELLOW}[openpoet]${RESET} %s" "Proceed with reinstall/update? [y/N] "
                    read -r REPLY < /dev/tty
                    case "$REPLY" in
                        [yY]|[yY][eE][sS]) ;;
                        *) info "Aborted."; exit 0 ;;
                    esac
                fi
                ;;
        esac
    fi

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

    # Create install directory if needed
    mkdir -p "$INSTALL_DIR"

    # Install binary
    if [ -w "$INSTALL_DIR" ]; then
        mv "${TMPDIR}/openpoet" "${INSTALL_DIR}/openpoet"
    else
        error "Cannot write to ${INSTALL_DIR}. Set OPENPOET_INSTALL to a writable directory."
    fi
    chmod +x "${INSTALL_DIR}/openpoet"

    info "Installed openpoet to ${INSTALL_DIR}/openpoet"

    # Check if install dir is already in PATH
    case ":${PATH}:" in
        *":${INSTALL_DIR}:"*) ;; # already in PATH, nothing to do
        *)
            ensure_path_setup
            ;;
    esac

    info ""
    info "Run 'openpoet' to start. Web UI: http://localhost:8080"
}

case "${1:-}" in
    --uninstall) uninstall ;;
    *)           main ;;
esac
