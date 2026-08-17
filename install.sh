#!/usr/bin/env sh
# Build and install gtme. Intentionally boring: it compiles from this checkout and
# copies one static binary into place. No curl-pipe-to-shell, no network fetch.
#
#   ./install.sh                  # installs to ~/.local/bin (or $PREFIX/bin)
#   PREFIX=/usr/local ./install.sh # may need sudo
set -eu

PREFIX="${PREFIX:-$HOME/.local}"
BIN_DIR="$PREFIX/bin"
REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
VERSION="$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)"

if ! command -v go >/dev/null 2>&1; then
  echo "install.sh: Go is required (https://go.dev/dl/); gtme needs 1.24 or newer" >&2
  exit 1
fi

case "$(uname -s)" in
  Darwin|Linux) ;;
  *) echo "install.sh: v0 supports macOS and Linux only" >&2; exit 1 ;;
esac

echo "building gtme $VERSION"
mkdir -p "$BIN_DIR"
( cd "$REPO_DIR" && go build \
    -ldflags "-X github.com/elegant-atomics/gtme/internal/cli.Version=$VERSION" \
    -o "$BIN_DIR/gtme" ./cmd/gtme )

# Install the external adapters that ship with the repo, so `mock-enrich-py`
# resolves without setting GTME_ADAPTER_PATH.
ADAPTER_DIR="${GTME_HOME:-$HOME/.gtme}/adapters"
mkdir -p "$ADAPTER_DIR"
for dir in "$REPO_DIR"/adapters/*/; do
  [ -d "$dir" ] || continue
  name="$(basename "$dir")"
  mkdir -p "$ADAPTER_DIR/$name"
  cp "$dir/manifest.json" "$ADAPTER_DIR/$name/manifest.json"
  cp "$dir/run" "$ADAPTER_DIR/$name/run"
  chmod +x "$ADAPTER_DIR/$name/run"
  echo "installed adapter $name"
done

"$BIN_DIR/gtme" init

echo
echo "gtme installed: $BIN_DIR/gtme"
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "note: $BIN_DIR is not on your PATH — add it to your shell profile" ;;
esac
echo "next: gtme plan examples/apollo-to-instantly.yaml"
