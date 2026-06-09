# Chat Assistant — AI 助手

基于 Go 的 AI 聊天应用，支持 WebSocket 实时通信、DeepSeek V4 Pro 流式对话、用户认证和会话超时管理。

## 架构

```
浏览器 ──▶ Caddy (:80) ──▶ Go Server (:8080) ──▶ OpenClaw (:18789) ──▶ DeepSeek API (stream)
  │                           │    │    │
  └── WebSocket ──────────────┘    │    └── cn.bing.com (联网搜索)
                                   └── MySQL (127.0.0.1:3306)
```

## 技术栈

| 层级 | 技术 |
|------|------|
| 后端 | Go 1.25, gorilla/websocket |
| AI 网关 | OpenClaw 2026.6.1（Agent 能力层 + 模型路由） |
| AI 模型 | DeepSeek V4 Pro（OpenAI 兼容接口，SSE 流式） |
| 前端 | 原生 HTML/CSS/JS（零依赖） |
| 数据库 | MySQL 8.4 |
| 反代 | Caddy v2 |
| 认证 | bcrypt + Session Cookie |
| 部署 | systemd, 阿里云 ECS (Ubuntu 26.04) |

## 功能

- [x] **OpenClaw AI Agent 网关**：Agent 能力层 + 模型路由 + 多通道扩展基础
- [x] **DeepSeek V4 Pro AI 对话**，流式逐字输出
- [x] **多会话管理**：最多 3 个，MySQL 持久化，登出/重登不丢失，切换/重命名/删除
- [x] **Session 同步**：chat-assistant 会话 ↔ OpenClaw session 一对一，创建/复用/关闭同步
- [x] 对话上下文管理（每会话独立，最近 20 轮）
- [x] WebSocket 实时通信（多客户端广播）+ 30s Ping 保活
- [x] 用户登录/登出（bcrypt 密码哈希）
- [x] **10 分钟无操作自动退出**（会话超时 + 前端警告条）
- [x] 未登录自动重定向登录页
- [x] 流式渲染动画（打字指示器 + 闪烁光标）
- [x] **🔍 联网搜索**（🌐 开关，Bing 内置免费 + Serper.dev + 自定义 API，来源卡片展示）
- [x] 连接状态 Toast 通知（断线警告 + 重连成功）
- [x] 自动重连（指数退避，断开时立即重连）
- [x] 深色主题 + 响应式布局
- [x] **HTTPS**（fengyin.xin + DigiCert SSL 证书）
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
# 设置 DeepSeek API Key
export DEEPSEEK_API_KEY=sk-xxxxxxxx

go mod tidy
go run .
# 访问 http://localhost:8080
```

### 部署到服务器

```bash
export DEPLOY_HOST=<your-server>
export DEPLOY_USER=root
export DEPLOY_PASS='<server-password>'

# 一键构建 + 部署 + 测试
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
├── main.go              # 入口 + DeepSeek 客户端 + MySQL + 路由
├── models.go            # 数据结构 / 常量 / 类型定义
├── handlers.go          # HTTP/WS 处理器 + 认证中间件
├── store.go             # 用户/会话/对话存储 + 数据库操作
├── hub.go               # WebSocket 连接管理
├── search.go            # 联网搜索（Bing + Serper.dev + 自定义 API）
├── go.mod / go.sum
├── static/
│   ├── index.html       # 聊天页面
│   ├── style.css        # 聊天样式 + 流式动画
│   ├── app.js           # 聊天逻辑 + 流式渲染 + 空闲检测
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
| GET | `/api/session` | 公开 | 会话状态 + 剩余秒数 |
| GET | `/ws` | 需要 | WebSocket（含 AI 流式消息） |
| GET | `/health` | 公开 | 健康检查 |
| GET | `/api/conversations` | 需要 | 列出用户会话 |
| POST | `/api/conversations` | 需要 | 新建会话（最多 3 个） |
| GET | `/api/conversations/{id}/messages` | 需要 | 加载历史消息 |
| DELETE | `/api/conversations/{id}` | 需要 | 删除会话及消息 |
| PUT | `/api/conversations/{id}` | 需要 | 重命名会话 |

## WebSocket 消息协议

```json
// 客户端 → 服务端
{"type": "message", "content": "你好", "conversation_id": 1, "enable_search": true}

// 服务端 → 客户端
{"type": "search_results", "content": "[{...}]", "conversation_id": 1}  // 搜索来源（开启联网时）
{"type": "stream_start", "conversation_id": 1}                          // 开始生成
{"type": "stream_chunk", "content": "你", "conversation_id": 1}         // 逐 chunk 推送
{"type": "stream_end", "content": "你好！", "conversation_id": 1}       // 生成完毕
```

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `DEEPSEEK_API_KEY` | (必填) | DeepSeek API 密钥（OpenClaw 使用，直连回退时也使用） |
| `DEEPSEEK_BASE_URL` | `https://api.deepseek.com` | DeepSeek API 地址（直连回退用） |
| `DEEPSEEK_MODEL` | `deepseek-chat` | 模型名称 |
| `MYSQL_DSN` | (必填) | MySQL 连接串 |
| `OPENCLAW_ENABLED` | `false` | 是否启用 OpenClaw 网关路由 |
| `OPENCLAW_BASE_URL` | `http://127.0.0.1:18789` | OpenClaw 网关地址 |
| `OPENCLAW_AUTH_TOKEN` | (必填) | OpenClaw 网关鉴权令牌 |
| `SERPER_API_KEY` | (可选) | Serper.dev Google 搜索 Key |
| `SEARCH_API_URL` | (可选) | 自定义搜索端点 |

所有密钥存储在服务器 `/opt/chat-assistant/.env`（权限 600），systemd 通过 `EnvironmentFile` 注入。

## 会话超时

- 10 分钟无 HTTP 请求 → Session 自动过期
- 前端每 60 秒轮询 `/api/session` 续期
- 剩余 ≤60 秒时显示橙色警告条 `⏳ 会话即将过期`
- 过期后重定向 `/login?expired=1`，页面提示"会话已过期，请重新登录"

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

- [x] 接入 DeepSeek API，流式对话
- [x] 多会话管理（MySQL 持久化，最多 3 个）
- [x] 联网搜索（Bing 内置免费，零配置）
- [x] OpenClaw AI Agent 网关集成 + Session 同步
- [x] HTTPS + 域名绑定（fengyin.xin）
- [ ] Markdown 渲染 + 代码高亮
- [ ] Docker 化
- [ ] 多通道接入（Telegram / 微信 / 飞书等，通过 OpenClaw）
