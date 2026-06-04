---
name: run-chat-assistant
description: Build, deploy, run, and smoke-test the chat-assistant Go web server. Use when asked to run the app, start the server, deploy changes, test the chat UI, or take a screenshot.
---

# run-chat-assistant

Go WebSocket chat server with a browser UI, deployed behind Caddy on an Ubuntu VPS. See `DEVLOG.md` for architecture and deployment history.

Paths below are relative to the project root (`chat-assistant/`).

## Prerequisites

```bash
# macOS (dev machine)
brew install go sshpass

# Ubuntu server (if not already set up)
apt-get install -y caddy mysql-server
```

## Build

```bash
go mod tidy
GOOS=linux GOARCH=amd64 go build -o chat-server .
```

## Run (agent path)

The driver is `.claude/skills/run-chat-assistant/smoke.sh`. It handles build, deploy, and testing in one pass.

```bash
# Set deployment credentials
export DEPLOY_HOST=47.95.244.175
export DEPLOY_USER=root
export DEPLOY_PASS='<server-password>'

# Full cycle: build → deploy → smoke test
bash .claude/skills/run-chat-assistant/smoke.sh

# Smoke test only (server already running)
bash .claude/skills/run-chat-assistant/smoke.sh --test-only

# Build + local smoke only (no deploy)
bash .claude/skills/run-chat-assistant/smoke.sh --local-only
```

### What the smoke test checks

| Test | What it verifies |
|------|-----------------|
| Health | `GET /health` returns `{"status":"ok"}` |
| HTML | `GET /` serves chat page with `<title>AI 助手</title>` |
| CSS | `GET /style.css` contains `.chat-header` |
| JS | `GET /app.js` contains `WebSocket` |
| WebSocket | `/ws` accepts upgrade (HTTP 101) |
| Browser | Playwright types message, verifies echo reply, saves screenshot |
| systemd | `chat-assistant.service` is `active` |

### Browser screenshot (Playwright)

The smoke test's Test 6 requires a one-time Playwright setup:

```bash
# In the skill directory
cd .claude/skills/run-chat-assistant
npm init -y && npm install playwright && npx playwright install chromium
```

Then run the screenshot test directly:

```bash
BASE_URL=http://47.95.244.175 node .claude/skills/run-chat-assistant/screenshot.cjs
# Saves screenshot.png to project root
```

### Quick health check (no deploy, no dependencies)

```bash
curl -s http://47.95.244.175/health
# → {"status":"ok","time":"..."}
```

## Run (human path)

```bash
# Local development (port 8080, no Caddy/MySQL needed)
go run . &
curl http://localhost:8080/health

# Deploy to server
GOOS=linux GOARCH=amd64 go build -o chat-server .
sshpass -p '<pwd>' scp chat-server root@47.95.244.175:/opt/chat-assistant/
sshpass -p '<pwd>' scp static/* root@47.95.244.175:/opt/chat-assistant/static/
ssh root@47.95.244.175 'systemctl restart chat-assistant'
```

## Direct invocation

For PRs that touch Go internals without needing the full server:

```bash
# Unit-test specific functions (add _test.go files as needed)
go test ./...

# Build check only
go build -o /dev/null .
```

## Test

```bash
go test ./...
go vet ./...
```

## Gotchas

- **WebSocket test hangs with curl**: The server sends HTTP 101 then holds the connection open. Always use `timeout` (already handled in `smoke.sh`).
- **Caddy already on port 80**: Don't install nginx. The server uses Caddy as reverse proxy. Config at `/etc/caddy/Caddyfile`.
- **MySQL only on localhost**: `chat_app@localhost` can only connect from the server itself. External connections get `Connection refused`.
- **Cross-compile required**: Dev is on macOS, server is Linux amd64. Always set `GOOS=linux GOARCH=amd64`.
- **apt mirrors broken**: The Aliyun mirrors may be unreachable. Fix: edit `/etc/apt/sources.list.d/ubuntu.sources` to use `http://archive.ubuntu.com/ubuntu`.

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `go: command not found` | `brew install go` |
| `sshpass: command not found` | `brew install sshpass` |
| `curl: (7) Failed to connect` | Check `systemctl status chat-assistant caddy` on server |
| WebSocket stuck on "连接中..." | Caddy not proxying — check `/etc/caddy/Caddyfile`: `reverse_proxy localhost:8080` |
| MySQL `Connection refused` | MySQL only listens on 127.0.0.1 — connect from server, not remotely |
| Playwright test skipped | `cd .claude/skills/run-chat-assistant && npm install playwright && npx playwright install chromium` |
