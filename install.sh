#!/usr/bin/env bash
set -euo pipefail

REPO="${REPO:-netquirk/mon-agent}"
BASE_URL="${BASE_URL:-https://api.github.com/repos/${REPO}}"
RAW_TAG="${TAG:-latest}"
MONITOR_ID="${MONITOR_ID:-}"
INTERVAL_SECONDS="${INTERVAL_SECONDS:-}"
LOCATION="${LOCATION:-agent}"
DISK_PATHS="${DISK_PATHS:-/,/tmp}"
SERVICE_NAME="${SERVICE_NAME:-mon-agent}"

usage() {
  cat <<'USAGE'
Usage:
  sudo ./install.sh -id <monitor_uuid> -interval <seconds> [-location agent] [-disk-paths "/,/tmp"] [-service-name mon-agent] [-tag latest|1.2.3]

Environment alternatives:
  MONITOR_ID, INTERVAL_SECONDS, LOCATION, DISK_PATHS, SERVICE_NAME, TAG
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -id|--id)
      MONITOR_ID="${2:-}"
      shift 2
      ;;
    -interval|--interval)
      INTERVAL_SECONDS="${2:-}"
      shift 2
      ;;
    -location|--location)
      LOCATION="${2:-}"
      shift 2
      ;;
    -disk-paths|--disk-paths)
      DISK_PATHS="${2:-}"
      shift 2
      ;;
    -service-name|--service-name)
      SERVICE_NAME="${2:-}"
      shift 2
      ;;
    -tag|--tag)
      RAW_TAG="${2:-latest}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 1
      ;;
  esac
done

if [[ "${EUID}" -ne 0 ]]; then
  echo "Please run as root (use sudo)." >&2
  exit 1
fi

if [[ -z "${MONITOR_ID}" ]]; then
  echo "Monitor ID is required (-id or MONITOR_ID)." >&2
  exit 1
fi

if [[ -z "${INTERVAL_SECONDS}" ]]; then
  echo "Interval is required (-interval or INTERVAL_SECONDS)." >&2
  exit 1
fi

if ! [[ "${INTERVAL_SECONDS}" =~ ^[0-9]+$ ]] || [[ "${INTERVAL_SECONDS}" -lt 5 ]]; then
  echo "Interval must be an integer >= 5." >&2
  exit 1
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH_RAW="$(uname -m)"
case "${ARCH_RAW}" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: ${ARCH_RAW}" >&2
    exit 1
    ;;
esac

if [[ "${OS}" != "linux" ]]; then
  echo "This installer currently supports Linux only (detected ${OS})." >&2
  exit 1
fi

if [[ "${RAW_TAG}" == "latest" ]]; then
  TAG_NAME="$(curl -fsSL "${BASE_URL}/releases/latest" | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
  if [[ -z "${TAG_NAME}" ]]; then
    echo "Failed to resolve latest release tag from GitHub." >&2
    exit 1
  fi
else
  TAG_NAME="${RAW_TAG}"
  if [[ "${TAG_NAME}" != agent-v* ]]; then
    TAG_NAME="agent-v${TAG_NAME#v}"
  fi
fi

VERSION="${TAG_NAME#agent-v}"
VERSION="${VERSION#v}"
ASSET_NAME="mon-agent_${VERSION}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${TAG_NAME}/${ASSET_NAME}"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

echo "Downloading ${ASSET_NAME} from ${DOWNLOAD_URL}"
curl -fsSL "${DOWNLOAD_URL}" -o "${TMP_DIR}/agent.tar.gz"

tar -xzf "${TMP_DIR}/agent.tar.gz" -C "${TMP_DIR}"
BIN_PATH="$(find "${TMP_DIR}" -type f -name mon-agent | head -n 1)"
if [[ -z "${BIN_PATH}" ]]; then
  echo "Could not find mon-agent binary in downloaded archive." >&2
  exit 1
fi

if command -v systemctl >/dev/null 2>&1; then
  if systemctl list-unit-files "${SERVICE_NAME}.service" >/dev/null 2>&1; then
    if systemctl is-active --quiet "${SERVICE_NAME}.service"; then
      echo "Stopping ${SERVICE_NAME}.service before binary upgrade"
      systemctl stop "${SERVICE_NAME}.service"
    fi
  fi
fi

install -m 0755 "${BIN_PATH}" /usr/local/bin/mon-agent

echo "Installing and enabling ${SERVICE_NAME}.service"
"${BIN_PATH}" \
  -install \
  -id "${MONITOR_ID}" \
  -interval "${INTERVAL_SECONDS}" \
  -location "${LOCATION}" \
  -disk-paths "${DISK_PATHS}" \
  -service-name "${SERVICE_NAME}"

echo "Installed mon-agent ${VERSION} successfully."
