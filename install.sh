#!/bin/sh
# llamacloudctl installer.
#
#   sh -c "$(curl -fsSL https://raw.githubusercontent.com/run-llama/llamacloudctl/main/install.sh)"
#
# Detects your OS + CPU architecture, downloads the matching release archive
# from GitHub, verifies its SHA-256 checksum, and installs the `llamacloudctl`
# binary. POSIX sh — no bash-isms — so it runs under sh, dash, ash (Alpine), zsh.
#
# Environment overrides:
#   LLAMACLOUDCTL_VERSION      release to install, e.g. v1.2.0 (default: latest)
#   LLAMACLOUDCTL_INSTALL_DIR  install location (default: /usr/local/bin, or
#                              ~/.local/bin when /usr/local/bin isn't writable)
#   LLAMACLOUDCTL_REPO         owner/repo to pull from (default: run-llama/llamacloudctl)
set -eu

REPO="${LLAMACLOUDCTL_REPO:-run-llama/llamacloudctl}"
BINARY="llamacloudctl"

# --- pretty output ---------------------------------------------------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  BOLD="$(printf '\033[1m')"; RED="$(printf '\033[31m')"
  GREEN="$(printf '\033[32m')"; YELLOW="$(printf '\033[33m')"; RESET="$(printf '\033[0m')"
else
  BOLD=""; RED=""; GREEN=""; YELLOW=""; RESET=""
fi
info()  { printf '%s==>%s %s\n' "$BOLD" "$RESET" "$1"; }
warn()  { printf '%s==>%s %s\n' "$YELLOW" "$RESET" "$1" >&2; }
ok()    { printf '%s==>%s %s\n' "$GREEN" "$RESET" "$1"; }
die()   { printf '%serror:%s %s\n' "$RED" "$RESET" "$1" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

# --- pick a downloader -----------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  DL="curl -fsSL"; DLO="curl -fsSL -o"
elif command -v wget >/dev/null 2>&1; then
  DL="wget -qO-"; DLO="wget -qO"
else
  die "need either curl or wget installed"
fi
need tar

# --- detect OS -------------------------------------------------------------
os="$(uname -s)"
case "$os" in
  Linux)  OS="linux" ;;
  Darwin) OS="darwin" ;;
  MINGW*|MSYS*|CYGWIN*) die "Windows detected — download the .zip from https://github.com/$REPO/releases and add it to PATH manually" ;;
  *) die "unsupported OS: $os" ;;
esac

# --- detect arch -----------------------------------------------------------
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)        ARCH="amd64" ;;
  aarch64|arm64)       ARCH="arm64" ;;
  *) die "unsupported architecture: $arch (llamacloudctl ships amd64 and arm64)" ;;
esac

# --- resolve version -------------------------------------------------------
VERSION="${LLAMACLOUDCTL_VERSION:-latest}"
if [ "$VERSION" = "latest" ]; then
  info "Resolving latest release of $REPO..."
  # The redirect target of /releases/latest ends in the tag — no jq needed.
  VERSION="$($DL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
  [ -n "$VERSION" ] || die "could not determine the latest release (is one published yet?)"
fi
# Archive names use the version without the leading 'v'.
NUM_VERSION="${VERSION#v}"

ARCHIVE="${BINARY}_${NUM_VERSION}_${OS}_${ARCH}.tar.gz"
BASE_URL="https://github.com/$REPO/releases/download/$VERSION"

# --- choose install dir ----------------------------------------------------
if [ -n "${LLAMACLOUDCTL_INSTALL_DIR:-}" ]; then
  INSTALL_DIR="$LLAMACLOUDCTL_INSTALL_DIR"
elif [ -w /usr/local/bin ] 2>/dev/null; then
  INSTALL_DIR="/usr/local/bin"
elif [ "$(id -u)" = "0" ]; then
  INSTALL_DIR="/usr/local/bin"
else
  INSTALL_DIR="$HOME/.local/bin"
fi

# --- download + verify + install ------------------------------------------
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

info "Downloading $ARCHIVE ($VERSION, $OS/$ARCH)..."
$DLO "$TMP/$ARCHIVE" "$BASE_URL/$ARCHIVE" \
  || die "download failed: $BASE_URL/$ARCHIVE (no build for $OS/$ARCH in $VERSION?)"

info "Verifying checksum..."
if $DLO "$TMP/checksums.txt" "$BASE_URL/checksums.txt" 2>/dev/null; then
  if command -v sha256sum >/dev/null 2>&1; then
    sha_cmd="sha256sum"
  elif command -v shasum >/dev/null 2>&1; then
    sha_cmd="shasum -a 256"
  else
    sha_cmd=""
    warn "no sha256sum/shasum available; skipping checksum verification"
  fi
  if [ -n "$sha_cmd" ]; then
    expected="$(grep " ${ARCHIVE}\$" "$TMP/checksums.txt" | awk '{print $1}')"
    [ -n "$expected" ] || die "no checksum entry for $ARCHIVE in checksums.txt"
    actual="$(cd "$TMP" && $sha_cmd "$ARCHIVE" | awk '{print $1}')"
    [ "$expected" = "$actual" ] || die "checksum mismatch for $ARCHIVE (expected $expected, got $actual)"
    ok "Checksum verified."
  fi
else
  warn "checksums.txt not found for $VERSION; skipping checksum verification"
fi

info "Extracting..."
tar -xzf "$TMP/$ARCHIVE" -C "$TMP"
[ -f "$TMP/$BINARY" ] || die "archive did not contain the $BINARY binary"
chmod +x "$TMP/$BINARY"

mkdir -p "$INSTALL_DIR" 2>/dev/null || true
if [ -w "$INSTALL_DIR" ]; then
  mv "$TMP/$BINARY" "$INSTALL_DIR/$BINARY"
elif command -v sudo >/dev/null 2>&1; then
  warn "$INSTALL_DIR is not writable; using sudo"
  sudo mkdir -p "$INSTALL_DIR"
  sudo mv "$TMP/$BINARY" "$INSTALL_DIR/$BINARY"
else
  die "cannot write to $INSTALL_DIR and sudo is unavailable; set LLAMACLOUDCTL_INSTALL_DIR to a writable path"
fi

ok "Installed $BINARY $VERSION to $INSTALL_DIR/$BINARY"

# --- PATH hint -------------------------------------------------------------
case ":$PATH:" in
  *":$INSTALL_DIR:"*) : ;;
  *) warn "$INSTALL_DIR is not on your PATH. Add it, e.g.:"
     printf '      export PATH="%s:$PATH"\n' "$INSTALL_DIR" >&2 ;;
esac

if command -v "$BINARY" >/dev/null 2>&1; then
  printf '\n'
  "$BINARY" version 2>/dev/null || true
  info "Run '$BINARY --help' to get started."
fi
