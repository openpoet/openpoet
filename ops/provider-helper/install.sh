#!/bin/sh
set -eu

# claude-code-proxy is a community MIT-licensed compatibility bridge. Keep the
# version and archive digests pinned here so installs are reproducible and do
# not trust mutable PATH contents or a remotely supplied checksum file.
VERSION="v0.1.21"
BASE_URL="https://github.com/raine/claude-code-proxy/releases/download/$VERSION"

case "$(uname -s)-$(uname -m)" in
    Darwin-arm64)
        TARGET="darwin-arm64"
        EXPECTED_SHA256="12c340342f0dcd476a29041272eb65476c5d73054f00c9bba1ca9300020cf267"
        ;;
    Darwin-x86_64)
        TARGET="darwin-amd64"
        EXPECTED_SHA256="1b4a1259dc74da299ee2cd72832f7b18cd5b82dc056cb19cd21fd940ebd6bf1c"
        ;;
    Linux-aarch64|Linux-arm64)
        TARGET="linux-arm64"
        EXPECTED_SHA256="18b15eccca713eafb07f2016f4b4ec939684b8ff92aa69a37200d0afb828a59c"
        ;;
    Linux-x86_64|Linux-amd64)
        TARGET="linux-amd64"
        EXPECTED_SHA256="f27f01aeec673f33a1f8690137e4f736d96e66f23f7578f778083585bf486fe1"
        ;;
    *)
        echo "Unsupported platform for claude-code-proxy: $(uname -s) $(uname -m)" >&2
        exit 1
        ;;
esac

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
DESTINATION="$PROJECT_ROOT/build/claude-code-proxy"
ARCHIVE="claude-code-proxy-$TARGET.tar.gz"
TEMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/openpoet-provider-helper.XXXXXX")
trap 'rm -rf "$TEMP_DIR"' EXIT HUP INT TERM

echo "Downloading claude-code-proxy $VERSION for $TARGET..."
curl -fsSL "$BASE_URL/$ARCHIVE" -o "$TEMP_DIR/$ARCHIVE"

if command -v shasum >/dev/null 2>&1; then
    ACTUAL_SHA256=$(shasum -a 256 "$TEMP_DIR/$ARCHIVE" | awk '{print $1}')
elif command -v sha256sum >/dev/null 2>&1; then
    ACTUAL_SHA256=$(sha256sum "$TEMP_DIR/$ARCHIVE" | awk '{print $1}')
else
    echo "A SHA-256 utility (shasum or sha256sum) is required." >&2
    exit 1
fi

if [ "$ACTUAL_SHA256" != "$EXPECTED_SHA256" ]; then
    echo "Checksum mismatch for $ARCHIVE" >&2
    exit 1
fi

tar -xzf "$TEMP_DIR/$ARCHIVE" -C "$TEMP_DIR"
if [ ! -f "$TEMP_DIR/claude-code-proxy" ]; then
    echo "Downloaded archive does not contain claude-code-proxy." >&2
    exit 1
fi

mkdir -p "$PROJECT_ROOT/build"
install -m 0755 "$TEMP_DIR/claude-code-proxy" "$DESTINATION"
echo "Installed verified bridge: $DESTINATION"
