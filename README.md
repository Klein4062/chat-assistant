# Chat Assistant — AI 助手

基于 Go 的实时聊天应用，支持 WebSocket 通信、用户认证和会话管理，最终目标为接入 LLM 的 AI 助手。

## 架构

```
浏览器 ──▶ Caddy (:80) ──▶ Go Server (:8080) ──▶ MySQL (127.0.0.1:3306)
  │                           │
  └── WebSocket ──────────────┘
```

## 技术栈

| 层级 | 技术 |
|------|------|
| 后端 | Go 1.25, gorilla/websocket |
| 前端 | 原生 HTML/CSS/JS（零依赖） |
| 数据库 | MySQL 8.4 |
| 反代 | Caddy v2 |
| 认证 | bcrypt + Session Cookie |
| 部署 | systemd, 阿里云 ECS (Ubuntu 26.04) |

## 功能

- [x] WebSocket 实时聊天（多客户端广播）
- [x] 用户登录/登出（bcrypt 密码哈希）
- [x] **10 分钟无操作自动退出**（会话超时 + 前端警告）
- [x] 未登录自动重定向登录页
- [x] 消息 Echo 回显（AI 接口预留）
- [x] 自动重连（指数退避）
- [x] 深色主题 + 响应式布局
- [x] systemd 守护 + 开机自启

## 快速开始

### 前提

```bash
# macOS 开发机
brew install go sshpass

# Ubuntu 服务器
apt-get install -y caddy mysql-server
```

### 本地开发

```bash
go mod tidy
go run .
# 访问 http://localhost:8080
```

### 部署到服务器

```bash
# 设置环境变量
export DEPLOY_HOST=47.95.244.175
export DEPLOY_USER=root
export DEPLOY_PASS='<server-password>'

# 一键构建 + 部署 + 冒烟测试
bash .claude/skills/run-chat-assistant/smoke.sh
```

或手动：

```bash
GOOS=linux GOARCH=amd64 go build -o chat-server .
scp chat-server root@<host>:/opt/chat-assistant/
scp static/* root@<host>:/opt/chat-assistant/static/
ssh root@<host> 'systemctl restart chat-assistant'
```

## 项目结构

```
chat-assistant/
├── main.go              # Go 后端（HTTP + WebSocket + Auth）
├── go.mod / go.sum
├── static/
│   ├── index.html       # 聊天页面
│   ├── style.css        # 聊天样式
│   ├── app.js           # 聊天逻辑 + 空闲检测
│   ├── login.html       # 登录页面
│   ├── login.css        # 登录样式
│   └── login.js         # 登录逻辑
├── .claude/skills/run-chat-assistant/
│   ├── SKILL.md         # Agent 操作手册
│   ├── smoke.sh         # 构建+部署+测试驱动
│   └── screenshot.cjs   # Playwright 浏览器测试
├── DEVLOG.md            # 开发日志
└── README.md
```

## API 端点

| 方法 | 路径 | 认证 | 说明 |
|------|------|------|------|
| GET | `/` | 需要 | 聊天页面 |
| GET | `/login` | 公开 | 登录页面 |
| POST | `/api/login` | 公开 | 登录 `{username, password}` |
| POST | `/api/logout` | 公开 | 退出登录 |
| GET | `/api/session` | 公开 | 会话状态 + 剩余时间 |
| GET | `/ws` | 需要 | WebSocket 连接 |
| GET | `/health` | 公开 | 健康检查 |

## 会话超时

- 10 分钟无 HTTP 请求 → Session 自动过期
- 前端每 60 秒轮询 `/api/session` 续期
- 剩余 ≤60 秒时显示橙色警告条
- 过期后重定向 `/login?expired=1`

## 维护

```bash
# 服务状态
systemctl status chat-assistant caddy mysql

# 查看日志
journalctl -u chat-assistant -f

# 重启
systemctl restart chat-assistant
```

## 开发计划

- [ ] 接入 DeepSeek / Claude API
- [ ] 流式 AI 回复
- [ ] 多会话管理
- [ ] Markdown + 代码高亮
- [ ] HTTPS + 域名绑定
- [ ] Docker 化
