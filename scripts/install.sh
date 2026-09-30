#!/usr/bin/env bash
set -Eeuo pipefail

REPOSITORY="${1:-}"
if [[ ! "$REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  echo "Usage: install.sh OWNER/REPOSITORY" >&2
  exit 2
fi
if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  echo "Run this installer as root (for example, with sudo)." >&2
  exit 1
fi
for required_command in curl sha256sum awk install systemctl useradd getent; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "Required command not found: $required_command" >&2
    exit 1
  fi
done
if [[ ! -d /run/systemd/system ]]; then
  echo "A running systemd system is required." >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64) ASSET_SUFFIX="amd64" ;;
  aarch64|arm64) ASSET_SUFFIX="arm64" ;;
  armv7l|armv7|armhf) ASSET_SUFFIX="armv7" ;;
  *)
    echo "Unsupported CPU architecture: $(uname -m). Supported: amd64, arm64, armv7." >&2
    exit 1
    ;;
esac

ASSET_NAME="autohost-daemon-linux-${ASSET_SUFFIX}"
RELEASE_URL="https://github.com/${REPOSITORY}/releases/latest/download"
BINARY_PATH="/usr/local/bin/autohost-daemon"
SERVICE_PATH="/etc/systemd/system/autohost.service"
UPDATE_PATH="/usr/local/bin/autohost-update"
CONFIG_DIR="/etc/autohost"
DATA_DIR="/var/lib/autohost/data"
TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TEMP_DIR"' EXIT

curl --fail --location --silent --show-error --retry 3 \
  "${RELEASE_URL}/${ASSET_NAME}" --output "${TEMP_DIR}/${ASSET_NAME}"
curl --fail --location --silent --show-error --retry 3 \
  "${RELEASE_URL}/${ASSET_NAME}.sha256" --output "${TEMP_DIR}/${ASSET_NAME}.sha256"
curl --fail --location --silent --show-error --retry 3 \
  "${RELEASE_URL}/autohost.service" --output "${TEMP_DIR}/autohost.service"

EXPECTED_SHA256="$(awk 'NR == 1 { print $1 }' "${TEMP_DIR}/${ASSET_NAME}.sha256")"
ACTUAL_SHA256="$(sha256sum "${TEMP_DIR}/${ASSET_NAME}" | awk '{ print $1 }')"
if [[ -z "$EXPECTED_SHA256" || "$EXPECTED_SHA256" != "$ACTUAL_SHA256" ]]; then
  echo "SHA-256 verification failed for ${ASSET_NAME}." >&2
  exit 1
fi

if ! getent passwd autohost >/dev/null 2>&1; then
  useradd --system --home-dir /var/lib/autohost --no-create-home \
    --shell /usr/sbin/nologin autohost
fi
install -d -o autohost -g autohost -m 0750 /var/lib/autohost "$DATA_DIR"
install -d -o root -g root -m 0755 "$CONFIG_DIR"
printf '%s\n' "$REPOSITORY" > "${CONFIG_DIR}/repository"
chmod 0644 "${CONFIG_DIR}/repository"

HAD_BINARY=false
HAD_SERVICE=false
if [[ -f "$BINARY_PATH" ]]; then
  cp -p "$BINARY_PATH" "${BINARY_PATH}.previous"
  HAD_BINARY=true
fi
if [[ -f "$SERVICE_PATH" ]]; then
  cp -p "$SERVICE_PATH" "${SERVICE_PATH}.previous"
  HAD_SERVICE=true
fi

systemctl stop autohost >/dev/null 2>&1 || true
install -o root -g root -m 0755 "${TEMP_DIR}/${ASSET_NAME}" "$BINARY_PATH"
install -o root -g root -m 0644 "${TEMP_DIR}/autohost.service" "$SERVICE_PATH"

cat > "$UPDATE_PATH" <<'UPDATE_SCRIPT'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${EUID:-$(id -u)}" -ne 0 ]]; then
  exec sudo "$0" "$@"
fi
REPOSITORY="$(cat /etc/autohost/repository)"
curl --fail --location --silent --show-error --retry 3 \
  "https://raw.githubusercontent.com/${REPOSITORY}/main/scripts/install.sh" \
  | bash -s -- "$REPOSITORY"
UPDATE_SCRIPT
chmod 0755 "$UPDATE_PATH"

systemctl daemon-reload
systemctl enable autohost >/dev/null
if ! systemctl restart autohost; then
  echo "AutoHost failed to start. Restoring the previous installation." >&2
  if [[ "$HAD_BINARY" == true ]]; then
    install -o root -g root -m 0755 "${BINARY_PATH}.previous" "$BINARY_PATH"
  else
    rm -f "$BINARY_PATH"
  fi
  if [[ "$HAD_SERVICE" == true ]]; then
    install -o root -g root -m 0644 "${SERVICE_PATH}.previous" "$SERVICE_PATH"
  else
    rm -f "$SERVICE_PATH"
  fi
  systemctl daemon-reload
  systemctl restart autohost >/dev/null 2>&1 || true
  journalctl -u autohost -n 40 --no-pager || true
  exit 1
fi

systemctl --no-pager --full status autohost
printf '\nAutoHost is installed and enabled at boot. World data was left untouched in %s.\n' "$DATA_DIR"
printf 'To update later, run: sudo autohost-update\n'
