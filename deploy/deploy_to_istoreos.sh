#!/bin/sh
# deploy/deploy_to_istoreos.sh
# Deterministic automated deployment script for AI Agents & DevOps to deploy `reader` onto iStoreOS.

set -eu

SCRIPT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(dirname "$SCRIPT_DIR")
cd "$REPO_ROOT"

ROUTER_USER="${ROUTER_USER:-root}"
ROUTER_IP="${ROUTER_IP:-192.168.100.1}"
READER_BASE="${READER_BASE:-/mnt/sda1/reader}"
GUARDIAN_PIN="${GUARDIAN_PIN:-1234}"
PORT="${PORT:-8380}"

echo "==> [Phase 0] Probing iStoreOS target (${ROUTER_USER}@${ROUTER_IP})..."
REMOTE_ARCH=$(ssh "${ROUTER_USER}@${ROUTER_IP}" "uname -m")
echo "    Detected architecture: ${REMOTE_ARCH}"

case "$REMOTE_ARCH" in
    aarch64|arm64)
        MAKE_TARGET="build-arm64"
        BIN_PATH="dist/reader-linux-arm64"
        ;;
    x86_64|amd64)
        MAKE_TARGET="build-amd64"
        BIN_PATH="dist/reader-linux-amd64"
        ;;
    *)
        echo "ERROR: Unsupported architecture: ${REMOTE_ARCH}" >&2
        exit 1
        ;;
esac

echo "==> [Phase 1] Running E2E verification, building web SPA, cross-compiling (${MAKE_TARGET}) & packaging sample story..."
PYTHONPATH=tools python3 tools/verify_e2e.py
make web
make "${MAKE_TARGET}"
mkdir -p dist
python3 tools/story_packager.py --output dist/story_xiaoxiong_v1.zip

echo "==> [Phase 2] Verifying external disk mount and initializing directory layout at ${READER_BASE}..."
MOUNT_ROOT=$(dirname "${READER_BASE}")
ssh "${ROUTER_USER}@${ROUTER_IP}" "
    if ! grep -qs \" ${MOUNT_ROOT} \" /proc/mounts; then
        echo 'ERROR: ${MOUNT_ROOT} is not a mounted filesystem! Refusing to write to router flash.' >&2
        exit 1
    fi
    mkdir -p '${READER_BASE}/data' '${READER_BASE}/media/stories' '${READER_BASE}/media/audio' '${READER_BASE}/tmp' '${READER_BASE}/backups'
    touch '${READER_BASE}/.reader-disk'
"

echo "==> [Phase 2.2] Uploading binary, config, init scripts, and sample story package..."
scp "${BIN_PATH}" "${ROUTER_USER}@${ROUTER_IP}:${READER_BASE}/reader.new"
scp dist/story_xiaoxiong_v1.zip "${ROUTER_USER}@${ROUTER_IP}:${READER_BASE}/tmp/story_xiaoxiong_v1.zip"
scp deploy/reader.init "${ROUTER_USER}@${ROUTER_IP}:/etc/init.d/reader"
scp deploy/99-reader-hotplug "${ROUTER_USER}@${ROUTER_IP}:/etc/hotplug.d/block/99-reader"

echo "==> [Phase 3] Writing config.json, registering procd service, and configuring sysupgrade persistence..."
ssh "${ROUTER_USER}@${ROUTER_IP}" "
    if [ ! -f '${READER_BASE}/config.json' ]; then
      cat > '${READER_BASE}/config.json' <<EOF
{
  \"listen\": \"${ROUTER_IP}:${PORT}\",
  \"data_dir\": \"${READER_BASE}\",
  \"session_timeout_minutes\": 30,
  \"guardian_pin\": \"${GUARDIAN_PIN}\",
  \"backup\": {
    \"time\": \"03:30\",
    \"keep_daily\": 7,
    \"keep_weekly\": 4
  },
  \"upload_max_mb\": 200,
  \"min_free_disk_mb\": 1024
}
EOF
    else
      echo 'Existing config.json preserved.'
    fi
    sed -i 's|^BASE=.*|BASE=${READER_BASE}|g' /etc/init.d/reader
    sed -i 's|/mnt/sda1/reader|${READER_BASE}|g' /etc/hotplug.d/block/99-reader
    chmod +x '${READER_BASE}/reader.new' /etc/init.d/reader /etc/hotplug.d/block/99-reader
    /etc/init.d/reader stop 2>/dev/null || true
    mv '${READER_BASE}/reader.new' '${READER_BASE}/reader'
    grep -qxF '/etc/init.d/reader' /etc/sysupgrade.conf 2>/dev/null || echo '/etc/init.d/reader' >> /etc/sysupgrade.conf
    grep -qxF '/etc/hotplug.d/block/99-reader' /etc/sysupgrade.conf 2>/dev/null || echo '/etc/hotplug.d/block/99-reader' >> /etc/sysupgrade.conf
    /etc/init.d/reader enable
    /etc/init.d/reader start
"

echo "==> [Phase 4] Running post-deployment verification gates..."
sleep 2
ssh "${ROUTER_USER}@${ROUTER_IP}" "
    curl -fsS 'http://${ROUTER_IP}:${PORT}/api/v1/health'
    echo ''
    curl -fsS -X POST 'http://${ROUTER_IP}:${PORT}/api/v1/admin/import' \
        -H 'X-Guardian-Pin: ${GUARDIAN_PIN}' \
        -F 'package=@${READER_BASE}/tmp/story_xiaoxiong_v1.zip'
    echo ''
"
echo "==> Deployment to iStoreOS (http://${ROUTER_IP}:${PORT}) completed and verified!"
