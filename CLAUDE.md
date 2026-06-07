# Chat Assistant — AI Agent 开发指南

## 项目概览

Go 后端 + 原生前端 + MySQL 的 AI 聊天应用，部署在阿里云 ECS `47.95.244.175`。

## 架构

```
浏览器 → Caddy (:80) → Go Server (:8080) → MySQL (127.0.0.1:3306)
  ↕ WebSocket          ↕ DeepSeek API (stream)
                       ↕ cn.bing.com (搜索)
```

## 技术栈

| 层 | 技术 |
|----|------|
| 后端 | Go 1.25+, gorilla/websocket, go-sql-driver/mysql |
| AI | DeepSeek V4 Pro (`deepseek-chat`), OpenAI 兼容 SSE 流式 |
| 搜索 | Bing 内置（cn.bing.com HTML 抓取） / Serper.dev / 自定义 API |
| 前端 | 原生 HTML/CSS/JS，零依赖 |
| 数据库 | MySQL 8.4, InnoDB, utf8mb4, 仅 127.0.0.1 |
| 反代 | Caddy v2 |
| 认证 | bcrypt (cost=12) + Session Cookie (HttpOnly, SameSite=Lax) |
| 部署 | systemd, EnvironmentFile=/opt/chat-assistant/.env (chmod 600) |

## 关键文件

| 文件 | 职责 |
|------|------|
| `main.go` | 入口、DeepSeek 客户端、MySQL 初始化、路由注册 |
| `models.go` | 数据结构、常量、全局变量 |
| `handlers.go` | HTTP/WS 处理器、认证中间件、静态文件服务 |
| `store.go` | 用户/会话/对话存储、MySQL CRUD |
| `hub.go` | WebSocket Hub（注册/注销/广播） |
| `search.go` | 联网搜索：Bing 抓取、Serper.dev、自定义 API、上下文注入 |
| `static/index.html` | 聊天页面（侧边栏 + 聊天区 + 输入框 + 搜索开关） |
| `static/app.js` | 前端逻辑：WS 连接、消息队列、Toast、会话管理、搜索切换 |
| `static/style.css` | 深色主题、流式动画、侧边栏、搜索卡片、响应式 |
| `static/login.html` / `.js` / `.css` | 登录页面 |

## 构建 & 部署

```bash
# 本地编译
GOPROXY=https://goproxy.cn,direct GOOS=linux GOARCH=amd64 go build -o chat-server .

# 部署（需先停服）
ssh root@47.95.244.175 'systemctl stop chat-assistant'
scp chat-server root@47.95.244.175:/opt/chat-assistant/
scp static/* root@47.95.244.175:/opt/chat-assistant/static/
ssh root@47.95.244.175 'systemctl start chat-assistant'
```

## 服务器信息

| 项目 | 值 |
|------|-----|
| IP | 47.95.244.175 |
| 系统 | Ubuntu 26.04 LTS |
| 内存 | 1.6 GB |
| Go 服务 | systemd: `chat-assistant.service` |
| 数据库 | MySQL 8.4, `chat_app@localhost` |
| 环境变量 | `/opt/chat-assistant/.env` (chmod 600) |
| 部署路径 | `/opt/chat-assistant/` |

## WebSocket 协议

```json
// 客户端 → 服务端
{"type": "message", "content": "...", "conversation_id": 1, "enable_search": true}

// 服务端 → 客户端
{"type": "search_results", "content": "[{title,url,snippet}]", "conversation_id": 1}
{"type": "stream_start", "conversation_id": 1}
{"type": "stream_chunk", "content": "...", "conversation_id": 1}
{"type": "stream_end", "content": "完整内容", "conversation_id": 1}
```

## API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| GET/POST | `/login`, `/api/login` | 登录 |
| POST | `/api/logout` | 登出 |
| GET | `/api/session` | 会话状态 |
| GET/POST | `/api/conversations` | 列表 / 创建 |
| GET | `/api/conversations/{id}/messages` | 消息历史 |
| DELETE/PUT | `/api/conversations/{id}` | 删除 / 重命名 |
| GET | `/ws` | WebSocket |
| GET | `/health` | 健康检查 |

## 重要约定

- **Go 代理**：国内用 `GOPROXY=https://goproxy.cn,direct`
- **交叉编译**：`GOOS=linux GOARCH=amd64`
- **部署前停服**：先 `systemctl stop`，再 scp binary，再 `systemctl start`
- **环境变量**：通过 `.env` 文件注入，systemd `EnvironmentFile` 读取
- **会话超时**：10 分钟无操作自动过期
- **会话上限**：每用户最多 3 个会话
- **搜索**：不配任何 Key 时自动用内置 Bing（零配置可用）
