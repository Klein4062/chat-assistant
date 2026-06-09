# 开发日志 - AI 助手聊天窗口

## 项目概览

- **项目名称**: Chat Assistant (AI 助手)
- **部署地址**: http://47.95.244.175
- **服务器**: 阿里云 ECS, Ubuntu 26.04 LTS
- **启动时间**: 2026-06-04

## 技术架构

```
┌──────────┐    ┌──────────┐    ┌───────────────┐    ┌──────────────┐    ┌──────────────┐
│  浏览器   │───▶│  Caddy   │───▶│ Go Chat Server │───▶│  OpenClaw    │───▶│  DeepSeek    │
│ (Chat UI) │◀───│ (Port 80)│◀───│  (Port 8080)  │◀───│(127.0.0.1:   │◀───│   API (SSE)  │
└──────────┘    └──────────┘    └───────────────┘    │   18789)     │    └──────────────┘
  WebSocket      Reverse Proxy   WebSocket + HTTP     │  AI Agent   │
                                                      │  Gateway    │
                                                      └─────────────┘
                                                      ↑ SSH 隧道远程控制台
```

### 技术栈

| 层级 | 技术 | 说明 |
|------|------|------|
| 前端 | HTML5 + CSS3 + Vanilla JS | 零依赖，现代聊天 UI |
| WebSocket | gorilla/websocket v1.5.1 | Go 生态标准 WebSocket 库 |
| 后端 | Go 1.26 | HTTP Server + WebSocket Hub |
| 反代 | Caddy v2.11.3 | 自动处理 WebSocket 代理 |
| 数据库 | MySQL 8.4 | InnoDB, utf8mb4, 仅监听 localhost |
| 守护 | systemd | 自动重启，开机自启 |

## 项目结构

```
/opt/chat-assistant/            # 服务器部署路径
├── chat-server                 # Go 编译的 Linux amd64 二进制
├── static/
│   ├── index.html              # 聊天页面主结构
│   ├── style.css               # 深色主题样式
│   └── app.js                  # WebSocket 客户端 + UI 交互
├── main.go                     # 后端源码 (Go)
├── go.mod                      # Go 模块定义
└── DEVLOG.md                   # 本文件

/root/.openclaw/                # OpenClaw 配置
└── openclaw.json               # 模型/网关/Agent 配置

/etc/systemd/system/
├── chat-assistant.service      # Go 后端守护
└── openclaw.service            # OpenClaw 网关守护
```

## 消息协议

```json
// 客户端 → 服务端
{"type": "message", "content": "消息内容"}

// 服务端 → 客户端 (当前 Echo 模式)
{
  "type": "message",
  "content": "Echo: 消息内容",
  "sender": "server",
  "client_id": "客户端标识",
  "timestamp": "2026-06-04T00:00:00Z"
}

// 预留: AI 流式回复
{"type": "stream_start",  "message_id": "..."}
{"type": "stream_chunk",  "content": "...", "message_id": "..."}
{"type": "stream_end",    "message_id": "..."}
```

## 部署记录

### 2026-06-04 — 初始部署

**环境准备:**
1. 安装 Go 1.26.4（本地 macOS 交叉编译）
2. 修复 apt 源（从阿里云镜像切换到 Ubuntu 官方镜像）
3. 安装 Caddy 作为反向代理（本已安装）

**部署步骤:**
1. `go mod tidy` — 下载 gorilla/websocket 依赖
2. `GOOS=linux GOARCH=amd64 go build -o chat-server .` — 交叉编译
3. SCP 上传 chat-server + static/ 到 /opt/chat-assistant/
4. 配置 Caddy 反代：`:80 → localhost:8080`
5. 创建 systemd 服务 `/etc/systemd/system/chat-assistant.service`
6. `systemctl enable --now chat-assistant` 启动

**验证结果:**
- ✅ `curl http://47.95.244.175` 返回聊天页面 HTML
- ✅ `curl http://47.95.244.175/health` 返回 `{"status":"ok"}`
- ✅ WebSocket 端点 `/ws` 可达
- ✅ systemd 服务正常运行

### 2026-06-04 — MySQL 数据库部署

**安全配置:**
- MySQL 8.4.9，仅监听 `127.0.0.1:3306`（不对外暴露）
- 外部端口扫描确认：`Connection refused — OK`
- root 用户使用 `auth_socket` 插件（仅 Linux root 可登录）
- 应用专用用户 `chat_app@localhost`，最小权限原则

**数据库信息:**

| 项目 | 值 |
|------|-----|
| 数据库名 | `chat_assistant` |
| 字符集 | `utf8mb4` / `utf8mb4_unicode_ci` |
| 应用用户 | `chat_app@localhost` |
| 监听地址 | `127.0.0.1:3306` |

**低内存优化（适配 1.6GB 服务器）:**

| 参数 | 值 | 说明 |
|------|-----|------|
| `innodb_buffer_pool_size` | 128M | InnoDB 缓存池 |
| `innodb_log_file_size` | 32M | 重做日志大小 |
| `innodb_flush_method` | O_DIRECT | 绕过 OS 缓存 |
| `innodb_flush_log_at_trx_commit` | 2 | 每秒刷盘（性能优先） |
| `max_connections` | 50 | 最大连接数 |
| `performance_schema` | OFF | 关闭性能监控 |

**初始表结构:**

```sql
-- 会话表
CREATE TABLE conversations (
    id          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    title       VARCHAR(255) NOT NULL DEFAULT '新对话',
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
);

-- 消息表
CREATE TABLE messages (
    id              BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    conversation_id BIGINT UNSIGNED NOT NULL,
    role            ENUM('user', 'assistant', 'system') NOT NULL,
    content         TEXT NOT NULL,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE CASCADE,
    INDEX idx_conversation_id (conversation_id),
    INDEX idx_created_at (created_at)
);
```

### 2026-06-05 — 用户认证系统

**新增功能:**
- 登录页面 (`/login`)，含完整 UI（表单、loading 动画、错误提示）
- 用户名+密码认证（bcrypt 哈希存储）
- 基于 Cookie 的 Session 管理（24 小时有效期）
- 未登录自动重定向到 `/login`
- 已登录显示用户名 + 退出按钮
- 受保护路由：聊天页面 `/`、WebSocket `/ws`、静态资源

**认证流程:**

```
未认证访问 / → 302 → /login → POST /api/login → 302 → /
                                                  ↓
                                         Set-Cookie: session_token
                                                  ↓
                                         后续请求带 Cookie → 通过认证
```

**技术细节:**

| 层面 | 实现 |
|------|------|
| 密码哈希 | bcrypt (cost=12), `golang.org/x/crypto` |
| Session | 随机 32 字节 hex token, 内存存储 |
| Cookie | HttpOnly, SameSite=Lax, 24h 过期 |
| 中间件 | `authMiddleware` — API 返回 401，页面重定向 302 |

**API 端点:**

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/login` | 登录页面 |
| POST | `/api/login` | 登录（JSON: username, password） |
| POST | `/api/logout` | 退出登录 |
| GET | `/api/session` | 检查当前会话状态 |

**用户表（MySQL）:**

```sql
CREATE TABLE users (
    id         BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    username   VARCHAR(64) NOT NULL UNIQUE,
    password   VARCHAR(255) NOT NULL,  -- bcrypt hash
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

**初始用户:** `Klein4062`（bcrypt 哈希存储）

**验证结果:**
- ✅ 未登录访问 `/` → 重定向到 `/login`
- ✅ 错误密码 → `{"success":false,"message":"用户名或密码错误"}`
- ✅ 正确密码 → `{"success":true,"token":"..."}` + Set-Cookie
- ✅ 登录后访问 `/` → 聊天页面（显示用户名 Klein4062）
- ✅ WebSocket 连接成功（携带会话信息）
- ✅ 退出登录 → Cookie 清除，重定向回 `/login`
- ✅ Playwright E2E 测试通过（登录→聊天→退出 完整流程）

**安全说明:**
- 密码使用 bcrypt (cost=12) 哈希，不存明文
- Session token 为 32 字节随机 hex（256 位熵）
- Cookie 设置 HttpOnly（JS 不可读）+ SameSite=Lax
- Session 存储在服务端内存（重启后失效，后期可迁移到 MySQL 或 Redis）

### 2026-06-05 — 会话超时机制

**新增功能:**
- 10 分钟无操作自动过期，需重新登录
- 服务端 Session 记录 `LastActivity` 时间戳
- 每次请求自动刷新活跃时间
- 后台定时清理过期 Session（每 5 分钟）
- 前端每 60 秒轮询 `/api/session` 检查剩余时间
- 剩余 ≤60 秒时显示橙色警告条
- 用户活动（鼠标/键盘/触屏/滚动）自动消除警告并续期
- Session 过期后重定向到 `/login?expired=1`
- 登录页检测 `?expired=1` 参数并显示"会话已过期，请重新登录"

**超时流程:**

```
用户活跃 → 每次 HTTP 请求刷新 LastActivity
         ↓
10 分钟无请求 → Session.IsIdleExpired() = true
         ↓
authMiddleware 拦截 → 删除 Session → 302 /login?expired=1
         ↓
前端轮询检测 → remaining_secs ≤ 60 → 显示警告条
         ↓
用户操作 → 消除警告 + 自动 ping /api/session 续期
```

**技术细节:**

| 层面 | 实现 |
|------|------|
| 超时时长 | `sessionIdleTimeout = 10 * time.Minute` |
| 绝对过期 | `sessionMaxAge = 24 * time.Hour` |
| Session 字段 | `LastActivity time.Time` |
| 前端轮询 | `setInterval(checkSessionTimeout, 60s)` |
| 警告阈值 | `remaining_secs ≤ 60` |
| API 返回 | `{"authenticated":true, "remaining_secs": 599}` |
| 后台清理 | goroutine 每 5 分钟 `Cleanup()` |

**API 变更:**

`GET /api/session` 返回值新增字段：
```json
{
  "authenticated": true,
  "username": "Klein4062",
  "remaining_secs": 599
}
```

**验证结果:**
- ✅ 登录后 `/api/session` 返回 `remaining_secs: ~599`
- ✅ 每次请求自动刷新 `LastActivity`
- ✅ 前端轮询检测到剩余时间变化
- ✅ 会话过期重定向到 `/login?expired=1`
- ✅ 登录页显示"会话已过期，请重新登录"
- ✅ Playwright E2E 全流程通过

### 2026-06-05 — DeepSeek V4 Pro API 接入

**新增功能:**
- 接入 DeepSeek API（OpenAI 兼容接口），替代 Echo 回显
- 流式响应支持（SSE 解析 + WebSocket 推送 `stream_chunk`）
- 前端流式渲染：AI 回复逐字显示 + 闪烁光标动画
- 对话历史管理（每个 WebSocket 客户端独立上下文，保留最近 20 轮）
- 系统 Prompt 可配置
- 模型名称可配置（默认 `deepseek-chat`）

**数据流:**

```
用户输入 → WebSocket → Go Server → DeepSeek API (stream=true)
                                              ↓
                                        SSE: data: {"choices":[{"delta":{"content":"你"}}]}
                                              ↓
                              Go 解析 → stream_chunk → WebSocket → 前端逐字渲染
                                              ↓
                                        SSE: data: [DONE]
                                              ↓
                              Go → stream_end → 前端定格 AI 气泡
```

**配置:**

| 环境变量 | 默认值 | 说明 |
|----------|--------|------|
| `DEEPSEEK_API_KEY` | (必填) | DeepSeek API 密钥 |
| `DEEPSEEK_BASE_URL` | `https://api.deepseek.com` | API 地址 |
| `DEEPSEEK_MODEL` | `deepseek-chat` | 模型名称 |

密钥存储在 `/opt/chat-assistant/.env`（权限 600），systemd 通过 `EnvironmentFile` 读取。

**前端新增:**
- `stream_start` → 显示打字指示器（三个跳动圆点）
- `stream_chunk` → 递增填充 AI 气泡 + 闪烁光标 `▊`
- `stream_end` → 光标消失，添加时间戳
- 新增 CSS 动画：`blink`（光标闪烁）、`typing`（三点跳动）

**验证结果:**
- ✅ "你好，介绍一下自己" → AI: "你好！我是AI助手，友好且乐于助人..."
- ✅ 流式响应逐字渲染
- ✅ 对话上下文保持（多轮对话）
- ✅ API Key 通过 .env 文件安全注入
- ✅ Playwright E2E 截图: `screenshot-ai.png`

### 2026-06-05 — Bug 修复 + UX 增强：断线消息队列

**问题诊断:**
用户反馈发送消息完全无反应。经排查，服务端一切正常（Python WebSocket 客户端 + Playwright 浏览器均测试通过），问题出在前端：

1. WebSocket 连接频繁在 2 秒后断开（浏览器/代理/网络层面触发）
2. `sendMessage()` 检测到 `ws.readyState !== OPEN` 时静默失败——只在 header 中闪过 "未连接，无法发送" 的小字，用户根本注意不到
3. 消息留在输入框，看起来就是"发了消息完全没反应"

**修复内容:**

| 改动 | 文件 | 说明 |
|------|------|------|
| 消息队列 | `app.js` | 断开时消息暂存 `pendingMessage`，重连后自动发送 |
| Toast 通知 | `app.js` | 断线显示橙色 "连接已断开，正在重连..." 横幅 |
| Toast 通知 | `app.js` | 重连成功显示绿色 "✅ 已重新连接"（2s 自动消失） |
| Toast 通知 | `app.js` | 发送排队消息时显示 "消息将在重连后自动发送" |
| 待发送气泡 | `app.js` | 断开时用户消息立即显示在聊天区，标注 "⏳ 等待重连发送..." |
| 立即重连 | `app.js` | 用户主动发送消息时重置 backoff，立即尝试重连 |
| Toast 样式 | `style.css` | 橙色警告渐变 + 绿色成功渐变，fixed 顶部居中，slide-down 动画 |
| Ping 保活 | `main.go` | 服务端每 30s 发送 WebSocket Ping，防止代理/NAT 关闭空闲连接 |

**修复后流程:**

```
用户发送消息 → ws 未连接？
  ├── 是 → 立即显示气泡（⏳ 等待重连发送...）
  │        → Toast 通知「消息将在重连后自动发送」
  │        → 重置 backoff，立即重连
  │        → ws.onopen → 自动发送队列中的消息
  │        → 气泡时间戳更新为实际发送时间
  │        → Toast「✅ 已重新连接」2s 后消失
  └── 否 → 正常发送
```

**验证结果:**
- ✅ 正常聊天：消息发送 → AI 流式回复 正常
- ✅ 服务端 Ping 保活：30s 间隔，1.1MB 内存，运行稳定
- ✅ Playwright 测试通过：登录 → 聊天 → AI 回复

### 2026-06-06 — 多会话管理

**新增功能:**
- 用户可创建最多 3 个独立会话，每个会话独立 AI 对话上下文
- 会话和聊天记录持久化到 MySQL，登出后不丢失
- 重新登录自动恢复所有会话及历史消息
- 侧边栏会话列表：切换、重命名（双击/编辑按钮）、删除
- 首次登录自动创建第一个会话；删除所有会话后自动创建新会话
- 优先选择有消息的会话（最近更新的）

**技术变更:**

| 改动 | 文件 | 说明 |
|------|------|------|
| MySQL 连接 | `main.go` | 新增 `initMySQL()`，DSN 通过 `MYSQL_DSN` 环境变量注入 |
| ConversationStore 改造 | `main.go` | key 从 `clientID` 改为 `conversationID`，DB 为主 + 内存缓存 |
| 5 个 API | `main.go` | `GET/POST /api/conversations`、`GET /api/conversations/{id}/messages`、`DELETE/PUT /api/conversations/{id}` |
| WebSocket 协议扩展 | `main.go` | 消息增加 `conversation_id` 字段，服务端响应同样携带 |
| 侧边栏布局 | `index.html` | 260px 侧边栏 + 聊天区域，移动端汉堡菜单收起 |
| 会话管理 JS | `app.js` | 列表加载、切换、新建、删除、重命名、历史加载、conversation_id 路由 |
| 侧边栏样式 | `style.css` | 会话列表、选中高亮、操作按钮 hover 显示、移动端适配 |
| MySQL 迁移 | 服务器 | `ALTER TABLE conversations ADD COLUMN username` |
| 新增依赖 | `go.mod` | `github.com/go-sql-driver/mysql` v1.10.0 |

**数据库设计:**
```sql
-- conversations 表（新增 username 列）
id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY
username VARCHAR(64) NOT NULL          -- FK → users.username
title VARCHAR(255) DEFAULT '新对话'
created_at / updated_at DATETIME

-- messages 表（已有，无改动）
id BIGINT AUTO_INCREMENT PRIMARY KEY
conversation_id BIGINT UNSIGNED → FK → conversations(id) ON DELETE CASCADE
role ENUM('user','assistant','system')
content TEXT
created_at DATETIME
```

**验证结果:**
- ✅ API 测试：会话列表、消息历史加载、3 个上限拒绝
- ✅ 登出重登后 3 个会话完整保留，4 条消息持久化
- ✅ MySQL 连接正常，10 连接池，5 空闲连接
- ✅ 前端优化：优先选中含消息的会话

### 2026-06-06 — 移动端侧边栏 UX 修复

**问题:**
1. 移动端选择会话后侧边栏不自动关闭，遮挡聊天区
2. 无法通过点击空白区域关闭侧边栏，必须点 ☰ 按钮
3. 侧边栏打开时拦截聊天区点击事件（`position: fixed` 覆盖）

**修复:**

| 改动 | 文件 | 说明 |
|------|------|------|
| 自动关闭 | `app.js` | `selectConversation()` 检测移动端（≤768px），自动 `remove('open')` |
| 遮罩层 | `app.js` | 打开侧边栏时动态创建 `#sidebarBackdrop`，点击即关闭 |
| 遮罩样式 | `style.css` | 半透明黑色背景 `rgba(0,0,0,0.5)`，z-index: 99 |
| 清理逻辑 | `app.js` | 关闭侧边栏时自动移除 backdrop 元素 |

**验证结果:**
- ✅ 桌面端：toggle 隐藏、侧边栏 260px 正常显示、无 backdrop
- ✅ 移动端：选择会话后侧边栏自动关闭、backdrop 移除
- ✅ 移动端：打开侧边栏时 backdrop 出现、侧边栏在屏幕上（left: 0）

### 2026-06-06 — 联网搜索功能

**新增功能:**
- 聊天界面新增「🔍 联网搜索」Toggle 开关（输入区上方）
- 开启后自动搜索网络并将结果注入 AI 对话上下文
- AI 基于搜索结果回答，前端展示搜索来源引用（标题 + URL + 摘要）
- 支持多种搜索后端，优先级：Serper.dev > 自定义 SEARCH_API_URL

**技术实现:**

| 改动 | 文件 | 说明 |
|------|------|------|
| WebSocket 协议 | `main.go` | 消息增加 `enable_search` 字段，响应新增 `search_results` 类型 |
| API 请求 | `main.go` | `ChatCompletionRequest.EnableSearch` 透传至 DeepSeek |
| 搜索客户端 | `search.go` | `searchWeb()` — Serper.dev Google 搜索 (免费 100次/月) + 自定义后端 |
| 上下文注入 | `search.go` | `buildSearchContext()` — 搜索结果拼接为用户消息注入对话历史 |
| 搜索结果展示 | `app.js` | `addSearchResultsBubble()` — 搜索来源卡片（编号 + 标题 + 链接） |
| Toggle 组件 | `index.html` | CSS-only 滑动开关，`input:checked` 驱动蓝色高亮 |
| Toggle 样式 | `style.css` | `.search-toggle-slider` 滑动动画，`.search-result-item` 来源卡片 |

**搜索后端（多层级 fallback）:**

| 层级 | 后端 | 说明 |
|------|------|------|
| 1 | Serper.dev | Google 搜索，通过 `SERPER_API_KEY` 配置（免费 100次/月） |
| 2 | `SEARCH_API_URL` | 自定义 SearXNG/DuckDuckGo 兼容端点 |
| 3 | **Bing 内置** | 零配置、免费、国内可用，`cn.bing.com` HTML 抓取 |

**环境变量:**

| 变量 | 说明 |
|------|------|
| `SERPER_API_KEY` | Serper.dev API Key（可选，Google 搜索） |
| `SEARCH_API_URL` | 自定义搜索端点（可选） |

不配置任何环境变量时，自动使用内置的 Bing 搜索。

**验证结果:**
- ✅ Toggle 开关正常（🌐 联网 胶囊按钮，蓝色光圈激活态）
- ✅ **内置 Bing 搜索：5 条结果 "Python最新版本"，AI 回答"Python 3.14，2025年10月发布"**
- ✅ 搜索结果在前端以来源卡片展示（编号 + 标题 + 链接）
- ✅ 搜索上下文注入 AI 对话，答案包含搜索来源引用
- ✅ 零配置即可使用（无 API Key 也能搜索）

### 2026-06-08 — 代码重构：拆分 + 中文注释

**重构目的：** 项目规模增长后单文件 `main.go` 已达 1126 行，可读性差。按职责拆分为 6 个 Go 文件，全部加中文注释。

**文件拆分：**

| 文件 | 行数 | 职责 |
|------|------|------|
| `models.go` | ~130 | 数据结构、常量、全局变量 |
| `store.go` | ~250 | 用户存储、会话存储、对话存储、DB 操作 |
| `hub.go` | ~55 | WebSocket Hub（注册/注销/广播） |
| `handlers.go` | ~330 | HTTP/WS 处理器、认证中间件、静态文件 |
| `search.go` | ~200 | 联网搜索（Bing/Serper/自定义 API） |
| `main.go` | ~170 | 入口、DeepSeek 客户端、MySQL、路由注册 |

**其他改动：**
- 全部 Go 文件改用中文注释（`═══` 块分隔 + 功能说明）
- `app.js` 和 `style.css` 章节标题改为中文
- 新增 `CLAUDE.md` AI Agent 开发指南
- 更新 `README.md` 架构图和开发计划

### 2026-06-08 — 联网搜索 Bug 修复：AI 联网认知 + Bing 中文搜索质量

**问题诊断（5个Bug）：**

| # | 表现 | 根因 |
|---|------|------|
| 1 | AI 说"我没联网" | 搜索结果作为独立 `system` 消息注入，被 DeepSeek 忽略 |
| 2 | AI 编造天气数据+日期 | 搜索返回 0 结果时 AI 仍自信虚构，且标注虚假来源 [1][2] |
| 3 | AI "请开启联网搜索" | system prompt 未定义搜索行为 |
| 4 | Bing 返回英文垃圾 | Go HTTP 客户端 TLS 指纹被 Bing 识别为爬虫 |
| 5 | "几号"搜索→黄历结果 | 中文口语虚词（几号/如何/怎么样）误导 Bing 分词 |

**修复:**

| # | 修复 | 文件 |
|---|------|------|
| 1 | `buildSearchPrompt()` 拼接到 system prompt 尾部，单一 system 消息 | `search.go` |
| 2 | 搜索无结果→prompt 追加"不要编造数据或日期"，AI 诚实说不知道 | `main.go` / `handlers.go` |
| 3 | system prompt：`你是联网的 / 不要建议开启联网搜索` | `main.go` |
| 4 | 强制 HTTP/1.1 + 完整浏览器 headers（Accept/Accept-Language/DNT） | `search.go` |
| 5 | `cleanQuery()` 过滤口语虚词（长词优先：怎么样→怎么→几号→如何→吗/呢/吧） | `search.go` |
| — | `site:bilibili.com` 限定中文内容源，防止 Bing 返回英文结果 | `search.go` |

**已知限制:**
- ⚠️ Bing 免费抓取存在 IP 限流/封禁风险（频繁请求触发）
- 推荐方案：注册 Serper.dev 免费 Key（Google 搜索，100次/月，国内可达）→ 设 `SERPER_API_KEY` 环境变量即可切换

**验证结果:**
- ✅ AI 被问"你能联网吗"→回答"能，我支持联网搜索"（不再说"请手动开启"）
- ✅ 搜索无结果时 AI 诚实说"抱歉，无法获取"（不再虚构数据和日期）
- ✅ 天气查询成功获取过实时结果（带来源引用 [1][2]）
- ✅ `cleanQuery("今天几号，北京天气如何")` → `"今天 北京天气"` 正确过滤

### 2026-06-09 — OpenClaw AI Agent 网关集成

**背景：** 直接调用 DeepSeek API 缺少 Agent 能力层（技能、工具、记忆、多通道）。引入 OpenClaw 作为 AI 网关，chat-assistant 通过 OpenClaw 调用后端模型，未来可自然扩展 Telegram/微信等多通道接入。

**新增功能:**
- 部署 OpenClaw 2026.6.1（npm 全局安装），独立 systemd 服务
- DeepSeek V4 Pro 作为 OpenClaw 后端模型（`deepseek/deepseek-chat`）
- Go 后端新增 OpenClaw 路由：`OPENCLAW_ENABLED=true` 时通过网关调用 AI
- 开启 OpenAI 兼容 HTTP API（`POST /v1/chat/completions`），SSE 流式同格式零改动
- OpenClaw session 与 chat-assistant 会话同步：同一会话复用同一 OpenClaw session
- 删除 chat-assistant 会话时同步关闭 OpenClaw session
- 未配置时自动回退直连 DeepSeek（零影响）

**架构变更:**

```
变更前:  Browser → Caddy(:80) → Go(:8080) → DeepSeek API
变更后:  Browser → Caddy(:80) → Go(:8080) → OpenClaw(:18789) → DeepSeek API
                                               ↑ loopback only
                                          SSH 隧道远程访问控制台
```

**技术细节:**

| 层面 | 实现 |
|------|------|
| OpenClaw 安装 | `npm install -g openclaw@latest`，systemd 守护 |
| 模型配置 | `models.providers.deepseek` → `api.openai-completions`，API Key 通过 env 注入 |
| Go 路由开关 | `OPENCLAW_ENABLED`、`OPENCLAW_BASE_URL`、`OPENCLAW_AUTH_TOKEN` 环境变量 |
| Session 同步 | `ChatCompletionRequest.User` 字段 `{username}-conv-{conversationID}` + `x-openclaw-session-key` header |
| 会话关闭 | `DELETE /v1/sessions/{key}` 在删除会话时调用 |
| 远程控制台 | SSH 隧道 `ssh -Nf -L 18789:127.0.0.1:18789 root@47.95.244.175` |

**新增文件:**

| 文件 | 说明 |
|------|------|
| `/etc/systemd/system/openclaw.service` | OpenClaw 守护进程 |
| `/root/.openclaw/openclaw.json` | OpenClaw 配置（模型/网关/Agent） |

**Go 代码变更:**

| 文件 | 改动 |
|------|------|
| `models.go` | 新增 `OpenClawSessionStore`、`App.openclawSessions`、`ChatCompletionRequest.User` |
| `main.go` | 新增 `initOpenClaw()`、`callOpenClawStream()`、`closeOpenClawSession()` |
| `store.go` | 新增 `newOpenClawSessionStore()` / `Get` / `Set` / `Delete` |
| `handlers.go` | WS 消息路由走 OpenClaw，删除会话时同步关闭 OpenClaw session |

**环境变量新增:**

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `OPENCLAW_ENABLED` | `false` | 是否启用 OpenClaw 网关路由 |
| `OPENCLAW_BASE_URL` | `http://127.0.0.1:18789` | OpenClaw 网关地址 |
| `OPENCLAW_AUTH_TOKEN` | (必填) | 网关鉴权令牌 |

**回退机制：** `OPENCLAW_ENABLED != true` 或 `OPENCLAW_AUTH_TOKEN` 为空时，自动使用 `callDeepSeekStream` 直连，完全兼容已有行为。

**内存占用:**

| 进程 | 内存 |
|------|------|
| OpenClaw | ~168 MB |
| chat-server | ~20 MB |
| MySQL | ~200 MB |
| Caddy | ~30 MB |
| **总计** | **~420 MB / 1.6 GB** |

**验证结果:**
- ✅ OpenClaw 安装 + systemd 开机自启
- ✅ `curl http://127.0.0.1:18789/healthz` → `{"ok":true}`
- ✅ OpenClaw API 调用 DeepSeek 返回正常
- ✅ SSE 流式响应格式与 DeepSeek 完全兼容
- ✅ Session 一致性：同一 `user` 字段多轮对话保持上下文
- ✅ `cached_tokens` 命中率接近 100%（同 session 重复 prompt 前缀）
- ✅ chat-assistant 删除会话 → OpenClaw session 同步关闭
- ✅ `OPENCLAW_ENABLED=false` 时回退直连 DeepSeek（日志: "OpenClaw 未启用，使用直连 DeepSeek"）
- ✅ SSH 隧道远程访问 OpenClaw 控制台正常

### 2026-06-10 — 移除联网搜索 + OpenClaw 优化

**背景：** 联网搜索结果通过 system prompt 注入，但 OpenClaw Agent 人格会覆盖搜索指令，导致搜索结果无效。同时 OpenClaw 自身维护 session 上下文，无需每次发送完整对话历史。

**改动：**

| 类型 | 文件 | 说明 |
|------|------|------|
| 移除 | `index.html` | 删除搜索 Toggle UI（🌐 联网按钮） |
| 移除 | `app.js` | 删除 `searchEnabled`、搜索 toggle 事件、`search_results` 消息处理、`addSearchResultsBubble()` |
| 移除 | `style.css` | 删除 `.search-toggle`、`.search-result-*` 全部样式 |
| 简化 | `handlers.go` | 删除 `searchWeb()` 调用、搜索结果发送、`searchMsgNoResults()` |
| 简化 | `main.go` | `callOpenClawStream` 移除 `searchResults`/`onSearchResults` 参数，只发 system+当前消息 |
| 保留 | `search.go` | 文件保留作为参考，不再被调用 |

**OpenClaw 消息优化：**
- 不再每次发送完整对话历史（OpenClaw session 自行维护上下文）
- 仅发送 `system prompt` + `当前用户消息`，大幅减少 token 开销
- 修复空 history 导致发送空消息的 Bug（`userMessage` 独立传参）

**验证结果:**
- ✅ 输入区干净无搜索按钮
- ✅ 发送消息 → AI 正常流式回复
- ✅ OpenClaw session 上下文保持正常
- ✅ token 消耗显著减少

## 当前功能

- [x] 多客户端 WebSocket 实时通信（+ 30s Ping 保活）
- [x] 多会话管理（最多 3 个，MySQL 持久化，登出保留）
- [x] 会话切换、重命名、删除 + 聊天记录持久化
- [x] ~~服务端 Echo 回显~~ → DeepSeek V4 Pro AI 对话
- [x] **OpenClaw AI Agent 网关集成**（Agent 能力层 + 多通道扩展基础）
- [x] **OpenClaw Session 同步**（会话创建/复用/关闭与 chat-assistant 一致）
- [x] 连接状态指示器
- [x] 断线消息队列 + Toast 通知
- [x] 自动重连（指数退避，发送消息时立即重连）
- [x] 响应式设计（移动端适配）
- [x] Enter 发送、Shift+Enter 换行
- [x] 深色主题 UI
- [x] 用户登录/登出（bcrypt + Session Cookie）
- [x] 受保护路由（未登录自动跳转登录页）
- [x] 远程 OpenClaw 控制台（SSH 隧道）

- [x] 多客户端 WebSocket 实时通信（+ 30s Ping 保活）
- [x] 多会话管理（最多 3 个，MySQL 持久化，登出保留）
- [x] 会话切换、重命名、删除 + 聊天记录持久化
- [x] ~~服务端 Echo 回显~~ → DeepSeek V4 Pro AI 对话
- [x] 连接状态指示器
- [x] 断线消息队列 + Toast 通知
- [x] 自动重连（指数退避，发送消息时立即重连）
- [x] 响应式设计（移动端适配）
- [x] Enter 发送、Shift+Enter 换行
- [x] 深色主题 UI
- [x] **🔍 联网搜索**（Bing 内置 + Serper.dev + 自定义 API，零配置可用）
- [x] 用户登录/登出（bcrypt + Session Cookie）
- [x] 受保护路由（未登录自动跳转登录页）

## 后续开发路线

### Phase 1: AI 接入 ✅
- [x] 对接 DeepSeek API（OpenAI 兼容接口）
- [x] 实现流式响应（`stream_start/chunk/end` 协议）
- [x] 系统 Prompt 管理
- [x] 断线消息队列 + Toast 通知

### Phase 2: 功能增强
- [x] ~~消息持久化~~ → MySQL 8.4 已部署，待 Go 后端接入
- [x] ~~多会话管理~~ → MySQL 持久化 + 侧边栏 UI
- [x] ~~联网搜索~~ → Bing 内置 + Serper.dev + 自定义 API，零配置可用
- [x] ~~OpenClaw AI Agent 网关~~ → Agent 能力层 + Session 同步 + 多通道扩展基础
- [ ] Markdown 渲染 + 代码高亮
- [ ] 消息编辑 / 删除
- [ ] 文件上传（图片等）

### Phase 3: 工程化
- [x] ~~用户认证~~ → bcrypt + Session Cookie 已实现
- [ ] HTTPS 支持（Caddy 自动证书）
- [ ] 速率限制
- [ ] Docker 化部署
- [ ] CI/CD 自动部署
- [ ] 多通道接入（Telegram / 微信 / 飞书等，通过 OpenClaw）

## 维护命令

```bash
# 查看服务状态
systemctl status chat-assistant

# 查看日志
journalctl -u chat-assistant -f

# 重启服务
systemctl restart chat-assistant

# 停止服务
systemctl stop chat-assistant

# 重新部署（本地执行）
GOOS=linux GOARCH=amd64 go build -o chat-server .
ssh root@47.95.244.175 'systemctl stop chat-assistant'
scp chat-server root@47.95.244.175:/opt/chat-assistant/
scp -r static/* root@47.95.244.175:/opt/chat-assistant/static/
ssh root@47.95.244.175 'systemctl start chat-assistant'

# MySQL 维护
mysql -u root chat_assistant                      # root 管理（服务器本地）
mysql -u chat_app -p chat_assistant               # 应用用户登录
systemctl status mysql                             # MySQL 服务状态
systemctl restart mysql                            # 重启 MySQL
journalctl -u mysql -f                             # MySQL 日志
```
