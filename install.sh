#!/bin/sh
set -e

REPO="iluxav/etunl-cli"
INSTALL_DIR="/usr/local/bin"
BINARY="etunl"

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  linux)  OS="linux" ;;
  darwin) OS="darwin" ;;
  *) echo "Unsupported OS: $OS"; exit 1 ;;
esac

# Detect architecture
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  aarch64|arm64)  ARCH="arm64" ;;
  armv7l|armhf)   ARCH="armv7" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Get latest version
if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
  if [ -z "$VERSION" ]; then
    echo "Failed to determine latest version"
    exit 1
  fi
fi

FILENAME="${BINARY}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${FILENAME}"

echo "Downloading etunl ${VERSION} for ${OS}/${ARCH}..."
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

curl -fsSL "$URL" -o "${TMPDIR}/${FILENAME}"

# Verify checksum if sha256sum is available
SHA_URL="${URL}.sha256"
if command -v sha256sum > /dev/null 2>&1; then
  curl -fsSL "$SHA_URL" -o "${TMPDIR}/checksum.sha256"
  cd "$TMPDIR"
  sha256sum -c checksum.sha256
  cd - > /dev/null
elif command -v shasum > /dev/null 2>&1; then
  EXPECTED=$(curl -fsSL "$SHA_URL" | awk '{print $1}')
  ACTUAL=$(shasum -a 256 "${TMPDIR}/${FILENAME}" | awk '{print $1}')
  if [ "$EXPECTED" != "$ACTUAL" ]; then
    echo "Checksum verification failed!"
    exit 1
  fi
  echo "Checksum OK"
fi

# Extract
tar xzf "${TMPDIR}/${FILENAME}" -C "$TMPDIR"

# Install
if [ -w "$INSTALL_DIR" ]; then
  mv "${TMPDIR}/${BINARY}_${OS}_${ARCH}" "${INSTALL_DIR}/${BINARY}"
else
  echo "Installing to ${INSTALL_DIR} (requires sudo)..."
  sudo mv "${TMPDIR}/${BINARY}_${OS}_${ARCH}" "${INSTALL_DIR}/${BINARY}"
fi

chmod +x "${INSTALL_DIR}/${BINARY}"

echo ""
echo "etunl ${VERSION} installed to ${INSTALL_DIR}/${BINARY}"
echo ""

# --- Detect existing config ---

HOME_DIR=$(eval echo "~$(whoami)")
CLIENT_CFG="${HOME_DIR}/.etunl/config.yaml"
SERVER_CFG="${HOME_DIR}/.etunl/server.yaml"
EXISTING_CONFIG=""

if [ -f "$CLIENT_CFG" ]; then
  EXISTING_CONFIG="client"
elif [ -f "$SERVER_CFG" ]; then
  EXISTING_CONFIG="server"
fi

# --- Upgrade: restart existing service ---

if [ "$OS" = "linux" ] && command -v systemctl > /dev/null 2>&1; then
  if systemctl is-active --quiet etunl 2>/dev/null; then
    echo "Existing etunl service detected. Restarting..."
    sudo systemctl restart etunl
    echo "etunl service restarted with ${VERSION}."
    exit 0
  fi
fi

# --- Skip setup if config already exists ---

if [ -n "$EXISTING_CONFIG" ]; then
  echo "Config found — skipping setup."
  MODE="$EXISTING_CONFIG"
else
  # --- Interactive setup ---

  printf "Would you like to configure etunl now? [Y/n] "
  read -r SETUP_ANSWER < /dev/tty
  case "$SETUP_ANSWER" in
    [nN]*) echo "Skipping setup. Run 'etunl init --help' to configure later."; exit 0 ;;
  esac

  # Ask for mode
  printf "Setup mode — (s)erver or (c)lient? [c] "
  read -r MODE_ANSWER < /dev/tty
  case "$MODE_ANSWER" in
    [sS]*) MODE="server" ;;
    *)     MODE="client" ;;
  esac

  if [ "$MODE" = "server" ]; then
    printf "HTTP listen port [80]: "
    read -r HTTP_PORT < /dev/tty
    HTTP_PORT="${HTTP_PORT:-80}"

    printf "TCP listen port [15432]: "
    read -r TCP_PORT < /dev/tty
    TCP_PORT="${TCP_PORT:-15432}"

    etunl init --mode server
    echo ""
    echo "Server config created. Copy the token above and use it on the client."
    echo "Start the server with: etunl server"

  else
    printf "Server address (e.g. etunl.com): "
    read -r SERVER_ADDR < /dev/tty
    if [ -z "$SERVER_ADDR" ]; then
      echo "Server address is required."
      exit 1
    fi

    printf "Auth token (from server): "
    read -r TOKEN < /dev/tty
    if [ -z "$TOKEN" ]; then
      echo "Token is required."
      exit 1
    fi

    etunl init --mode client --server "$SERVER_ADDR" "$TOKEN"
    echo ""
  fi
fi

# --- systemd service (Linux only) ---

if [ "$OS" != "linux" ]; then
  echo "Done! Start etunl with: etunl ${MODE}"
  exit 0
fi

if ! command -v systemctl > /dev/null 2>&1; then
  echo "systemd not found. Start etunl manually: etunl ${MODE}"
  exit 0
fi

printf "Install as systemd service? [Y/n] "
read -r SERVICE_ANSWER < /dev/tty
case "$SERVICE_ANSWER" in
  [nN]*) echo "Done! Start etunl with: etunl ${MODE}"; exit 0 ;;
esac

# Determine user for the service
ETUNL_USER=$(whoami)
ETUNL_HOME=$(eval echo "~${ETUNL_USER}")

if [ "$MODE" = "client" ]; then
  DASHBOARD_FLAG="--dashboard :8080"
else
  DASHBOARD_FLAG=""
fi

cat > /tmp/etunl.service <<EOF
[Unit]
Description=etunl tunnel ${MODE}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${ETUNL_USER}
ExecStart=${INSTALL_DIR}/${BINARY} ${MODE} ${DASHBOARD_FLAG}
Restart=always
RestartSec=5
Environment=HOME=${ETUNL_HOME}

[Install]
WantedBy=multi-user.target
EOF

if [ -w "/etc/systemd/system" ]; then
  mv /tmp/etunl.service /etc/systemd/system/etunl.service
else
  sudo mv /tmp/etunl.service /etc/systemd/system/etunl.service
fi

sudo systemctl daemon-reload
sudo systemctl enable etunl
sudo systemctl start etunl

echo ""
echo "etunl is running as a systemd service (${MODE} mode)."
echo ""
echo "Useful commands:"
echo "  sudo systemctl status etunl    — check status"
echo "  sudo journalctl -u etunl -f    — view logs"
echo "  sudo systemctl restart etunl   — restart"
echo "  sudo systemctl stop etunl      — stop"
if [ "$MODE" = "client" ]; then
  echo ""
  echo "Dashboard: http://localhost:8080"
fi
if [ "$MODE" = "server" ]; then
  echo ""
  echo "Health check: curl http://localhost/health"
fi
