# 开发日志 - AI 助手聊天窗口

## 项目概览

- **项目名称**: Chat Assistant (AI 助手)
- **部署地址**: http://47.95.244.175
- **服务器**: 阿里云 ECS, Ubuntu 26.04 LTS
- **启动时间**: 2026-06-04

## 技术架构

```
┌──────────┐    ┌──────────┐    ┌───────────────┐    ┌──────────────┐
│  浏览器   │───▶│  Caddy   │───▶│ Go Chat Server │───▶│    MySQL     │
│ (Chat UI) │◀───│ (Port 80)│◀───│  (Port 8080)  │◀───│ (127.0.0.1)  │
└──────────┘    └──────────┘    └───────────────┘    └──────────────┘
  WebSocket      Reverse Proxy   WebSocket + HTTP      仅本地访问
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

## 当前功能

- [x] 多客户端 WebSocket 实时通信
- [x] 消息广播（所有连接客户端可见）
- [x] ~~服务端 Echo 回显~~ → DeepSeek V4 Pro AI 对话
- [x] 连接状态指示器
- [x] 自动重连（指数退避，最大 30s）
- [x] 响应式设计（移动端适配）
- [x] Enter 发送、Shift+Enter 换行
- [x] 深色主题 UI
- [x] 用户登录/登出（bcrypt + Session Cookie）
- [x] 受保护路由（未登录自动跳转登录页）

## 后续开发路线

### Phase 1: AI 接入
- [ ] 对接 DeepSeek API（或其他 LLM API）
- [ ] 实现流式响应（`stream_start/chunk/end` 协议）
- [ ] 添加系统 Prompt 管理

### Phase 2: 功能增强
- [x] ~~消息持久化~~ → MySQL 8.4 已部署，待 Go 后端接入
- [ ] 多会话管理
- [ ] Markdown 渲染 + 代码高亮
- [ ] 消息编辑 / 删除
- [ ] 文件上传（图片等）

### Phase 3: 工程化
- [x] ~~用户认证~~ → bcrypt + Session Cookie 已实现
- [ ] HTTPS 支持（Caddy 自动证书）
- [ ] 速率限制
- [ ] Docker 化部署
- [ ] CI/CD 自动部署

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
scp chat-server root@47.95.244.175:/opt/chat-assistant/
scp -r static/* root@47.95.244.175:/opt/chat-assistant/static/
ssh root@47.95.244.175 'systemctl restart chat-assistant'

# MySQL 维护
mysql -u root chat_assistant                      # root 管理（服务器本地）
mysql -u chat_app -p chat_assistant               # 应用用户登录
systemctl status mysql                             # MySQL 服务状态
systemctl restart mysql                            # 重启 MySQL
journalctl -u mysql -f                             # MySQL 日志
```
