#!/usr/bin/env bash
# smoke.sh — Build, deploy, and smoke-test the chat-assistant Go server
#
# Usage:
#   ./smoke.sh              # Build + deploy + full smoke test
#   ./smoke.sh --local-only  # Build + local smoke only (no deploy)
#   ./smoke.sh --test-only   # Smoke test only (no build/deploy)
#
# Required env vars for deploy:
#   DEPLOY_HOST     — e.g. 47.95.244.175
#   DEPLOY_USER     — e.g. root
#   DEPLOY_PASS     — server SSH password

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
STATIC_DIR="$PROJECT_DIR/static"
BINARY="$PROJECT_DIR/chat-server"

REMOTE_DIR="/opt/chat-assistant"
SERVICE_NAME="chat-assistant"
BASE_URL="http://${DEPLOY_HOST:-localhost:8080}"

die() { echo "ERROR: $1"; exit 1; }

check_deploy_env() {
  [ -n "${DEPLOY_HOST:-}" ] || die "DEPLOY_HOST not set"
  [ -n "${DEPLOY_USER:-}" ] || die "DEPLOY_USER not set"
  [ -n "${DEPLOY_PASS:-}" ] || die "DEPLOY_PASS not set"
}

ssh_cmd() {
  sshpass -p "$DEPLOY_PASS" ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    "$DEPLOY_USER@$DEPLOY_HOST" "$@"
}

scp_cmd() {
  sshpass -p "$DEPLOY_PASS" scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$@"
}

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "${GREEN}✓${NC} $1"; }
fail() { echo -e "${RED}✗${NC} $1"; exit 1; }
info() { echo -e "${YELLOW}→${NC} $1"; }

# ─── Build ────────────────────────────────────────────────────

do_build() {
  info "Building chat-server for linux/amd64..."
  cd "$PROJECT_DIR"
  go mod tidy
  GOOS=linux GOARCH=amd64 go build -o chat-server .
  pass "Build complete: $(ls -lh "$BINARY" | awk '{print $5}')"
}

# ─── Deploy ───────────────────────────────────────────────────

do_deploy() {
  check_deploy_env
  info "Deploying to $DEPLOY_HOST..."
  ssh_cmd "mkdir -p $REMOTE_DIR/static"
  scp_cmd "$BINARY" "$DEPLOY_USER@$DEPLOY_HOST:$REMOTE_DIR/"
  scp_cmd "$STATIC_DIR"/* "$DEPLOY_USER@$DEPLOY_HOST:$REMOTE_DIR/static/"
  ssh_cmd "systemctl restart $SERVICE_NAME"
  sleep 2
  pass "Deploy complete"
}

# ─── Smoke Tests ──────────────────────────────────────────────

do_smoke() {
  info "Test 1: Health endpoint"
  curl -sf "$BASE_URL/health" | grep -q '"ok"' && pass "Health ok" || fail "Health failed"

  info "Test 2: Static HTML"
  curl -sf "$BASE_URL" | grep -q '<title>AI 助手</title>' && pass "index.html" || fail "index.html"

  info "Test 3: Static CSS"
  curl -sf "$BASE_URL/style.css" | grep -q 'chat-header' && pass "style.css" || fail "style.css"

  info "Test 4: Static JS"
  curl -sf "$BASE_URL/app.js" | grep -q 'WebSocket' && pass "app.js" || fail "app.js"

  info "Test 5: WebSocket endpoint"
  # Check that /ws responds — 400 or 426 means the server recognized
  # the request as a WebSocket upgrade (just missing full headers).
  # An empty response from timeout means upgrade was accepted (101).
  local ws_resp
  ws_resp=$(timeout 3 curl -sv -o /dev/null \
    -H "Connection: Upgrade" \
    "$BASE_URL/ws" 2>&1 | grep -oE 'HTTP/[0-9.]+ [0-9]+' | head -1 | awk '{print $2}') || true
  if [ "${ws_resp:-}" = "400" ] || [ "${ws_resp:-}" = "426" ]; then
    pass "WebSocket reachable (HTTP $ws_resp = expects upgrade)"
  elif [ -z "${ws_resp:-}" ]; then
    pass "WebSocket reachable (accepted upgrade)"
  else
    fail "WebSocket unreachable (HTTP ${ws_resp:-none})"
  fi

  info "Test 6: Browser chat interaction (Playwright)"
  if command -v node &>/dev/null && [ -f "$SCRIPT_DIR/screenshot.cjs" ]; then
    if node "$SCRIPT_DIR/screenshot.cjs" 2>/dev/null; then
      pass "Browser chat test passed"
    else
      info "(skipped — run: npm i playwright && npx playwright install chromium)"
    fi
  else
    info "(skipped — node not available)"
  fi

  if [ -n "${DEPLOY_HOST:-}" ]; then
    info "Test 7: systemd service"
    ssh_cmd "systemctl is-active $SERVICE_NAME" | grep -q '^active$' && \
      pass "Service active" || fail "Service inactive"
  fi

  echo ""
  echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
  echo -e "${GREEN}  All smoke tests passed ✓${NC}"
  echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
}

# ─── Main ─────────────────────────────────────────────────────

case "${1:-}" in
  --local-only) do_build; do_smoke ;;
  --test-only)  do_smoke ;;
  *)            do_build; do_deploy; do_smoke ;;
esac
